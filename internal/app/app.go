// Package app 是进程的装配点：把池、租户解析器、repository、service、handler
// 接成一条链路，并规定它们的启动顺序。
//
// 装配单独成包（而不是全写在 cmd/keel/main.go 里），是为了让启动顺序可被测试。
// main 里的 package main 也能测，但那样只测得到 main() 全跑完的结果 ——
// 而这里最要紧的一条恰恰是「Preflight 失败时后面那步不许发生」。
package app

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/handler"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// 环境变量名。
//
// 这几个字符串是常量而不是散落各处的字面量，因为它们同时出现在两个地方：
// 这里读，以及 tenant.Preflight 的错误信息里。两边写得不一样的后果特别刺眼 ——
// 自检会礼貌地告诉运维「请清空 KEEL_DEFAULT_MERCHANT」，而那个变量谁也没设过。
const (
	EnvDefaultMerchant = "KEEL_DEFAULT_MERCHANT"
	EnvBaseDomain      = "KEEL_BASE_DOMAIN"
	EnvAddr            = "KEEL_ADDR"

	// EnvDTMDSN 是嵌入式事务协调器自己的存储。**没有默认值，空着就拒绝启动。**
	//
	// 这一条比另外三个更需要人明确回答，因为它错了不报错。协调器存的是
	// 「哪些全局事务还没跑完」；这份状态丢了，正向阶段已经扣掉的库存与已经核销
	// 的券就再也没人回补 —— 架构 §5 说的「少卖」从可恢复变成永久漏账。
	// 给它一个「反正能跑起来」的默认值（比如当前目录下的一个 sqlite 文件），
	// 就是让每一个忘了配它的部署都安静地拿到这个结局。
	//
	// 它也**不能**直接用业务库那份连接。实测两条：
	//   - Start() 会跑 dtmrs 自己的 migrate()，建 trans_global / trans_branch_op /
	//     auth_token 三张表，还会对已有表发 ALTER TABLE ADD COLUMN。用 keel_app
	//     去做，报的是 `permission denied for schema public`。
	//   - 换成管理员角色能建出来，但那三张表就落进了业务库的 public 下，
	//     db/tenancy.json 的四道闸门当场全红（实测：ENABLE/FORCE、租户策略、
	//     唯一约束、GRANT 面各一条），而且应用进程从此握着能绕过 RLS 的凭据。
	//
	// 所以它是一个独立的存储。单机形态（架构 §6 形态 A）用挂在卷上的 sqlite；
	// 多实例形态必须换成 Postgres/MySQL/Redis，并且那套库要有自己的角色 ——
	// sqlite 撑不住多实例并发写（dtmrs 自己的部署文档写明了这一条）。
	EnvDTMDSN = "KEEL_DTM_DSN"
)

// Config 是一次部署的全部配置。
type Config struct {
	Addr   string
	DTMDSN string
	Tenant tenant.Config
}

// ConfigFromEnv 从环境变量读配置。
func ConfigFromEnv() Config {
	addr := os.Getenv(EnvAddr)
	if addr == "" {
		addr = ":8080"
	}
	return Config{
		Addr:   addr,
		DTMDSN: os.Getenv(EnvDTMDSN),
		Tenant: tenant.Config{
			DefaultCode: os.Getenv(EnvDefaultMerchant),
			BaseDomain:  os.Getenv(EnvBaseDomain),
		},
	}
}

// Router 装路由。测试与 main 共用它，所以测试打的是真实的那套链路，
// 而不是一份在旁边慢慢跑偏的复制品。
func Router(pool *pgxpool.Pool, res *tenant.Resolver) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())

	// 没匹配上的路径与方法也要回契约里的 Problem。
	//
	// gin 默认回的是 text/plain 的 "404 page not found"，而契约里每个接口的响应
	// 集合都是 `200` 加 `default: Problem` —— 一个 text/plain 的 404 两头都不沾，
	// 按契约生成的客户端会在它最需要读懂的那类响应上解析失败。路由拼错、
	// 版本前缀漏掉、用 POST 打了个只读接口，都走这两条。
	//
	// HandleMethodNotAllowed 必须显式打开：默认是 false，那时方法不匹配会掉进
	// NoRoute 变成 404，而 404 和 405 对调用方是两件事（「没这个接口」
	// 和「接口在，但不收这个方法」）。
	r.HandleMethodNotAllowed = true
	r.NoRoute(func(c *gin.Context) {
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "接口不存在")
	})
	r.NoMethod(func(c *gin.Context) {
		problem.Write(c, http.StatusMethodNotAllowed,
			problem.TypeMethodNotAllowed, "该接口不支持这个方法")
	})

	// healthz 在租户中间件之外：它回答的是「这个进程还活着吗」，
	// 挂在中间件后面的话，一个没配对的 Host 会让编排系统以为进程死了。
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	ph := handler.NewProductHandler(service.NewProductService(repository.New(pool)))

	v1 := r.Group("/api/v1", res.Middleware())
	v1.GET("/products", ph.List)
	return r
}

