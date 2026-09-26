package handler_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
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

	// NotYetImplementedResponse 是**响应体**里契约声明了、这条 handler 刻意不
	// 填的字段，与前两笔账是同一件事的第三种形状。
	//
	// 眼下只有一条：下单两条接口的 freight_cents。运费模板在数据模型里没有落地，
	// 所以这条链路**没有算过运费** —— 而「算出来是 0」与「没算」对客户端是两件
	// 不同的事（前者意味着包邮，后者意味着这个数还会变）。契约里它是可选字段，
	// 所以「没算」的诚实形状是整个不出现，不是 0。
	//
	// 两个方向都锁得住：
	//   - 契约把这个字段改名或删掉 → 下面那条对账测试红（清单在描述一个不存在
	//     的东西）；
	//   - 真的实现了运费、字段开始出现在响应里 → order_test.go 的
	//     TestFreightIsAbsentNotZero 红，逼人回来删掉这一行。
	NotYetImplementedResponse map[string]string

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
			"in_stock_only": "只看当前门店有货的。库存本轮按门店分完了，" +
				"所以这一条不再缺表 —— **缺的是过滤本身**：那条列表查询今天不 JOIN " +
				"inventories。默认值与 SearchFilters 对齐成 false（M3 独立验收 I10），" +
				"两边仍然都没有执行者。挂在这里而不是顺手实现，是因为它会改变" +
				"「一页有几行」，而分页与筛选一起改要重新过一遍空页与总数那几条断言。",
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
		NotYetImplementedResponse: map[string]string{
			"freight_cents": "运费模板没有设计落地，这条链路没有算过运费。响应里整个不出现，" +
				"而不是填 0：0 意味着包邮，缺席意味着这个数还会变。",
		},
	},
	{
		ContractPath:   "/orders",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "order.go",
		NoQueryParams:  "下单的参数在请求体与 Idempotency-Key 请求头里，没有 query 参数",
		NotYetImplementedResponse: map[string]string{
			"freight_cents": "同 /orders/preview。库里 orders.freight_cents 是 0（chk_amount " +
				"的恒等式要它），但那是账，不是「算过了」。",
		},
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
		NotYetImplementedResponse: map[string]string{
			"image_url": "商品主图。**缺的东西本轮（M4 任务 1）变了，理由要跟着改**：" +
				"原先写的是「products 表上没有图片列，uploads 与商品没有任何关联」，" +
				"那个缺口已经在数据模型 §3 补上了（product_images 关联表，sort_order 最小的即主图）。" +
				"现在缺的是**迁移与 handler**，属于 M4 的下一个任务。" +
				"在那之前仍然缺席而不是回空串：空串会让客户端渲染一个「加载失败」的占位图。",
			"images": "商品图集，同 image_url —— 表设计好了，迁移与 handler 还没写。" +
				"回空数组会让轮播图组件显示「无图」，而那与「这件商品确实没有配图」是两件事。",
		},
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
		ContractPath:   "/admin/refunds/{refund_no}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "admin_order_detail.go",
		NoQueryParams:  "详情只吃路径参数 refund_no",
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
			"活动失效": "业务重排的 w_promo（语义检索层 §6）。数据模型里没有活动表：" +
				"券（00026）挂在买家身上、不挂在商品上，「这件商品有没有进行中 / 已结束的活动」" +
				"无从回答。业务乘子目前只含 w_stock，explain 的 scores.business 就是它，" +
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
		NoQueryParams:  "改哪家店在路径上，改什么（名字、状态）在请求体里",
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
	// —— 读文件（M4 收尾）。买家侧，没有 /admin/ 前缀。
	{
		ContractPath:   "/uploads/{upload_id}",
		ContractMethod: "get",
		HTTPMethod:     http.MethodGet,
		HandlerFile:    "upload.go",
		NoQueryParams: "要读哪个文件在路径上；契约里这条接口一个 query 参数都没有。" +
			"第二跳（GET /uploads/{upload_id}/blob，不在契约里）确实要读 exp 与 sig，" +
			"而它因此单独占了 upload_blob.go —— 下面那条对账按 HandlerFile 解析" +
			"**整份源码**里的 c.Query，两跳同文件会让这一行登记当场红。" +
			"同一条纪律让 GET /admin/products 单独占了 admin_product_list.go",
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
	// 营销活动（00044）。后台四条；带 query 参数的列表单独一个文件，理由同券模板列表。
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
		HandlerFile:    "address.go",
		NoQueryParams:  "地址簿一次返回全部，不分页；契约里这条接口没有 query 参数",
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
	"GET /healthz": "存活探针，给编排系统和 compose 用；契约描述的是业务接口",
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
func TestAdminContractOperationsAreRoutedOrListed(t *testing.T) {
	doc := loadContract(t)

	registered := map[string]bool{}
	for _, ri := range testEngine.Routes() {
		registered[ri.Method+" "+ri.Path] = true
	}
	if len(registered) == 0 {
		t.Fatal("gin 路由表是空的 —— 这条测试没在检查任何东西")
	}

	implemented := map[string]bool{}
	for _, r := range routes {
		implemented[strings.ToUpper(r.ContractMethod)+" "+r.ContractPath] = true
	}

	listed := map[string]bool{}
	for _, op := range notYetRouted {
		key := strings.ToUpper(op.ContractMethod) + " " + op.ContractPath
		if listed[key] {
			t.Errorf("notYetRouted 里 %s 挂了两次", key)
		}
		listed[key] = true
		if strings.TrimSpace(op.Why) == "" {
			t.Errorf("notYetRouted 里 %s 没写理由 —— 一笔不写理由的欠账，"+
				"下一个人只会把它当成一行豁免", key)
		}
	}

	// 方向一：契约 → 清单。漏登记就红。
	seen, done := 0, 0
	for path, item := range doc.Paths {
		if !strings.HasPrefix(path, "/admin/") {
			continue
		}
		for method := range item {
			if !contractHTTPMethods[method] {
				continue
			}
			seen++
			key := strings.ToUpper(method) + " " + path
			if implemented[key] {
				done++
				continue
			}
			if listed[key] {
				continue
			}
			t.Errorf("契约里有 %s，但它既没有在 routes 表里（没实现），"+
				"也没有在 notYetRouted 里挂账 —— 后台接口的缺口会这样静默地长出来，"+
				"请二选一", key)
		}
	}
	// 阳性对照：一条 /admin/ 操作都没解析出来，说明契约的形状变了或路径改了名，
	// 上面那个循环一次也不执行，整条测试恒绿。
	if seen == 0 {
		t.Fatal("从契约里一条 /admin/ 操作都没解析出来 —— 这条测试没在检查任何东西")
	}

	for _, op := range notYetRouted {
		key := strings.ToUpper(op.ContractMethod) + " " + op.ContractPath

		// 方向二：清单 → 契约。清单不能描述一个不存在的东西。
		item, ok := doc.Paths[op.ContractPath]
		if !ok {
			t.Errorf("notYetRouted 里挂着 %s（%s），但契约里已经没有这个路径了 —— "+
				"清单烂了，请删掉这一行", key, op.Why)
			continue
		}
		if _, ok := item[op.ContractMethod]; !ok {
			t.Errorf("notYetRouted 里挂着 %s（%s），但契约里那个路径没有这个方法了 —— "+
				"清单烂了，请删掉这一行", key, op.Why)
			continue
		}

		// 方向三：实现了就必须从这里划掉。
		if registered[strings.ToUpper(op.ContractMethod)+" "+ginPathOf(op.ContractPath)] {
			t.Errorf("%s 已经注册了路由，但它还挂在 notYetRouted 里（%s）—— "+
				"实现完请删掉这一行，并在 routes 表里加一行", key, op.Why)
		}
	}

	// 「已实现」是数出来的，不是 seen - len(listed) 减出来的：清单出问题时
	// 减法会打印出负数或虚高的「已实现」，而这行日志正是给排查的人看的。
	t.Logf("契约里 %d 条 /admin/ 操作：已实现 %d，挂账 %d", seen, done, len(listed))
}

// 注册的路由与 routes 表必须一一对应。
//
// 这条是表驱动的意义所在：没有它，第二条路由可以悄悄挂上去，而参数清账、
// 契约对齐这些检查一条也不会覆盖到它 —— 测试全绿，覆盖面却在缩水。
func TestEveryRegisteredRouteIsAccountedFor(t *testing.T) {
	registered := map[string]bool{}
	for _, ri := range testEngine.Routes() {
		registered[ri.Method+" "+ri.Path] = true
	}
	if len(registered) == 0 {
		t.Fatal("gin 路由表是空的 —— 这条测试没在检查任何东西")
	}

	inTable := map[string]bool{}
	for _, r := range routes {
		inTable[r.key()] = true
	}

	for key := range registered {
		if inTable[key] {
			continue
		}
		if why, ok := nonContractRoutes[key]; ok {
			t.Logf("%s 刻意不在契约里：%s", key, why)
			continue
		}
		t.Errorf("路由 %q 注册了，但 contract_test.go 的 routes 表里没有它 —— "+
			"于是参数清账、契约对齐这些检查一条也覆盖不到它。"+
			"请在表里加一行，或者（如果它刻意不属于契约）加进 nonContractRoutes 并写明理由", key)
	}

	for key := range inTable {
		if !registered[key] {
			t.Errorf("routes 表里登记了 %q，但它没有被注册 —— "+
				"路径改了、或者这条接口被删了，表烂了", key)
		}
	}

	for key := range nonContractRoutes {
		if !registered[key] {
			t.Errorf("nonContractRoutes 里挂着 %q，但它没有被注册 —— 请删掉这一行", key)
		}
	}
}

// 契约声明的 query 参数，要么被 handler 读了，要么在 NotYetImplemented 里挂着账；
// 反过来，handler 读的每一个 query 参数也必须是契约声明过的。
//
// 这条测试读的是契约本身（YAML）与 handler 的源码（AST），两边都不是人手写的
// 清单 —— 手写清单会和它描述的东西各走各的，而那种漂移不会有任何症状。
func TestContractQueryParamsAreHandledOrListed(t *testing.T) {
	for _, r := range routes {
		t.Run(r.HTTPMethod+" "+r.ContractPath, func(t *testing.T) {
			declared := contractQueryParams(t, r)
			handled := queryParamsReadByHandler(t, r)

			if r.NoQueryParams != "" {
				// 登记过「这条接口没有 query 参数」。两个方向都要核对，
				// 否则这行登记就成了一条永不失效的豁免。
				if len(declared) > 0 {
					t.Fatalf("表里写着这条接口没有 query 参数（%s），"+
						"但契约里声明了 %v —— 登记过期了，要么实现它们，"+
						"要么改成 NotYetImplemented 挂账", r.NoQueryParams, sorted(declared))
				}
				if len(handled) > 0 {
					t.Fatalf("表里写着这条接口没有 query 参数（%s），"+
						"但 %s 在读 %v —— handler 读了一个契约里没有的参数",
						r.NoQueryParams, r.HandlerFile, sortedBool(handled))
				}
				t.Logf("契约与 handler 两侧都没有 query 参数：%s", r.NoQueryParams)
				return
			}

			for name := range declared {
				if handled[name] {
					continue
				}
				if _, listed := r.NotYetImplemented[name]; listed {
					continue
				}
				t.Errorf("契约里有 query 参数 %q，%s 没读它，NotYetImplemented 里也没挂账 —— "+
					"它会被静默忽略，客户端以为筛过了，其实没有", name, r.HandlerFile)
			}

			// 反向：handler 读了一个契约里没有的参数。
			//
			// 这个方向同样是接口与契约的分叉，而且更隐蔽：功能「能用」，
			// 只是这份能力从来没被写进约定 —— 前端不知道它存在，下一次
			// 按契约生成客户端时它也不会出现。少了这个循环，上面那些断言
			// 对这类漂移是全绿的。
			for name := range handled {
				if _, ok := declared[name]; !ok {
					t.Errorf("%s 读了 query 参数 %q，但契约里 %s %s 没有声明它 —— "+
						"先改契约，再实现（CONTRIBUTING 硬规矩二）",
						r.HandlerFile, name, r.ContractMethod, r.ContractPath)
				}
			}

			for name, why := range r.NotYetImplemented {
				if _, ok := declared[name]; !ok {
					t.Errorf("NotYetImplemented 里挂着 %q（%s），但契约里已经没有这个参数了 —— "+
						"清单烂了，请删掉这一行", name, why)
				}
				if handled[name] {
					t.Errorf("%s 已经在读 %q 了，但它还挂在 NotYetImplemented 里（%s）—— "+
						"实现完请删掉这一行", r.HandlerFile, name, why)
				}
			}

			// 阳性对照：两边都不能是空集。任何一边解析失败（路径改名、
			// handler 改文件名、YAML 结构变了）都会让上面所有循环一次也不执行，
			// 于是整条测试恒绿。
			if len(declared) == 0 {
				t.Fatalf("从契约里一个 query 参数都没解析出来 —— %s %s 还在吗？",
					r.ContractMethod, r.ContractPath)
			}
			// 这一条本轮放宽了一次，理由要写清楚，因为放宽一个阳性对照是个
			// 该被质疑的动作。
			//
			// 原先它是无条件的：handler 一个 c.Query 都没读就 Fatal。
			// 那默认了「契约声明了 query 参数 ⇒ handler 至少实现了一个」，
			// 而多门店这一轮出现了第三种状态：**契约声明了，但全部挂在
			// NotYetImplemented 上**（GET /products/{product_id} 只有一个
			// store_id，而门店表还没建）。此时 handled 合法地为空集。
			//
			// 放宽之后还剩什么在守：handler 文件改名或改坏时，
			// queryParamsReadByHandler 里的 parser.ParseFile 会先 t.Fatal ——
			// 「解析失败让整条测试恒绿」那个真正要防的失败模式由它挡住，
			// 不靠这一条。所以这里只在「本该有人实现」时才要求非空。
			if len(handled) == 0 && len(declared) > len(r.NotYetImplemented) {
				t.Fatalf("从 %s 里一个 c.Query 调用都没解析出来，而契约声明了 %d 个参数、"+
					"只有 %d 个挂了账 —— 这条测试没在检查任何东西",
					r.HandlerFile, len(declared), len(r.NotYetImplemented))
			}
			t.Logf("契约声明 %v；handler 读取 %v", sorted(declared), sortedBool(handled))
		})
	}
}

// contractQueryParams 从 OpenAPI 契约里取出该接口的全部 query 参数名，
// 顺带解析 $ref（契约里 Page / PageSize 是 $ref 到 components.parameters 的）。
func contractQueryParams(t *testing.T, r route) map[string]string {
	t.Helper()

	doc := loadContract(t)

	item, ok := doc.Paths[r.ContractPath]
	if !ok {
		t.Fatalf("契约里没有路径 %s", r.ContractPath)
	}
	if _, ok := item[r.ContractMethod]; !ok {
		t.Fatalf("契约里 %s 没有 %s 方法", r.ContractPath, r.ContractMethod)
	}

	// path item 级的 parameters 对该路径下每个方法都生效，所以两处都要收。
	var params []any
	if v, ok := item["parameters"].([]any); ok {
		params = append(params, v...)
	}
	op, ok := item[r.ContractMethod].(map[string]any)
	if !ok {
		t.Fatalf("契约里 %s %s 不是一个映射", r.ContractMethod, r.ContractPath)
	}
	if v, ok := op["parameters"].([]any); ok {
		params = append(params, v...)
	}

	out := map[string]string{}
	for _, raw := range params {
		p, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("参数项不是映射: %#v", raw)
		}
		name, in := "", ""
		if ref, ok := p["$ref"].(string); ok {
			// 只认 #/components/parameters/X 这一种引用形式；别的形式出现时
			// 宁可让测试红，也不要静默漏掉一个参数。
			const prefix = "#/components/parameters/"
			if !strings.HasPrefix(ref, prefix) {
				t.Fatalf("不认识的参数引用 %q", ref)
			}
			c, ok := doc.Components.Parameters[strings.TrimPrefix(ref, prefix)]
			if !ok {
				t.Fatalf("契约里的引用 %q 指向一个不存在的参数", ref)
			}
			name, in = c.Name, c.In
		} else {
			name, _ = p["name"].(string)
			in, _ = p["in"].(string)
		}
		if in == "query" && name != "" {
			out[name] = in
		}
	}
	return out
}

// queryParamsReadByHandler 从 handler 源码里抽出所有 c.Query("x") 之类调用的参数名。
//
// 读 AST 而不是维护一份「handler 读了哪些参数」的手写清单：手写清单和代码之间
// 只有人的注意力在维系，而它们分叉时没有任何症状 —— 正是这条测试要消灭的那类漂移。
func queryParamsReadByHandler(t *testing.T, r route) map[string]bool {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, r.HandlerFile, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	// gin 上读 query 的几个方法。少写一个的后果是「漏认」，会让测试**多报**，
	// 那是安全的失败方向：有人用了没列进来的方法，测试会红并指向这里。
	readers := map[string]bool{
		"Query": true, "DefaultQuery": true, "GetQuery": true,
		"QueryArray": true, "QueryMap": true,
	}

	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !readers[sel.Sel.Name] {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			// 参数名不是字面量（变量、常量、拼出来的）。这条测试没法跟踪它，
			// 而静默跳过等于悄悄少测一块。
			t.Errorf("%s 里 %s 的第一个参数不是字符串字面量，这条测试跟不过去",
				r.HandlerFile, sel.Sel.Name)
			return true
		}
		name, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = true
		return true
	})
	return out
}

