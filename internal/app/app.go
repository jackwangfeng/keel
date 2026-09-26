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
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/buildinfo"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/handler"
	"github.com/keel/keel/internal/inference"
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

	// EnvUploadRoot 是本地磁盘 driver 的根目录（数据模型 §13）。
	// 没配就落临时目录并告警，完整论证在 uploadStoreFromEnv 上。
	EnvUploadRoot = "KEEL_UPLOAD_ROOT"
	EnvBaseDomain = "KEEL_BASE_DOMAIN"
	EnvAddr       = "KEEL_ADDR"

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

	// EnvAuthSecret 是签买家令牌用的 HMAC 密钥（至少 32 字节）。
	//
	// **它有一个「没配就随机取一个」的兜底，而 KEEL_DTM_DSN 没有。** 两者看着
	// 同类，代价却不同量级，所以处置也不同：
	//
	//   - 协调器的存储丢了，正向阶段已扣的库存与已核销的券再没人回补 ——
	//     架构 §5 的「少卖」从可恢复变成永久漏账。那是**不可恢复的数据损失**，
	//     所以它宁可拒绝启动。
	//   - 签名密钥丢了，全体买家被登出一次，重新登录即可。**可恢复**。
	//     为它拒绝启动，代价是 README 承诺的那条 `docker compose up`
	//     再也不是一条命令 —— 而 docker/ 下的 compose 文件不在本任务的范围里。
	//
	// 但兜底不是免费的，两条代价必须说清楚，所以它会打一条 WARN：
	//
	//   - 进程一重启，之前签发的全部令牌当场失效（密钥换了）；
	//   - **多实例部署下这个兜底是错的**：A 实例签的令牌在 B 实例验不过，
	//     症状是「刷新几次页面就要重新登录一次」，而且只在多实例下复现。
	//
	// 所以：单机跑着玩可以不配；任何一个真的在服务客人的部署都必须配。
	EnvAuthSecret = "KEEL_AUTH_SECRET"

	// EnvPaymentSandbox 关掉沙箱支付。**默认是开的**，值为 "off" / "false" /
	// "0" 时关闭。
	//
	// 默认开而不是默认关，是因为本轮一个真实支付渠道都没接：关掉之后
	// POST /orders/{order_no}/payments 除了 501 什么也回不了，而 README 承诺的
	// 那条 `docker compose up` Demo 要走到支付这一步（架构 §13 的 M2 产出标志
	// 就是「能下单能支付（沙箱）」）。一个默认关的开关会让每一个照着 README
	// 走的人都在支付那一步撞墙。
	//
	// 代价说清楚：沙箱开着的时候，买家能给**自己的**订单造一份合法签名的回调，
	// 也就是能免费把自己的单推成已支付（只有这一单、只有这个金额 ——
	// 报文的每个字节都进了 HMAC，密钥本身不会泄露）。那正是沙箱的定义。
	// 所以进程启动时会为它打一条 WARN，而任何真的在收钱的部署都必须显式关掉它。
	//
	// 它与 KEEL_AUTH_SECRET 那个「没配就随机取一个」的兜底不同类：那个是
	// 「没配也能跑，代价是重启掉线」，这个是「默认开着一条本来就不该在生产上
	// 存在的路」。所以它不是兜底，是一个需要被关掉的默认值，而 WARN 是它的提醒。
	EnvPaymentSandbox = "KEEL_PAYMENT_SANDBOX"
)

// Config 是一次部署的全部配置。
type Config struct {
	Addr       string
	DTMDSN     string
	AuthSecret string
	Tenant     tenant.Config
	Payment    service.PaymentConfig
}

// ConfigFromEnv 从环境变量读配置。
func ConfigFromEnv() Config {
	addr := os.Getenv(EnvAddr)
	if addr == "" {
		addr = ":8080"
	}
	return Config{
		Addr:       addr,
		DTMDSN:     os.Getenv(EnvDTMDSN),
		AuthSecret: os.Getenv(EnvAuthSecret),
		Tenant: tenant.Config{
			DefaultCode: os.Getenv(EnvDefaultMerchant),
			BaseDomain:  os.Getenv(EnvBaseDomain),
		},
		Payment: service.PaymentConfig{Sandbox: sandboxEnabled(os.Getenv(EnvPaymentSandbox))},
	}
}

