package db

import (
	"context"
	"os"
	"strconv"
	"strings"

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
// EnvDBMaxConns 是业务连接池的上限；不配或配非正数时用 pgxpool 的默认值（max(4, CPU 核数)）。
const EnvDBMaxConns = "KEEL_DB_MAX_CONNS"

// EnvInventoryDSN 是库存库的完整连接串（拆分部署用，阶段 1 起生效；
// 见 docs/电商系统-微服务拆分方案.md）。空 = 库存与业务同一个库、同一个池。
//
// 它是一整串 DSN 而不是再来一组 PGHOST / PGUSER 分量：拆分形态下库存库是
// 另一个实例，两组分量变量交叉着配，漏掉一个就会安静地连回业务库 ——
// 那时两个服务写的是同一张表，什么都测不出来，直到有人去停业务库。
const EnvInventoryDSN = "KEEL_INVENTORY_DSN"

func NewPool(ctx context.Context) (*pgxpool.Pool, error) {
	return NewPoolFromDSN(ctx, DSN())
}

// NewPoolFromDSN 与 NewPool 相同，只是连接串由调用方给：同一道 Guard、
// 同一个 KEEL_DB_MAX_CONNS、同样建池即 Ping。
//
// 它为拆分部署的库存库而存在（KEEL_INVENTORY_DSN）。开这个口子不会把
// 「用一条能绕过 RLS 的连接跑应用」这个洞带回来：那个洞的闸门从来不是
// 「连接串由谁拼」，而是挂在 AfterConnect 上的 Guard —— 连接串指向哪个库、
// 用哪个角色都行，只要那个角色能绕过 RLS，这里一条物理连接都建不出来。
// pool_test.go 对这个函数单独钉了同样的断言。
//
// KEEL_DB_MAX_CONNS 两个池共用一个值：单体形态下库存池就是业务池（同一个
// *pgxpool.Pool），不存在第二份；拆分形态下两个池在两个进程里、连两个库，
// 各自按同一个上限算连接数，部署指南里那条公式不用改。
func NewPoolFromDSN(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return Guard(ctx, conn)
	}
	// 连接池上限。pgxpool 的默认值是 max(4, CPU 核数)：20 核的机器上一个实例就是 20 条，
	// 再加上事务协调器自己的池（DTMRS_DB_POOL，默认 32），一个实例最多 52 条 ——
	// 多实例实测 3 个实例就把 Postgres 默认的 max_connections = 100 打满，读写一起 500。
	// 多实例部署按「实例数 × (KEEL_DB_MAX_CONNS + DTMRS_DB_POOL) + 留给运维的几条 < max_connections」配。
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvDBMaxConns))); err == nil && n > 0 {
		cfg.MaxConns = int32(n)
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
