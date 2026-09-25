package db_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// db/tenancy.json 是多租户闸门的单一真相源，这里是它的 Go 侧读取器。
//
// 为什么要有这个文件：此前 migrate_test.go 里的 notBusinessTables 有 3 张表，
// scripts/check_tenancy.py 的 EXEMPT 有 7 张，两份清单谁也不知道对方豁免了什么。
// 后果是具体的：M2 接 dtmrs 会建出 barrier（跨租户基础设施，本就不该有租户策略），
// 它在 Python 侧早就豁免了，Go 侧却会当场红；order_status_transitions /
// refund_status_transitions 同理。**两头各自成立、接起来不成立。**
//
// 所以两边现在读同一份 JSON。Go 侧不解析设计文档，Python 侧不连数据库，
// 但「哪张表属于哪一类」这个判断只有一份。
//
// 用 JSON 而不是 TOML/YAML：Go 与 Python 的标准库都直接能读，
// 一个为了测试固件而进主模块 go.mod 的依赖是不划算的。
// 代价是 JSON 没有注释语法，所以说明文字走 "_readme" / "_" / "reason" 字段——
// 凡是以下划线开头的键都是说明，不是条目。

const manifestPath = "../../db/tenancy.json"

type tenancyClass struct {
	// "required" = 必须有 merchant_id 列；"absent" = 必须没有；
	// "special"  = 这张表的租户列形态特殊（merchants 根本没有这一列，
	//              shop_settings 的主键就是它），不做机械判断。
	MerchantID string `json:"merchant_id"`
	// "column" = 策略谓词是 merchant_id 的列比较；
	// "parent" = 谓词是对父表的 EXISTS 子查询；
	// "none"   = 不该有任何策略。
	Policy string `json:"policy"`
	// 规矩三（唯一约束收进租户内）是否适用。
	UniqueScoped bool     `json:"unique_scoped"`
	Grants       []string `json:"grants"`
}

type tenancyTable struct {
	Class  string `json:"class"`
	Parent string `json:"parent"`
	Via    string `json:"via"`
	// 逐表覆盖类别默认的 GRANT 面；nil 表示吃类别默认值。
	Grants []string `json:"grants"`
	Reason string   `json:"reason"`

	// Documented 为显式的 false 时，表示这张表**刻意**不在设计文档里
	// （goose 的迁移记录表那种）。指针是为了区分「写了 false」与「没写」。
	Documented *bool `json:"documented"`
	// DocumentedOnly 表示文档里有、库里还没有。表一旦真的建出来，
	// 这个标记就该摘掉——TestEveryTableInTheDatabaseIsDocumented 会催。
	DocumentedOnly bool `json:"documented_only"`
}

type tenancyManifest struct {
	PolicyName  string                  `json:"policy_name"`
	Classes     map[string]tenancyClass `json:"classes"`
	Tables      map[string]tenancyTable `json:"tables"`
	FKMissingOK map[string]any          `json:"fk_missing_ok"`
	FKSingleOK  map[string]any          `json:"fk_single_column_ok"`
	UniqueOK    map[string]any          `json:"unique_global_ok"`
}

// class 返回这张表的类别规格。清单里没登记的表一律按 tenant 查——
// 默认值必须是最严的那一个，否则「忘了登记」就成了一条静默的豁免。
func (m tenancyManifest) class(table string) (tenancyClass, tenancyTable) {
	e := m.Tables[table]
	if e.Class == "" {
		e.Class = "tenant"
	}
	return m.Classes[e.Class], e
}

// grants 返回这张表上 keel_app 应有的权限集合。
func (m tenancyManifest) grants(table string) []string {
	c, e := m.class(table)
	if e.Grants != nil {
		return e.Grants
	}
	return c.Grants
}

// exempt 判断 key 是否在某张豁免表里。以下划线开头的键是说明文字，不算条目。
func exempt(m map[string]any, key string) bool {
	if strings.HasPrefix(key, "_") {
		return false
	}
	_, ok := m[key]
	return ok
}

func loadManifest(t *testing.T) tenancyManifest {
	t.Helper()
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("读不到 %s: %v", filepath.Clean(manifestPath), err)
	}
	var m tenancyManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s 不是合法 JSON: %v", manifestPath, err)
	}
	// 清单被读空、或字段被改名之后，下面每一条断言都会静默地「无事可查」。
	// 那不是通过，是没在检查。
	if m.PolicyName == "" || len(m.Classes) == 0 || len(m.Tables) == 0 {
		t.Fatalf("%s 解析出来是空的（policy_name=%q classes=%d tables=%d）——"+
			"清单的字段名改了而读取器没跟上时，下面所有断言都会变成空转",
			manifestPath, m.PolicyName, len(m.Classes), len(m.Tables))
	}
	for name, e := range m.Tables {
		if _, ok := m.Classes[e.Class]; !ok {
			t.Fatalf("%s: 表 %s 的类别 %q 不存在", manifestPath, name, e.Class)
		}
		if e.Reason == "" {
			t.Fatalf("%s: 表 %s 没写豁免理由——往清单里加表是一个需要解释的动作",
				manifestPath, name)
		}
	}
	return m
}

// parentTableOf 把形如 sku_id 的列名解析成它该指向的父表名（skus）。
// 只认能直接拼出表名的几种复数形式；推不出来一律返回 ""，当作「这不是外键」。
//
// 宁可漏报也不误报：误报会逼着往豁免清单里加一堆根本不是外键的列
// （target_id、cluster_id、biz_id、trace_id…），而清单一旦变成噪音，
// 人就不再逐条读它了，那等于清单不存在。
//
// **这段规则在 scripts/check_tenancy.py 的 parent_of() 里有一份等价实现。**
// 两边各自作用于不同的输入（真库的系统目录 / 设计文档的 DDL），
// 没法共用代码；改其中一边请把另一边一起改。
func parentTableOf(col string, exists map[string]bool) string {
	if !strings.HasSuffix(col, "_id") {
		return ""
	}
	stem := strings.TrimSuffix(col, "_id")
	cands := []string{stem + "s", stem + "es"}
	if strings.HasSuffix(stem, "y") {
		cands = append(cands, strings.TrimSuffix(stem, "y")+"ies")
	}
	for _, c := range cands {
		if exists[c] {
			return c
		}
	}
	return ""
}
