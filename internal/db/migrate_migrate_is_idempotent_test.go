package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/testdb"
)

func TestMigrateIsIdempotent(t *testing.T) {
	// 不走 migrate()：那条在版本已对齐时会跳过 goose，而这正是要被跑两遍的东西。
	for i := 0; i < 2; i++ {
		if err := testdb.Reset(context.Background()); err != nil {
			t.Fatal(err)
		}
		out, err := testdb.Migrate(context.Background())
		if err != nil {
			t.Fatalf("第 %d 次迁移失败: %v\n%s", i+1, err, out)
		}
	}

	conn, err := db.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())

	// current_merchant() 必须是 STABLE —— IMMUTABLE 会被折进缓存计划，
	// 导致同一条预编译语句对所有租户返回第一个租户的数据。
	var volatility string
	err = conn.QueryRow(context.Background(),
		`SELECT provolatile FROM pg_proc WHERE proname = 'current_merchant'`).
		Scan(&volatility)
	if err != nil {
		t.Fatal(err)
	}
	if volatility != "s" {
		t.Fatalf("current_merchant() 的 volatility 是 %q，必须是 \"s\"(STABLE)", volatility)
	}

	// 凡是该有租户策略的表，都必须同时 ENABLE 且 FORCE —— 只 ENABLE 的话
	// 表属主绕过 RLS，而迁移工具跑出来的属主通常就是应用自己。
	man := loadManifest(t)
	for _, tbl := range allTables(t, conn) {
		spec, _ := man.class(tbl)
		if spec.Policy == "none" {
			continue
		}
		var enabled, forced bool
		err := conn.QueryRow(context.Background(),
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`,
			tbl).Scan(&enabled, &forced)
		if err != nil {
			t.Fatal(err)
		}
		if !enabled || !forced {
			t.Fatalf("%s: ENABLE=%v FORCE=%v，两者都必须为 true", tbl, enabled, forced)
		}
	}
}

type policy struct{ name, permissive, cmd, qual, withCheck string }

// checkDirectoryLogPolicies 是 directory-log 类（merchant_revisions）的策略断言。
// 理由写在调用处。期望值写死在这里而不是清单里：这两条谓词就是这个类别本身，
// 清单里的类别说明已经逐字写着它们。
func checkDirectoryLogPolicies(t *testing.T, tbl string, got []policy) {
	t.Helper()
	want := []policy{
		// pg_policies 按 policyname 排序（policiesOf 里的 ORDER BY）。
		{name: "directory_read", permissive: "PERMISSIVE", cmd: "SELECT", qual: "true", withCheck: ""},
		{name: "platform_write", permissive: "PERMISSIVE", cmd: "INSERT", qual: "", withCheck: "platform_scope()"},
	}
	if len(got) != len(want) {
		t.Errorf("%s: 期望恰好 %d 条策略（directory_read / platform_write），实际 %d 条: %+v",
			tbl, len(want), len(got), got)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: 第 %d 条策略是 %+v，期望 %+v —— 写侧一旦放宽，"+
				"租户作用域里就能改商家目录（停用别家店），而读侧看不出任何异常",
				tbl, i+1, got[i], want[i])
		}
	}
}

// checkPlatformCurrentPolicies 是 platform-current 类（merchant_domains，00340）的
// 策略断言。它是 directory-log 的孪生，多的那一条是 DELETE：这张表是当前态而不是
// 日志，「摘掉一家店的域名」必须能表达，而它的写法是删掉那一行。
//
// 三条都要逐字钉死，少任何一条都是一条静默的域名劫持路径：
//
//	· 把 read 的 cmd 从 SELECT 放宽成 ALL，USING (true) 于是同时充当写谓词；
//	· 把 insert 的 WITH CHECK 放宽成 true，商家级会话就能把自己登记成别家的域名；
//	· 把 delete 的 USING 放宽成 true，任何一条上下文都能摘掉别家店的入口
//	  ——那是一次精确的 DoS，而且读侧一切照旧。
//
// 多出第四条 permissive 策略同理（策略之间是 OR）。
func checkPlatformCurrentPolicies(t *testing.T, tbl string, got []policy) {
	t.Helper()
	want := []policy{
		// pg_policies 按 policyname 排序（policiesOf 里的 ORDER BY）。
		{name: "platform_current_delete", permissive: "PERMISSIVE", cmd: "DELETE", qual: "platform_scope()", withCheck: ""},
		{name: "platform_current_insert", permissive: "PERMISSIVE", cmd: "INSERT", qual: "", withCheck: "platform_scope()"},
		{name: "platform_current_read", permissive: "PERMISSIVE", cmd: "SELECT", qual: "true", withCheck: ""},
	}
	if len(got) != len(want) {
		t.Errorf("%s: 期望恰好 %d 条策略（read / insert / delete），实际 %d 条: %+v",
			tbl, len(want), len(got), got)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: 第 %d 条策略是 %+v，期望 %+v —— 这三条里任何一条放宽，"+
				"商家级会话就能改别人的域名入口，而按域名解析一切照旧",
				tbl, i+1, got[i], want[i])
		}
	}
}

func policiesOf(t *testing.T, conn *pgx.Conn, tbl string) []policy {
	rows, err := conn.Query(context.Background(),
		`SELECT policyname, permissive, cmd,
		        coalesce(qual, ''), coalesce(with_check, '')
		   FROM pg_policies
		  WHERE schemaname = 'public' AND tablename = $1
		  ORDER BY policyname`, tbl)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []policy
	for rows.Next() {
		var p policy
		if err := rows.Scan(&p.name, &p.permissive, &p.cmd, &p.qual, &p.withCheck); err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// RLS 由三个元素共同成立：①current_merchant() ②表上的 ENABLE+FORCE ③策略本身。
// 上面那个测试覆盖了 ①②，但 ③ 漏了——而 ③ 恰恰是最容易被「优化」掉的那个：
// 将来有人为了修一个「查不到数据」的 bug，会去放宽谓词或直接 DROP POLICY，
// 前两条断言对此一声不吭，库照样漏。
//
// 这里逐表钉死：有且只有一条名为 tenant 的策略，作用于 ALL 而不只是 SELECT，
// 且写谓词没有被单独放开。
//
// 读侧和写侧要分别断言，它们是两件事：
//
//   - cmd='ALL' 只保证策略「作用于」写入。改成 FOR SELECT 的话写入侧就没人管了，
//     这条断言会红。
//   - 但作用于写入不等于写入用的是租户谓词。写入实际套用的是 with_check，
//     省略时才回退到 using。所以 `ALTER POLICY ... WITH CHECK (true)` 能在
//     policyname / qual / cmd / permissive 四项全部不变的情况下，把写入侧整个放开——
//     租户 A 可以往租户 B 名下插行，而读侧一切正常。
//
// 因此 with_check 要么为空（回退到上面已经钉死的 qual），要么必须等于同一个谓词。
//
// **谓词的形状由 db/tenancy.json 的类别决定，不是所有表共用一个常量。**
// 留出按类别不同的余地是必需的：inventories 按规矩一豁免了 merchant_id
// （见数据模型 §4），它的策略谓词是一个对 skus 的 EXISTS 子查询，
// 和 tenant 类各表的列比较不是一个形状。而没在清单里登记的表一律按最严的
// tenant 类查——M2 新建一张表时，测试会红在「谓词对不上」或「没有策略」上，
// 逼作者去清单里把这张表的隔离方式**写下来**。
