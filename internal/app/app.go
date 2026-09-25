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
)

// Config 是一次部署的全部配置。
type Config struct {
	Addr   string
	Tenant tenant.Config
}

// ConfigFromEnv 从环境变量读配置。
func ConfigFromEnv() Config {
	addr := os.Getenv(EnvAddr)
	if addr == "" {
		addr = ":8080"
	}
	return Config{
		Addr: addr,
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
// listen 可注入是为了让「Preflight 没过就不监听」这句话可以被测试观察到。
// 只在 main 里写一行 Preflight 是测不出来的：把那行删掉，所有测试照样绿，
// 而 tenant 包里那四道检查会一声不响地变成死代码。
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

	return listen(cfg.Addr, Router(pool, res))
}
