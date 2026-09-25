package handler_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// contractPath 是被检查的那个接口在契约里的路径与方法。
const (
	contractPath   = "/products"
	contractMethod = "get"
	handlerFile    = "product.go"
)

// notYetImplemented 是契约里声明了、handler 还没实现的 query 参数。
//
// 这份清单是一笔**必须维护的欠账**，不是豁免。三条规矩：
//
//   - 契约新增一个 query 参数而 handler 没读 → 这条测试红，逼人当场决定
//     「实现它」还是「先记在这里」。默认行为是红，不是悄悄忽略。
//   - 实现了某个参数（handler 开始读它）却忘了从这里划掉 → 同样红。
//     所以「实现完删掉一行」是天然的验收动作，不靠人记得。
//   - 这里写了一个契约里根本没有的名字 → 还是红。清单不能烂掉。
//
// 为什么不是返回 400：契约里这四个都是 optional，对一份冻结的契约把可选参数
// 判成错误是违约，而且实现之后还得把那个 400 撤回去。也不在契约的 description
// 里写「未实现」—— 契约描述的是接口，不是实现进度。
var notYetImplemented = map[string]string{
	"category_id":     "分类筛选，等 Catalog 那个任务",
	"sort":            "排序策略，同上",
	"min_price_cents": "价格区间下界，同上",
	"max_price_cents": "价格区间上界，同上",
}

// 契约声明的 query 参数，要么被 handler 读了，要么在 notYetImplemented 里挂着账。
//
// 这条测试读的是契约本身（YAML）与 handler 的源码（AST），两边都不是人手写的
// 清单 —— 手写清单会和它描述的东西各走各的，而那种漂移不会有任何症状。
func TestContractQueryParamsAreHandledOrListed(t *testing.T) {
	declared := contractQueryParams(t)
	handled := queryParamsReadByHandler(t)

	for name := range declared {
		if handled[name] {
			continue
		}
		if _, listed := notYetImplemented[name]; listed {
			continue
		}
		t.Errorf("契约里 %s %s 有 query 参数 %q，handler 没读它，"+
			"notYetImplemented 里也没挂账 —— 它会被静默忽略，"+
			"客户端以为筛过了，其实没有", contractMethod, contractPath, name)
	}

	for name, why := range notYetImplemented {
		if _, ok := declared[name]; !ok {
			t.Errorf("notYetImplemented 里挂着 %q（%s），但契约里已经没有这个参数了 —— "+
				"清单烂了，请删掉这一行", name, why)
		}
		if handled[name] {
			t.Errorf("handler 已经在读 %q 了，但它还挂在 notYetImplemented 里（%s）—— "+
				"实现完请删掉这一行", name, why)
		}
	}

	// 阳性对照：两边都不能是空集。任何一边解析失败（路径改名、handler 改文件名、
	// YAML 结构变了）都会让上面所有循环一次也不执行，于是整条测试恒绿。
	if len(declared) == 0 {
		t.Fatalf("从契约里一个 query 参数都没解析出来 —— %s %s 还在吗？",
			contractMethod, contractPath)
	}
	if len(handled) == 0 {
		t.Fatalf("从 %s 里一个 c.Query 调用都没解析出来 —— 这条测试没在检查任何东西",
			handlerFile)
	}
	t.Logf("契约声明 %v；handler 读取 %v", sorted(declared), sortedBool(handled))
}

// contractQueryParams 从 OpenAPI 契约里取出该接口的全部 query 参数名，
// 顺带解析 $ref（契约里 Page / PageSize 是 $ref 到 components.parameters 的）。
func contractQueryParams(t *testing.T) map[string]string {
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

	item, ok := doc.Paths[contractPath]
	if !ok {
		t.Fatalf("契约里没有路径 %s", contractPath)
	}
	if _, ok := item[contractMethod]; !ok {
		t.Fatalf("契约里 %s 没有 %s 方法", contractPath, contractMethod)
	}

	// path item 级的 parameters 对该路径下每个方法都生效，所以两处都要收。
	var params []any
	if v, ok := item["parameters"].([]any); ok {
		params = append(params, v...)
	}
	op, ok := item[contractMethod].(map[string]any)
	if !ok {
		t.Fatalf("契约里 %s %s 不是一个映射", contractMethod, contractPath)
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
func queryParamsReadByHandler(t *testing.T) map[string]bool {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, handlerFile, nil, 0)
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
				handlerFile, sel.Sel.Name)
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
