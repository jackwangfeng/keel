package rpc

import (
	"bytes"
	"context"
	"crypto/hmac"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/buildinfo"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/tenant"
)

// Prefix 是内网接口的公共前缀。
const Prefix = "/internal/v1"

// SagaPrefix 是 SAGA 分支路由的前缀，分支 name 接在后面：
// /internal/v1/saga/<name>。dtm.BranchResolver 按同一个前缀拼地址。
const SagaPrefix = Prefix + "/saga"

// maxBody 是内网请求体的上限。服务间请求是本仓库自己拼的结构化 JSON，
// 1 MiB 远超任何一个正常请求；上限存在是为了验签之前不把一个无界正文读进内存 ——
// 验签必须先读完正文，而那时还不知道对方是不是自己人。
const maxBody = 1 << 20

// ServerConfig 是内网引擎的配置。
type ServerConfig struct {
	// Secret 是 KEEL_INTERNAL_SECRET。空串时 NewRouter panic：一个不验签的
	// 内网引擎不是「降级」，是一个对任意租户开放的写接口。Run 在更早的地方
	// 就会因为没配它而拒绝启动，走到这里的空串只可能是代码写错了。
	Secret string

	// PreviousSecrets 是 KEEL_INTERNAL_SECRET_PREVIOUS 解析后的结果（见
	// rpc.ParsePreviousSecrets）：轮换期间仍要接受的旧密钥，可以有多个。
	// 只影响验证——请求验签（Verify）与分支令牌校验（BranchTokenAuth）拿
	// Secret 与它们一起试，对上任意一个都算数；本引擎自己不会用它们签任何
	// 东西。nil = 只认 Secret（今天的行为）。轮换步骤见 EnvInternalSecretPrevious。
	PreviousSecrets []string

	// Ready 是 /readyz 的检查（通常是 Ping 本服务的库）。nil = /readyz 恒为 ok。
	Ready func(ctx context.Context) error
}

// Routes 是内网引擎上三类准入规则不同的分组。调用方（阶段 1 起是库存服务）
// 把自己的 handler 挂到对应分组上。
type Routes struct {
	// Signed：/internal/v1 下，验签，不要求租户。给跨租户的内部操作用
	// （目前没有；对账任务之类的会是第一个）。
	Signed *gin.RouterGroup

	// Tenant：/internal/v1 下，验签 + X-Keel-Merchant-ID 必填。handler 拿到的
	// ctx 里已经有租户，repository.WithTenant / RLS 照旧工作。绝大多数接口挂这里。
	Tenant *gin.RouterGroup

	// Saga：/internal/v1/saga 下，**不验 HMAC 签名，验分支令牌**（?bt=...）。
	//
	// 原因是调用方不是我们：SAGA 分支由 dtmrs 的推进器直接 POST 过来
	// （driver.rs call_http：url?gid=&trans_type=&branch_id=&op=，正文是步骤的
	// payload），它不会、也没法按我们的规则给每个请求算签名。能由编排方塞进去的
	// 只有 URL 本身 —— 所以准入凭据就放在 URL 里：编排方用 dtm.BranchResolver
	// 生成 action / compensate 地址时带上 bt=<BranchToken(secret)>，dtmrs 追加
	// 自己的参数时保留已有参数，令牌原样到达。
	//
	// 这是一个静态的持有者令牌，代价说清楚：
	//   - 它会随 URL 进协调器的存储与日志。所以它是从 KEEL_INTERNAL_SECRET
	//     **单向派生**的（见 BranchToken），泄露它推不出签名密钥，
	//     拿不到 Signed / Tenant 两组接口。
	//   - 拿到它的人能调分支接口。分支本身按 gid 找租户、按子事务屏障幂等，
	//     能做的事以「重放某个 gid 的正向或补偿」为限 —— 与协调器本来就会做的
	//     重试同一类，而不是任意读写。它仍然只该监听在内网。
	//   - 轮换 KEEL_INTERNAL_SECRET 本会让**已持久化**的在途事务的旧地址 401
	//     （401 在 dtmrs 那里是 Unknown，不是 Failure，协调器会一直重试而不会
	//     误补偿，但也推不完）。KEEL_INTERNAL_SECRET_PREVIOUS 解决的就是这个：
	//     验证方在轮换期间同时认当前值与旧值，旧地址里的令牌照样通过，等在途
	//     事务自然跑完再摘掉旧值。步骤与安全的顺序见该常量的注释。
	//
	// 失败一律回 401：dtmrs 把非 2xx / 409 / 425 的响应都当 Unknown 重试，
	// 这正是配错令牌时该有的行为 —— 配错不等于业务失败，不能触发补偿。
	Saga *gin.RouterGroup
}

