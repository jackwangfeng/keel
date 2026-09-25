package db_test

import (
	"context"
	"os/exec"
	"testing"

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

	// 三张业务表必须同时 ENABLE 且 FORCE —— 只 ENABLE 的话表属主绕过 RLS，
	// 而迁移工具跑出来的属主通常就是应用自己。
	for _, tbl := range []string{"categories", "products", "skus"} {
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

	const wantQual = "(merchant_id = current_merchant())"

	for _, tbl := range []string{"categories", "products", "skus"} {
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
		if p.qual != wantQual {
			t.Errorf("%s: 策略谓词是 %q，期望 %q", tbl, p.qual, wantQual)
		}
		if p.cmd != "ALL" {
			t.Errorf("%s: 策略作用于 %q，期望 \"ALL\"——只保 SELECT 的话写入侧没人管", tbl, p.cmd)
		}
		// 空表示省略 WITH CHECK，PostgreSQL 回退到 qual，而 qual 上面刚断言过。
		// 非空就必须是同一个谓词：WITH CHECK (true) 时上面四项全部照旧通过，
		// 但租户 A 能往租户 B 名下插行。
		if p.withCheck != "" && p.withCheck != wantQual {
			t.Errorf("%s: 策略的写谓词是 %q，期望为空（回退到读谓词）或 %q——"+
				"写谓词被单独放开时，读侧看不出任何异常", tbl, p.withCheck, wantQual)
		}
		if p.permissive != "PERMISSIVE" {
			t.Errorf("%s: 策略是 %q", tbl, p.permissive)
		}
	}
}
