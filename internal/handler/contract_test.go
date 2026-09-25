package handler_test

import (
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
}

func (r route) ginPath() string { return apiPrefix + r.ContractPath }
func (r route) key() string     { return r.HTTPMethod + " " + r.ginPath() }

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

	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "电商系统-OpenAPI.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	// 用 map[string]any 逐层走，而不是一把 unmarshal 进结构体：OpenAPI 的
	// path item 里，`parameters`（一个序列）和 `get`/`post`（映射）是平级的
	// 兄弟键，强类型的 map[string]操作对象 会在解析别的路径时直接炸掉。
	var doc struct {
		Paths      map[string]map[string]any `yaml:"paths"`
		Components struct {
			Parameters map[string]struct {
				Name string `yaml:"name"`
				In   string `yaml:"in"`
			} `yaml:"parameters"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析契约失败: %v", err)
	}

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
