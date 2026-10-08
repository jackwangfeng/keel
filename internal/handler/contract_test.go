package handler_test

import (
	"net/http"
	"strings"
	"testing"
)

// apiPrefix 是契约路径在 gin 里的挂载前缀。契约写的是 /products，
// 实际注册的是 /api/v1/products。
const apiPrefix = "/api/v1"

// route 把一条接口的三个身份钉在一起：契约里的 (path, method)、gin 注册的路由、
// 实现它的 handler 源文件。
//
// 做成表而不是几个常量，是因为常量只描述得了第一条路由。第二条落地时，
// 常量版的测试不会红、不会报、不会提醒任何人 —— 新接口的参数清账就这么漏掉了。
// 表加上下面那条与 gin 路由表的对账，让「加路由必须在这里登记一行」成为机械约束。
type route struct {
	ContractPath   string // 契约里的路径，如 /products
	ContractMethod string // 契约里的方法，小写
	HTTPMethod     string // gin 注册时的方法，大写
	HandlerFile    string // 实现它的 handler 源文件（相对本包目录）

	// NotYetImplemented 是契约声明了、这条 handler 还没读的 query 参数。
	//
	// 这是一笔**必须维护的欠账**，不是豁免。它三个方向都会红：
	//   - 契约新增一个 query 参数而 handler 没读、这里也没挂账 → 红，
	//     逼人当场决定「实现它」还是「先记在这里」。默认行为是红，不是静默忽略。
	//   - 实现了某个参数却忘了从这里划掉 → 红。「实现完删掉一行」于是成了
	//     天然的验收动作，不靠人记得。
	//   - 这里写了一个契约里根本没有的名字 → 红。清单不能烂掉。
	//
	// 为什么不是返回 400：契约里这些都是 optional，对一份冻结的契约把可选参数
	// 判成错误是违约，而且实现之后还得把那个 400 撤回去。也不在契约的 description
	// 里写「未实现」—— 契约描述的是接口，不是实现进度。
	NotYetImplemented map[string]string

	// NoQueryParams 非空时表示「这条接口在契约里一个 query 参数都没有」，
	// 值是写下这句话的依据。
	//
	// 需要这个字段，是因为下面那条参数对账测试有一道阳性对照：契约侧解析出
	// 零个参数就 Fatal —— 那是在防「路径改名之后测试恒绿」。而 /auth/* 三条
	// 接口本来就没有 query 参数，它们会当场撞上这道对照。
	//
	// 处理方式不是给对照开个口子（那会让路径改名重新变得无声无息），
	// 而是把「没有参数」也变成一条**登记过的事实**：登记了却在契约里长出
	// 参数来 → 红；登记了而 handler 却在读 query → 红。
	NoQueryParams string

	// NotYetImplementedBody 是**请求体**里契约声明了、这条 handler 还没实现的
	// 字段，与 NotYetImplemented 是同一笔账的两种形状。
	//
	// 眼下只有一条：/auth/login 的 code（短信验证码）。本项目没有接短信服务，
	// 所以那条路返回一个明确的 501，而不是假装失败。
	//
	// 它比 query 那份弱一格，要说清楚：query 参数能从 handler 的 AST 里读出
	// c.Query("x") 来做双向对账，而「请求体里的某个字段有没有被实现」读不出来
	// —— 结构体上有这个 json tag 只说明它被解析了，不说明它被实现了。
	// 所以反向（实现了却忘了划掉）由行为测试盯着：
	// auth_test.go 的 TestSMSLoginSaysItIsNotImplemented 断言那条路真的返回 501,
	// 并且断言这里真的挂着这一笔。实现了它，那条测试就会红。
	NotYetImplementedBody map[string]string

	// （这里原先还有第三种形状 NotYetImplementedHeader：契约声明为 required 的
	// 请求头里 handler 还没实现的那些。最后挂着的两笔是 POST /admin/merchants 与
	// POST /admin/staff 的 Idempotency-Key，00028 给幂等键加了平台级落点之后
	// 两条都实现了幂等，挂账清空，字段与它那条对账测试一起删掉 —— 留着一条恒绿的
	// 测试比没有更糟。行为由 admin_auth_test.go 的 TestPlatformScopedWritesAreIdempotent
	// 与 TestMerchantStaffCreateIsIdempotentToo 盯着。）

	// （这里原先还有第三笔账 NotYetImplementedResponse：**响应体**里契约声明了、
	// handler 刻意不填的字段。挂过的有下单两条接口的 freight_cents（00056 运费落地
	// 后划掉，行为由 freight_test.go 的 TestPreviewAndOrderCarryComputedFreight 盯着）
	// 与商品详情的 image_url / images（商品图接进买家读路径后划掉，行为由
	// product_image_test.go 盯着：有图时按 sort_order 返回、没图时字段缺席）。
	// 清单空了，字段连同它那条「清单 → 契约」的对账测试一起删掉 —— 留着一条恒绿的
	// 测试比没有更糟。下次再有响应字段要挂账，把字段和那条测试从历史里捡回来。）

	// NotYetImplementedStage 是契约的 **description** 里写着、这条 handler
	// 还没跑的流水线阶段，与前三笔账是同一件事的第四种形状。
	//
	// 需要第四种形状，是因为前三种都够不着这一笔。/search 的响应形状是完整的
	// （items / latency_ms / total / strategy 一个不少），请求参数也全实现了 ——
	// 少做的是**流水线里的某一层**（M3 时是精排与业务重排两层，M5 之后剩精排，
	// 外加业务重排里的两个因子），而那件事只写在 description 的一句话里：
	// 「四层流水线：双路召回 → RRF 融合 → Reranker 精排 → 业务重排」。
	// 没有这笔账的话，「接口看上去全实现了，实际只跑了一部分」在任何闸门里
	// 都留不下痕迹。
	//
	// 键是**契约描述里那几个字**，逐字。两个方向都锁得住：
	//   - 契约把这个阶段改名或删掉 → TestNotYetImplementedStagesAreNamedInContract
	//     红（清单在描述一个契约里不存在的东西）；
	//   - 真的实现了这一层、响应里开始出现它的得分 → search_test.go 的
	//     TestExplainListsExactlyTheStagesThatRan 红，逼人回来删掉这一行。
	NotYetImplementedStage map[string]string
}

// ginPath 把契约路径翻成 gin 注册的那一个：加前缀，并把 OpenAPI 的 {name}
// 换成 gin 的 :name。
//
// 两种写法在这张表里必须有一份是源、另一份是导出来的，不能各写各的：
// 手写 gin 路径的话，`/webhooks/payments/{channel}` 与
// `/api/v1/webhooks/payments/:channel` 之间任何一个字母的出入都不会红 ——
// 表里那一行只是永远配不上任何已注册路由，而那条对账测试会把它报成
// 「登记了但没注册」，指向一个错的方向。
func (r route) ginPath() string { return ginPathOf(r.ContractPath) }

// ginPathOf 是上面那段推理的实现，抽成自由函数是因为下面那张「契约里有、还没
// 注册路由」的清单也要用同一个翻译规则 —— 两处各写一遍就是两份会各自跑偏的规则，
// 而它们跑偏时的症状是「登记了但没注册」，指向一个错的方向。
func ginPathOf(contractPath string) string {
	p := apiPrefix + contractPath
	p = strings.ReplaceAll(p, "{", ":")
	return strings.ReplaceAll(p, "}", "")
}
func (r route) key() string { return r.HTTPMethod + " " + r.ginPath() }

