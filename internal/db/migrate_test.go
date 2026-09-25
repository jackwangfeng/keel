package db_test

import (
	"context"
	"os/exec"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
)

// migrate 通过根 Makefile 的 migrate 目标跑迁移。
//
// 不直接 exec "goose"：goose 钉在 tools/go.mod 里（见 Makefile 顶部关于工具
// 子模块的说明），PATH 上没有、也不该有一个 goose 二进制。测试调 make 这个
// 稳定入口，而不是把 `go -C tools run ...` 这条长命令抄一份到测试里——抄一份
// 就意味着以后改调用方式要改两个地方，而漏改的那个会以「本地能过 CI 红」的
// 形式暴露。
func migrate(t *testing.T) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("make", "-C", "../..", "migrate", "GOOSE_DBSTRING="+db.AdminDSN())
	return cmd.CombinedOutput()
}

// 迁移必须能在已经迁过的库上再跑一次而不报错。
// goose 靠版本表保证这一点，但 RLS 策略与函数的 CREATE 是最容易踩的地方——
// 如果有人把它们写进了会重复执行的位置，这里会红。
func TestMigrateIsIdempotent(t *testing.T) {
	for i := 0; i < 2; i++ {
		out, err := migrate(t)
		if err != nil {
			t.Fatalf("第 %d 次迁移失败: %v\n%s", i+1, err, out)
		}
	}

	// 走 db.Connect 而不是 pgx.Connect：它会当场确认这条连接不能绕过 RLS。
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

	// 每张业务表都必须同时 ENABLE 且 FORCE —— 只 ENABLE 的话表属主绕过 RLS，
	// 而迁移工具跑出来的属主通常就是应用自己。
	for _, tbl := range businessTables(t, conn) {
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

// RLS 由三个元素共同成立：①current_merchant() ②表上的 ENABLE+FORCE ③策略本身。
// 上面那个测试覆盖了 ①②，但 ③ 漏了——而 ③ 恰恰是最容易被「优化」掉的那个：
// 将来有人为了修一个「查不到数据」的 bug，会去放宽谓词或直接 DROP POLICY，
// 前两条断言对此一声不吭，库照样漏。
//
// 这里对三张表逐一钉死：有且只有一条名为 tenant 的策略，读谓词正好是
// merchant_id = current_merchant()，作用于 ALL 而不只是 SELECT，
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
// 早先的注释和 b57068f 的 commit message 都写过「cmd='ALL' 顺带把写入侧钉死」，
// 那是错的：它钉住的是「写入受不受策略管」，不是「写入用哪个谓词」。
func TestTenantPoliciesArePresentAndExact(t *testing.T) {
	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	conn, err := db.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())

	// 谓词逐表申明，而不是所有表共用一个常量。
	//
	// 这张表本身就是断言的一部分：下面会要求它和系统目录枚举出来的业务表
	// 一一对上。M2 新建一张表时，测试会红在「没申明谓词」上，逼作者把这张表
	// 的隔离方式**写下来**——而不是让它悄悄享受一条为别的表写的断言。
	//
	// 留出按表不同的余地是必需的：inventories 按规矩一豁免了 merchant_id
	// （见数据模型文档 §4），它的策略谓词是一个对 skus 的 EXISTS 子查询，
	// 和这里其余各表的列比较不是一个形状。
	wantQual := map[string]string{
		"categories": "(merchant_id = current_merchant())",
		"products":   "(merchant_id = current_merchant())",
		"skus":       "(merchant_id = current_merchant())",
	}

	tables := businessTables(t, conn)
	for _, tbl := range tables {
		if _, ok := wantQual[tbl]; !ok {
			t.Errorf("业务表 %s 没有申明期望的策略谓词——"+
				"新表必须在 wantQual 里写明它的租户隔离谓词，不能默认继承别的表的断言", tbl)
		}
	}
	for tbl := range wantQual {
		if !slices.Contains(tables, tbl) {
			t.Errorf("wantQual 里的 %s 不是（或不再是）业务表，该清理了", tbl)
		}
	}

	for _, tbl := range tables {
		want, ok := wantQual[tbl]
		if !ok {
			continue // 上面已经报过了
		}
		rows, err := conn.Query(context.Background(),
			`SELECT policyname, permissive, cmd,
			        coalesce(qual, ''), coalesce(with_check, '')
			   FROM pg_policies
			  WHERE schemaname = 'public' AND tablename = $1
			  ORDER BY policyname`, tbl)
		if err != nil {
			t.Fatal(err)
		}
		type policy struct{ name, permissive, cmd, qual, withCheck string }
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

		// 不是「至少有一条」而是「有且只有一条」：permissive 策略之间是 OR，
		// 多加一条 USING (true) 就能把隔离整个抵消掉，且不动原策略一个字。
		if len(got) != 1 {
			t.Fatalf("%s: 期望恰好 1 条策略，实际 %d 条: %+v", tbl, len(got), got)
		}
		p := got[0]
		if p.name != "tenant" {
			t.Errorf("%s: 策略名是 %q，期望 \"tenant\"", tbl, p.name)
		}
		if p.qual != want {
			t.Errorf("%s: 策略谓词是 %q，期望 %q", tbl, p.qual, want)
		}
		if p.cmd != "ALL" {
			t.Errorf("%s: 策略作用于 %q，期望 \"ALL\"——只保 SELECT 的话写入侧没人管", tbl, p.cmd)
		}
		// 空表示省略 WITH CHECK，PostgreSQL 回退到 qual，而 qual 上面刚断言过。
		// 非空就必须是同一个谓词：WITH CHECK (true) 时上面四项全部照旧通过，
		// 但租户 A 能往租户 B 名下插行。
		if p.withCheck != "" && p.withCheck != want {
			t.Errorf("%s: 策略的写谓词是 %q，期望为空（回退到读谓词）或 %q——"+
				"写谓词被单独放开时，读侧看不出任何异常", tbl, p.withCheck, want)
		}
		if p.permissive != "PERMISSIVE" {
			t.Errorf("%s: 策略是 %q", tbl, p.permissive)
		}
	}
}

// 业务表的清单从系统目录里枚举，不写死。
//
// 原先这里是 []string{"categories", "products", "skus"} 这样的字面量，出现在
// 两个测试里。它的问题不在今天对不对，而在明天：M2 会新增 inventories、orders、
// order_items……新表只要没人记得往这两个字面量里补名字，就不在任何断言的视野里，
// 而「忘了给新表挂 RLS」恰恰是最可能发生、后果最重的那种疏忽。
//
// 反过来枚举之后，新建一张表却没挂策略，测试当场变红，不依赖任何人的记性。
//
// 豁免必须写明理由——往这里加表是一个需要解释的动作，不是默认行为。
var notBusinessTables = map[string]string{
	"goose_db_version": "goose 自己的迁移记录表，不由本项目定义",

	// 下面两张是真正的例外，理由是同一个：租户解析发生在 SET LOCAL 之前。
	// 请求刚进来时还不知道是哪个租户，正是要靠读这两张表才能知道；
	// 此时 current_merchant() 为 NULL，挂上 RLS 会让解析永远查不到行，
	// 整个站点在第一跳就 404。它们的防护靠的是 keel_app 的 GRANT 面
	// （见 00003_app_role.sql）与解析层自身，不是 RLS。
	"merchants":     "租户表自身；租户解析要在确定租户之前读它",
	"shop_settings": "同上，解析期就要读，此时还没有 current_merchant()",
}

func businessTables(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	rows, err := conn.Query(context.Background(),
		`SELECT c.relname
		   FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = 'public' AND c.relkind = 'r'
		  ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var all, out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		all = append(all, name)
		if _, exempt := notBusinessTables[name]; !exempt {
			out = append(out, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// 豁免清单里写着一张库里根本没有的表，说明清单过期了——它会静默地
	// 豁免掉一张将来重名的表。
	for name := range notBusinessTables {
		if !slices.Contains(all, name) {
			t.Errorf("豁免清单里的 %s 并不存在于库中，清单该清理了", name)
		}
	}
	if len(out) == 0 {
		t.Fatal("一张业务表都没枚举到——这个检查本身失效了")
	}
	return out
}
