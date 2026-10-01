// Package worker 是后台任务的统一生命周期：崩溃恢复、停机等待、多实例选主。
//
// 以前十几个后台任务在 app.Run 里各自 `go x.Run(bgCtx)`：
//
//   - 没有 recover：任何一个任务里的 panic（一行空指针、一次越界）直接崩掉整个进程，
//     公网请求跟着一起断。后台任务出错的代价本该是「这一轮没跑成」，不是「全站 502」。
//   - 没有 WaitGroup：停机时只取消不等，正在事务中间的扫描会撞上已经关掉的池。
//   - 多实例部署时每个实例各跑一份扫描：N 个实例就是 N 倍的全表扫描与 N 倍的日志，
//     而这些扫描的正确性本来就不依赖并发（条件更新 / 幂等），多跑只是浪费。
//
// Runner 把这三件事收在一个地方，任务本身的逻辑一行不改：每个任务仍是
// `Run(ctx)`，只是改由 Runner 启动。
//
// # 选主
//
// 只给「扫描 / 对账 / 刷新」类任务选主（Task.Leader）。jobs 队列与 outbox 的消费者
// 不选：它们靠 FOR UPDATE SKIP LOCKED 天然多实例并行，选主反而把吞吐压回一个实例。
//
// 选主用 PostgreSQL 的会话级咨询锁 `pg_try_advisory_lock(class, key)`，每个任务一个
// 固定 key（任务名的哈希）。几个要点：
//
//   - 会话级：锁跟着**连接**走，所以锁要持有在一条**专用连接**上（不是池里的连接 ——
//     池会把连接交给别的请求，锁就跟着漂走了；而长期占着池里的 N 条连接又会把池压小）。
//     一个进程一条，这个进程抢到的所有任务锁都在这一条上。
//   - 连接断了（库重启、网络抖、被 pg_terminate_backend），锁随会话一起没了，
//     别的实例可能已经接手。所以 Runner 定期 ping 这条连接，ping 不通就取消本实例上
//     全部选主任务、丢掉连接，下一轮重连、重新竞选。ping 的间隔内可能出现两个实例
//     同时在跑同一个任务 —— 与今天「每个实例都跑」相比不会更糟，这些任务本来就要
//     容忍并发。
//   - 拿不到锁不是错误：本轮跳过，RetryInterval 之后再试。持锁的实例停机
//     （连接关闭即释放）之后，最多一个 RetryInterval 就有别的实例接手。
//   - 不能经过 PgBouncer 的事务池模式：那种模式下会话级锁没有意义。
//
// # 活性：当选者冻住了怎么办
//
// 上面那条「连接关闭即释放」有一个前提：连接**真的**会关。当选实例的进程被冻住
// （docker pause、SIGSTOP、宿主机卡死）时，它不 ping 了，可 TCP 连接还在、会话还在、锁也就还在
// —— 别的实例永远抢不到，扫描类任务全部停摆（2026-10 破坏性测试）。网络分区同理：服务端那一侧
// 要等内核的 TCP keepalive（Linux 默认 2 小时起）才发现对面没了。
//
// 两道兜底都设在选主那条专用连接上（prepareLeaderConn），由**服务端**动手，不依赖冻住的那一方：
//
//   - idle_session_timeout（PG 14+）：会话在事务之外空闲超过这个时长，服务端断开它，锁随会话释放。
//     值取 ping 间隔的 3 倍（IdleSessionTimeout，默认 45 秒）：正常的当选者每个 RetryInterval
//     至少 ping 一次，差两次 ping 都不会被误断；冻住的当选者最多 45 秒就让出锁。
//   - tcp_keepalives_idle / interval / count（服务端那一侧的 keepalive）：分区时服务端发探测，
//     15 + 5×3 = 30 秒内确认对面没了就断。它也是 PG 13 及以下（没有 idle_session_timeout）的兜底。
//
// 客户端那一侧另配了同样的 keepalive（app/background.go 的拨号器，KeepAlive），让分区里的当选者
// 自己也尽快发现连接死了 —— 不过 ping 本身带 5 秒超时，那一侧主要还是靠 ping。
//
// 代价是「两个实例同时跑」的窗口：冻住的实例被唤醒时，它的任务还在跑，要到它下一次 ping 失败
// （至多一个 RetryInterval）才停。与上面断线那条同一个代价，这些任务本来就要容忍并发。
package worker

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Task 是一个后台任务。
type Task struct {
	// Name 用于日志，也决定选主用的锁 key —— 改名等于换一把锁（滚动升级期间新旧两版
	// 会各自拿到锁、同时跑一会儿）。
	Name string
	// Run 一直跑到 ctx 被取消。返回（或 panic）而 ctx 没取消时，Runner 按退避重启它。
	Run func(ctx context.Context)
	// Leader 为 true 时多实例部署下只在一个实例上跑（见包注释「选主」）。
	Leader bool
}

