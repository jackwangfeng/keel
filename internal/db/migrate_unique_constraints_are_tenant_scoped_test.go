package db_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestUniqueConstraintsAreTenantScoped(t *testing.T) {
	conn := migratedConn(t)
	man := loadManifest(t)

	rows, err := conn.Query(context.Background(), `
		SELECT c.relname AS tbl, i.relname AS idx,
		       (SELECT array_agg(a.attname ORDER BY k.ord)
		          FROM unnest(ix.indkey::int2[]) WITH ORDINALITY k(att, ord)
		          JOIN pg_attribute a
		            ON a.attrelid = c.oid AND a.attnum = k.att) AS cols
		  FROM pg_index ix
		  JOIN pg_class i ON i.oid = ix.indexrelid
		  JOIN pg_class c ON c.oid = ix.indrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind = 'r' AND ix.indisunique
		 ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	used := map[string]bool{}
	checked := 0
	for rows.Next() {
		var tbl, idx string
		var cols []string
		if err := rows.Scan(&tbl, &idx, &cols); err != nil {
			t.Fatal(err)
		}
		spec, _ := man.class(tbl)
		if !spec.UniqueScoped {
			continue // 没有租户维度的类别，「收进租户内」无从谈起
		}
		if len(cols) == 0 {
			t.Errorf("%s 上的唯一索引 %s 解析不出列（表达式索引？）——"+
				"这条规矩对它失效了，请手工判断并在清单里登记", tbl, idx)
			continue
		}
		checked++
		if cols[0] == "merchant_id" || cols[0] == "id" {
			continue
		}
		key := fmt.Sprintf("%s(%s)", tbl, strings.Join(cols, ","))
		if exempt(man.UniqueOK, key) {
			used[key] = true
			continue
		}
		t.Errorf("唯一索引 %s 是 %s，首列不是 merchant_id——"+
			"全局唯一的东西在多租户下几乎都是错的（两家店各有一个 "+
			"sku_code = \"A001\" 是正常的）。收进租户内，"+
			"或把 %s 登记进 db/tenancy.json 的 unique_global_ok 并写明理由",
			idx, key, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("一条唯一索引都没查到——这个检查本身失效了")
	}

	live := map[string]bool{}
	for _, tbl := range allTables(t, conn) {
		live[tbl] = true
	}
	for key := range man.UniqueOK {
		if strings.HasPrefix(key, "_") || used[key] {
			continue
		}
		if tbl, _, ok := strings.Cut(key, "("); ok && live[tbl] {
			t.Errorf("unique_global_ok 里的 %s 在库里不存在，该清理了", key)
		}
	}
}

// keel_app 的 GRANT 面必须和清单里的类别一一对上。
//
// migrate_test.go 原先把 merchants / shop_settings 豁免出 RLS 检查，理由写着
// 「它们的防护靠的是 keel_app 的 GRANT 面与解析层自身」。**实测那个 GRANT 面
// 是 SELECT/INSERT/UPDATE/DELETE 全给**，也就是说那句注释是一句没有兑现的话：
//
//	租户 1 的上下文里改租户 2 的 domain（域名劫持）→ UPDATE 1
//	租户 1 的上下文里把租户 2 停用（全站 DoS）     → UPDATE 1
//	完全没有租户上下文时改 merchants               → UPDATE 1
//
// M1 不可利用（一个写接口都没有），但 M2 之后一旦有「商家改自己的店铺设置」
// 这类接口，它就是域名劫持加一键停掉别家店。
//
// 00005 把写权限收掉了。而修复本身也需要闸门守着——否则下一个人一句
// `GRANT ALL ON merchants TO keel_app` 就能把它退回去，且没有任何测试会红。
// 顺带钉死的还有 ALTER DEFAULT PRIVILEGES 的收窄：它原先让**每一张将来新建的表**
// 自动拿到四权，包括 order_status_transitions 这类全租户共用的静态参考数据
// ——任何租户都能改状态机，而那比跨租户读取更难发现。
