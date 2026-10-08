package db_test

import (
	"context"
	"testing"
)

func TestForeignKeysAreNotSilentlyMissing(t *testing.T) {
	conn := migratedConn(t)
	man := loadManifest(t)

	tables := allTables(t, conn)
	exists := map[string]bool{}
	for _, tbl := range tables {
		exists[tbl] = true
	}

	// 被任意一条外键引用到的 表.列。
	covered := map[string]bool{}
	for _, fk := range foreignKeys(t, conn) {
		for _, c := range fk.cols {
			covered[fk.from+"."+c] = true
		}
	}

	rows, err := conn.Query(context.Background(), `
		SELECT c.relname, a.attname
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid = a.attrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind = 'r'
		   AND a.attnum > 0 AND NOT a.attisdropped
		 ORDER BY 1, a.attnum`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	resolved := 0
	for rows.Next() {
		var tbl, col string
		if err := rows.Scan(&tbl, &col); err != nil {
			t.Fatal(err)
		}
		parent := parentTableOf(col, exists)
		if parent == "" || parent == tbl {
			continue
		}
		resolved++
		if covered[tbl+"."+col] {
			continue
		}
		key := tbl + "." + col
		if exempt(man.FKMissingOK, key) {
			continue
		}
		t.Errorf("%s 指向 %s 却没有任何外键约束——违规做得更彻底反而绕过了规矩二："+
			"A 商家的行可以引用 B 商家的行，数据库不拒、闸门不红。"+
			"补成 FOREIGN KEY (%s, merchant_id) REFERENCES %s(id, merchant_id)，"+
			"或把 %s 登记进 db/tenancy.json 的 fk_missing_ok 并写明理由",
			key, parent, col, parent, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// 一条 <x>_id → 父表 都没解析出来，说明解析规则跟命名约定脱钩了，
	// 这个检查从此对任何缺失外键都视而不见。
	if resolved == 0 {
		t.Fatal("一列都没解析到父表——这个检查本身失效了")
	}
}

// 规矩三：唯一约束一律收进租户内。
//
// 规矩一有 check_tenancy.py、规矩二有上面两条测试，规矩三在设计文档里被引用
// 七八次却一直没有任何闸门。今天库里合规，但 M2 的 orders.order_no、
// payments.channel_txn_id、idempotency_keys 全靠人记得——而它们恰好都是
// 「全局唯一是错的」的典型：两个商家各自有一个 sku_code = "A001" 完全正常，
// 而一条全局唯一约束会让**后建的那家店建不了商品**，报错还指向一个
// 自己根本看不见的行。
//
// 两条自动放行，见 db/tenancy.json 里的说明：首列是 merchant_id（规矩三本身），
// 首列是本表的代理主键 id（按构造就全局唯一，`UNIQUE (id, merchant_id)` 这种
// 给子表复合外键当落点的索引一律属于这一类）。
