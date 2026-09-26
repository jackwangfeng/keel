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