// Config 是 Runner 的参数。零值取默认。
type Config struct {
	// Connect 建选主用的那条专用连接。没有选主任务时不会被调用；
	// 有选主任务却为 nil 时，选主任务**不启动**（并告警）—— 宁可不跑，也不能每个实例都跑。
	Connect func(ctx context.Context) (*pgx.Conn, error)
	// RetryInterval 是拿不到锁之后多久再试、以及 ping 那条连接的间隔。默认 15 秒（DefaultRetryInterval）。
	RetryInterval time.Duration
	// IdleSessionTimeout 是选主连接上的 idle_session_timeout：当选者这么久没说话（冻住、分区），
	// 服务端断开它的会话、锁随之释放（包注释「活性」）。默认 RetryInterval 的 3 倍，且不少于 1 秒
	// （测试里 RetryInterval 只有几十毫秒，3 倍会短到一次调度抖动就误断）。
	// 必须明显大于 RetryInterval，否则正常 ping 着的当选者也会被断。
	IdleSessionTimeout time.Duration
	// MinBackoff / MaxBackoff 是任务 panic（或意外返回）之后重启的退避，逐次翻倍。默认 1 秒 / 1 分钟。
	MinBackoff, MaxBackoff time.Duration
	Log                    *slog.Logger
}

// DefaultRetryInterval 是 RetryInterval 的默认值：选主连接每 15 秒 ping 一次。
const DefaultRetryInterval = 15 * time.Second

// idleSessionTimeoutFactor：idle_session_timeout 取 ping 间隔的几倍。3 倍 = 连续丢两次 ping
// 都不误断，冻住的当选者至多 3 个间隔（默认 45 秒）让出锁。
const idleSessionTimeoutFactor = 3

// 选主连接两侧的 TCP keepalive：空闲 15 秒开始探测，每 5 秒一次，3 次没回就断 ——
// 分区之后约 30 秒确认对面没了（内核默认是 2 小时起）。
const (
	keepAliveIdle     = 15 * time.Second
	keepAliveInterval = 5 * time.Second
	keepAliveCount    = 3
)

// KeepAlive 是选主连接**客户端**那一侧的 TCP keepalive，给建连的拨号器用（app/background.go）。
// 服务端那一侧的同一组参数由 prepareLeaderConn 设。
func KeepAlive() net.KeepAliveConfig {
	return net.KeepAliveConfig{Enable: true, Idle: keepAliveIdle, Interval: keepAliveInterval, Count: keepAliveCount}
}

// LockClass 是选主锁的第一个键（两参数形式 pg_try_advisory_lock(int4, int4)）。
// 'keel' 的 ASCII 加 8：与 testdb 在维护库上用的两个 class（…212 / …213）错开。
const LockClass int32 = 1801807220

// LockKey 是任务名对应的第二个键。
func LockKey(name string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return int32(h.Sum32())
}

