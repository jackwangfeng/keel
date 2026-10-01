package db_test

import (
	"context"
	"testing"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/outcome"
)

// 池上挂的 outcome.Tracer 要分得清「落地了」与「没落地」—— 语句超时的兜底能不能说
// 「这次没有生效」全凭它（internal/outcome 包注释第 2 层）。
//
// 两个方向都要钉：把只读事务的提交也算落地，兜底就永远说不出「没生效」（退化回 500，
// 不危险但修复白做）；把回滚了的写也算没落地之外的任何东西漏掉，兜底就会对一个
// 已经生效的请求说「没生效」—— 那是这一套里唯一危险的错。
//
// 全程用一条 Acquire 出来的连接：临时表只在本会话可见。
func TestPoolTracerTellsDurableFromRolledBack(t *testing.T) {
	ctx := context.Background()
	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	p, err := db.NewPool(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	conn, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	// 建表时 ctx 上没有记录器：这一步不算任何请求的落地。
	if _, err := conn.Exec(ctx, `CREATE TEMP TABLE outcome_probe (n int)`); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		run  func(ctx context.Context) error
		want bool
	}{
		{"只读事务提交", func(ctx context.Context) error {
			tx, err := conn.Begin(ctx)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT count(*) FROM outcome_probe`); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}, false},
		{"写了又回滚", func(ctx context.Context) error {
			tx, err := conn.Begin(ctx)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO outcome_probe VALUES (1)`); err != nil {
				return err
			}
			return tx.Rollback(ctx)
		}, false},
		{"写了之后语句被取消、事务回滚", func(ctx context.Context) error {
			tx, err := conn.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `INSERT INTO outcome_probe VALUES (1)`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '50ms'`); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `SELECT pg_sleep(1)`)
			if !outcome.IsDBBusy(err) {
				t.Errorf("pg_sleep 撞 statement_timeout 应得 57014，实得 %v", err)
			}
			return nil
		}, false},
		{"写了并提交", func(ctx context.Context) error {
			tx, err := conn.Begin(ctx)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO outcome_probe VALUES (1)`); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}, true},
		{"自动提交的写", func(ctx context.Context) error {
			_, err := conn.Exec(ctx, `UPDATE outcome_probe SET n = n + 1`)
			return err
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rctx := outcome.Track(ctx)
			if err := c.run(rctx); err != nil {
				t.Fatal(err)
			}
			if got := outcome.MaybeDurable(rctx); got != c.want {
				t.Errorf("MaybeDurable = %v，期望 %v", got, c.want)
			}
		})
	}

	if !outcome.MaybeDurable(ctx) {
		t.Error("没挂记录器的 ctx 必须回 true（不知道就当作可能生效了）")
	}
}