// routes 是本包已实现的全部契约接口。加一条路由就要在这里加一行。
var routes = []route{
	{
		ContractPath:   "/products",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "product.go",
		NotYetImplemented: map[string]string{
			// category_id 本轮接上了（买家端要做类目浏览），含子孙，规则在
			// db/queries/products.sql 的文件头。
			"sort":            "排序策略，等 Catalog 那个任务",
			"min_price_cents": "价格区间下界，同上",
			"max_price_cents": "价格区间上界，同上",
			// store_id 与响应里那个 store 本轮（00020）落地了，两笔账一起划掉。
			// 现在这条接口返回的是**门店级**目录：可见性过两层排除表，
			// 价走 sku_prices_by_store 视图。
			// in_stock_only 2026-10-01 接上了：只取有货排序标记的「有货」那一段，total 按同一段数
			// （service.ProductService.List）。
		},
	},
	{
		ContractPath:   "/categories",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "category.go",
		NoQueryParams:  "契约里这条接口一个参数都没有：返回整棵启用中的类目树，不分页",
	},
	{
		ContractPath:   "/auth/login",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "auth.go",
		NoQueryParams:  "登录参数全在请求体里（契约的 requestBody）",
		NotYetImplementedBody: map[string]string{
			"code": "短信验证码登录。本项目还没有短信服务，这条路返回 501 " +
				"（contract 的 default: Problem 收得住），而不是一个假装失败的 401。" +
				"连带地，契约里「验证码登录且手机号未注册时首登即注册」也没有实现 —— " +
				"那个语义只属于这条路，密码路径刻意不带它。",
		},
	},
	{
		ContractPath:   "/orders/preview",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "order.go",
		NoQueryParams:  "试算的入参全在请求体里（与 POST /orders 共用 OrderCreateRequest）",
		// freight_cents 的挂账 00056 划掉了：运费按收货地址算好返回（必返）。
	},
	{
		ContractPath:   "/orders",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "order.go",
		NoQueryParams:  "下单的参数在请求体与 Idempotency-Key 请求头里，没有 query 参数",
		// freight_cents 的挂账 00056 划掉了：下单时算好写进订单。
	},
	{
		ContractPath:   "/admin/channel-kinds",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_channel.go",
		NoQueryParams:  "渠道管理的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-bindings",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_channel.go",
		NoQueryParams:  "渠道管理的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-bindings",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_channel.go",
		NoQueryParams:  "渠道管理的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-bindings/{binding_id}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_channel.go",
		NoQueryParams:  "渠道管理的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-bindings/{binding_id}",
		ContractMethod: "patch",
		HTTPMethod:     http.MethodPatch,
		HandlerFile:    "admin_channel.go",
		NoQueryParams:  "渠道管理的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-bindings/{binding_id}/secrets",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_channel.go",
		NoQueryParams:  "渠道管理的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-bindings/{binding_id}/store-links",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_channel.go",
		NoQueryParams:  "渠道管理的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-bindings/{binding_id}/store-links/{store_id}",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_channel.go",
		NoQueryParams:  "渠道管理的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-bindings/{binding_id}/store-links/{store_id}",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "admin_channel.go",
		NoQueryParams:  "渠道管理的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-bindings/{binding_id}/sku-links/{sku_id}",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_channel.go",
		NoQueryParams:  "渠道管理的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-bindings/{binding_id}/stock-rules",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_channel.go",
		NoQueryParams:  "渠道管理的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-bindings/{binding_id}/stock-rules",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_channel.go",
		NoQueryParams:  "渠道管理的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-bindings/{binding_id}/stock-rules/{rule_id}",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "admin_channel.go",
		NoQueryParams:  "渠道管理的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-bindings/{binding_id}/price-rules",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_channel.go",
		NoQueryParams:  "渠道管理的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-bindings/{binding_id}/price-rules",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_channel.go",
		NoQueryParams:  "渠道管理的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-bindings/{binding_id}/price-rules/{rule_id}",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "admin_channel.go",
		NoQueryParams:  "渠道管理的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-bindings/{binding_id}/listings",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_channel_listing.go",
	},
	{
		ContractPath:   "/admin/channel-bindings/{binding_id}/catalog-pulls",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_channel.go",
		NoQueryParams:  "渠道管理的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-orders",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_channel_order_list.go",
	},
	{
		ContractPath:   "/admin/channel-orders/{channel_order_id}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_channel_order.go",
		NoQueryParams:  "渠道订单的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-orders/{channel_order_id}/retry",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_channel_order.go",
		NoQueryParams:  "渠道订单的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-orders/{channel_order_id}/accept",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_channel_order.go",
		NoQueryParams:  "渠道订单的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-orders/{channel_order_id}/reject",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_channel_order.go",
		NoQueryParams:  "渠道订单的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/admin/channel-order-requests/{request_id}/decision",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_channel_order.go",
		NoQueryParams:  "渠道订单的参数都在路径与请求体里",
	},
	{
		ContractPath:   "/webhooks/channels/{binding_id}",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "webhook_channel.go",
		NoQueryParams:  "binding 在路径里，报文与签名头原样交给该渠道的适配器验签（Shopify / 美团的签名都不在 query 里）",
	},
	{
		ContractPath:   "/webhooks/payments/{channel}",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "webhook.go",
		NoQueryParams: "渠道在路径里（path 参数 channel），报文在请求体里；" +
			"签名在 X-Keel-Signature 请求头里 —— 刻意不放 query：" +
			"query 会进访问日志，而签名进日志等于每一条日志都是一次密钥泄露的半成品",
	},
	{
		ContractPath:   "/products/{product_id}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "product_detail.go",
		// 唯一的 query 参数 store_id 本轮实现了，所以这里既不写 NoQueryParams
		// 也不挂账 —— 对账测试会两个方向都核一遍。
		// （这条接口原先登记的是 NoQueryParams「详情只吃路径参数」，
		// 那句话从 00020 起不再成立：门店决定了 SKU 的价与水位。）
	},
	{
		ContractPath:   "/orders",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "order_list.go",
		// 四个 query 参数（page / page_size / status / refund_status）全都实现了，
		// 所以这里既不写 NoQueryParams 也不挂账 —— 对账测试会两个方向都核一遍。
	},
	{
		ContractPath:   "/orders/{order_no}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "order_detail.go",
		NoQueryParams:  "详情只吃路径参数 order_no",
		// store_id / region_id / store 三笔早先结清；refunds 那一笔在退款域落地
		// （00034）时结清：详情在同一个事务里读退款单与每一行的在途件数。
	},
	{
		ContractPath:   "/orders/{order_no}/payments",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "payment_intent.go",
		NoQueryParams: "渠道在请求体里，订单号在路径上，幂等键在 Idempotency-Key 请求头里；" +
			"契约里这条接口没有任何 query 参数",
	},
	// —— 订单后半程（00033）：买家取消与确认收货、后台发货。
	{
		ContractPath:   "/orders/{order_no}/cancel",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "order_fulfillment.go",
		NoQueryParams:  "要取消哪一单在路径上，幂等键在 Idempotency-Key 请求头里；没有请求体",
	},
	{
		ContractPath:   "/orders/{order_no}/confirm",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "order_fulfillment.go",
		NoQueryParams:  "要确认哪一单在路径上，幂等键在 Idempotency-Key 请求头里；没有请求体",
	},
	{
		ContractPath:   "/admin/orders/{order_no}/shipments",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_order.go",
		NoQueryParams:  "承运商与运单号在请求体里，订单号在路径上，幂等键在请求头里",
	},
	// —— 售后（00034）。GET /refunds 带 page / page_size / status，单独一个文件。
	{
		ContractPath:   "/orders/{order_no}/refunds",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "refund.go",
		NoQueryParams:  "退哪几行、几件、为什么在请求体里，订单号在路径上，幂等键在请求头里；**金额不在任何一处**，由服务端按优惠分摊倒算",
	},
	{
		ContractPath:   "/orders/{order_no}/refunds",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "refund.go",
		NoQueryParams:  "一个订单的全部退款单，没有分页也没有筛选",
	},
	{
		ContractPath:   "/refunds",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "refund_list.go",
		// page / page_size / status 三个全都实现了，既不写 NoQueryParams 也不挂账。
	},
	{
		ContractPath:   "/refunds/{refund_no}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "refund.go",
		NoQueryParams:  "详情只吃路径参数 refund_no",
	},
	{
		ContractPath:   "/refunds/{refund_no}/cancel",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "refund.go",
		NoQueryParams:  "要撤回哪一张在路径上，幂等键在请求头里；没有请求体",
	},
	{
		ContractPath:   "/refunds/{refund_no}/return-shipment",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "refund.go",
		NoQueryParams:  "承运商与运单号在请求体里，退款单号在路径上，幂等键在请求头里",
	},
	{
		ContractPath:   "/admin/refunds/{refund_no}/audit",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_order.go",
		NoQueryParams:  "通过还是驳回、驳回理由、裁定的退运费都在请求体里，退款单号在路径上，幂等键在请求头里",
	},
	{
		ContractPath:   "/admin/refunds/{refund_no}/receipt",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_order.go",
		NoQueryParams:  "要确认哪一张在路径上，幂等键在请求头里；没有请求体",
	},
	// —— 后台订单与退款单的列表 / 详情（00035）。两条列表各有一串 query 参数，
	// 各自一个文件；两条详情一个参数都没有，放在一起。
	{
		ContractPath:   "/admin/orders",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_order_list.go",
		// page / page_size / status / store_id / created_from / created_to /
		// order_no / phone 全部实现了，既不写 NoQueryParams 也不挂账。
	},
	{
		ContractPath:   "/admin/orders/{order_no}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_order_detail.go",
		NoQueryParams:  "详情只吃路径参数 order_no",
	},
	{
		ContractPath:   "/admin/refunds",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_refund_list.go",
	},
	{
		ContractPath:   "/admin/payment-returns",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "payment_return.go",
	},
	{
		ContractPath:   "/admin/refunds/{refund_no}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_order_detail.go",
		NoQueryParams:  "详情只吃路径参数 refund_no",
	},
	// —— 店铺设置（00059）。
	{
		ContractPath:   "/admin/shop-settings",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_shop_settings.go",
		NoQueryParams:  "一家店只有一份设置，租户由会话（或 X-Keel-Merchant）决定",
	},
	{
		ContractPath:   "/admin/shop-settings",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_shop_settings.go",
		NoQueryParams:  "整份设置在请求体里；PUT 天然幂等，不收 Idempotency-Key",
	},
	// —— 经营报表（契约 Report tag，00057）。参数集合不同的接口各自一个文件
	// （对账按文件读 c.Query 的字面量）；概览与趋势参数一模一样，共用一个。
	// 全部参数都实现了，既不写 NoQueryParams 也不挂账。
	{
		ContractPath:   "/admin/reports/overview",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_report_overview.go",
	},
	{
		ContractPath:   "/admin/reports/trend",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_report_overview.go",
	},
	{
		ContractPath:   "/admin/reports/products",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_report_products.go",
	},
	{
		ContractPath:   "/admin/reports/stores",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_report_stores.go",
	},
	// 两份 CSV 导出与各自的 JSON 版参数集合相同，放在同一个 handler 文件里（对账按文件读 c.Query）。
	{
		ContractPath:   "/admin/reports/products.csv",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_report_products.go",
	},
	{
		ContractPath:   "/admin/reports/stores.csv",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_report_stores.go",
	},
	{
		ContractPath:   "/admin/reports/inventory-alerts",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_report_inventory.go",
	},
	{
		ContractPath:   "/admin/reports/search",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_report_search.go",
	},
	// —— 消息通知（00053，数据模型 §16）。两份列表各读 page / page_size / unread_only，
	// 各自一个文件；其余六条一个 query 参数都没有，放在 notification.go。
	{
		ContractPath:   "/me/notifications",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "notification_list.go",
	},
	{
		ContractPath:   "/me/notifications/unread-count",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "notification.go",
		NoQueryParams:  "只回一个未读数，没有任何筛选",
	},
	{
		ContractPath:   "/me/notifications/{notification_id}/read",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "notification.go",
		NoQueryParams:  "要标哪一条在路径上，没有请求体",
	},
	{
		ContractPath:   "/me/notifications/read-all",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "notification.go",
		NoQueryParams:  "标的是调用者自己的全部未读，没有参数",
	},
	{
		ContractPath:   "/admin/notifications",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_notification_list.go",
	},
	{
		ContractPath:   "/admin/notifications/unread-count",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "notification.go",
		NoQueryParams:  "只回调用者范围内的未读数，没有任何筛选",
	},
	{
		ContractPath:   "/admin/notifications/{notification_id}/read",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "notification.go",
		NoQueryParams:  "要标哪一条在路径上，没有请求体",
	},
	{
		ContractPath:   "/admin/notifications/read-all",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "notification.go",
		NoQueryParams:  "标的是调用者范围内的全部未读，没有参数",
	},
	{
		ContractPath:   "/webhooks/refunds/{channel}",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "webhook_refund.go",
		NoQueryParams: "渠道在路径里，报文在请求体里，签名在 X-Keel-Signature 请求头里 —— " +
			"刻意不放 query，理由同支付回调：query 会进访问日志",
	},
	{
		ContractPath:   "/search",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "search.go",
		NoQueryParams: "检索的参数全在请求体里（query / filters / size / strategy / explain）；" +
			"契约里这条接口一个 query 参数都没有",
		// NotYetImplementedResponse 在这里挂过两笔：store（门店上下文，00020 结清）与
		// trace_id（POST /search/events 落地那一轮结清 —— 那时它才第一次有了收得下它的
		// 接口）。反向由 search_event_test.go 的 TestSearchReturnsTheTraceIDOfItsLogRow 盯着。
		NotYetImplementedStage: map[string]string{
			"Reranker 精排": "cross-encoder 精排（语义检索层 §5 / §11 阶段 3，路线图 M5）。" +
				"它是延迟大头（§8 给 80 ms），而本轮连离线评测集（§9.1）都还没有 —— " +
				"没有评测集就上精排，等于把一层没人能判断好坏的东西放进排序里。" +
				"explain=true 时 scores.rerank **整个不出现**，而不是填 0。",
			// 业务重排本身 M5 接上了（缺货降权，internal/search/business.go），
			// 这里只挂它**没做**的那两个因子 —— 契约描述里点了名的那两个。
			"活动失效": "业务重排的 w_promo（语义检索层 §6）。营销活动的表（00058）已经有了，" +
				"「这件商品此刻有没有生效的活动」答得出来（商品标签用的就是它），但检索路径本轮没接：" +
				"§6 的 w_promo 说的是「活动失效降权」，而「失效」要先定口径（活动结束的商品算失效，" +
				"还是等同于从没参加过活动）。业务乘子目前只含 w_stock，explain 的 scores.business 就是它，" +
				"不含任何没算的因子。",
			"负毛利": "按毛利调权（语义检索层 §6「关于毛利权重的诚实建议」：默认应当关闭）。" +
				"本轮照那条建议不做，连开关都没有 —— 一个默认关闭、没有调用方会打开的开关" +
				"是一段没有执行者的代码。另：skus.cost_cents 默认 0，「成本未知」与「零成本」" +
				"分不开，按它判负毛利会把所有没填成本的商品一并误判。",
		},
	},
	{
		ContractPath:   "/search/events",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "search_event.go",
		NoQueryParams: "trace_id / event / product_id 全在请求体里；契约里这条接口一个 query 参数都没有。" +
			"与 /search 分文件，理由同 /stores 与 /stores/resolve：参数对账按文件做",
	},
	// —— 后台身份（M4 本轮）。契约 AdminAuth 与 Admin 两个 tag 的 7 条。
	{
		ContractPath:   "/admin/auth/bootstrap",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_auth.go",
		NoQueryParams: "引导 token 与邮箱都在请求体里；契约里这条接口一个 query 参数都没有。" +
			"**token 尤其不能进 query** —— query 会进访问日志，而这一串换得出" +
			"整个部署的后台全权（理由同 /webhooks/payments 那条里对签名的处理）",
	},
	{
		ContractPath:   "/admin/auth/email-link",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_auth.go",
		NoQueryParams:  "邮箱在请求体里；契约里这条接口一个 query 参数都没有",
		NotYetImplementedBody: map[string]string{
			"email": "**本项目没有接邮件服务**，所以这条路返回 501，而不是契约里那个 202。" +
				"202 的含义是「已受理」，而一封信都发不出去时它是一句在邮件服务上线之前" +
				"都不会被纠正的假话：操作员去收件箱等一封永远不来的信，而服务端这一侧" +
				"没有任何东西显示出问题 —— 没有失败的任务、没有错误日志、没有待发队列。" +
				"契约那条「无论邮箱是否存在都返回 202，否则就是账号枚举接口」的推理没有被推翻：" +
				"这条路**连查都不查**，不管邮箱是什么都走同一条路，枚举面依然是零。" +
				"形状与 /auth/login 的 code 完全一样，两个方向都锁：" +
				"admin_auth_test.go 的 TestAdminEmailLinkSaysItIsNotImplemented 断言" +
				"那条路真的返回 501，并且断言这里真的挂着这一笔。",
		},
	},
	{
		ContractPath:   "/admin/auth/session",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_auth.go",
		NoQueryParams:  "一次性 token 在请求体里，理由同 bootstrap 那条",
	},
	{
		ContractPath:   "/admin/me",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_auth.go",
		NoQueryParams:  "当前身份由 Authorization 头里那串会话 token 决定，没有任何参数",
	},
	{
		ContractPath:   "/admin/staff",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_staff_list.go",
		// page / page_size 两个参数都实现了，所以这里既不写 NoQueryParams
		// 也不挂账 —— 对账测试会两个方向都核一遍。
	},
	{
		ContractPath:   "/admin/staff",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_auth.go",
		NoQueryParams: "新员工的字段在请求体里，幂等键在 Idempotency-Key 请求头里；" +
			"**租户不在任何一处** —— 它从调用者的会话继承（契约与数据模型 §14 " +
			"认证流程 ④ 都写着这一条），落地方式是 staff.merchant_id 的 " +
			"DEFAULT staff_scope_merchant()，整条链路上没有一个 merchant_id 参数可以传错",
	},
	{
		ContractPath:   "/admin/staff/{staff_id}",
		ContractMethod: "patch",
		HTTPMethod:     http.MethodPatch,
		HandlerFile:    "admin_auth.go",
		NoQueryParams:  "要改谁在路径上，改什么在请求体里",
	},
	{
		// 重签一次性登录 token。与 PATCH 同一个 handler 文件、同一个权限判据。
		ContractPath:   "/admin/staff/{staff_id}/login-token",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_auth.go",
		NoQueryParams:  "给谁签在路径上，没有请求体，也不接受 Idempotency-Key（签新的同时作废旧的，天然幂等）",
	},
	{
		// 开店（M4 收尾）。它单独占一个 handler 文件，理由写在
		// admin_merchant.go 的头上。
		ContractPath:   "/admin/merchants",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_merchant.go",
		NoQueryParams: "店名、code 与第一个管理员的邮箱都在请求体里，幂等键在 Idempotency-Key 请求头里；" +
			"**租户不在任何一处，而这条比别的更彻底** —— 这条接口建的就是那个租户，" +
			"它由 repository.WithNewTenant 在同一个事务里造出来再切进去，" +
			"整条链路上没有一个 merchant_id 参数可以传错",
	},
	{
		// 商家列表。读 page / page_size，所以自己一个文件（admin_merchant_list.go），
		// 理由同 admin_staff_list.go。
		ContractPath:   "/admin/merchants",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_merchant_list.go",
	},
	{
		ContractPath:   "/admin/merchants/{merchant_id}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_merchant.go",
		NoQueryParams:  "要看哪家店在路径上",
	},
	{
		ContractPath:   "/admin/merchants/{merchant_id}",
		ContractMethod: "patch",
		HTTPMethod:     http.MethodPatch,
		HandlerFile:    "admin_merchant.go",
		NoQueryParams: "改哪家店在路径上，改什么（名字、状态、自有域名）在请求体里。" +
			"domain 那个键的「不传 / 显式 null / 字符串」三个态由 bindPatchBody 返回的键集合判，" +
			"路径参数与请求体之外没有任何 query；清空登记是一个功能，不是漏传",
	},
	// —— 商家自助发布（M4 Task 3）。契约 Admin + Catalog 两个 tag 的 16 条写接口。
	//
	// 它们分在四个 handler 文件里，而**分法是闸门定的**：下面那条 query 参数
	// 对账按 HandlerFile 解析整份源码里的 c.Query 调用，所以带参数的那一条
	// （GET /admin/products）必须自己一个文件，否则同文件里另外 15 条
	// NoQueryParams 的登记会被判成「handler 读了一个契约里没有的参数」。
	{
		ContractPath:   "/admin/uploads",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_upload.go",
		NoQueryParams:  "文件在 multipart 的 file 那一项里，purpose 由路径决定（固定 1 商品图）；契约里这条接口一个 query 参数都没有",
	},
	{
		// 这 16 条里**唯一**带 query 参数的一条，五个全都实现了，
		// 所以这里既不写 NoQueryParams 也不挂账 —— 对账测试会两个方向都核一遍。
		// 它单独占一个 handler 文件，理由写在 admin_product_list.go 的头上。
		ContractPath:   "/admin/products",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_product_list.go",
	},
	{
		ContractPath:   "/admin/products",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_product.go",
		NoQueryParams:  "新建商品的字段全在请求体里（ProductCreateRequest），幂等键在请求头里",
	},
	{
		ContractPath:   "/admin/products/{product_id}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_product.go",
		NoQueryParams:  "后台详情只吃路径参数 product_id；它比前台那条多返回成本、库存与图片，而那不需要任何参数",
	},
	{
		ContractPath:   "/admin/products/{product_id}",
		ContractMethod: "patch",
		HTTPMethod:     http.MethodPatch,
		HandlerFile:    "admin_product.go",
		NoQueryParams:  "要改哪件在路径上，改什么在请求体里",
	},
	{
		ContractPath:   "/admin/products/{product_id}",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "admin_product.go",
		NoQueryParams:  "软删只吃路径参数；**刻意没有一个 force 之类的参数** —— 在架商品要先下架，那是一次单独的调用，不是一个开关",
	},
	{
		ContractPath:   "/admin/products/{product_id}/publication",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_product.go",
		NoQueryParams:  "上架还是下架在请求体的 action 里，幂等键在请求头里",
	},
	{
		ContractPath:   "/admin/products/{product_id}/images",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_product.go",
		NoQueryParams:  "整组图在请求体的 images 数组里；顺序就是数组下标，没有任何参数可以改变它",
	},
	{
		ContractPath:   "/admin/products/{product_id}/skus",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_sku.go",
		NoQueryParams:  "新建规格的字段全在请求体里（SkuCreateRequest），商品在路径上，幂等键在请求头里",
	},
	{
		ContractPath:   "/admin/skus/{sku_id}",
		ContractMethod: "patch",
		HTTPMethod:     http.MethodPatch,
		HandlerFile:    "admin_sku.go",
		NoQueryParams:  "要改哪个规格在路径上，改什么在请求体里；**available_qty 不在其中任何一处**，它有自己的端点",
	},
	{
		ContractPath:   "/admin/skus/{sku_id}",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "admin_sku.go",
		NoQueryParams:  "软删只吃路径参数",
	},
	{
		ContractPath:   "/admin/skus/{sku_id}/inventory",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_sku.go",
		NoQueryParams:  "expected_available_qty 与 available_qty 都在请求体里。**expected 尤其不能进 query** —— 它是一次比较并设置的条件，而 query 会进访问日志，让一次写操作的前置条件散落在日志里没有任何好处",
	},
	{
		ContractPath:   "/admin/skus/{sku_id}/inventory/adjustments",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_sku.go",
		NoQueryParams:  "delta 与 reason 都在请求体里；门店由服务端推出（恰好一家），不是一个可以从 query 指定的开关",
	},
	{
		ContractPath:   "/admin/categories",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_category.go",
		NoQueryParams:  "后台分类列表返回整棵树拍平之后的全部节点，没有分页也没有筛选 —— 契约里这条接口一个 query 参数都没有",
	},
	{
		ContractPath:   "/admin/categories",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_category.go",
		NoQueryParams:  "名字与父节点在请求体里，path / level 由服务端算，幂等键在请求头里",
	},
	{
		ContractPath:   "/admin/categories/{category_id}",
		ContractMethod: "patch",
		HTTPMethod:     http.MethodPatch,
		HandlerFile:    "admin_category.go",
		NoQueryParams:  "要改哪个在路径上，改什么（含移动子树的 parent_id）在请求体里",
	},
	{
		ContractPath:   "/admin/categories/{category_id}",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "admin_category.go",
		NoQueryParams:  "软删只吃路径参数；两条 409 闸门没有任何可以绕过它们的参数",
	},
	// —— 商品批量导入（下载模板 → 预检 → 确认导入）。模板那一条带 format 参数且实现了，
	// 单独占一个 handler 文件（理由同 admin_product_list.go）；另外两条的输入全在 multipart 里。
	{
		ContractPath:   "/admin/product-imports/template",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_product_import_template.go",
	},
	{
		ContractPath:   "/admin/product-imports/preview",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_product_import.go",
		NoQueryParams:  "文件在 multipart 的 file 那一项里；预检只读不写，没有幂等键也没有 query 参数",
	},
	{
		ContractPath:   "/admin/product-imports",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_product_import.go",
		NoQueryParams:  "文件与类目选择在 multipart 里（file / categories），幂等键在请求头里",
	},
	// —— AI 员工写的经营简报（AI 经营 M9 任务 5）。
	{
		ContractPath:   "/admin/agent-briefs",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_agent_brief_list.go",
	},
	{
		ContractPath:   "/admin/agent-briefs/{brief_id}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_agent_brief.go",
		NoQueryParams:  "简报 id 在路径上",
	},
	// —— POI 与地址（docs/POI-设计.md）。两条各占一个文件：都读 query 参数。
	{
		ContractPath:   "/geo/reverse",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "geo_reverse.go",
	},
	{
		ContractPath:   "/geo/suggest",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "geo_suggest.go",
	},
	// 地图底图：两条都不读 query（配置没有参数，瓦片的层名与行列号在路径上）。
	{
		ContractPath:   "/geo/map",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "geo_map.go",
		NoQueryParams:  "底图配置没有参数：由部署配置决定",
	},
	{
		ContractPath:   "/geo/tiles/{layer}/{z}/{x}/{y}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "geo_map.go",
		NoQueryParams:  "层名、级别、行列号全在路径上",
	},
	// —— AI 员工的提案（AI 经营 M9 任务 4）。列表读 status / agent_staff_id / page，单独一个文件。
	{
		ContractPath:   "/admin/agent-proposals",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_agent_proposal_list.go",
	},
	{
		ContractPath:   "/admin/agent-proposals/{proposal_id}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_agent_proposal.go",
		NoQueryParams:  "提案 id 在路径上，驳回理由在请求体里",
	},
	{
		ContractPath:   "/admin/agent-proposals/{proposal_id}/approve",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_agent_proposal.go",
		NoQueryParams:  "提案 id 在路径上，驳回理由在请求体里",
	},
	{
		ContractPath:   "/admin/agent-proposals/{proposal_id}/reject",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_agent_proposal.go",
		NoQueryParams:  "提案 id 在路径上，驳回理由在请求体里",
	},
	// —— AI 员工与接入密钥（AI 经营 M9，docs/AI经营-M9设计.md §2）。
	{
		ContractPath:   "/admin/agents",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_agent.go",
		NoQueryParams:  "AI 员工与接入密钥（AI 经营 M9）：参数全在路径与请求体里",
	},
	{
		ContractPath:   "/admin/agents",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_agent.go",
		NoQueryParams:  "AI 员工与接入密钥（AI 经营 M9）：参数全在路径与请求体里",
	},
	{
		ContractPath:   "/admin/agents/{staff_id}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_agent.go",
		NoQueryParams:  "AI 员工与接入密钥（AI 经营 M9）：参数全在路径与请求体里",
	},
	{
		ContractPath:   "/admin/agents/{staff_id}",
		ContractMethod: "patch",
		HTTPMethod:     http.MethodPatch,
		HandlerFile:    "admin_agent.go",
		NoQueryParams:  "AI 员工与接入密钥（AI 经营 M9）：参数全在路径与请求体里",
	},
	{
		ContractPath:   "/admin/agents/{staff_id}/keys",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_agent.go",
		NoQueryParams:  "AI 员工与接入密钥（AI 经营 M9）：参数全在路径与请求体里",
	},
	{
		ContractPath:   "/admin/agents/{staff_id}/keys/{key_id}",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "admin_agent.go",
		NoQueryParams:  "AI 员工与接入密钥（AI 经营 M9）：参数全在路径与请求体里",
	},
	{
		ContractPath:   "/admin/agents/{staff_id}/webhook",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_agent_webhook.go",
		NoQueryParams:  "AI 员工的事件 webhook（AI 经营 M10）：参数全在路径与请求体里",
	},
	{
		ContractPath:   "/admin/agents/{staff_id}/webhook",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_agent_webhook.go",
		NoQueryParams:  "AI 员工的事件 webhook（AI 经营 M10）：参数全在路径与请求体里",
	},
	{
		ContractPath:   "/admin/agents/{staff_id}/webhook",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "admin_agent_webhook.go",
		NoQueryParams:  "AI 员工的事件 webhook（AI 经营 M10）：参数全在路径与请求体里",
	},
	{
		ContractPath:   "/agent/whoami",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_agent.go",
		NoQueryParams:  "AI 员工与接入密钥（AI 经营 M9）：参数全在路径与请求体里",
	},
	// —— 读文件（M4 收尾）。买家侧，没有 /admin/ 前缀。
	{
		ContractPath:   "/uploads/{upload_id}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		// 读 w（缩略图宽度，2026-09-28），所以单独占 upload_redirect.go。第二跳（/blob，不在契约里）
		// 读 exp / sig / w，单独占 upload_blob.go —— 对账按 HandlerFile 解析整份源码里的 c.Query。
		HandlerFile: "upload_redirect.go",
	},
	// —— 买家上传与后台读文件（售后链路补齐那一轮）。契约 GET /uploads/{upload_id} 那张
	// 可读者表的「仅上传者本人与后台客服」原先挂在上面那条的 NotYetImplementedStage 里，
	// 本轮两半都实现了：本人带令牌走 GET /uploads/{id}（auth.OptionalBearer），
	// 后台客服走 GET /admin/uploads/{id}。那笔挂账随之删掉。
	{
		ContractPath:   "/uploads",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "upload.go",
		NoQueryParams:  "purpose 与 file 都在 multipart 请求体里，幂等键在请求头里",
	},
	{
		ContractPath:   "/admin/uploads/{upload_id}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "upload.go",
		NoQueryParams:  "要读哪个文件在路径上；判权按引用它的退款单，没有任何参数可以绕过",
	},
	{
		ContractPath:   "/auth/refresh",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "auth.go",
		NoQueryParams:  "refresh_token 在请求体里",
	},
	{
		ContractPath:   "/auth/logout",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "auth.go",
		NoQueryParams:  "没有参数：吊销哪个会话由 access_token 里的 sid 决定",
	},

	// -----------------------------------------------------------------------
	// 门店：买家侧两条（00020）
	// -----------------------------------------------------------------------
	//
	// 两条在契约里都是 security: []（公开的），而且**分在两个文件里** ——
	// 参数对账按文件做：这条读 {page, page_size}，resolve 读 {lat, lng, size}，
	// 放一起的话两条会互相把对方的参数报成「契约里没有的参数」。
	{
		ContractPath:   "/stores",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "store.go",
	},
	{
		ContractPath:   "/stores/resolve",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "store_resolve.go",
	},

	// -----------------------------------------------------------------------
	// 门店 / 大区后台那 21 条（00020）
	// -----------------------------------------------------------------------
	//
	// 五条带 query 参数的各自在自己的文件里（集合两两不同，除了两条
	// products 列表），其余 16 条全登记 NoQueryParams —— 它们在
	// admin_store.go 里，而那个文件一个 c.Query 都没有。
	// 优惠券（数据模型 §7）。买家侧四条、后台六条。两条带 query 参数的列表各自
	// 一个文件（coupon_mine.go / coupon_center.go / admin_coupon_list.go），
	// 理由同 admin_region_list.go：这张表按文件核对 query 参数。
	{
		ContractPath:   "/coupons",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "coupon_mine.go",
	},
	{
		ContractPath:   "/coupons/applicable",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "coupon.go",
		NoQueryParams:  "拟购商品与门店全在请求体里（CouponApplicableRequest）",
	},
	{
		ContractPath:   "/coupon-templates",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "coupon_center.go",
	},
	{
		ContractPath:   "/coupon-templates/{template_id}/claim",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "coupon.go",
		NoQueryParams:  "领哪一批在路径上，幂等键在 Idempotency-Key 请求头",
	},
	{
		ContractPath:   "/admin/coupon-templates",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_coupon_list.go",
	},
	{
		ContractPath:   "/admin/coupon-templates",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_coupon.go",
		NoQueryParams:  "建模板的参数全在请求体里；幂等键在 Idempotency-Key 请求头",
	},
	{
		ContractPath:   "/admin/coupon-templates/{template_id}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_coupon.go",
		NoQueryParams:  "详情只吃路径参数",
	},
	{
		ContractPath:   "/admin/coupon-templates/{template_id}",
		ContractMethod: "patch",
		HTTPMethod:     http.MethodPatch,
		HandlerFile:    "admin_coupon.go",
		NoQueryParams:  "改哪一个在路径上，改什么在请求体里",
	},
	{
		ContractPath:   "/admin/coupon-templates/{template_id}/scopes",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_coupon.go",
		NoQueryParams:  "整组范围在请求体里",
	},
	{
		ContractPath:   "/admin/coupon-templates/{template_id}/grants",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_coupon.go",
		NoQueryParams:  "手机号在请求体里，幂等键在 Idempotency-Key 请求头",
	},
	// 营销活动（00058）。后台四条；带 query 参数的列表单独一个文件，理由同券模板列表。
	{
		ContractPath:   "/admin/promotions",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_promotion_list.go",
	},
	{
		ContractPath:   "/admin/promotions",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_promotion.go",
		NoQueryParams:  "建活动的参数全在请求体里；幂等键在 Idempotency-Key 请求头",
	},
	{
		ContractPath:   "/admin/promotions/{promotion_id}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_promotion.go",
		NoQueryParams:  "详情只吃路径参数",
	},
	{
		ContractPath:   "/admin/promotions/{promotion_id}",
		ContractMethod: "patch",
		HTTPMethod:     http.MethodPatch,
		HandlerFile:    "admin_promotion.go",
		NoQueryParams:  "改哪一个在路径上，改什么在请求体里",
	},
	// 运费模板（00055）。列表读 query，单独一个文件（同 admin_coupon_list.go）。
	{
		ContractPath:   "/admin/freight-templates",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_freight_list.go",
	},
	{
		ContractPath:   "/admin/freight-templates",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_freight.go",
		NoQueryParams:  "模板全在请求体里；幂等键在 Idempotency-Key 请求头",
	},
	{
		ContractPath:   "/admin/freight-templates/{template_id}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_freight.go",
		NoQueryParams:  "详情只吃路径参数",
	},
	{
		ContractPath:   "/admin/freight-templates/{template_id}",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_freight.go",
		NoQueryParams:  "改哪一个在路径上，整个模板在请求体里",
	},
	{
		ContractPath:   "/admin/freight-templates/{template_id}",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "admin_freight.go",
		NoQueryParams:  "删哪一个在路径上",
	},
	{
		ContractPath:   "/admin/regions",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_region_list.go",
	},
	{
		ContractPath:   "/admin/regions",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_store.go",
		NoQueryParams:  "建大区的参数全在请求体里；幂等键在 Idempotency-Key 请求头",
	},
	{
		ContractPath:   "/admin/regions/{region_id}",
		ContractMethod: "patch",
		HTTPMethod:     http.MethodPatch,
		HandlerFile:    "admin_store.go",
		NoQueryParams:  "改哪一个在路径上，改什么在请求体里",
	},
	{
		ContractPath:   "/admin/regions/{region_id}",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "admin_store.go",
		NoQueryParams:  "软删只吃路径参数；「名下还有门店」那条 409 没有任何可以绕过它的参数",
	},
	{
		ContractPath:   "/admin/regions/{region_id}/products",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_scoped_products.go",
	},
	{
		ContractPath:   "/admin/regions/{region_id}/products/{product_id}/listing",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_store.go",
		NoQueryParams:  "两个 id 都在路径上，listed 在请求体里",
	},
	{
		ContractPath:   "/admin/regions/{region_id}/skus/{sku_id}/price",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_store.go",
		NoQueryParams:  "两个 id 在路径上，price_cents 在请求体里",
	},
	{
		ContractPath:   "/admin/regions/{region_id}/skus/{sku_id}/price",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "admin_store.go",
		NoQueryParams:  "撤销覆盖只吃路径参数",
	},
	{
		ContractPath:   "/admin/stores",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_store_list.go",
	},
	{
		ContractPath:   "/admin/stores",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_store.go",
		NoQueryParams:  "建店的参数全在请求体里；围栏不在这里传（先建店后画围栏，各有各的端点）",
	},
	{
		ContractPath:   "/admin/stores/{store_id}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_store.go",
		NoQueryParams:  "详情只吃路径参数 store_id；围栏随详情一起回，不用参数开关",
	},
	{
		ContractPath:   "/admin/stores/{store_id}",
		ContractMethod: "patch",
		HTTPMethod:     http.MethodPatch,
		HandlerFile:    "admin_store.go",
		NoQueryParams:  "改哪一家在路径上，改什么在请求体里",
	},
	{
		ContractPath:   "/admin/stores/{store_id}",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "admin_store.go",
		NoQueryParams:  "软删只吃路径参数",
	},
	{
		ContractPath:   "/admin/stores/{store_id}/fence",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_store.go",
		NoQueryParams:  "围栏是一整块 GeoJSON，在请求体里；传 null 即清空",
	},
	{
		ContractPath:   "/admin/stores/{store_id}/local-delivery",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_local_delivery.go",
		NoQueryParams:  "读哪一家在路径上",
	},
	{
		ContractPath:   "/admin/stores/{store_id}/local-delivery",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_local_delivery.go",
		NoQueryParams:  "整份配置在请求体里",
	},
	{
		ContractPath:   "/admin/stores/{store_id}/local-delivery",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "admin_local_delivery.go",
		NoQueryParams:  "改回跟随默认模板，只吃路径参数",
	},
	{
		ContractPath:   "/admin/local-delivery-templates",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_local_delivery.go",
		NoQueryParams:  "模板不分页，一家店至多几十个",
	},
	{
		ContractPath:   "/admin/local-delivery-templates",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_local_delivery.go",
		NoQueryParams:  "整个模板在请求体里",
	},
	{
		ContractPath:   "/admin/local-delivery-templates/{template_id}",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_local_delivery.go",
		NoQueryParams:  "整个模板在请求体里",
	},
	{
		ContractPath:   "/admin/local-delivery-templates/{template_id}",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "admin_local_delivery.go",
		NoQueryParams:  "删哪一个在路径上",
	},
	{
		ContractPath:   "/admin/agents/{staff_id}/auto-policies",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_agent_policy.go",
		NoQueryParams:  "四种一次列全，没有筛选",
	},
	{
		ContractPath:   "/admin/agents/{staff_id}/auto-policies/{kind}",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_agent_policy.go",
		NoQueryParams:  "策略在请求体里",
	},
	{
		ContractPath:   "/ai-log",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "public_ai_log.go",
		NoQueryParams:  "固定最近 10 份简报、30 条提案",
	},
	{
		ContractPath:   "/admin/ai-log/settings",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "public_ai_log.go",
		NoQueryParams:  "一个开关",
	},
	{
		ContractPath:   "/admin/ai-log/settings",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "public_ai_log.go",
		NoQueryParams:  "开关在请求体里",
	},
	{
		ContractPath:   "/admin/agents/{staff_id}/scorecard",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_agent_scorecard.go",
		NoQueryParams:  "固定近 30 天，没有筛选",
	},
	{
		ContractPath:   "/admin/stores/{store_id}/default",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_store.go",
		NoQueryParams:  "没有参数：设哪一家在路径上，「先清旧再置新」是服务端的事，不给开关",
	},
	{
		ContractPath:   "/admin/stores/{store_id}/products",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_scoped_products.go",
	},
	{
		ContractPath:   "/admin/stores/{store_id}/products/{product_id}/listing",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_store.go",
		NoQueryParams:  "两个 id 都在路径上，listed 在请求体里",
	},
	{
		ContractPath:   "/admin/stores/{store_id}/skus/{sku_id}/price",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_store.go",
		NoQueryParams:  "两个 id 在路径上，price_cents 在请求体里",
	},
	{
		ContractPath:   "/admin/stores/{store_id}/skus/{sku_id}/price",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "admin_store.go",
		NoQueryParams:  "撤销覆盖只吃路径参数",
	},
	{
		ContractPath:   "/admin/stores/{store_id}/inventories",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_store_inventories.go",
	},
	{
		ContractPath:   "/admin/stores/{store_id}/skus/{sku_id}/inventory",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "admin_store.go",
		NoQueryParams:  "两个 id 在路径上，三个数量在请求体里；CAS 的 expected 刻意不是 query —— 它是请求体的一部分，不是一个开关",
	},
	{
		ContractPath:   "/admin/stores/{store_id}/skus/{sku_id}/inventory/adjustments",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "admin_store.go",
		NoQueryParams:  "两个 id 在路径上，delta 与 reason 在请求体里",
	},

	// —— 买家自己的三组：个人信息 / 地址簿 / 购物车（契约 User 与 Cart tag）。
	// 「每一条都要令牌」由 buyer_self_test.go 的 TestBuyerSelfRoutesRequireToken 逐条证明。
	{
		ContractPath:   "/me",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "me.go",
		NoQueryParams:  "「我」由 Authorization 头里的令牌决定，没有任何参数",
	},
	{
		ContractPath:   "/me",
		ContractMethod: "patch",
		HTTPMethod:     http.MethodPatch,
		HandlerFile:    "me.go",
		NoQueryParams:  "要改的字段在请求体里",
	},
	{
		ContractPath:   "/me/identities",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "me.go",
		NoQueryParams:  "列出全部绑定，不分页（一个人至多五种 provider）",
	},
	{
		ContractPath:   "/me/identities/wechat",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "me.go",
		NoQueryParams:  "code 在请求体里，幂等键在请求头里",
		NotYetImplementedBody: map[string]string{
			"code": "绑定微信要拿 code 去微信换 openid / unionid，本项目没有接微信开放平台，" +
				"这条路返回 501（契约的 default: Problem 收得住），而不是假装绑定成功或回一个 4xx。" +
				"形状与 /auth/login 的 code 一样，两个方向都锁：buyer_self_test.go 的 " +
				"TestIdentityAndPhoneBindingSayNotImplemented 断言那条路真的返回 501，" +
				"并且断言这里真的挂着这一笔。",
		},
	},
	{
		ContractPath:   "/me/identities/{provider}",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "me.go",
		NoQueryParams:  "provider 在路径上",
	},
	{
		ContractPath:   "/me/phone",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "me.go",
		NoQueryParams:  "新号码与验证码在请求体里，幂等键在请求头里",
		NotYetImplementedBody: map[string]string{
			"code": "绑定 / 换绑手机号要校验新号码收到的短信验证码，本项目没有短信服务" +
				"（与 /auth/login 的 code 是同一个缺口），这条路返回 501。" +
				"反向由 buyer_self_test.go 的 TestIdentityAndPhoneBindingSayNotImplemented 盯着。",
		},
	},
	{
		ContractPath:   "/addresses",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		// 地址簿一次返回全部、不分页；唯一的 query 参数 store_id（标 in_service_area）
		// 实现了，既不写 NoQueryParams 也不挂账。放在单独的文件里，理由见 address.go 文件头。
		HandlerFile: "address_list.go",
	},
	{
		ContractPath:   "/addresses",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "address.go",
		NoQueryParams:  "地址字段在请求体里，幂等键在请求头里",
	},
	{
		ContractPath:   "/addresses/{address_id}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "address.go",
		NoQueryParams:  "只吃路径参数 address_id",
	},
	{
		ContractPath:   "/addresses/{address_id}",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "address.go",
		NoQueryParams:  "id 在路径上，整条地址在请求体里",
	},
	{
		ContractPath:   "/addresses/{address_id}",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "address.go",
		NoQueryParams:  "只吃路径参数 address_id",
	},
	{
		ContractPath:   "/addresses/{address_id}/default",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "address.go",
		NoQueryParams:  "只吃路径参数 address_id",
	},
	{
		// 返回 Cart 的 5 条都读 store_id（契约 CartStoreId），所以在同一个文件；
		// 两条回 204 的删除不读任何参数，在 cart_delete.go。
		ContractPath:   "/cart",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "cart.go",
	},
	{
		ContractPath:   "/cart",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "cart_delete.go",
		NoQueryParams:  "清空整辆车，回 204，不涉及门店",
	},
	{
		ContractPath:   "/cart/items",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "cart.go",
	},
	{
		ContractPath:   "/cart/selection",
		ContractMethod: "put",
		HTTPMethod:     http.MethodPut,
		HandlerFile:    "cart.go",
	},
	{
		ContractPath:   "/cart/items/batch-delete",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "cart.go",
	},
	{
		ContractPath:   "/cart/items/{item_id}",
		ContractMethod: "patch",
		HTTPMethod:     http.MethodPatch,
		HandlerFile:    "cart.go",
	},
	{
		ContractPath:   "/cart/items/{item_id}",
		ContractMethod: "delete",
		HTTPMethod:     http.MethodDelete,
		HandlerFile:    "cart_delete.go",
		NoQueryParams:  "删一行，回 204，不涉及门店",
	},
}