func sorted(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedBool(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NotYetImplementedBody 里挂的每一笔账，都必须是契约请求体里真有的字段。
//
// 与 query 那条的差别写在 route.NotYetImplementedBody 的注释里：这里只能做
// 「清单 → 契约」这一个方向的机械对账，反向（实现了却忘了划掉）由行为测试
// auth_test.go 的 TestSMSLoginSaysItIsNotImplemented 盯着。
//
// 一个方向也比没有强：契约把 code 改名或删掉时，这条会红并指出那一行清单
// 已经在描述一个不存在的东西 —— 而挂账一旦烂掉，它就从「欠账」退化成
// 「一段没人读的注释」。
func TestNotYetImplementedBodyFieldsExistInContract(t *testing.T) {
	checked := 0
	for _, r := range routes {
		if len(r.NotYetImplementedBody) == 0 {
			continue
		}
		t.Run(r.HTTPMethod+" "+r.ContractPath, func(t *testing.T) {
			props := contractBodyProps(t, r)
			if len(props) == 0 {
				t.Fatalf("契约里 %s %s 的请求体一个属性都没解析出来 —— "+
					"这条测试没在检查任何东西", r.ContractMethod, r.ContractPath)
			}
			for name, why := range r.NotYetImplementedBody {
				if !props[name] {
					t.Errorf("NotYetImplementedBody 里挂着 %q（%s），"+
						"但契约的请求体里没有这个字段了 —— 清单烂了，请删掉这一行",
						name, why)
				}
				checked++
			}
			t.Logf("契约请求体声明 %v；挂账 %v", sortedBool(props), sorted(r.NotYetImplementedBody))
		})
	}
	if checked == 0 {
		t.Fatal("一笔请求体挂账都没查到 —— 挂账清空了就该把这条测试一起删掉，" +
			"留着一条恒绿的测试比没有更糟")
	}
}

// contractDoc 是契约里这几条测试要用到的那几块。
//
// 用 map[string]any 逐层走，而不是一把 unmarshal 进强类型结构体：OpenAPI 的
// path item 里 parameters（序列）与 get/post（映射）是平级的兄弟键。
type contractDoc struct {
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		Parameters map[string]struct {
			Name string `yaml:"name"`
			In   string `yaml:"in"`
			// Required 只有请求头那条对账用得上，但它必须解在这里：
			// 两份 contractDoc 意味着两份会各自跑偏的对契约结构的假设。
			Required bool `yaml:"required"`
		} `yaml:"parameters"`
		Schemas map[string]map[string]any `yaml:"schemas"`
	} `yaml:"components"`
}

// loadContract 读一次契约。
//
// 三条测试共用它，而不是各自 ReadFile + Unmarshal 一遍：三份解析意味着三份
// 会各自跑偏的对契约结构的假设，而它们跑偏时谁也不会红。
func loadContract(t *testing.T) contractDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "电商系统-OpenAPI.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc contractDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析契约失败: %v", err)
	}
	if len(doc.Paths) == 0 || len(doc.Components.Schemas) == 0 {
		t.Fatalf("契约解析出来是空的（paths=%d schemas=%d）—— 这些测试没在检查任何东西",
			len(doc.Paths), len(doc.Components.Schemas))
	}
	return doc
}

