package repository_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// lock_timeout 只设在锁热点行的入口上（repository/tenant.go 的 withLockingTenantTx），
// 而且只活到事务结束。
//
// 三件事各有一种坏法：
//   - 锁行入口没设上 → 一个持锁不放的事务照旧能让同 SKU 的下单排到池耗尽；
//   - 普通事务也设上了 → 订单 / 退款那些「排队就是想要的串行化」的 FOR UPDATE
//     会在 3 秒后莫名失败；
//   - 设成了会话级 → 池把这条连接交给下一个请求时它还带着，上一条就等于没守住。
func TestLockTimeoutOnlyOnLockingEntries(t *testing.T) {
	ctx := context.Background()
	idA, _ := seedTwoTenants(t)
	r := repository.New(pool(t))
	tctx := tenant.NewContext(ctx, idA)

	show := func(tx pgx.Tx) string {
		var v string
		if err := tx.QueryRow(ctx, `SHOW lock_timeout`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	var locking, plain string
	if err := r.RawLockingTenantTx(tctx, func(tx pgx.Tx) error { locking = show(tx); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.RawTenantTx(tctx, func(tx pgx.Tx) error { plain = show(tx); return nil }); err != nil {
		t.Fatal(err)
	}
	if locking != "3s" {
		t.Errorf("锁行入口的 lock_timeout = %q，期望默认 3s", locking)
	}
	if plain != "0" {
		t.Errorf("普通事务的 lock_timeout = %q，期望 0（不设）—— 可能是锁行入口那句漏成了会话级", plain)
	}
}