// NewRouter 建内网 gin 引擎。它与公网 Router 完全分开（不同的引擎、不同的端口），
// 公网请求无论怎么拼路径都到不了这里。
func NewRouter(cfg ServerConfig) (*gin.Engine, Routes) {
	if cfg.Secret == "" {
		panic("rpc.NewRouter: 没有 KEEL_INTERNAL_SECRET，拒绝建一个不验签的内网引擎")
	}
	r := gin.New()
	r.Use(gin.Recovery())
	r.HandleMethodNotAllowed = true
	r.NoRoute(func(c *gin.Context) {
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "接口不存在")
	})
	r.NoMethod(func(c *gin.Context) {
		problem.Write(c, http.StatusMethodNotAllowed, problem.TypeMethodNotAllowed, "该接口不支持这个方法")
	})

	// 与公网引擎同一对探针，理由也相同：它们不经过任何准入中间件。
	// inventory 形态下这是这个进程唯一的端口，编排系统只能探它。
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/version", func(c *gin.Context) { c.JSON(http.StatusOK, buildinfo.Get()) })
	// readyz 与 healthz 分开：库暂时连不上时进程是活的（不该被重启），
	// 只是还不该接流量。回的是 Problem 而不是错误原文 —— 错误里有主机名与角色名。
	r.GET("/readyz", func(c *gin.Context) {
		if cfg.Ready != nil {
			if err := cfg.Ready(c.Request.Context()); err != nil {
				slog.WarnContext(c.Request.Context(), "内网服务未就绪", "err", err)
				problem.Write(c, http.StatusServiceUnavailable, problem.TypeInternal, "服务未就绪")
				return
			}
		}
		c.String(http.StatusOK, "ok")
	})

	// secrets：Secret 在前——不影响正确性（每个都要完整试一遍），但让最常见的
	// 那条路径（没在轮换）在列表第一个位置命中。
	secrets := append([]string{cfg.Secret}, cfg.PreviousSecrets...)
	signed := r.Group(Prefix, Verify(secrets))
	return r, Routes{
		Signed: signed,
		Tenant: signed.Group("", RequireTenant()),
		Saga:   r.Group(SagaPrefix, BranchTokenAuth(secrets)),
	}
}

// Verify 是验签中间件。缺签名、签名不对、时间戳超出 ±MaxSkew 一律 401。
// secrets 是当前密钥与全部仍接受的旧密钥（见 ServerConfig.PreviousSecrets），
// 对上其中任意一个就放行。
//
// 拒绝原因只进日志、不进响应：告诉对方「差在时间戳」还是「差在签名」，
// 是在帮一个正在试探的人缩小范围。
func Verify(secrets []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxBody))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				problem.Write(c, http.StatusRequestEntityTooLarge, problem.TypeInvalidRequest, "请求体过大")
				return
			}
			problem.Write(c, http.StatusBadRequest, problem.TypeInvalidRequest, "请求体读取失败")
			return
		}
		// 读完放回去：后面的 handler 还要 ShouldBindJSON。
		c.Request.Body = io.NopCloser(bytes.NewReader(body))

		// EscapedPath 而不是 Path：客户端签的是线上那串转义后的路径（见 Client.do）。
		ok, why := verify(secrets, c.Request.Method, c.Request.URL.EscapedPath(), c.Request.URL.RawQuery,
			c.GetHeader(HeaderMerchantID), c.GetHeader(HeaderTimestamp), c.GetHeader(HeaderSignature),
			body, time.Now())
		if !ok {
			slog.WarnContext(c.Request.Context(), "内网请求验签失败", "reason", why,
				"method", c.Request.Method, "path", c.Request.URL.Path, "remote", c.ClientIP())
			problem.Write(c, http.StatusUnauthorized, problem.TypeUnauthorized, "内网请求签名无效")
			return
		}
		c.Next()
	}
}

// RequireTenant 把 X-Keel-Merchant-ID 放进 ctx，与公网租户中间件用的是同一个
// tenant.NewContext —— 于是 handler 以下的代码分不出、也不需要分出请求是从
// 公网还是内网来的，repository.WithTenant 与 RLS 一行不改。
//
// **它不查库确认这个商家存在。** 公网中间件要查，是因为它要把 Host 翻译成 id；
// 这里的 id 是 core 已经解析过、并且被签名覆盖了的（见 canonical），
// 再查一次只是把 core 的判断重做一遍 —— 而拆分形态下库存库里根本没有 merchants 表。
//
// 缺失或不是正整数一律 400：那是调用方的 bug，不是「这家店不存在」。
func RequireTenant() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.TrimSpace(c.GetHeader(HeaderMerchantID))
		id, err := strconv.ParseInt(raw, 10, 64)
		if raw == "" || err != nil || id <= 0 {
			problem.Write(c, http.StatusBadRequest, problem.TypeInvalidRequest,
				"缺少或无效的 "+HeaderMerchantID)
			return
		}
		c.Request = c.Request.WithContext(tenant.NewContext(c.Request.Context(), id))
		c.Next()
	}
}

// BranchTokenAuth 校验 SAGA 分支路由的 ?bt=。为什么是它而不是签名，见 Routes.Saga。
// secrets 是当前密钥与全部仍接受的旧密钥：分支地址一旦生成就被协调器持久化，
// 轮换 KEEL_INTERNAL_SECRET 不能让已经发出去的地址失效，见 EnvInternalSecretPrevious。
func BranchTokenAuth(secrets []string) gin.HandlerFunc {
	wants := make([][]byte, len(secrets))
	for i, s := range secrets {
		wants[i] = []byte(BranchToken(s))
	}
	return func(c *gin.Context) {
		got := []byte(c.Query("bt"))
		for _, want := range wants {
			if hmac.Equal(got, want) {
				c.Next()
				return
			}
		}
		// 这条日志要响：配错令牌时协调器会无限重试，而重试日志在它那边，
		// 这边若不喊，排查的人只会看到「分支一直 Unknown」。
		slog.ErrorContext(c.Request.Context(), "SAGA 分支请求的令牌无效（KEEL_INTERNAL_SECRET 两边不一致？）",
			"path", c.Request.URL.Path, "gid", c.Query("gid"), "remote", c.ClientIP())
		problem.Write(c, http.StatusUnauthorized, problem.TypeUnauthorized, "分支令牌无效")
	}
}