// contractBodyProps 取出该接口 application/json 请求体的顶层属性名。
//
// 只认 `requestBody.content["application/json"].schema.properties` 这一种形状：
// 契约里出现别的写法（$ref 到 components.schemas、multipart…）时宁可 Fatal，
// 也不要静默返回空集 —— 空集会让上面那条测试一次也不执行。
func contractBodyProps(t *testing.T, r route) map[string]bool {
	t.Helper()

	doc := loadContract(t)
	op, ok := doc.Paths[r.ContractPath][r.ContractMethod].(map[string]any)
	if !ok {
		t.Fatalf("契约里没有 %s %s", r.ContractMethod, r.ContractPath)
	}
	body, ok := op["requestBody"].(map[string]any)
	if !ok {
		t.Fatalf("契约里 %s %s 没有 requestBody", r.ContractMethod, r.ContractPath)
	}
	content, ok := body["content"].(map[string]any)
	if !ok {
		t.Fatalf("契约里 %s %s 的 requestBody 没有 content", r.ContractMethod, r.ContractPath)
	}
	media, ok := content["application/json"].(map[string]any)
	if !ok {
		t.Fatalf("契约里 %s %s 的请求体不是 application/json", r.ContractMethod, r.ContractPath)
	}
	schema, ok := media["schema"].(map[string]any)
	if !ok {
		t.Fatalf("契约里 %s %s 的请求体没有内联 schema", r.ContractMethod, r.ContractPath)
	}
	return schemaProps(t, doc.Components.Schemas, schema,
		fmt.Sprintf("%s %s 的请求体", r.ContractMethod, r.ContractPath))
}

