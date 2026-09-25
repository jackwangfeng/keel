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
// 这里对三张表逐一钉死：有且只有一条名为 tenant 的策略，谓词正好是
// merchant_id = current_merchant()，且作用于 ALL 而不只是 SELECT。
//
// cmd='ALL' 这条顺带把写入侧也钉住：FOR ALL 省略 WITH CHECK 时 PostgreSQL 拿
// USING 当 WITH CHECK 用，所以 INSERT/UPDATE 也受同一个谓词约束。有人改成
// FOR SELECT 的那天，写入侧的保护会静默消失，而这条断言会红。
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
			`SELECT policyname, permissive, cmd, coalesce(qual, '')
			   FROM pg_policies
			  WHERE schemaname = 'public' AND tablename = $1
			  ORDER BY policyname`, tbl)
		if err != nil {
			t.Fatal(err)
		}
		type policy struct{ name, permissive, cmd, qual string }
		var got []policy
		for rows.Next() {
			var p policy
			if err := rows.Scan(&p.name, &p.permissive, &p.cmd, &p.qual); err != nil {
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
		if p.permissive != "PERMISSIVE" {
			t.Errorf("%s: 策略是 %q", tbl, p.permissive)
		}
	}
}
