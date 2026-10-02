package repository_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 微服务拆分阶段 1b 之后，core 与库存服务可以跑在两个库上 —— 前提是两边的 SQL 各守各的表：
//
//   - db/queries/inventory_svc.sql（库存服务仓储唯一的 SQL 来源）只碰 inventories /
//     inventory_logs / activity_stocks / activity_sync_revs（00240）/ channel_merchants（00300）；
//   - 其余每一个查询文件（core）一个字都不提这三张表。
//
// 两库集成测试（internal/handler 的 two_db_test.go）在运行期证明了「core 的库里那三张表
// 一行都没被读写」，但它只覆盖跑到的路径；这一条从源头扫全部查询，一条新写的、没有测试
// 覆盖的 JOIN inventories 也逃不掉。只扫可执行行（注释里提到表名是正常的）。
var (
	inventoryTables = map[string]bool{"inventories": true, "inventory_logs": true, "activity_stocks": true, "activity_sync_revs": true, "channel_merchants": true}
	tableRefRE      = regexp.MustCompile(`(?i)\b(?:FROM|JOIN|INTO|UPDATE)\s+([a-z_][a-z0-9_]*)`)
	wordRE          = regexp.MustCompile(`[a-z_][a-z0-9_]*`)
)

func executableSQL(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func TestQueryFilesStayOnTheirSideOfTheSplit(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "db", "queries", "*.sql"))
	if err != nil || len(files) < 20 {
		t.Fatalf("db/queries 只找到 %d 个文件 —— 这条测试没在检查它该检查的东西（%v）", len(files), err)
	}
	sawInventory := false
	for _, f := range files {
		sql := executableSQL(t, f)
		if filepath.Base(f) == "inventory_svc.sql" {
			sawInventory = true
			refs := tableRefRE.FindAllStringSubmatch(sql, -1)
			if len(refs) < 10 {
				t.Fatalf("inventory_svc.sql 只解析出 %d 处表引用 —— 解析失效了", len(refs))
			}
			for _, m := range refs {
				name := strings.ToLower(m[1])
				// CTE 名（cur / wrote）不是表，「DO UPDATE SET」里的 set 也不是；库存服务自己的表之外只许出现 CTE。
				// 放行的不是表：cur / wrote / set 与 req_skus / day_idx / day_ends 是 CTE 名（后三个是
				// InvStockoutDays 的），generate_series 是函数。
				if inventoryTables[name] || name == "cur" || name == "wrote" || name == "set" ||
					name == "req_skus" || name == "day_idx" || name == "day_ends" || name == "generate_series" {
					continue
				}
				t.Errorf("inventory_svc.sql 引用了 %q —— 库存服务的仓储只许碰 inventories / inventory_logs / "+
					"activity_stocks / activity_sync_revs / channel_merchants；拆分部署下库存库里根本没有别的表", name)
			}
			continue
		}
		for _, w := range wordRE.FindAllString(strings.ToLower(sql), -1) {
			if inventoryTables[w] {
				t.Errorf("%s 的可执行 SQL 里出现了 %q —— core 不许碰库存服务的表（微服务拆分阶段 1b）："+
					"要库存的数请经 inventory.Service 批量问", filepath.Base(f), w)
			}
		}
	}
	if !sawInventory {
		t.Fatal("没找到 db/queries/inventory_svc.sql")
	}
}
