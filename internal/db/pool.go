package db

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/outcome"
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
	if err := applySessionTimeouts(cfg.ConnConfig.RuntimeParams); err != nil {
		return nil, err
	}
	// 按语句结果记「这个请求有没有落地过什么」（internal/outcome）：语句超时 / 等锁超时
	// 回 503 时，只有一次都没落地过的请求才能说「这次没有生效」。请求之外的 ctx 上没有
	// 记录器，Tracer 对它们什么也不做。
	cfg.ConnConfig.Tracer = outcome.Tracer{}
	// 锁等待上限不是会话参数（见 LockTimeout），但它的变量也在这里校验一次：
	// 写错了要在启动时报，而不是在第一笔下单时。
	if _, err := LockTimeout(); err != nil {
		return nil, err
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

// 会话级的超时兜底（2026-09-30 架构审查）。
//
// 在这之前池上什么超时都没有：一条卡住的语句、一个开着事务却不再说话的请求
// （客户端断了、goroutine 卡在别处），都会无限期地握着连接和行锁。库存扣减对
// 热点行 FOR UPDATE（db/queries/inventory_svc.sql 的 InvLockStoreStock），
// 一个这样的持锁事务就能让同 SKU 的下单全部排队，排着排着池就耗尽了 ——
// 症状是整站变慢然后 500，而罪魁那一条事务在任何日志里都不报错。
//
// 两条都作为**启动参数**（RuntimeParams）发给服务端，而不是 AfterConnect 里再
// SET 一次：启动参数在连接建立时就生效，不多一次往返；而且它是会话默认值，
// 业务里需要放宽的地方照样能 SET LOCAL 覆盖（repository/agent_sql.go 的
// statement_timeout 就是这么做的）。
//
//   - statement_timeout：单条语句的上限。这里的每条 OLTP 语句都是毫秒级，
//     报表按窗口限了范围（service/report.go），导入按行分事务。15 秒是
//     「绝不是正常慢」的线，不是性能目标。
//   - idle_in_transaction_session_timeout：事务开着却闲着的上限。这个仓库的约定是
//     不在事务里做网络调用（例如 service/product_import.go 在调推理引擎前就结束读事务），
//     所以闲 30 秒只可能是泄漏或卡死。超了服务端断开这条连接，锁随之释放；pgxpool
//     会丢弃坏连接再建。
//
// **迁移不受影响**：goose 用的是自己的连接串（GOOSE_DBSTRING / AdminDSN），不经这里。
// 需要长跑的一次性运维 SQL 也请走管理员连接，别在应用池上跑。
//
// **PgBouncer（事务模式）**：它默认拒绝不认识的启动参数，连接会当场失败。那种部署
// 把两个变量设成 0 不发，改用 ALTER ROLE keel_app SET statement_timeout = ... 在服务端
// 配默认值（或在 PgBouncer 的 ignore_startup_parameters 里放行）。
//
// 连接串里已经写了同名参数（如 ...?statement_timeout=5000）时以连接串为准：那是
// 部署方更具体的意图。
const (
	EnvDBStatementTimeout   = "KEEL_DB_STATEMENT_TIMEOUT"
	EnvDBIdleInTxTimeout    = "KEEL_DB_IDLE_IN_TX_TIMEOUT"
	EnvDBLockTimeout        = "KEEL_DB_LOCK_TIMEOUT"
	defaultStatementTimeout = 15 * time.Second
	defaultIdleInTxTimeout  = 30 * time.Second
	defaultLockTimeout      = 3 * time.Second
	paramStatementTimeout   = "statement_timeout"
	paramIdleInTxTimeout    = "idle_in_transaction_session_timeout"
)

// durationEnv 读一个时长变量：空 = def；0 = 关掉（返回 0）；解析失败是错误。
//
// 解析失败不回落默认值：把 "15" 当成「没配」悄悄用 15s 还好，把 "1500ms " 之类的
// 笔误悄悄吞掉，运维会以为自己放宽了超时，而线上照旧按默认值杀语句。启动时报错最便宜。
func durationEnv(name string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	if v == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s=%q 不是合法时长（形如 15s、500ms；0 表示不设）", name, v)
	}
	return d, nil
}

func applySessionTimeouts(params map[string]string) error {
	for _, c := range []struct {
		env, param string
		def        time.Duration
	}{
		{EnvDBStatementTimeout, paramStatementTimeout, defaultStatementTimeout},
		{EnvDBIdleInTxTimeout, paramIdleInTxTimeout, defaultIdleInTxTimeout},
	} {
		d, err := durationEnv(c.env, c.def)
		if err != nil {
			return err
		}
		if _, set := params[c.param]; set || d == 0 {
			continue
		}
		params[c.param] = strconv.FormatInt(d.Milliseconds(), 10)
	}
	return nil
}

// LockTimeout 是「锁热点行的事务」里 SET LOCAL lock_timeout 用的值（毫秒字符串）；
// 0 或空串表示不设。不是会话默认值：只有 repository 里显式声明要锁行的入口
// （库存扣减 / 回补、SAGA 分支）才用它，理由见 repository/tenant.go 的 withLockingTenantTx。
//
// 变量写错时返回错误，由 NewPool 之外的调用方（repository 初始化）决定怎么处理；
// 这里不 panic。
func LockTimeout() (string, error) {
	d, err := durationEnv(EnvDBLockTimeout, defaultLockTimeout)
	if err != nil {
		return "", err
	}
	if d == 0 {
		return "", nil
	}
	return strconv.FormatInt(d.Milliseconds(), 10), nil
}