// Runner 管一组后台任务。用 New 建，Add 注册，Start 启动，Stop 停止并等待。
type Runner struct {
	cfg   Config
	log   *slog.Logger
	tasks []Task

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New 建 Runner。
func New(cfg Config) *Runner {
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = DefaultRetryInterval
	}
	if cfg.IdleSessionTimeout <= 0 {
		cfg.IdleSessionTimeout = max(idleSessionTimeoutFactor*cfg.RetryInterval, time.Second)
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Runner{cfg: cfg, log: cfg.Log}
}

// Add 注册一个任务。必须在 Start 之前调用。
func (r *Runner) Add(t Task) {
	if r.ctx != nil {
		panic("worker: Start 之后不能再 Add")
	}
	r.tasks = append(r.tasks, t)
}

// Start 启动全部任务。ctx 取消等同于 Stop 的取消那一半（但不等待）。
func (r *Runner) Start(ctx context.Context) {
	r.ctx, r.cancel = context.WithCancel(ctx)
	var leaders []Task
	for _, t := range r.tasks {
		if t.Leader {
			leaders = append(leaders, t)
			continue
		}
		t := t
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			Supervise(r.ctx, r.log, t.Name, r.cfg.MinBackoff, r.cfg.MaxBackoff, t.Run)
		}()
	}
	if len(leaders) == 0 {
		return
	}
	if r.cfg.Connect == nil {
		names := make([]string, len(leaders))
		for i, t := range leaders {
			names[i] = t.Name
		}
		r.log.Warn("没有选主用的数据库连接，选主类后台任务不启动", "tasks", names)
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.elect(r.ctx, leaders)
	}()
}

// Stop 取消全部任务并等它们退出，最多 grace。返回是否在宽限期内全部退出。
// 超时只告警不阻塞：停机不能被一个不守 ctx 的任务卡住。
func (r *Runner) Stop(grace time.Duration) bool {
	if r.cancel == nil {
		return true
	}
	r.cancel()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		r.log.Warn("后台任务在宽限期内没有全部退出，继续停机", "grace", grace)
		return false
	}
}