// sandboxEnabled 解 KEEL_PAYMENT_SANDBOX。空 = 开。
//
// 只认三个明确的「关」，别的一律当开：一个写错的值（"no"、"disabled"）
// 被当成「关」的话，部署方会以为自己关掉了沙箱，而这条路其实还开着 ——
// 那是这个开关最坏的失效方向。反过来（写错时当成开）只会让 Demo 继续能跑，
// 而启动日志里那条 WARN 还在，看得见。
func sandboxEnabled(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "false", "0":
		return false
	default:
		return true
	}
}

// Router 装路由。测试与 main 共用它，所以测试打的是真实的那套链路，
// 而不是一份在旁边慢慢跑偏的复制品。
// orders 是外面造好之后传进来的，与 product / auth 两个服务在这里现场 new
// 不一样。理由是一个真实的环：下单服务的 SAGA 分支要注册进协调器，而注册必须
// 发生在 dtm.Start 之前 —— 也就是在 Router 被调用之前。所以它只能先在 Run 里
// 造出来、注册、Start，再带着一个接好的协调器进到这里。
//
// 传 nil 会让 /orders 那两条路由挂上去却在第一次下单时报 500。Run 不会这么做；
// 测试要这么做的话，那正是它想测的东西。
//
// embedder 与 orders 不同：**传 nil 是一个正常形态**，那时 /search 只跑关键词
// 那一路，仍然返回结果。这正是语义检索层 §8 的降级链（「任何一环故障，
// 搜索都必须仍能返回结果」），而 README 承诺的那条 `docker compose up`
// 里本来就没有推理引擎 —— 引擎是 infero，GPU-only，跑在 compose 之外的宿主机
// 进程里（scripts/infero-up.sh + compose.infero.yaml）。没有 NVIDIA GPU 的机器
// 今天**只有**这个形态：栈起得来、搜索有结果，只是没有语义召回。
// 它与「派生数据入库任务没有引擎就拒绝构造」刻意相反，两边的理由都写在
// service/search.go 与 service/index.go 的文件头。
func Router(pool *pgxpool.Pool, res *tenant.Resolver, signer *auth.Signer,
	orders *service.OrderService, payment service.PaymentConfig,
	embedder inference.Embedder) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())

	// 必须在所有业务中间件之外：它靠 c.Next() 返回之后 drain c.Errors，
	// 挂在里层会漏掉外层中间件（比如租户解析）记下的错误。
	r.Use(logHandlerErrors())

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

	// version 和 healthz 并排，理由是同一条：它们都不是业务接口，都不该经过
	// 租户中间件。**但两者刻意分开，没有把版本号塞进 healthz 的响应体**——
	// 存活探针的正文是编排系统在轮询的东西，往里加字段等于让每一次探活都多
	// 传一点没人读的数据；更要紧的是 compose 与 k8s 的探针配置里常常写着
	// 对正文的精确匹配，改它的形状是一次会在别人的部署里生效的破坏。
	//
	// 这条路由对外可见，而仓库本身是开源的 —— commit sha 不是秘密。
	// 它换来的是 issue 里「你跑的是哪一版」有一个不靠人回忆的答案。
	r.GET("/version", func(c *gin.Context) { c.JSON(http.StatusOK, buildinfo.Get()) })

	repo := repository.New(pool)
	ph := handler.NewProductHandler(service.NewProductService(repo))
	ah := handler.NewAuthHandler(service.NewAuthService(repo, signer, nil))
	oh := handler.NewOrderHandler(orders)

	// 发起支付与支付回调共用同一个 PaymentService —— 沙箱造出来的报文与回调
	// 认得的报文必须是同一个形状、同一把密钥、同一个签名算法。两个实例的话，
	// 它们分叉时的症状是「沙箱支付 401」，看上去像密钥配错了。
	payments := service.NewPaymentService(repo, payment, nil)

	v1 := r.Group("/api/v1", res.Middleware())
	v1.GET("/products", ph.List)

	// 混合检索。契约里它是 security: []（公开的）：还没登录的人也要搜得到东西，
	// 否则小程序首页的搜索框要先弹登录。**这不等于它不校验租户** ——
	// 租户由上面那道 res.Middleware() 从 Host 定出来，而真正挡住跨店结果的是
	// RLS：两条召回查询里一个 merchant_id 都没有（db/queries/search.sql）。
	//
	// 它走的是本仓库第一条**索引扫描**读路径（HNSW），与之前验过的顺序扫描
	// 不是同一条 —— internal/handler/search_test.go 里有一条用真实数据跑的
	// 跨租户断言专门盯这条路。
	//
	// 它是全仓库唯一一条**又公开、又每次请求都跑模型推理**的路由，所以也是
	// 唯一一条挂限流的：验收实测 20 个并发的 198 字查询（契约允许的长度）
	// 就能把整站压成纯关键词，而访客看不出任何异常。桶的形状、它挡得住什么、
	// 挡不住什么，都写在 ratelimit.go 的文件头。请求体大小闸门在 handler 里
	// （handler.MaxSearchBodyBytes），和 webhook 那处同一个顺序：先限大小再解析。
	v1.POST("/search", rateLimitByIP(searchRateLimiterFromEnv()), handler.NewSearchHandler(
		service.NewSearchService(repo, embedder, service.SearchConfig{}, nil)).Search)

	// 商品详情与列表一样是 security: []（契约里两条都写着）：还没登录的人
	// 也要看得到商品，否则小程序的首页到详情页这一跳就需要先登录。
	v1.GET("/products/:product_id", ph.Detail)

	// /auth/login 与 /auth/refresh 在契约里是 security: []（公开的）：
	// 一个还没有令牌的人要能打到它们。**这不等于它们不校验租户** ——
	// 租户由上面那道 res.Middleware() 从 Host 定出来，而 refresh 自己会再核对
	// 「这串令牌是不是签给这家店的」（见 service.AuthService.Refresh）。
	v1.POST("/auth/login", ah.Login)
	v1.POST("/auth/refresh", ah.Refresh)

	// /auth/logout 要令牌：契约里它没有 security: []，继承全局的 bearerAuth。
	// bearer 中间件挂在租户中间件**之后**（v1 这个组已经带着后者），
	// 顺序反了的话它取不到租户，也就没法校验令牌属不属于这家店。
	v1.POST("/auth/logout", auth.Bearer(signer, nil), ah.Logout)

	// 下单两条接口都要令牌：契约里它们没有 security: []，继承全局 bearerAuth。
	// 试算也要 —— 它读的是这个买家的价格，而且 Create 与它共用同一份定价，
	// 一条要身份另一条不要会让「两边算出来一样」这条性质多一个可以破的口子。
	v1.POST("/orders/preview", auth.Bearer(signer, nil), oh.Preview)
	v1.POST("/orders", auth.Bearer(signer, nil), oh.Create)

	// 买家侧的两条读接口与发起支付。三条都要令牌，而且理由比下单更硬：
	// **它们读的是「我的」东西**。租户由 res.Middleware() 挡住，
	// 而「同一家店里这一单是不是你的」只能由令牌里的 user_id 回答 ——
	// 这道 auth.Bearer 摘掉之后，service 那边 auth.FromContext 会返回
	// ErrNoUser（它刻意不回落到任何默认用户），于是请求 500 而不是
	// 匿名读到全店的订单。
	v1.GET("/orders", auth.Bearer(signer, nil), oh.List)
	v1.GET("/orders/:order_no", auth.Bearer(signer, nil), oh.Detail)
	v1.POST("/orders/:order_no/payments", auth.Bearer(signer, nil),
		handler.NewPaymentHandler(payments).Create)

	// 支付渠道异步回调。契约里它是 security: []（调用方是渠道，它没有令牌），
	// 所以**没有** auth.Bearer —— 但它仍然在 v1 组里，也就仍然带着上面那道
	// res.Middleware()。
	//
	// 这一点值得在装配这一处再写一遍，因为它是这条路由与别人唯一的差别：
	// **不需要令牌不等于不需要租户。** 租户照旧从 Host 来，和别的每一条接口
	// 一模一样，没有为 webhook 破例（internal/tenant/resolver.go 写着
	// 「刻意不支持用请求头指定租户」，而一条未认证的接口正是那条规矩最该守的
	// 地方）。Host 回答「哪家店」，签名回答「这是不是真的」——
	// 两者合起来才成立，完整论证在 service/payment.go 的文件头。
	//
	// 它与上面那条发起支付共用同一个 PaymentService，而两者的 security 相反
	// （那条要 bearer，这条不要）。同一个服务上挂着一条认证接口和一条未认证
	// 接口，差别只在这一行有没有 auth.Bearer —— 所以这一行是要盯着看的那一行。
	v1.POST("/webhooks/payments/:channel",
		handler.NewPaymentWebhookHandler(payments).Notify)

	// -----------------------------------------------------------------------
	// 后台（契约约定 6：后台接口一律挂在 /admin/ 前缀下，与前台分开鉴权）
	// -----------------------------------------------------------------------
	//
	// 它们仍然在 v1 组里，也就仍然带着 res.Middleware()。**后台不需要买家的
	// 令牌，但它一样需要租户**：一个商家级操作员打的是自己那家店的域名，
	// 而 auth.StaffBearer 要拿那个租户去比对令牌里的那一个。
	// internal/tenant/resolver.go 那句「刻意不支持用请求头指定租户」在这里
	// 同样成立，理由还更硬 —— 后台 token 能读全部订单与客户手机号。
	//
	// 前三条是 security: []（未认证的），所以它们**不挂** auth.StaffBearer：
	// 一个还没有会话的人要能打到它们。「不需要令牌」不等于「不需要租户」。
	staffSvc := service.NewStaffService(repo, signer, nil)
	adm := handler.NewAdminAuthHandler(staffSvc)
	v1.POST("/admin/auth/bootstrap", adm.Bootstrap)
	v1.POST("/admin/auth/email-link", adm.EmailLink)
	v1.POST("/admin/auth/session", adm.Session)

	// 其余的都要后台会话。中间件挂在租户中间件**之后**（v1 这个组已经带着
	// 后者），顺序反了的话它取不到租户，也就没法校验令牌属不属于这家店。
	//
	// **它与买家那道 auth.Bearer 是两个不同的中间件、两个不同的 context key。**
	// 共用一个的话，一条 /admin/ 路由会把 staff_id 当成 user_id 用，
	// 而两张表的 id 来自同一种自增序列 —— 撞上不是小概率，是日常。
	staffAuth := auth.StaffBearer(signer, staffSvc, nil)
	v1.GET("/admin/me", staffAuth, adm.Me)
	v1.GET("/admin/staff", staffAuth, adm.ListStaff)
	v1.POST("/admin/staff", staffAuth, adm.CreateStaff)
	v1.PATCH("/admin/staff/:staff_id", staffAuth, adm.UpdateStaff)

	// 开店（M4 收尾）。它挂同一道 staffAuth，而「只有平台级管理员能调」
	// 是业务规则，在 service.StaffService.OpenShop 里 —— 不在这里再套一层
	// 中间件：那会让同一个判据有两份实现，而中间件那一份没有任何测试盯着
	// 「它到底挂没挂在这条路由上」。
	v1.POST("/admin/merchants", staffAuth, adm.OpenShop)

	// -----------------------------------------------------------------------
	// 商家自助发布：商品 / SKU / 库存 / 类目 / 上传（M4 Task 3，契约 16 条）
	// -----------------------------------------------------------------------
	//
	// **每一条都挂 staffAuth**，一条都不能漏。漏掉一条的症状不是 401，
	// 是一个匿名请求能改这家店的商品价格并拿到 200 —— 租户中间件仍然会把
	// Host 解出来，WithTenant 照常开事务。
	//
	// 这件事有两道执行者，缺一不可：
	//   · internal/app/run_test.go 的 TestRouterServesContractPaths 核路径；
	//   · internal/handler 的 TestAdminCatalogRoutesAllRequireStaffSession
	//     **逐条**不带令牌打一次，断言它们全是 401。只核路径的话，
	//     把这一行的 staffAuth 删掉，路由表一个字都不会变。
	// **同一个 store 实例**交给写那一侧与读那一侧。建两个的话它们各自算一遍
	// 根目录（uploadStoreFromEnv 没配环境变量时会落一个临时目录），
	// 于是「写进 A 目录、从 B 目录读」—— 症状是每一张刚传上去的图都 500，
	// 而两边的配置看上去都对。
	store := uploadStoreFromEnv()

	cat := handler.NewAdminCatalogHandler(
		service.NewAdminCatalogService(repo, store))

	v1.POST("/admin/uploads", staffAuth, cat.CreateUpload)

	v1.GET("/admin/products", staffAuth, cat.ListProducts)
	v1.POST("/admin/products", staffAuth, cat.Create)
	v1.GET("/admin/products/:product_id", staffAuth, cat.Detail)
	v1.PATCH("/admin/products/:product_id", staffAuth, cat.Update)
	v1.DELETE("/admin/products/:product_id", staffAuth, cat.Delete)
	v1.POST("/admin/products/:product_id/publication", staffAuth, cat.Publication)
	v1.PUT("/admin/products/:product_id/images", staffAuth, cat.ReplaceImages)
	v1.POST("/admin/products/:product_id/skus", staffAuth, cat.CreateSKU)

	v1.PATCH("/admin/skus/:sku_id", staffAuth, cat.UpdateSKU)
	v1.DELETE("/admin/skus/:sku_id", staffAuth, cat.DeleteSKU)
	v1.PUT("/admin/skus/:sku_id/inventory", staffAuth, cat.SetInventory)

	v1.GET("/admin/categories", staffAuth, cat.ListCategories)
	v1.POST("/admin/categories", staffAuth, cat.CreateCategory)
	v1.PATCH("/admin/categories/:category_id", staffAuth, cat.UpdateCategory)
	v1.DELETE("/admin/categories/:category_id", staffAuth, cat.DeleteCategory)

	// -----------------------------------------------------------------------
	// 读文件（M4 收尾）。**买家侧**，所以一道后台鉴权都没有。
	// -----------------------------------------------------------------------
	//
	// 上面那条 POST /admin/uploads 返回的 url 指的就是第一条。在它落地之前，
	// 后台传完图拿到的地址打过去是 404。
	//
	// 两条路由：第一条判归属再 302，第二条是那个限时地址本身，只认签名与
	// 过期时间。第二条**不在契约里**（契约描述的是「跳到 driver 生成的限时
	// 地址」，形状随 driver 变），它在 contract_test.go 的 nonContractRoutes
	// 里挂着账。两跳各自挡什么，写在 service/upload.go 的文件头。
	//
	// 它们不挂 auth.Bearer：契约里这条接口没有 security，商品图本就公开。
	// **这不等于它们不校验租户** —— 租户由 res.Middleware() 从 Host 定出来，
	// 而挡住跨店读取的是 uploads 上那条 RLS 策略（db/queries/uploads.sql 里
	// 一个 merchant_id 都没有）。
	uh := handler.NewUploadHandler(service.NewUploadService(repo, store, signer))
	v1.GET("/uploads/:upload_id", uh.Redirect)
	v1.GET("/uploads/:upload_id/blob", uh.Blob)
	return r
}

