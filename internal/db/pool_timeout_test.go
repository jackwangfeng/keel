package db_test

import (
	"context"
	"testing"

	"github.com/keel/keel/internal/db"
)

// 池上的两条超时兜底（pool.go 的 applySessionTimeouts）必须真的到了服务端。
//
// 只测 applySessionTimeouts 的返回值不够：参数名拼错一个字母（比如
// idle_in_transaction_timeout），服务端要么拒绝连接、要么当成自定义参数收下 ——
// 后者不报错也不生效。所以这里建一个真池，SHOW 回来比。
func TestPoolSetsSessionTimeouts(t *testing.T) {
	ctx := context.Background()
	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	show := func(t *testing.T, name string) string {
		t.Helper()
		p, err := db.NewPool(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		var v string
		if err := p.QueryRow(ctx, "SHOW "+name).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	t.Run("默认值", func(t *testing.T) {
		t.Setenv(db.EnvDBStatementTimeout, "")
		t.Setenv(db.EnvDBIdleInTxTimeout, "")
		if got := show(t, "statement_timeout"); got != "15s" {
			t.Errorf("statement_timeout = %q，期望 15s", got)
		}
		if got := show(t, "idle_in_transaction_session_timeout"); got != "30s" {
			t.Errorf("idle_in_transaction_session_timeout = %q，期望 30s", got)
		}
	})
	t.Run("环境变量覆盖", func(t *testing.T) {
		t.Setenv(db.EnvDBStatementTimeout, "2500ms")
		t.Setenv(db.EnvDBIdleInTxTimeout, "1m")
		if got := show(t, "statement_timeout"); got != "2500ms" {
			t.Errorf("statement_timeout = %q，期望 2500ms", got)
		}
		if got := show(t, "idle_in_transaction_session_timeout"); got != "1min" {
			t.Errorf("idle_in_transaction_session_timeout = %q，期望 1min", got)
		}
	})
	t.Run("0 表示不发（PgBouncer 部署）", func(t *testing.T) {
		t.Setenv(db.EnvDBStatementTimeout, "0")
		if got := show(t, "statement_timeout"); got != "0" {
			t.Errorf("statement_timeout = %q，期望服务端默认 0", got)
		}
	})
	t.Run("写错了拒绝建池", func(t *testing.T) {
		for _, name := range []string{db.EnvDBStatementTimeout, db.EnvDBIdleInTxTimeout, db.EnvDBLockTimeout} {
			t.Setenv(name, "15 秒")
			if p, err := db.NewPool(ctx); err == nil {
				p.Close()
				t.Errorf("%s 写错了却建池成功 —— 运维会以为自己改了超时", name)
			}
			t.Setenv(name, "")
		}
	})
}

// 语句超时真的会杀语句（阳性对照：上面那条只证明了参数到了服务端）。
func TestPoolStatementTimeoutCancels(t *testing.T) {
	ctx := context.Background()
	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	t.Setenv(db.EnvDBStatementTimeout, "200ms")
	p, err := db.NewPool(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Exec(ctx, "SELECT pg_sleep(2)"); err == nil {
		t.Fatal("200ms 的 statement_timeout 下 pg_sleep(2) 跑完了")
	}
}
