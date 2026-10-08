package db_test

import (
	"context"
	"os"
	"regexp"
	"testing"

	"github.com/keel/keel/internal/db"
)

func TestEveryTableInTheDatabaseIsDocumented(t *testing.T) {
	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	conn, err := db.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())

	manifest := loadManifest(t)

	const schemaDoc = "../../docs/电商系统-数据模型设计.md"
	doc, err := os.ReadFile(schemaDoc)
	if err != nil {
		t.Fatal(err)
	}
	// 只认 DDL，不认散文里提到的表名 —— 「§14 用一段散文补丁修改 §13 的 DDL」
	// 那种写法躲过了所有机械检查，正是这里不该重蹈的。
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile(
		`(?m)^CREATE TABLE (?:IF NOT EXISTS )?([a-z_]+)\s*\(`).FindAllSubmatch(doc, -1) {
		documented[string(m[1])] = true
	}
	if len(documented) < 30 {
		t.Fatalf("设计文档里只解析出 %d 张表的 DDL，这个检查本身失效了", len(documented))
	}

	rows, err := conn.Query(context.Background(),
		`SELECT c.relname FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = 'public' AND c.relkind = 'r' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	live := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		live[name] = true

		entry := manifest.Tables[name]
		exempt := entry.Documented != nil && !*entry.Documented
		if !documented[name] && !exempt {
			t.Errorf("表 %s 在库里，但设计文档里没有它的 CREATE TABLE——"+
				"要么把 DDL 补进文档（CONTRIBUTING 硬规矩二：表结构的唯一真相源是"+
				"数据模型文档），要么在 db/tenancy.json 里写 \"documented\": false 并说明理由",
				name)
		}
		if exempt && entry.Reason == "" {
			t.Errorf("表 %s 标了 documented:false 却没写理由", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// documented_only 是「文档里有、库里还没有」。表真的建出来之后这个标记就过期了，
	// 留着会让下一个人以为它还没落地。
	for name, entry := range manifest.Tables {
		if entry.DocumentedOnly && live[name] {
			t.Errorf("表 %s 在 db/tenancy.json 里还标着 documented_only，"+
				"但它已经建出来了——把这个标记摘掉", name)
		}
	}
}

// 库里的每一张视图都必须带 WITH (security_invoker = true)。
//
// ===========================================================================
// 为什么这条检查此前不存在，以及它挡的是什么
// ===========================================================================
//
// 本包的七条租户检查（RLS 策略、复合外键、缺失外键、唯一索引、GRANT 面、
// updated_at 触发器、文档覆盖）**全部写着 relkind = 'r'** —— 实测七处。
// 也就是说视图一张都不在它们的视野里。在 00020 之前这不要紧，因为库里一张视图
// 都没有；00020 建出了 sku_prices_by_store，那个盲区当场变成一个真的洞。
//
// 洞的形状：PostgreSQL 的视图**默认以视图属主的权限求值**，于是底层表的 RLS
// 策略按属主判定，而迁移跑出来的属主通常就是应用自己。实测（PostgreSQL 16.15，
// 两个商家、各一个大区一家店、各自的 SKU，keel_app 同款的受限角色，
// SET LOCAL app.merchant_id = '1'）：
//
//	WITH (security_invoker = true) 的视图  → 商家 1 读到 4 行（对）
//	默认（属主权限）的视图                 → 商家 1 读到 5 行
//
// 多出来的那一行是**商家 2 的店 × 商家 2 的 SKU**。一行。不是报错，不是空集，
// 是安静地多出一行别人的价格 —— 只有两个商家、且刻意去数行数，才看得见。
// 这与 §2 开头那句「这类 bug 在单租户测试数据下完全看不出来」是同一个形状，
// 也是「默认值在多租户下是不安全的那一侧，而它不报错」的第三个实例
// （前两个是 FORCE ROW LEVEL SECURITY 与 current_merchant() 返回 NULL）。
//
// ===========================================================================
// 扩展自有的视图按 pg_depend 排除，不按名字排除
// ===========================================================================
//
// postgis 会在 public 下建出 geometry_columns / geography_columns 两张视图。
// 它们不由本项目定义，也不读本项目的任何一张表，谈不上跨租户泄露。
//
// 排除它们的判据是 pg_depend.deptype = 'e'（这个对象属于某个扩展），
// **不是一张名字清单**：名字清单会在下一个扩展进来时再失效一次，而那一次
// 的症状是一条本该守着东西的测试变成红的，于是下一个人往清单里再加两个名字 ——
// 清单从此是噪音。结构性的判据不会有这个问题：凡是扩展自己带的对象，
// 它的正确性由上游负责，不由本仓库的闸门负责。