// uploadStoreFromEnv 按 KEEL_UPLOAD_ROOT 建本地磁盘 driver（数据模型 §13）。
//
// 在 Router 里读环境变量，与 searchRateLimiterFromEnv 同一个先例：
// 它是一件「装配时决定、之后不再变」的事，而把它提成 Router 的参数会让
// 每一个调用点（cmd、四个测试包）都要多传一个它们不关心的东西。
//
// ===========================================================================
// 没配它的时候用临时目录，并且喊出来
// ===========================================================================
//
// 这与 KEEL_DTM_DSN 那条「空着就拒绝启动」不同类，判据还是看代价
// （与 KEEL_AUTH_SECRET 那一段同构）：
//
//   - 协调器的存储丢了是**不可恢复的数据损失**（已扣的库存再没人回补），
//     所以它宁可拒绝启动。
//   - 商品图丢了，商家重新传一次即可。**可恢复**。为它拒绝启动，代价是
//     README 承诺的那条 docker compose up 再也不是一条命令 ——
//     而 docker/ 与 compose*.yaml 不在本任务的范围里，我加不了那个卷。
//
// 但兜底不是免费的，两条代价必须说清楚，所以有这条 WARN：
//
//   - 临时目录会被系统清理，也不跨容器重建存活 —— **已上架商品的图会消失**，
//     而 uploads 表里那些记录还在，指着一堆不存在的文件；
//   - 多实例部署下这个兜底是错的：A 实例写的文件 B 实例读不到。
//
// 任何一个真的在服务客人的部署都必须把它指到一个挂载的卷上。
func uploadStoreFromEnv() *service.LocalDiskStore {
	root := strings.TrimSpace(os.Getenv(EnvUploadRoot))
	if root == "" {
		root = filepath.Join(os.TempDir(), "keel-uploads")
		slog.Warn("没有配置 " + EnvUploadRoot + "，商品图落在临时目录 " + root +
			"：系统清理或容器重建之后这些文件会消失，而 uploads 表里的记录还在，" +
			"指着一堆不存在的文件；多实例部署下这个兜底是错的（A 实例写的 B 实例读不到）。" +
			"任何在服务客人的部署都请把它指到一个挂载的卷上")
	}
	return service.NewLocalDiskStore(root)
}

