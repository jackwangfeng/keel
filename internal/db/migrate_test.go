package db_test

import (
	"context"
	"os/exec"
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
	cmd := exec.Command("make", "-C", "../..", "migrate", "GOOSE_DBSTRING="+db.DSN())
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

	conn, err := pgx.Connect(context.Background(), db.DSN())
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
