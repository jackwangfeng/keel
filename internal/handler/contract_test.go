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
}

// ginPath 把契约路径翻成 gin 注册的那一个：加前缀，并把 OpenAPI 的 {name}
// 换成 gin 的 :name。
//
// 两种写法在这张表里必须有一份是源、另一份是导出来的，不能各写各的：
// 手写 gin 路径的话，`/webhooks/payments/{channel}` 与
// `/api/v1/webhooks/payments/:channel` 之间任何一个字母的出入都不会红 ——
// 表里那一行只是永远配不上任何已注册路由，而那条对账测试会把它报成
// 「登记了但没注册」，指向一个错的方向。
func (r route) ginPath() string {
	p := apiPrefix + r.ContractPath
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
			"image_url": "商品主图。products 表上没有图片列，商品图在数据模型里还没有落地" +
				"（uploads 那张表存的是上传件，没有与商品的关联）。回空串会让客户端渲染一个" +
				"「加载失败」的占位图，缺席说的才是实话：这个字段还没有数据来源。",
			"images": "商品图集，同 image_url。回空数组会让轮播图组件显示「无图」，" +
				"而那与「这件商品确实没有配图」是两件事。",
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