// Branches 是要注册到协调器上的全部进程内分支，键就是编排里 "local://" 后面
// 那个名字。
//
// **注册必须发生在 Start 之前**（顺序封在 dtm.Start 里）。这条顺序踩错了不当场
// 报错 —— 症状要等到第一次提交时才出现（未注册的 local:// 名字在提交期被拒），
// 那时错误指向的是提交它的那段业务代码。
//
// 分支清单由下单服务自己给出（service.OrderService.Branches），而不是在这里
// 抄一份名字：抄一份就意味着编排 JSON、注册表、这里，三处要同时对得上，
// 而其中两处对不上时没有任何编译错误。
func Branches(orders *service.OrderService) map[string]dtm.BranchFunc {
	return orders.Branches()
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
	// 版本号排在所有事情之前，包括建连接池。
	//
	// 启动失败的日志才是最需要它的那一份：连不上库、自检不过、协调器起不来、
	// 这三条都会让 Run 直接返回，而排查的第一个问题永远是「跑的是哪一版」。
	// 放在成功路径上的话，恰恰是最该有版本号的那些日志里没有。
	slog.InfoContext(ctx, "keel "+buildinfo.String())

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
	signer, err := authSigner(ctx, cfg)
	if err != nil {
		return err
	}

	// 下单服务要先造出来才能拿到它的分支，而协调器要先拿到分支才能 Start，
	// 服务又要在 Start 之后才能拿到协调器 —— 这个环在
	// service.OrderService.AttachCoordinator 那里被打开，理由写在那儿。
	orders := service.NewOrderService(repository.New(pool), nil, nil)

	tc, err := dtm.Start(cfg.DTMDSN, 0, Branches(orders))
	if err != nil {
		return fmt.Errorf("启动事务协调器失败（%s）: %w", EnvDTMDSN, err)
	}
	orders.AttachCoordinator(tc)
	// 干净收尾：listen 返回（不论正常还是出错）之后把协调器关掉，
	// 它才有机会把 tokio 运行时停下来、把注册分支的 cgo.Handle 还回去。
	// Close 是幂等的，所以这条 defer 与将来可能加的显式收尾不会撞车。
	defer tc.Close()

	// 超时补偿定时任务（Task 6）。
	//
	// **它排在协调器之后、监听之前，而且两头都是硬的。**
	//
	//   - 排在协调器之后：它处理的正是 SAGA 在正向阶段扣掉、而用户始终没付钱的
	//     那批库存。协调器没起来就一笔订单也做不了，也就没有它要补的东西；
	//     更要紧的是协调器起不来时 Run 会直接返回，那时不该已经有一个后台
	//     goroutine 在扫库。
	//   - 排在监听之前：这个任务停掉的代价不是「慢一点」，是**永久漏卖**
	//     （架构 §5 的「少卖」从可恢复变成不可恢复）。先开始接单、再去起补偿，
	//     中间那段时间里下的单如果没付，它们的库存要等到下一次重启才有人管。
	//
	// 用一个跟着 listen 的生命周期走的 ctx：listen 返回（进程要退了）时
	// cancel，Run 里那个 select 会走 ctx.Done() 那一支干净退出，
	// 而不是被进程退出从一次事务中间掐断。
	// 两个后台任务（超时补偿、派生数据入库）共用这一个 ctx：它们的生命周期是
	// 同一条 —— 跟着 listen 走，进程要退时一起收到取消。
	bgCtx, stopBackground := context.WithCancel(ctx)
	defer stopBackground()
	sweeper := service.NewSweepService(repository.New(pool), service.SweepConfig{}, nil)
	go sweeper.Run(bgCtx)

	// 派生数据入库的增量任务（M3 Task 3）：商品变了就把文本向量与 bigram 串重算。
	//
	// **没配 KEEL_EMBED_ENDPOINT 时它不启动，而且要喊出来。**
	//
	// 这里与 KEEL_DTM_DSN 那条「空着就拒绝启动」不同类，理由与 KEEL_AUTH_SECRET
	// 那一段同构 —— 看代价：协调器的存储丢了是不可恢复的数据损失，而没有推理引擎
	// 只是**检索效果**降级（关键词召回那一半还在，因为 bigram 串不依赖引擎……
	// 不，它也停了：没有引擎就没有这个任务，两份派生数据一起停）。
	//
	// 那为什么还不拒绝启动？因为 README 承诺的 `docker compose up` 里没有引擎 ——
	// M4 起引擎是 infero，它**根本不在 compose 里**（GPU-only，跑在宿主机进程里，
	// 见 scripts/infero-up.sh 的文件头）。让主 compose 因为缺它而起不来，
	// 等于把那条一行命令的 Demo 废掉，而且是在一台可能根本没有 GPU 的机器上。
	//
	// 代价说清楚，所以有这条 WARN：不启动它的后果是**新品与改过的商品搜不到**，
	// 而且症状出现在几小时后 —— 索引是异步的，没有人在等它的返回码。
	embedder, embErr := inference.FromEnv()
	if embErr != nil {
		slog.WarnContext(ctx, "没有配置 "+inference.EnvEndpoint+
			"，商品派生数据入库任务不启动：文本向量与 bigram 关键词串都不会被维护。"+
			"后果是新建与改过的商品搜不到（向量表没有它们的行，search_text 还是 NULL），"+
			"而且不会有任何报错 —— 索引是异步的，没有人在等它的返回码。"+
			"POST /search 仍然可用，但只剩关键词那一路（语义检索层 §8 的降级链）—— "+
			"而关键词那一路依赖的 search_text 也由这个任务维护，所以新品两路都搜不到。"+
			"要开起来（需要 NVIDIA GPU）：先在宿主机 ./scripts/infero-up.sh，"+
			"再带上 compose.infero.yaml 这个叠加层起栈，它会把 "+inference.EnvEndpoint+
			" 指到 http://host.docker.internal:18081。"+
			"没有 GPU 的机器今天没有语义检索这条路 —— infero 只有 CUDA / Metal 后端",
			"err", embErr)
	} else {
		indexer, err := service.NewIndexService(repository.New(pool), embedder,
			service.IndexConfig{}, nil)
		if err != nil {
			return fmt.Errorf("建派生数据入库任务失败: %w", err)
		}
		go indexer.Run(bgCtx)
	}

	if cfg.Payment.Sandbox {
		// 这条 WARN 是那个默认值的另一半。没有它，一个忘了配
		// KEEL_PAYMENT_SANDBOX 的部署里，「买家能免费把自己的订单推成已支付」
		// 这件事在任何地方都没有痕迹。
		slog.WarnContext(ctx, "沙箱支付是开着的（"+EnvPaymentSandbox+" 未设为 off）："+
			"POST /orders/{order_no}/payments 返回的是带 KEEL-SANDBOX- 前缀的沙箱参数，"+
			"买家可以据此给自己的订单造一份合法签名的回调、把它推成已支付。"+
			"这不是真实支付，任何在收钱的部署都必须设 "+EnvPaymentSandbox+"=off")
	}

	// 没配引擎时交给检索的必须是一个**真的 nil 接口**。
	//
	// 不能写成 `var e inference.Embedder = embedder`：embedder 的静态类型是
	// *inference.Client，FromEnv 出错时它是一个 nil 指针，而一个装着 nil 指针的
	// 接口值 `!= nil`。那样 NewSearchService 会以为自己拿到了引擎，
	// 第一次检索在 s.emb.Embed 上 panic —— 一个只在「没配引擎的部署」上出现、
	// 而且发生在请求处理中的 nil 解引用。降级链要挡的正是这种部署。
	var searchEmbedder inference.Embedder
	if embErr == nil {
		searchEmbedder = embedder
	}

	// 后台引导（数据模型 §14 认证流程 ①）。**排在监听之前**，与 Preflight
	// 同一个理由：它要在有人能打到 /admin/auth/bootstrap 之前就把那串
	// 一次性 token 打进日志，否则第一个请求可能落在「账号已经建了、
	// 而运维还没见过 token」的那一瞬间。
	//
	// 它不拒绝启动：引导失败（比如库里已经有平台管理员了，那是**正常**情况）
	// 不该让一个能正常服务买家的进程起不来。
	if err := bootstrapStaff(ctx, pool); err != nil {
		return err
	}

	return listen(cfg.Addr, Router(pool, res, signer, orders, cfg.Payment, searchEmbedder))
}