// contractResponseProps 取出该接口**成功响应**的 application/json body 的顶层属性名。
//
// 成功码按 201 → 200 的顺序找：契约里建单是 201、试算是 200，而写死其中一个会
// 让另一条路悄悄退化成「一个属性都没解析出来」，那正是下面那条测试的 Fatal 要
// 抓的东西。
func contractResponseProps(t *testing.T, r route) map[string]bool {
	t.Helper()

	doc := loadContract(t)
	op, ok := doc.Paths[r.ContractPath][r.ContractMethod].(map[string]any)
	if !ok {
		t.Fatalf("契约里没有 %s %s", r.ContractMethod, r.ContractPath)
	}
	resps, ok := op["responses"].(map[string]any)
	if !ok {
		t.Fatalf("契约里 %s %s 没有 responses", r.ContractMethod, r.ContractPath)
	}
	var body map[string]any
	for _, code := range []string{"201", "200"} {
		if v, ok := resps[code].(map[string]any); ok {
			body = v
			break
		}
	}
	if body == nil {
		t.Fatalf("契约里 %s %s 既没有 200 也没有 201 响应", r.ContractMethod, r.ContractPath)
	}
	content, ok := body["content"].(map[string]any)
	if !ok {
		t.Fatalf("契约里 %s %s 的成功响应没有 content", r.ContractMethod, r.ContractPath)
	}
	media, ok := content["application/json"].(map[string]any)
	if !ok {
		t.Fatalf("契约里 %s %s 的成功响应不是 application/json", r.ContractMethod, r.ContractPath)
	}
	schema, ok := media["schema"].(map[string]any)
	if !ok {
		t.Fatalf("契约里 %s %s 的成功响应没有 schema", r.ContractMethod, r.ContractPath)
	}
	return schemaProps(t, doc.Components.Schemas, schema,
		fmt.Sprintf("%s %s 的成功响应", r.ContractMethod, r.ContractPath))
}