// routeOf 按方法与契约路径取出登记行。取不到就 Fatal —— 调用方（别的测试文件）
// 拿它来交叉引用这张表，而一个静默的零值会让那种引用变成空转。
func routeOf(t *testing.T, method, contractPath string) route {
	t.Helper()
	for _, r := range routes {
		if r.HTTPMethod == method && r.ContractPath == contractPath {
			return r
		}
	}
	t.Fatalf("routes 表里没有 %s %s", method, contractPath)
	return route{}
}

// nonContractRoutes 是注册了但刻意不在契约里的路由，每条写明理由。
//
// 往这里加一行是一个需要解释的动作：它等于说「这个 URL 对外存在，但契约里
// 找不到它」，而契约是前后端唯一的约定。
var nonContractRoutes = map[string]string{
	"POST /api/v1/mcp": "AI 员工的 MCP 入口（AI 经营 M9，handler/mcp.go）：形状由 MCP 规范（streamable HTTP + JSON-RPC）定义，" +
		"不是 REST 资源；工具清单与参数由 MCP 的 tools/list 自描述。鉴权是 kagt_ 接入密钥（契约里 /agent/whoami 描述了它）",
	"GET /api/v1/mcp": "AI 员工的 MCP 入口（AI 经营 M9，handler/mcp.go）：形状由 MCP 规范（streamable HTTP + JSON-RPC）定义，" +
		"不是 REST 资源；工具清单与参数由 MCP 的 tools/list 自描述。鉴权是 kagt_ 接入密钥（契约里 /agent/whoami 描述了它）",
	"DELETE /api/v1/mcp": "AI 员工的 MCP 入口（AI 经营 M9，handler/mcp.go）：形状由 MCP 规范（streamable HTTP + JSON-RPC）定义，" +
		"不是 REST 资源；工具清单与参数由 MCP 的 tools/list 自描述。鉴权是 kagt_ 接入密钥（契约里 /agent/whoami 描述了它）",
	"GET /healthz": "存活探针，给编排系统和 compose 用；契约描述的是业务接口",
	"GET /readyz": "就绪探针（能 ping 通业务库才是 200），与 healthz 同类：给编排系统与负载均衡用，" +
		"不是业务接口。与 healthz 分开是因为库暂时连不上时进程是活的、不该被重启，只是不该接流量（app/lifecycle.go）",
	"GET /version": "构建信息（版本 / commit / 构建时间 / Go 版本），给运维与 issue 里" +
		"「你跑的是哪一版」用；和 healthz 同类，不是业务接口。" +
		"刻意不进契约：契约是前后端的约定，而没有任何客户端该按版本号分支行为 —— " +
		"真要那样做，那是一次显式的能力协商设计，不是读一个字符串。",

	"GET /api/v1/uploads/:upload_id/blob": "GET /uploads/{upload_id} 跳过去的那个限时地址本身。" +
		"契约在那条接口上写的是「302，跳转到 driver 生成的**限时**地址：本地磁盘 driver " +
		"跳到带签名与过期时间的站内地址，S3 driver 跳到预签名 URL」—— " +
		"也就是说这个地址的形状**随 driver 变**，把它写进契约等于把本地磁盘这一种形态钉死，" +
		"而换 S3 那天契约就成了假话。契约里那条（/uploads/{upload_id}）永远是要过归属校验的" +
		"那一跳，它才是客户端该拿在手里的形状（Upload.url 的描述也是这么写的：" +
		"「客户端不应解析它，原样回传即可」）。\n" +
		"它实现在 upload_blob.go 而不是和第一跳同一个文件：routes 表那条 query 参数对账" +
		"按 HandlerFile 解析整份源码，而这一跳要读 exp 与 sig 两个 query 参数 —— " +
		"同文件会让第一跳那行 NoQueryParams 登记当场红。两跳各自挡什么，" +
		"写在 service/upload.go 的文件头。",
}