// bootstrapStaff 在库里一个在岗平台级管理员都没有时，建一个并把那串一次性
// 引导 token **明文打进日志**。
//
// 为什么是 stdout 而不是邮件，数据模型 §14 写过完整论证：全新部署时 SMTP
// 大概率还没配，如果登录的唯一入口是邮件，操作员第一次就进不去后台 ——
// 而本项目的卖点是 docker compose up 一条命令。Jupyter 与 Gitea 都这么做。
//
// 代价是容器日志里会短暂出现一个高权限凭据。所以它 24 小时过期、用掉即失效，
// 并且只在没有在岗平台管理员时才生成（service.StaffService.EnsureBootstrapAdmin
// 里写着这个判据为什么不是 §14 原话里的「库里一个 staff 都没有」）。
//
// **它在这里而不是在那条路由里**，这是「POST /admin/auth/bootstrap 凭什么能
// 被调用」的全部答案：那条接口是未认证的，如果它自己能凭空建出一个管理员，
// 任何能打到这个端口的人都能给自己开一个后台全权账号。账号只在进程启动时、
// 在进程内部建出来，不接受任何外部输入；那条接口只做一件事 ——
// 拿一串只有能读到日志的人才见过的一次性 token 换会话。
//
// signer 不传进来：引导只写库，不签会话令牌（会话是那条路由签的）。
func bootstrapStaff(ctx context.Context, pool *pgxpool.Pool) error {
	token, err := service.NewStaffService(repository.New(pool), nil, nil).
		EnsureBootstrapAdmin(ctx)
	if err != nil {
		return fmt.Errorf("后台引导失败: %w", err)
	}
	if token == "" {
		return nil
	}
	// 用 Warn 而不是 Info：这是一条**高权限凭据**，它出现在日志里这件事
	// 本身就该被看见。level=info 在很多部署里是被过滤掉的，而这一行被过滤掉
	// 意味着这个部署的后台从此谁也进不去（除非接上 SMTP）。
	slog.WarnContext(ctx, "后台还没有平台级管理员，已创建一个并签发引导 token。"+
		"用它换会话：POST /api/v1/admin/auth/bootstrap {\"token\":\"<下面这串>\","+
		"\"email\":\"<你的邮箱>\"}。24 小时有效，用掉即失效，明文只出现这一次",
		"bootstrap_token", token)
	return nil
}

