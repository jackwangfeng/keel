package handler_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

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
