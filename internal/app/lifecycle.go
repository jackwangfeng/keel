package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/problem"
)

// 进程生命周期：监听、优雅停机、就绪探针。
//
// 停机顺序（Run 与 Listen 合起来实现，SIGTERM / SIGINT 触发）：
//
//  1. 停止接新连接（Shutdown 关掉监听套接字）；
//  2. 等在途请求做完，最多 KEEL_SHUTDOWN_GRACE（默认 20 秒），超时就强关；
//  3. 取消后台任务的 ctx，等它们各自退出（同一个宽限期）；
//  4. 关事务协调器（tc.Close）；
//  5. 关连接池。
//
// 3～5 是 Run 里那几条 defer 的逆序 —— 它们以前也写着，只是 main 用的是
// context.Background()、Listen 是裸 ListenAndServe，SIGTERM 下进程直接被杀，
// 一条 defer 都轮不到：正在跑的下单 SAGA 被从中间掐断，要等协调器下次启动再推进。

const (
	// EnvShutdownGrace 是收到停机信号之后等在途请求（与后台任务）的宽限期，Go duration 格式。
	// 要比编排系统的强杀期限短（docker stop 默认 10 秒、k8s terminationGracePeriodSeconds 默认 30 秒）：
	// 比它长的话，宽限期还没到进程就被 SIGKILL 了，等于没有宽限期。
	EnvShutdownGrace = "KEEL_SHUTDOWN_GRACE"

	defaultShutdownGrace = 20 * time.Second

	// readHeaderTimeout 挡的是慢速请求头（slowloris）：一条连接只发半个请求头就能
	// 永远占着一个 goroutine 与一个文件描述符。请求头是几 KB 的东西，10 秒绰绰有余。
	readHeaderTimeout = 10 * time.Second

	// idleTimeout 是 keep-alive 连接在两个请求之间最多空闲多久。不设时它取 ReadTimeout，
	// 而 ReadTimeout 也没设（见下）—— 于是空闲连接永不回收，客户端不关就一直占着。
	idleTimeout = 120 * time.Second
)

// ListenFunc 是 Run 的监听方式：在 addr 上用 h 服务，直到 ctx 被取消后优雅退出。
//
// 它是 Run 的参数（而不是 Run 里写死的 http.Server），好让测试换掉它 ——
// Run 的启动顺序（Preflight / 协调器在监听之前）要靠「监听有没有被调到」来观察。
// ctx 是停机信号：实现必须在 ctx 被取消之后停止接新请求、等在途请求、然后返回。
type ListenFunc func(ctx context.Context, addr string, h http.Handler) error

// Listen 是默认的监听方式：http.Server，带请求头超时与空闲超时，ctx 取消时优雅停机。
//
// **刻意不设 WriteTimeout 与 ReadTimeout。** 它们是整个请求的一刀切上限，而本服务
// 有正当的长请求：商品批量导入（上传一个表格、同步解析落库）、报表 CSV 导出
// （admin_report_csv.go 边查边写）、上传图片。慢一点的网络上这些请求超过任何
// 「够短才有意义」的上限，而 WriteTimeout 触发时客户端只看到连接被重置，
// 服务端连一行日志都没有。慢速攻击由 ReadHeaderTimeout 挡住请求头那一段；
// 请求体的大小各接口自己用 MaxBytesReader 限。
func Listen(ctx context.Context, addr string, h http.Handler) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return Serve(ctx, ln, h)
}

// Serve 是 Listen 去掉「自己开端口」的那一半：在 ln 上服务，ctx 取消时优雅停机。
// 单独导出是为了测试能交一个端口 0 的 Listener 走真的 http.Server 与真的 Shutdown，
// 而不必去抢一个固定端口。
func Serve(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := newServer(ln.Addr().String(), h)
	return serveUntilDone(ctx, srv, func() error { return srv.Serve(ln) }, shutdownGraceFromEnv())
}

func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// serveUntilDone 跑 serve，直到它自己出错返回，或 ctx 被取消 —— 后者走 Shutdown。
func serveUntilDone(ctx context.Context, srv *http.Server, serve func() error, grace time.Duration) error {
	errc := make(chan error, 1)
	go func() { errc <- serve() }()
	select {
	case err := <-errc:
		// 没收到停机信号就返回了：端口被占、权限不够之类。原样上浮，Run 据此退出。
		return err
	case <-ctx.Done():
	}

	slog.Info("收到停机信号：停止接新请求，等在途请求做完", "addr", srv.Addr, "grace", grace)
	// 宽限期的 ctx 不能从 ctx 派生：ctx 已经取消了，从它派生的 ctx 一出生就是取消的，
	// Shutdown 会立刻返回、在途请求被当场掐断 —— 那正是这里要避免的。
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		// 宽限期到了还有请求没做完。强关：继续等下去，编排系统的 SIGKILL 会替我们做同一件事，
		// 而那时后面的收尾（后台任务、协调器、池）一步都来不及。
		slog.Warn("宽限期内还有请求没做完，强制关闭连接", "addr", srv.Addr, "grace", grace, "err", err)
		_ = srv.Close()
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// shutdownGraceFromEnv 读 KEEL_SHUTDOWN_GRACE。解析不了就用默认值并告警 ——
// 不拒绝启动：写错一个停机参数不该让一个能正常服务的进程起不来。
func shutdownGraceFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv(EnvShutdownGrace))
	if raw == "" {
		return defaultShutdownGrace
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		slog.Warn(EnvShutdownGrace+" 解析不了，用默认值", "value", raw, "default", defaultShutdownGrace)
		return defaultShutdownGrace
	}
	return d
}

// readyz 回答「这个进程现在该不该接流量」：能 ping 通业务库才是 200。
//
// 与 healthz 分开，理由与内网服务那一对相同（rpc/server.go）：库暂时连不上时进程是活的，
// 不该被重启（重启解决不了库的问题，只会让所有实例一起陷进重启循环），只是不该再
// 被负载均衡分到流量。回 Problem 而不是错误原文 —— 错误里有主机名与角色名。
func readyz(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 短超时：探针本身有超时，库卡住时让探针先拿到一个明确的 503，而不是一起超时。
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			slog.WarnContext(c.Request.Context(), "未就绪：业务库 ping 不通", "err", err)
			problem.Write(c, http.StatusServiceUnavailable, problem.TypeInternal, "服务未就绪")
			return
		}
		c.String(http.StatusOK, "ok")
	}
}

// waitOrTimeout 等 done 关闭，最多 d。返回是否等到了。
func waitOrTimeout(done <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// background 是后台任务的 WaitGroup：Run 停机时取消它们的 ctx，然后等它们真的退出，
// 再去关协调器与池 —— 否则一个正在事务中间的扫描会撞上一个已经关掉的池。
type background struct {
	ctx context.Context
	wg  sync.WaitGroup
}

// Go 起一个后台任务。name 只用于日志。
func (b *background) Go(name string, run func(context.Context)) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		run(b.ctx)
		slog.Debug("后台任务已退出", "task", name)
	}()
}

// Wait 等全部后台任务退出，最多 d。超时只告警不阻塞：停机不能被一个不守 ctx 的任务卡住。
func (b *background) Wait(d time.Duration) {
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	if !waitOrTimeout(done, d) {
		slog.Warn("后台任务在宽限期内没有全部退出，继续停机", "grace", d)
	}
}
