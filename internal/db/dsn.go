// Package db 提供数据库连接相关的公共设施。
package db

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func dsn(user, password string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		user, password,
		env("PGHOST", "127.0.0.1"), env("PGPORT", "5432"),
		env("PGDATABASE", "keel"))
}

// DSN 是应用与测试用的连接串，走非超级用户角色 keel_app。
//
// 默认值刻意不是建库用的那个角色：超级用户和带 BYPASSRLS 的角色无条件绕过
// 行级安全，FORCE 也拦不住。用它连上来，00002 里的租户隔离就是一张废纸——
// 既读得到别家租户的数据，也不会在忘记设 app.merchant_id 时报错。
// 拿连接请走 Connect，它会把这件事当场查出来。
func DSN() string {
	return dsn(env("PGUSER", "keel_app"), env("PGPASSWORD", "keel_app"))
}

// AdminDSN 只给迁移用：建表、建角色、授权需要属主或超级用户权限。
// 除了 `make migrate`，没有别的地方该用它。
func AdminDSN() string {
	return dsn(env("KEEL_ADMIN_USER", "keel"), env("KEEL_ADMIN_PASSWORD", "keel"))
}

// Querier 是 Guard 需要的最小接口，*pgx.Conn 与 *pgxpool.Pool 都满足。
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Guard 在连接能绕过 RLS 时返回错误。
//
// 这是 fail-closed 的最后一道闸：RLS 失效不会报错、不会变慢、不会留下痕迹，
// 只会安静地把别家租户的数据交出去。唯一能可靠发现它的时机就是连接建立的那一刻。
// 它同样作用于测试进程——只在 main 里查的话，CI 会继承一模一样的盲区，
// 而「跨租户读不到数据」的测试会在超级用户连接上假绿。
func Guard(ctx context.Context, q Querier) error {
	var super, bypass bool
	err := q.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&super, &bypass)
	if err != nil {
		return fmt.Errorf("检查当前角色是否绕过 RLS 失败: %w", err)
	}
	if super || bypass {
		var who string
		if err := q.QueryRow(ctx, `SELECT current_user`).Scan(&who); err != nil {
			who = "?"
		}
		return fmt.Errorf(
			"角色 %q 会绕过行级安全 (rolsuper=%v rolbypassrls=%v)，拒绝在它上面跑应用。"+
				"应用与测试请用 keel_app 连接；建库与迁移才用管理员角色",
			who, super, bypass)
	}
	return nil
}

// Connect 按 DSN 建连接，并当场确认这条连接不能绕过 RLS。
// 应用和测试都该走这里，而不是自己 pgx.Connect。
func Connect(ctx context.Context) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, DSN())
	if err != nil {
		return nil, err
	}
	if err := Guard(ctx, conn); err != nil {
		conn.Close(ctx)
		return nil, err
	}
	return conn, nil
}