// authSigner 按配置建令牌签名器。没配 KEEL_AUTH_SECRET 时随机取一个并告警，
// 理由与两条代价写在 EnvAuthSecret 那段。
//
// 密钥长度下限 32 字节：HMAC-SHA256 的安全强度就是密钥熵，而一个
// "keel" 这样的密钥意味着任何人都能签出一串合法的令牌 —— 那不是「弱一点」，
// 是整套鉴权不成立。短了就拒绝启动：这一条与「没配」不同，
// 配了一个短密钥的人以为自己配好了。
func authSigner(ctx context.Context, cfg Config) (*auth.Signer, error) {
	const minLen = 32
	if cfg.AuthSecret == "" {
		key, err := auth.NewRandomKey()
		if err != nil {
			return nil, err
		}
		slog.WarnContext(ctx, "没有配置 "+EnvAuthSecret+"，本次启动随机取了一个签名密钥："+
			"进程一重启全体买家会被登出一次；多实例部署下这样是错的"+
			"（A 实例签的令牌在 B 实例验不过，症状是刷新几次就要重新登录）。"+
			"任何在服务客人的部署都请显式配置它（至少 32 字节随机值）")
		return auth.NewSigner(key), nil
	}
	if len(cfg.AuthSecret) < minLen {
		return nil, fmt.Errorf(
			"%s 只有 %d 字节，至少要 %d 字节：HMAC 的安全强度就是密钥的熵，"+
				"密钥猜得到的话任何人都能签出一串合法令牌，整套鉴权不成立",
			EnvAuthSecret, len(cfg.AuthSecret), minLen)
	}
	return auth.NewSigner([]byte(cfg.AuthSecret)), nil
}