// schemaProps 把一个 schema 解成顶层属性名集合，顺带跟 $ref 与 allOf。
//
// 跟 $ref 是必须的：契约里 OrderCreateRequest / Order 都是 $ref 到
// components.schemas 的。不跟的话这两条接口会解出空集，而空集会让上面那些
// 断言一次也不执行 —— 恒绿，正是这一整个文件要消灭的东西。
//
// 不认识的形状一律 Fatal，不返回空集。理由同上。
func schemaProps(t *testing.T, defs map[string]map[string]any,
	schema map[string]any, where string) map[string]bool {
	t.Helper()

	out := map[string]bool{}
	var walk func(s map[string]any, depth int)
	walk = func(s map[string]any, depth int) {
		if depth > 8 {
			t.Fatalf("%s 的 schema 嵌套太深或成环", where)
		}
		if ref, ok := s["$ref"].(string); ok {
			const prefix = "#/components/schemas/"
			if !strings.HasPrefix(ref, prefix) {
				t.Fatalf("%s 里不认识的引用 %q", where, ref)
			}
			target, ok := defs[strings.TrimPrefix(ref, prefix)]
			if !ok {
				t.Fatalf("%s 的引用 %q 指向一个不存在的 schema", where, ref)
			}
			walk(target, depth+1)
			return
		}
		if all, ok := s["allOf"].([]any); ok {
			for _, one := range all {
				m, ok := one.(map[string]any)
				if !ok {
					t.Fatalf("%s 的 allOf 里有一项不是映射", where)
				}
				walk(m, depth+1)
			}
		}
		props, ok := s["properties"].(map[string]any)
		if !ok {
			return
		}
		for name := range props {
			out[name] = true
		}
	}
	walk(schema, 0)
	if len(out) == 0 {
		t.Fatalf("%s 一个属性都没解析出来 —— 这个解析器跟不上契约的形状了", where)
	}
	return out
}