// pendingOp 是契约里声明了、这个包**还没有注册任何路由**的一个操作。
//
// 上面那张 routes 表锁的是「注册了的路由都被覆盖到」，它对「契约里有一整段接口
// 而服务端一行都没有」是全绿的 —— 那正是 M4 任务 1 之前的真实状态：
// 契约里 9 条 /admin/ 路径，handler 里零条，没有任何东西会响。
//
// 所以缺口这一侧也要登记。三个方向都锁得住：
//   - 契约里新增一条 /admin/ 操作而这里没挂账 → 红。逼人当场决定「实现它」
//     还是「先记在这里」，默认行为是红，不是静默忽略。
//   - 这里挂的操作契约里根本没有（改名、删了） → 红。清单不能烂掉。
//   - 实现了、路由注册上了，却忘了从这里划掉 → 红。「实现完删掉一行」
//     于是成了天然的验收动作。
//
// 范围**只到 /admin/ 前缀**，这是刻意的。买家侧那几十条未实现的接口不在这里，
// 因为它们的缺口是「这个里程碑还没做到」，人人都知道；而后台侧的缺口是
// 「设计阶段漏了一整块」——同一份契约里，商家能处理订单却没法上架商品，
// 这种缺口只有机械检查发现得了。范围写小一点、锁得住一点，好过写大一点、
// 挂一百行没人读的账。
type pendingOp struct {
	ContractPath   string // 契约里的路径
	ContractMethod string // 契约里的方法，小写
	Why            string // 为什么还没实现。空字符串会红。
}

