package db

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool 建应用用的连接池，并保证池里每一条物理连接都绕不过 RLS。
//
// 应用与测试都该走这里，不要自己 pgxpool.New(ctx, db.DSN()) —— 那条路径上
// 没有任何东西强迫调用方去查角色，于是「测试跑在一条能绕过 RLS 的连接上」
// 这个洞会从池这一侧原样回来：所有「跨租户读不到数据」的断言在超级用户
// 连接上照样是绿的。
//
// Guard 挂在 AfterConnect 而不是「建池时查一次」：
// 池会在运行期按需新建物理连接，也会在连接断掉后重连。建池时查一次只覆盖
// 第一条连接，而角色属性是可以在运行期被 ALTER ROLE 改掉的（一次「临时给
// keel_app 加 BYPASSRLS 排查问题」就够了）。AfterConnect 覆盖每一次新建。
func NewPool(ctx context.Context) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(DSN())
	if err != nil {
		return nil, err
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return Guard(ctx, conn)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// pgxpool 默认懒连接：NewWithConfig 不会碰数据库，AfterConnect 要等到第一次
	// Acquire 才跑。不 Ping 的话「连不上」和「角色能绕过 RLS」都要等到第一个
	// 请求进来才暴露 —— 那时进程已经在接流量，健康检查也已经报绿。
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