// NotYetImplementedResponse 里挂的每一笔账，都必须是契约成功响应里真有的字段。
//
// 与请求体那条同理，这里只做「清单 → 契约」这一个方向的机械对账。
// 反向（实现了却忘了划掉）由行为测试 order_test.go 的 TestFreightIsAbsentNotZero
// 盯着：它断言响应里**没有**这个键，真的实现了运费它就会红。
func TestNotYetImplementedResponseFieldsExistInContract(t *testing.T) {
	checked := 0
	for _, r := range routes {
		if len(r.NotYetImplementedResponse) == 0 {
			continue
		}
		t.Run(r.HTTPMethod+" "+r.ContractPath, func(t *testing.T) {
			props := contractResponseProps(t, r)
			for name, why := range r.NotYetImplementedResponse {
				if !props[name] {
					t.Errorf("NotYetImplementedResponse 里挂着 %q（%s），"+
						"但契约的成功响应里没有这个字段了 —— 清单烂了，请删掉这一行",
						name, why)
				}
				checked++
			}
			t.Logf("契约成功响应声明 %v；挂账 %v", sortedBool(props), sorted(r.NotYetImplementedResponse))
		})
	}
	if checked == 0 {
		t.Fatal("一笔响应体挂账都没查到 —— 挂账清空了就该把这条测试一起删掉，" +
			"留着一条恒绿的测试比没有更糟")
	}
}