// Branches 是要注册到协调器上的全部进程内分支，键就是编排里 "local://" 后面
// 那个名字。
//
// 今天是空的：下单 SAGA 的三个正向分支与它们的补偿属于任务 5，屏障属于任务 2。
// 先把这个口子开出来，是因为**注册必须发生在 Start 之前**，而这条顺序踩错了
// 不当场报错 —— 症状要等到第一次提交时才出现（未注册的 local:// 名字在提交期
// 被拒），那时错误指向的是提交它的那段业务代码。顺序封在 dtm.Start 里，
// 任务 5 只要往这个 map 里加条目。
//
// 注册机制本身由 internal/dtm 的 TestBranchRecoversTenantFromGID 覆盖（它注册
// 真分支、跑真事务）；这里为空不代表那条路没被测到。
func Branches() map[string]dtm.BranchFunc { return nil }

// Listen 是默认的监听方式。它是 Run 的一个参数，好让测试换掉它。
func Listen(addr string, h http.Handler) error {
	return (&http.Server{Addr: addr, Handler: h}).ListenAndServe()
}

// Run 建池、建解析器、跑启动自检、装路由，然后才开始监听。
//
// 顺序是这个函数存在的理由。Preflight 检查的四种误配（两个租户来源同时配置 /
// 默认商家加多家活跃商家 / 默认商家不可服务 / 活跃商家没有任何入口）都不会让
// 任何请求报错，它们只会让请求安静地答错：客人看到别家的店、全站 404、
// 某几家店谁也打不开。开始监听之后再发现这些，代价是已经答错的那些请求。
//
// 事务协调器排在 Preflight 之后、监听之前，两头都是硬的：
//
//   - 排在 Preflight 之后：一份注定要被拒绝的配置，不该先把协调器的存储建出来
//     （dtmrs 的 Start 会建表）。启动失败留下三张表，下一次排查会从那三张表开始。
//   - 排在监听之前：协调器起不来就一笔订单也做不了，而此时开始接请求，
//     客人看到的是「提交订单」按钮按下去之后的 500，编排系统看到的是一个健康的进程。
//
// listen 可注入是为了让上面这两句话可以被测试观察到。只在 main 里写一行
// Preflight 是测不出来的：把那行删掉，所有测试照样绿，而 tenant 包里那四道检查
// 会一声不响地变成死代码。协调器这一段同理。
func Run(ctx context.Context, listen func(addr string, h http.Handler) error) error {
	cfg := ConfigFromEnv()

	pool, err := db.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("建连接池失败: %w", err)
	}
	defer pool.Close()

	res := tenant.NewResolver(pool, cfg.Tenant)
	if err := res.Preflight(ctx); err != nil {
		return fmt.Errorf("启动自检未通过，拒绝启动: %w", err)
	}

	if cfg.DTMDSN == "" {
		return fmt.Errorf("没有配置 %s，拒绝启动：事务协调器的存储必须显式指定。"+
			"单机形态用挂在卷上的 sqlite（%s=sqlite:/var/lib/keel/dtm.db），"+
			"多实例形态换成 Postgres/MySQL/Redis 并给它自己的角色 —— "+
			"它不能用业务库那份凭据（keel_app 没有建表权限，而管理员角色会让"+
			"应用握着能绕过 RLS 的连接）",
			EnvDTMDSN, EnvDTMDSN)
	}
	tc, err := dtm.Start(cfg.DTMDSN, 0, Branches())
	if err != nil {
		return fmt.Errorf("启动事务协调器失败（%s）: %w", EnvDTMDSN, err)
	}
	// 干净收尾：listen 返回（不论正常还是出错）之后把协调器关掉，
	// 它才有机会把 tokio 运行时停下来、把注册分支的 cgo.Handle 还回去。
	// Close 是幂等的，所以这条 defer 与将来可能加的显式收尾不会撞车。
	defer tc.Close()

	return listen(cfg.Addr, Router(pool, res))
}
