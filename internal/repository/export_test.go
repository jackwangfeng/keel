package repository

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 只在测试里存在的口子：把 withTenantTx 里那条 pgx.Tx 交给同目录的测试。
//
// 为什么需要它：Tx 接口上刻意没有任何通往原始连接的出口（product.go 里
// Tx 的注释写了理由），于是「WithTenant 真的把那三个 hnsw.* GUC 设上了吗」
// 这个问题在包外无法直接回答 —— 只能从后果（召回够不够）反推，
// 而那条推断在数据量小的时候是不成立的。
//
// 它在 _test.go 里，所以**生产构建里这个方法不存在**：谁想在业务代码里
// 用它，拿到的是编译错误，不是一个悄悄绕过 Tx 接口的后门。
// 这与 internal/inference/fake 那个替身的第一道闸门是同一条路子。
func (r *Repo) RawTenantTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return r.withTenantTx(ctx, func(tx pgx.Tx, _ Tx) error { return fn(tx) })
}

// RawLockingTenantTx 与 RawTenantTx 相同，走的是锁行入口用的 withLockingTenantTx
// （库存扣减 / SAGA 分支）。给「lock_timeout 只设在这几个入口上」那条测试用。
func (r *Repo) RawLockingTenantTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return r.withLockingTenantTx(ctx, func(tx pgx.Tx, _ Tx) error { return fn(tx) })
}

// PerCategoryCheaper 把按类目取页的选法交给同目录的测试（product_listing_plans_test.go）：
// 两种取法都要逐行对照单句排序，测试得能确认自己真的走到了想测的那一条。
var PerCategoryCheaper = perCategoryCheaper

// RawAndTenantTx 同时交出 pgx.Tx 与 Tx：给「ListProducts 深页在这个事务里关了 JIT」那条测试用 ——
// 那是一条事务内的 GUC，只能在同一个事务里读回来。
func (r *Repo) RawAndTenantTx(ctx context.Context, fn func(pgx.Tx, Tx) error) error {
	return r.withTenantTx(ctx, fn)
}
