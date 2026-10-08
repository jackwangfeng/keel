package handler_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

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
