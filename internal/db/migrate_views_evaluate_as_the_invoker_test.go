package db_test

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestViewsEvaluateAsTheInvoker(t *testing.T) {
	conn := migratedConn(t)

	rows, err := conn.Query(context.Background(), `
		SELECT c.relname, coalesce(array_to_string(c.reloptions, ','), '')
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind = 'v'
		   AND NOT EXISTS (SELECT 1 FROM pg_depend d
		                    WHERE d.objid = c.oid
		                      AND d.classid = 'pg_class'::regclass
		                      AND d.deptype = 'e')
		 ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	checked := 0
	for rows.Next() {
		var name, opts string
		if err := rows.Scan(&name, &opts); err != nil {
			t.Fatal(err)
		}
		checked++
		if !slices.Contains(strings.Split(opts, ","), "security_invoker=true") {
			t.Errorf("视图 %s 的 reloptions 是 %q，缺 security_invoker=true——"+
				"它会以属主权限求值，底层表的 RLS 按属主判定，"+
				"于是这张视图对每一个租户都吐出**全部**租户的行。"+
				"实测两个商家时它多出的正好是别家那一行：不报错、不是空集，"+
				"只是安静地多一行。改法是 CREATE VIEW %s WITH (security_invoker = true) AS ...",
				name, opts, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// 阳性对照。这条测试是「对每一张视图断言」的形状，而空集上它恒真——
	// 视图被改名、被挪到别的 schema、或者查询里那个 relkind 打错一个字母，
	// 症状都是「一张视图都没枚举到」，而那时它已经不在检查任何东西了。
	if checked == 0 {
		t.Fatal("一张（非扩展自有的）视图都没枚举到——这个检查本身失效了")
	}
}
