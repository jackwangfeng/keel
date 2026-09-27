package db_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
)

// NewPool 的 RLS-bypass 自检此前没有任何测试。
//
// 实测：把 pool.go 里 `cfg.AfterConnect = ...` 整段删掉，`make test-db` **全绿**。
//
// db.Connect（单连接）那条路径被 TestAppRoleCannotBypassRLS 顺带守着——它自己
// 就走 Connect，Guard 失灵时它连不出一条能绕过 RLS 的连接来。但池这条没有：
// 应用和所有集成测试用的都是池，而池的 Guard 挂在 AfterConnect 上，
// 没有任何断言碰过它。于是 pool.go 里那段注释写得最详细的防线，
// 恰恰是唯一一条可以被静默删掉的。
//
// 这里正面构造那个场景：把连接参数指向管理员角色（它是超级用户，无条件绕过
// 行级安全），要求 NewPool 拒绝建池。
//
// 为什么改环境变量而不是直接调 NewPoolFromDSN(AdminDSN())：这条要覆盖的是
// 「有人在部署环境里把 PGUSER 填成了建库那个角色」这个真实误配，走的是
// NewPool → DSN() 那条路径。NewPoolFromDSN（拆分部署给库存库开的口子）
// 由下面的 TestNewPoolFromDSNKeepsTheGuard 单独守。
func TestNewPoolRejectsRLSBypassingRole(t *testing.T) {
	ctx := context.Background()
	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// 先确认这条路真的通：管理员角色连得上，且它确实会绕过 RLS。
	// 少了这一步，「NewPool 返回错误」可能只是因为口令错了或库没起来——
	// 那样这条断言会在 Guard 被删掉之后照样绿。
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatalf("管理员连接建不起来，这条测试无从验证: %v", err)
	}
	var super, bypass bool
	if err := admin.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	admin.Close(ctx)
	if !super && !bypass {
		t.Skip("管理员角色既不是超级用户也不带 BYPASSRLS，这个环境构造不出被测场景")
	}

	// db.DSN() 从 PGUSER / PGPASSWORD 取值，改掉它们就等于让应用连到
	// 管理员角色上——正是要拦的那个误配。
	adminUser := env("KEEL_ADMIN_USER", "keel")
	adminPass := env("KEEL_ADMIN_PASSWORD", "keel")
	t.Setenv("PGUSER", adminUser)
	t.Setenv("PGPASSWORD", adminPass)
	if !strings.Contains(db.DSN(), adminUser) {
		t.Fatalf("改了 PGUSER 之后 DSN 仍然是 %q——这条测试没有测到想测的东西", db.DSN())
	}

	pool, err := db.NewPool(ctx)
	if err == nil {
		pool.Close()
		t.Fatal("NewPool 在一条能绕过 RLS 的连接上建池成功了——" +
			"pool.go 的 AfterConnect 自检没生效。应用和全部集成测试用的都是池，" +
			"这一层失灵时，所有「跨租户读不到数据」的断言都会在一条无视 RLS 的连接上假绿")
	}
	if !strings.Contains(err.Error(), "绕过行级安全") {
		t.Fatalf("NewPool 确实失败了，但错误看起来不是 Guard 报的，"+
			"也就证明不了自检生效: %v", err)
	}
}

// NewPoolFromDSN 是给库存库（KEEL_INVENTORY_DSN）开的口子，它必须和 NewPool
// 挂着同一道 Guard —— 否则「连接串由调用方给」就真成了洞：把库存库的 DSN
// 配成建库用的超级用户，库存那一侧的 RLS 就是一张废纸。
//
// 阳性对照在同一条里：同一个函数拿 keel_app 的 DSN 必须建得出池，
// 否则「管理员被拒」可能只是因为这个函数什么都建不出来。
func TestNewPoolFromDSNKeepsTheGuard(t *testing.T) {
	ctx := context.Background()
	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	ok, err := db.NewPoolFromDSN(ctx, db.DSN())
	if err != nil {
		t.Fatalf("用 keel_app 的 DSN 建池失败，这条测试无从对照: %v", err)
	}
	ok.Close()

	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatalf("管理员连接建不起来: %v", err)
	}
	var super, bypass bool
	if err := admin.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	admin.Close(ctx)
	if !super && !bypass {
		t.Skip("管理员角色既不是超级用户也不带 BYPASSRLS，这个环境构造不出被测场景")
	}

	pool, err := db.NewPoolFromDSN(ctx, db.AdminDSN())
	if err == nil {
		pool.Close()
		t.Fatal("NewPoolFromDSN 在一条能绕过 RLS 的连接上建池成功了 —— 它没有挂 Guard")
	}
	if !strings.Contains(err.Error(), "绕过行级安全") {
		t.Fatalf("确实失败了，但错误不是 Guard 报的，证明不了自检生效: %v", err)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