// Supervise 跑 run 直到 ctx 取消。run panic 时记 ERROR（带堆栈）并按退避重启；
// run 在 ctx 没取消时返回也当成异常，同样退避重启（WARN）。
//
// 导出是为了让任务内部自己起的 goroutine（队列消费者）也能套一层：Runner 的 recover
// 只接得住任务 Run 所在的那个 goroutine，任务里另起的 goroutine panic 照样崩进程。
func Supervise(ctx context.Context, log *slog.Logger, name string, minBackoff, maxBackoff time.Duration,
	run func(context.Context)) {
	if log == nil {
		log = slog.Default()
	}
	if minBackoff <= 0 {
		minBackoff = time.Second
	}
	if maxBackoff < minBackoff {
		maxBackoff = minBackoff
	}
	backoff := minBackoff
	for {
		started := time.Now()
		panicked := runOnce(ctx, log, name, run)
		if ctx.Err() != nil {
			return
		}
		if !panicked {
			log.WarnContext(ctx, "后台任务在没收到停止信号时返回了，按退避重启", "task", name, "backoff", backoff)
		}
		// 跑了足够久才出事的，退避从头算：偶发的一次 panic 不该让任务之后一直晚一分钟。
		if time.Since(started) > maxBackoff {
			backoff = minBackoff
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func runOnce(ctx context.Context, log *slog.Logger, name string, run func(context.Context)) (panicked bool) {
	defer func() {
		if v := recover(); v != nil {
			panicked = true
			log.ErrorContext(ctx, "后台任务 panic，已恢复，按退避重启", "task", name,
				"panic", fmt.Sprint(v), "stack", string(debug.Stack()))
		}
	}()
	run(ctx)
	return false
}

// elect 是选主循环：持有专用连接，定期为没在跑的选主任务抢锁、ping 连接。
// 连接上的一切操作都在这一个 goroutine 里 —— pgx.Conn 不是并发安全的。
func (r *Runner) elect(ctx context.Context, tasks []Task) {
	type running struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	var conn *pgx.Conn
	held := map[string]*running{}

	// stopAll 取消本实例上全部选主任务并等它们退出。连接断了（锁已经没了）与停机都走它。
	stopAll := func() {
		for name, h := range held {
			h.cancel()
			<-h.done
			delete(held, name)
		}
	}
	defer func() {
		stopAll()
		if conn != nil {
			// 关连接即释放这个会话上的全部咨询锁。用新的 ctx：ctx 已经取消了。
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = conn.Close(cctx)
			cancel()
		}
	}()

	t := time.NewTicker(r.cfg.RetryInterval)
	defer t.Stop()
	for {
		// 任务自己退出了（只可能是 ctx 取消，Supervise 不会在别的情况下返回）：收掉。
		for name, h := range held {
			select {
			case <-h.done:
				delete(held, name)
			default:
			}
		}

		if conn != nil {
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := conn.Ping(pctx)
			cancel()
			if err != nil && ctx.Err() == nil {
				r.log.Error("选主连接断了，锁已随会话释放：停掉本实例上的选主任务，稍后重新竞选",
					"err", err, "tasks", len(held))
				stopAll()
				cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = conn.Close(cctx)
				cancel()
				conn = nil
			}
		}
		if conn == nil && ctx.Err() == nil {
			c, err := r.cfg.Connect(ctx)
			if err != nil {
				r.log.Warn("建选主连接失败，选主任务这一轮不跑，稍后再试", "err", err)
			} else {
				r.prepareLeaderConn(ctx, c)
				conn = c
			}
		}

		if conn != nil && ctx.Err() == nil {
			for _, task := range tasks {
				if _, ok := held[task.Name]; ok {
					continue
				}
				var got bool
				err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`,
					LockClass, LockKey(task.Name)).Scan(&got)
				if err != nil {
					if ctx.Err() == nil {
						r.log.Warn("抢选主锁失败，下一轮再试", "task", task.Name, "err", err)
					}
					break // 连接多半出事了，交给下一轮的 ping
				}
				if !got {
					r.log.Debug("选主锁在别的实例手里，本实例跳过", "task", task.Name)
					continue
				}
				r.log.Info("本实例当选，开始跑选主任务", "task", task.Name)
				tctx, cancel := context.WithCancel(ctx)
				h := &running{cancel: cancel, done: make(chan struct{})}
				held[task.Name] = h
				task := task
				go func() {
					defer close(h.done)
					Supervise(tctx, r.log, task.Name, r.cfg.MinBackoff, r.cfg.MaxBackoff, task.Run)
				}()
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// prepareLeaderConn 在选主连接上设活性兜底（包注释「活性」）：服务端那一侧的 TCP keepalive，
// 与 idle_session_timeout。会话级（set_config 第三个参数 false）：这条连接只给选主用，不进池。
//
// 设不上只告警、不放弃这条连接：没有活性兜底的选主仍然比不选主强（每个实例都跑），
// 而 idle_session_timeout 设不上最常见的原因是服务端是 PG 13 或更老（没有这个参数）——
// 那时还有 keepalive 兜着网络分区，冻住的进程则要等它自己恢复或被重启。
// 两组分开发：keepalive 在老版本上也认，不能被 idle_session_timeout 的失败连累。
func (r *Runner) prepareLeaderConn(ctx context.Context, c *pgx.Conn) {
	secs := func(d time.Duration) string { return strconv.Itoa(int(d / time.Second)) }
	if _, err := c.Exec(ctx, `SELECT set_config('tcp_keepalives_idle', $1, false),
	                                 set_config('tcp_keepalives_interval', $2, false),
	                                 set_config('tcp_keepalives_count', $3, false)`,
		secs(keepAliveIdle), secs(keepAliveInterval), strconv.Itoa(keepAliveCount)); err != nil {
		r.log.Warn("选主连接设不上服务端 TCP keepalive，网络分区时锁要等内核默认的 keepalive 才释放", "err", err)
	}
	ms := strconv.FormatInt(r.cfg.IdleSessionTimeout.Milliseconds(), 10)
	if _, err := c.Exec(ctx, `SELECT set_config('idle_session_timeout', $1, false)`, ms); err != nil {
		r.log.Warn("选主连接设不上 idle_session_timeout（PostgreSQL 14 之前没有这个参数）："+
			"当选实例冻住时别的实例不会接手", "err", err)
	}
}