// notYetRouted 是全部 /admin/ 操作里还没有落地的那些。
//
// M4 任务 1 落地时这里有 26 条 —— 契约里每一条 /admin/ 操作都在。
// 任务 2（后台身份）划掉了 7 条：三条 /admin/auth/*、/admin/me，
// 以及 /admin/staff 的三个操作。它们是别的 19 条的前置，
// 因为 26 条里没有一条不需要后台身份。
//
// M4 任务 3（商家自助发布）划掉了 16 条。随后契约又补进来 21 条门店 / 大区
// 的操作（多门店那一轮契约先行），这张表一度回到 24 条。
//
// 之后两条线各划掉了一批，**它们是并行做的，所以这段话是合并时才写全的**：
//
//	· M4 收尾划掉 POST /admin/merchants（开店）—— 它缺的那样东西，
//	  repository 上「在指定租户里开一个事务」的入口，那一轮建出来了，
//	  叫 repository.WithNewTenant；
//	· 多门店落地（00020）把那 21 条一条不剩地划掉。
//
// 于是剩下 2 条：发货与退款审核。订单后半程那一轮把它们一起划掉了
// （00033 发货、00034 退款域）—— **这张表今天是空的**。
//
// 空表不是删掉这套机制的理由：它是「还没实现」的锁，下一次契约先行加进来一条
// /admin/ 操作，这里就会重新长出一行，而不是让缺口静默地躺在契约里。
// （后台订单 / 退款单的列表与详情那四条是契约与实现同一轮落地的，没在这里挂过账。）
// （消息通知那一轮契约先行时这里挂过四条后台提醒接口，实现落地的同一轮划掉了。）
var notYetRouted = []pendingOp{}

// contractHTTPMethods 是 OpenAPI path item 里哪些键算一个操作。
// 其余的键（parameters、summary、servers…）不是操作，跳过。
var contractHTTPMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"patch": true, "head": true, "options": true, "trace": true,
}

// 契约里每一条 /admin/ 操作，要么已经注册了路由（在 routes 表里），
// 要么在 notYetRouted 里挂着一笔写明理由的欠账。两个方向都锁。
//
// 这条测试是 M4 任务 1 加的，起因很具体：契约里有 9 条 /admin/ 路径，
// handler 里一条都没有，而当时仓库全绿。「设计阶段漏了一整块」这种缺口
// 没有任何机械检查看得见 —— 只有人在查别的东西时偶然撞上。
