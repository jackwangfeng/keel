package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/worker"
)

// EnvBackground 决定本进程跑不跑后台任务：on（默认）/ off。
//
// 多实例部署的两种用法：每个实例都开（选主类任务靠咨询锁只在一个实例上跑，队列消费者
// 多实例并行），或者专门留一两个实例开、其余只接公网流量。写错拒绝启动而不是回落：
// 拼错的 "of" 若回落成 on，运维以为关掉了的实例其实在跑全套后台任务；回落成 off
// 则反过来，超时补偿没人跑 —— 永久漏卖，而页面上一切正常。
const EnvBackground = "KEEL_BACKGROUND"

func backgroundEnabledFromEnv() (bool, error) {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvBackground))); v {
	case "", "on", "true", "1":
		return true, nil
	case "off", "false", "0":
		return false, nil
	default:
		return false, fmt.Errorf("%s=%q 不认识，只能是 on（默认）或 off", EnvBackground, v)
	}
}

// background 是 Run 里注册后台任务的那一面：Go 注册多实例并行的任务，Leader 注册
// 多实例下只在一个实例上跑的任务（worker 包注释「选主」）。全部注册完再 Start。
type background struct {
	r       *worker.Runner
	enabled bool
}

// newBackground 建 Runner。选主用的专用连接与业务池同一份连接参数，但**不从池里拿**：
// 会话级咨询锁要一直持有，池里的连接会被交给别的请求（锁跟着漂走），长期占着又把池压小。
func newBackground(pool *pgxpool.Pool, enabled bool) *background {
	return &background{
		enabled: enabled,
		r: worker.New(worker.Config{
			Connect: func(ctx context.Context) (*pgx.Conn, error) {
				cfg := pool.Config().ConnConfig.Copy()
				// 客户端那一侧的 TCP keepalive（worker 包注释「活性」）：网络分区时当选者自己也尽快
				// 发现这条连接死了。服务端那一侧与 idle_session_timeout 由 worker 在连上之后设。
				// Unix 域套接字上 keepalive 没有意义，net.Dialer 会忽略它。
				// Timeout 照抄连接串里的 connect_timeout：换掉 pgconn 默认的拨号器不该把它丢了。
				d := &net.Dialer{Timeout: cfg.ConnectTimeout, KeepAliveConfig: worker.KeepAlive()}
				cfg.DialFunc = d.DialContext
				c, err := pgx.ConnectConfig(ctx, cfg)
				if err != nil {
					return nil, err
				}
				// 与池里每条连接同一道闸（internal/db.Guard）：这条连接只拿锁、不读业务数据，
				// 但一个能绕过 RLS 的角色出现在任何一条连接上都说明部署配错了。
				if err := db.Guard(ctx, c); err != nil {
					_ = c.Close(ctx)
					return nil, err
				}
				return c, nil
			},
		}),
	}
}

// Go 注册一个多实例并行的任务（队列 / outbox 消费者：靠 SKIP LOCKED 分活）。
func (b *background) Go(name string, run func(context.Context)) {
	b.r.Add(worker.Task{Name: name, Run: run})
}

// Leader 注册一个只在一个实例上跑的任务（扫描 / 对账 / 刷新）。
func (b *background) Leader(name string, run func(context.Context)) {
	b.r.Add(worker.Task{Name: name, Run: run, Leader: true})
}

// Start 启动全部任务；KEEL_BACKGROUND=off 时一个都不启动，并说出来。
func (b *background) Start(ctx context.Context) {
	if !b.enabled {
		slog.WarnContext(ctx, EnvBackground+"=off：本进程不跑任何后台任务（超时补偿、自动确认收货、"+
			"库存 outbox、通知投递……）。必须至少有一个实例开着它，否则超时未付的订单占着的库存永远不会放回")
		return
	}
	b.r.Start(ctx)
}

// Stop 取消并等待，最多 grace。
func (b *background) Stop(grace time.Duration) { b.r.Stop(grace) }
