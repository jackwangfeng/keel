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

	// NotYetImplementedHeader 是契约声明为 required 的**请求头**里，这条
	// handler 还没实现的那些。与上面两笔是同一笔账的第三种形状。
	//
	// 眼下只有一个名字：Idempotency-Key。它出现在 M4 那 5 条 POST 上
	// （/admin/uploads、/admin/products、.../publication、.../skus、
	// /admin/categories），而本轮没有实现幂等，理由逐条写在值里。
	//
	// 它比 query 那份弱一格，与 NotYetImplementedBody 完全同级，
	// 要说清楚为什么：query 参数能从 handler 的 AST 里读出 c.Query("x") 来做
	// **双向**对账；请求头不行 —— 已经实现了幂等的那两条
	// （POST /orders 与 POST /orders/{order_no}/payments）读的是
	// `c.GetHeader(idempotencyKeyHeader)`，第一个实参是一个包级常量而不是
	// 字符串字面量，AST 跟不过去。硬要跟就得在测试里实现一遍常量求值，
	// 而那份求值器自己会和它模仿的语言分叉。
	//
	// 所以这里只做「清单 → 契约」这一个方向的机械对账（那个名字必须真的是
	// 契约里一个 required 的请求头参数），反向由**行为测试**盯着：
	// admin_catalog_test.go 的 TestAdminWritesAreNotYetIdempotent 拿同一把
	// Idempotency-Key 建两次商品，断言真的建出了两件 —— 也就是说
	// 「还没实现」这件事本身有靶子。真的实现了幂等，那条测试就会红，
	// 逼人回来删掉这里的登记。
	NotYetImplementedHeader map[string]string

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
	// 少做的是**流水线里的两层**，而那件事只写在 description 的一句话里：
	// 「四层流水线：双路召回 → RRF 融合 → Reranker 精排 → 业务重排」。
	// 没有这笔账的话，「接口看上去全实现了，实际只跑了一半」在任何闸门里
	// 都留不下痕迹。
	//
	// 键是**契约描述里那几个字**，逐字。两个方向都锁得住：
	//   - 契约把这个阶段改名或删掉 → TestNotYetImplementedStagesAreNamedInContract
	//     红（清单在描述一个契约里不存在的东西）；
	//   - 真的实现了这一层、响应里开始出现它的得分 → search_test.go 的
	//     TestExplainOmitsStagesThatDidNotRun 红，逼人回来删掉这一行。
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
			"category_id":     "分类筛选，等 Catalog 那个任务",
			"sort":            "排序策略，同上",
			"min_price_cents": "价格区间下界，同上",
			"max_price_cents": "价格区间上界，同上",
		},
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
		NotYetImplementedBody: map[string]string{
			"user_coupon_id": "券的三张表（coupon_templates / user_coupons / coupon_scopes）" +
				"本轮没有建（M2 计划「券为什么从任务 5 里拆出来」）。传了它返回 501，" +
				"**不是静默忽略** —— 忽略会让用户以为试算价是用券后的价，而他正是照着" +
				"这个数决定要不要下单的。",
		},
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
		NotYetImplementedBody: map[string]string{
			"user_coupon_id": "同 /orders/preview。**传了券却被忽略 = 用户以为用了券、" +
				"实际按原价成交**，那是钱的问题，所以这条路返回 501 而不是当作没看见。",
		},
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
		NoQueryParams: "详情只吃路径参数 product_id；契约里这条接口一个 query 参数都没有" +
			"（列表那些筛选条件属于 /products，不属于这里）",
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
		NotYetImplementedResponse: map[string]string{
			"refunds": "退款域的三张表（refunds / refund_items / refund_logs）本轮没有建，" +
				"所以这里不是「这一单没有退款」，而是**没查过**。回空数组会让详情页显示" +
				"「无售后记录」—— 一句在退款上线之前都不会被纠正的假话。",
		},
	},
	{
		ContractPath:   "/orders/{order_no}/payments",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "payment_intent.go",
		NoQueryParams: "渠道在请求体里，订单号在路径上，幂等键在 Idempotency-Key 请求头里；" +
			"契约里这条接口没有任何 query 参数",
	},
	{
		ContractPath:   "/search",
		ContractMethod: "post",
		HTTPMethod:     http.MethodPost,
		HandlerFile:    "search.go",
		NoQueryParams: "检索的参数全在请求体里（query / filters / size / strategy / explain）；" +
			"契约里这条接口一个 query 参数都没有",
		NotYetImplementedResponse: map[string]string{
			"trace_id": "检索日志 search_logs 那张表本轮没有建，POST /search/events 也没有实现。" +
				"trace_id 在契约里唯一的用处就是把一次检索与它后续的点击 / 加购 / 下单串起来" +
				"（那条接口的描述原话），而串到的那一头不存在。回一个谁也存不进去的 id " +
				"不是「先占个位」，是让客户端以为它拿到的东西有下文。",
		},
		NotYetImplementedStage: map[string]string{
			"Reranker 精排": "cross-encoder 精排（语义检索层 §5 / §11 阶段 3，路线图 M5）。" +
				"它是延迟大头（§8 给 80 ms），而本轮连离线评测集（§9.1）都还没有 —— " +
				"没有评测集就上精排，等于把一层没人能判断好坏的东西放进排序里。" +
				"explain=true 时 scores.rerank **整个不出现**，而不是填 0。",
			"业务重排": "缺货 / 活动失效 / 负毛利降权（语义检索层 §6，路线图 M5）。" +
				"它要的是活动与毛利数据，而 promotions 与成本价在数据模型里都还没有落地 —— " +
				"眼下能做的只有「缺货降权」那一条，而那条已经由 filters.in_stock_only " +
				"（默认 true）以过滤的形式做掉了。只做三分之一再叫「业务重排」，" +
				"比不做更容易让人以为它在了。explain=true 时 scores.business 同样缺席。",
		},
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
		NotYetImplementedHeader: map[string]string{
			"Idempotency-Key": "这条接口**用不了** idempotency_keys 那张表，而不是没轮到：" +
				"那张表的 merchant_id 列默认值是 current_merchant()，而开店跑在平台作用域里 —— " +
				"app.merchant_id 根本没设，current_merchant() 在那里是 RAISE（00002 那条会说人话的异常），" +
				"不是 NULL。也就是说抢占插入那一句在这条路上会当场报错。" +
				"要让它可用，得给幂等键一个「平台级」的落点（merchant_id 可空 + 策略跟着改），" +
				"那是又一次 schema 决定，不该和本轮那次（00022，把主体列从 user_id 换成 " +
				"(subject_kind, subject_id)）混在一起做。\n" +
				"暴露面说清楚，而它比那 5 条轻得多：merchants.code 是全局唯一的，" +
				"所以重发同一个请求**建不出第二家店** —— 第二次撞 merchants_code_key，返回 409。" +
				"代价只是「重放本该回 201 存档，实际回 409」，客户端两种情况下都知道店已经开好了。",
		},
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
		NotYetImplementedHeader: map[string]string{
			"Idempotency-Key": "本轮没有实现幂等。**不是忘了，是缺一样东西**：idempotency_keys 的主键是 (scope, user_id, idem_key)，而 user_id 那一列在后台这条路上要放的是 staff_id —— 而「把 staff_id 当成 user_id 用」正是 auth/staff_middleware.go 与数据模型 §14 反复点名的那件事（两张表的 id 来自同一种自增序列）。把它做对要么给那张表换一个主体列、要么另起一张表，那是一次 schema 决定，与缺一个「在指定租户里开事务」入口的 POST /admin/merchants 属于同一批。\n暴露面说清楚：重发同一个请求会多建一件草稿商品 / 一个类目 / 一条上传记录。SKU 那条由 uk_skus_code 挡（第二次是 409），publication 天生幂等（状态机终点相同），上传的重复件 24 小时后被孤儿回收清掉。也就是说真正的代价是「后台列表里多一行，商家自己删掉」，不是钱。\n反向由行为测试盯着：TestAdminWritesAreNotYetIdempotent。",
		},
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
		NotYetImplementedHeader: map[string]string{
			"Idempotency-Key": "本轮没有实现幂等。**不是忘了，是缺一样东西**：idempotency_keys 的主键是 (scope, user_id, idem_key)，而 user_id 那一列在后台这条路上要放的是 staff_id —— 而「把 staff_id 当成 user_id 用」正是 auth/staff_middleware.go 与数据模型 §14 反复点名的那件事（两张表的 id 来自同一种自增序列）。把它做对要么给那张表换一个主体列、要么另起一张表，那是一次 schema 决定，与缺一个「在指定租户里开事务」入口的 POST /admin/merchants 属于同一批。\n暴露面说清楚：重发同一个请求会多建一件草稿商品 / 一个类目 / 一条上传记录。SKU 那条由 uk_skus_code 挡（第二次是 409），publication 天生幂等（状态机终点相同），上传的重复件 24 小时后被孤儿回收清掉。也就是说真正的代价是「后台列表里多一行，商家自己删掉」，不是钱。\n反向由行为测试盯着：TestAdminWritesAreNotYetIdempotent。",
		},
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
		NotYetImplementedHeader: map[string]string{
			"Idempotency-Key": "本轮没有实现幂等。**不是忘了，是缺一样东西**：idempotency_keys 的主键是 (scope, user_id, idem_key)，而 user_id 那一列在后台这条路上要放的是 staff_id —— 而「把 staff_id 当成 user_id 用」正是 auth/staff_middleware.go 与数据模型 §14 反复点名的那件事（两张表的 id 来自同一种自增序列）。把它做对要么给那张表换一个主体列、要么另起一张表，那是一次 schema 决定，与缺一个「在指定租户里开事务」入口的 POST /admin/merchants 属于同一批。\n暴露面说清楚：重发同一个请求会多建一件草稿商品 / 一个类目 / 一条上传记录。SKU 那条由 uk_skus_code 挡（第二次是 409），publication 天生幂等（状态机终点相同），上传的重复件 24 小时后被孤儿回收清掉。也就是说真正的代价是「后台列表里多一行，商家自己删掉」，不是钱。\n反向由行为测试盯着：TestAdminWritesAreNotYetIdempotent。",
		},
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
		NotYetImplementedHeader: map[string]string{
			"Idempotency-Key": "本轮没有实现幂等。**不是忘了，是缺一样东西**：idempotency_keys 的主键是 (scope, user_id, idem_key)，而 user_id 那一列在后台这条路上要放的是 staff_id —— 而「把 staff_id 当成 user_id 用」正是 auth/staff_middleware.go 与数据模型 §14 反复点名的那件事（两张表的 id 来自同一种自增序列）。把它做对要么给那张表换一个主体列、要么另起一张表，那是一次 schema 决定，与缺一个「在指定租户里开事务」入口的 POST /admin/merchants 属于同一批。\n暴露面说清楚：重发同一个请求会多建一件草稿商品 / 一个类目 / 一条上传记录。SKU 那条由 uk_skus_code 挡（第二次是 409），publication 天生幂等（状态机终点相同），上传的重复件 24 小时后被孤儿回收清掉。也就是说真正的代价是「后台列表里多一行，商家自己删掉」，不是钱。\n反向由行为测试盯着：TestAdminWritesAreNotYetIdempotent。",
		},
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
		NotYetImplementedHeader: map[string]string{
			"Idempotency-Key": "本轮没有实现幂等。**不是忘了，是缺一样东西**：idempotency_keys 的主键是 (scope, user_id, idem_key)，而 user_id 那一列在后台这条路上要放的是 staff_id —— 而「把 staff_id 当成 user_id 用」正是 auth/staff_middleware.go 与数据模型 §14 反复点名的那件事（两张表的 id 来自同一种自增序列）。把它做对要么给那张表换一个主体列、要么另起一张表，那是一次 schema 决定，与缺一个「在指定租户里开事务」入口的 POST /admin/merchants 属于同一批。\n暴露面说清楚：重发同一个请求会多建一件草稿商品 / 一个类目 / 一条上传记录。SKU 那条由 uk_skus_code 挡（第二次是 409），publication 天生幂等（状态机终点相同），上传的重复件 24 小时后被孤儿回收清掉。也就是说真正的代价是「后台列表里多一行，商家自己删掉」，不是钱。\n反向由行为测试盯着：TestAdminWritesAreNotYetIdempotent。",
		},
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
		NotYetImplementedStage: map[string]string{
			"仅上传者本人与后台客服": "契约描述里那张 purpose 准入表的第三行（3 退款凭证）。" +
				"本轮的处置是**一律 403**，而不是「认上传者」—— 判「是不是上传者本人」要一个" +
				"可选鉴权中间件（这条路由是公开的，契约里没有 security），" +
				"而那条中间件今天**没有任何可测的输入**：purpose=3 的行只能由 C 端的 " +
				"POST /uploads 产生，而那条接口还没有实现。也就是说写出来的会是一段" +
				"任何测试都够不着的鉴权代码，而鉴权代码恰恰是最不该没有执行者的那一类。\n" +
				"失败方向是安全的那一边：所有人都读不到，而不是所有人都读得到。" +
				"反向由 upload_test.go 的 TestRefundProofIsNotPubliclyReadable 盯着 —— " +
				"它直接插一行 purpose=3 再打这条接口，断言 403。真的实现了「认上传者本人」，" +
				"那条测试会红，逼人回来删掉这一行。",
		},
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
// M4 任务 3（商家自助发布）划掉了 16 条，剩下 3 条。
// **M4 收尾这一轮又划掉了 POST /admin/merchants**（开店）：它缺的那样东西
// —— repository 上「在指定租户里开一个事务」的入口 —— 本轮建出来了，
// 叫 repository.WithNewTenant。剩下 2 条。
var notYetRouted = []pendingOp{
	// —— M1 就在契约里的 10 条，任务 2 划掉了其中 7 条。
	//
	// 剩下这 2 条**不再是「缺后台鉴权」**了 —— 那套中间件已经有了
	// （auth.StaffBearer），它们缺的是各自的业务。理由要跟着改，
	// 否则下一个人会照着一句过期的话去找一个已经存在的东西。
	{"/admin/orders/{order_no}/shipments", "post", "发货。shipments 表已落地（数据模型 §5），" +
		"后台鉴权也已落地，缺的是 handler 与 §5 那三条发货规则。"},
	{"/admin/refunds/{refund_no}/audit", "post", "退款审核。退款域的表已落地（§11），" +
		"后台鉴权也已落地，缺的是 handler 与退款状态机那几条边。"},
}

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
			if len(handled) == 0 {
				t.Fatalf("从 %s 里一个 c.Query 调用都没解析出来 —— 这条测试没在检查任何东西",
					r.HandlerFile)
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
// TestExplainOmitsStagesThatDidNotRun 盯着。
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

// NotYetImplementedHeader 里挂的每一笔账，都必须是契约里**真有且必填**的一个
// 请求头参数。
//
// 与请求体那条同理，这里只做「清单 → 契约」这一个方向的机械对账，
// 理由写在 route.NotYetImplementedHeader 上（AST 跟不过一个包级常量）。
// 反向（真的实现了却忘了划掉）由 admin_catalog_test.go 的
// TestAdminWritesAreNotYetIdempotent 盯着：它断言同一把 Idempotency-Key
// 打两次真的建出了两件商品。
//
// 为什么判据里带上「必填」：一个 optional 的请求头没被读，是「这个可选能力
// 还没做」；一个 required 的请求头没被读，是**服务端在违约** ——
// 契约告诉客户端「你必须带上它」，而服务端连看都没看。
// 两者该被区别对待，所以这条断言只认后者。
func TestNotYetImplementedHeadersExistInContract(t *testing.T) {
	checked := 0
	for _, r := range routes {
		if len(r.NotYetImplementedHeader) == 0 {
			continue
		}
		t.Run(r.HTTPMethod+" "+r.ContractPath, func(t *testing.T) {
			required := contractRequiredHeaders(t, r)
			if len(required) == 0 {
				t.Fatalf("契约里 %s %s 一个必填请求头都没解析出来 —— "+
					"这条测试没在检查任何东西", r.ContractMethod, r.ContractPath)
			}
			for name, why := range r.NotYetImplementedHeader {
				if !required[name] {
					t.Errorf("NotYetImplementedHeader 里挂着 %q（%s），"+
						"但契约里 %s %s 已经没有这个必填请求头了 —— "+
						"清单烂了，请删掉这一行。当前必填请求头：%v",
						name, why, r.ContractMethod, r.ContractPath, sortedBool(required))
				}
				checked++
			}
			t.Logf("契约必填请求头 %v；挂账 %v", sortedBool(required), sorted(r.NotYetImplementedHeader))
		})
	}
	if checked == 0 {
		t.Fatal("一笔请求头挂账都没查到 —— 挂账清空了就该把这条测试一起删掉，" +
			"留着一条恒绿的测试比没有更糟")
	}
}

// contractRequiredHeaders 取出该接口全部 required: true 的请求头参数名。
//
// 与 contractQueryParams 走同一套 $ref 解析（Idempotency-Key 在契约里正是
// 一条 $ref 到 components.parameters 的引用），不认识的引用形式一律 Fatal ——
// 静默跳过等于让上面那条断言少检查一个名字，而它只有这一个判据。
func contractRequiredHeaders(t *testing.T, r route) map[string]bool {
	t.Helper()

	doc := loadContract(t)
	item, ok := doc.Paths[r.ContractPath]
	if !ok {
		t.Fatalf("契约里没有路径 %s", r.ContractPath)
	}
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

	out := map[string]bool{}
	for _, raw := range params {
		pm, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("参数项不是映射: %#v", raw)
		}
		name, in, required := "", "", false
		if ref, ok := pm["$ref"].(string); ok {
			const prefix = "#/components/parameters/"
			if !strings.HasPrefix(ref, prefix) {
				t.Fatalf("不认识的参数引用 %q", ref)
			}
			c, ok := doc.Components.Parameters[strings.TrimPrefix(ref, prefix)]
			if !ok {
				t.Fatalf("契约里的引用 %q 指向一个不存在的参数", ref)
			}
			name, in, required = c.Name, c.In, c.Required
		} else {
			name, _ = pm["name"].(string)
			in, _ = pm["in"].(string)
			required, _ = pm["required"].(bool)
		}
		if in == "header" && required && name != "" {
			out[name] = true
		}
	}
	return out
}