// NotYetImplementedStage 里挂的每一笔账，都必须是契约描述里真提到的那个阶段。
//
// 与请求体那条同理，这里只做「清单 → 契约」这一个方向的机械对账：
// 契约把某个阶段改名或删掉时这条会红，指出那一行清单已经在描述一个不存在的
// 东西。反向（真的实现了却忘了划掉）由行为测试 search_test.go 的
// TestExplainListsExactlyTheStagesThatRan 盯着。
//
// 为什么判据是「描述里出现过这几个字」而不是别的：因为那句描述**就是**契约对
// 这条接口的全部承诺 —— 契约在响应形状上分不出「跑了四层」和「跑了两层」，
// 它们的 items 长得一模一样。
func TestNotYetImplementedStagesAreNamedInContract(t *testing.T) {
	checked := 0
	for _, r := range routes {
		if len(r.NotYetImplementedStage) == 0 {
			continue
		}
		t.Run(r.HTTPMethod+" "+r.ContractPath, func(t *testing.T) {
			desc := contractOperationDescription(t, r)
			for name, why := range r.NotYetImplementedStage {
				if !strings.Contains(desc, name) {
					t.Errorf("NotYetImplementedStage 里挂着 %q（%s），"+
						"但契约里 %s %s 的描述已经不提它了 —— 清单烂了，"+
						"请对着新的描述改这一行或删掉它。当前描述：%q",
						name, why, r.ContractMethod, r.ContractPath, desc)
				}
				checked++
			}
			t.Logf("契约描述 %q；挂账 %v", desc, sorted(r.NotYetImplementedStage))
		})
	}
	if checked == 0 {
		t.Fatal("一笔流水线阶段挂账都没查到 —— 挂账清空了就该把这条测试一起删掉，" +
			"留着一条恒绿的测试比没有更糟")
	}
}

// contractOperationDescription 取出该接口的 description。
// 取不到或者是空串就 Fatal：一条对着空串做 strings.Contains 的测试恒绿。
func contractOperationDescription(t *testing.T, r route) string {
	t.Helper()

	doc := loadContract(t)
	op, ok := doc.Paths[r.ContractPath][r.ContractMethod].(map[string]any)
	if !ok {
		t.Fatalf("契约里没有 %s %s", r.ContractMethod, r.ContractPath)
	}
	desc, _ := op["description"].(string)
	if strings.TrimSpace(desc) == "" {
		t.Fatalf("契约里 %s %s 没有 description —— 这条测试没在检查任何东西",
			r.ContractMethod, r.ContractPath)
	}
	return desc
}
