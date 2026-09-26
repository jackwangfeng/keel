package understanding_test

import (
	"slices"
	"testing"

	"github.com/keel/keel/internal/understanding"
)

// 基准输入。每个字段都非空且互不相同 —— 扰动一个字段之后，
// 「指纹变了」不可能是因为两个字段恰好一样。
func baseInput() understanding.ProductInput {
	return understanding.ProductInput{
		ProductID:    7,
		Title:        "红色连衣裙",
		Subtitle:     "夏季新款",
		Description:  "面料柔软透气，适合日常通勤",
		CategoryName: "女装",
	}
}

// perturb 把某个字段改成一个不同的值。
func perturb(in understanding.ProductInput, field string) understanding.ProductInput {
	switch field {
	case understanding.FieldTitle:
		in.Title += "（改过）"
	case understanding.FieldSubtitle:
		in.Subtitle += "（改过）"
	case understanding.FieldDescription:
		in.Description += "（改过）"
	case understanding.FieldCategoryName:
		in.CategoryName += "（改过）"
	}
	return in
}

// allFields 是 ProductInput 里全部可扰动的字段。
//
// 它写死在这里，而不是从 InputFields() 的并集算出来：从并集算的话，
// 一个谁都不声明的字段（比如将来加进来的 brand_name）就不会被扰动，
// 于是「某个 processor 偷偷把它算进了指纹」这件事永远不会被发现。
// 这份清单与 ProductInput 的字段一一对应，加字段时要一起加。
var allFields = []string{
	understanding.FieldTitle,
	understanding.FieldSubtitle,
	understanding.FieldDescription,
	understanding.FieldCategoryName,
}

// **这是 internal/understanding 这个包存在的理由。**
//
// 商品理解服务设计 §3 给的目标是逐条的：
//
//	标题变更   → 仅重跑 text_embedding / category_classify / compliance_check
//	主图更换   → 仅重跑 image_embedding / image_quality
//	价格变更   → 不触发任何 processor
//
// 兑现它需要两个方向同时成立，而**只做一个方向的检查是假绿的**：
//
//	① 声明关心的字段改了，指纹必须变。
//	   少了它，一个「指纹恒为常量」的实现全绿，而那意味着什么都不会被重算。
//	② 没声明的字段改了，指纹必须不变。
//	   少了它，一个「把整行商品都喂进哈希」的实现全绿 —— 而那正是 §3 说
//	   「单一 content_hash 太粗：改一张图，文本向量也会被判定为需重算」
//	   要避免的东西，代价是一次改图触发十万商品级别的文本向量重算，
//	   账单是真的，而检索结果看上去完全正常。
func TestFingerprintDependsExactlyOnInputFields(t *testing.T) {
	base := baseInput()
	for _, p := range understanding.All() {
		want := p.Fingerprint(base)
		declared := p.InputFields()
		if len(declared) == 0 {
			t.Errorf("%s 一个 InputFields 都没声明 —— 那样下面两个方向的检查都是空转",
				p.Name())
			continue
		}
		for _, f := range allFields {
			got := p.Fingerprint(perturb(base, f))
			cares := slices.Contains(declared, f)
			switch {
			case cares && got == want:
				t.Errorf("%s 声明关心 %s，但改了它指纹没变 —— "+
					"这个字段改了之后这个能力不会被重算，它的结果从此停在旧输入上，"+
					"而且不会有任何东西报错", p.Name(), f)
			case !cares && got != want:
				t.Errorf("%s 没有声明关心 %s，但改了它指纹变了 —— "+
					"这个能力会被一次与它无关的改动触发重算。§3 那张表的整点"+
					"就是不让这件事发生（改一张图不该重算十万条文本向量）",
					p.Name(), f)
			}
		}
	}
}

// 两个 processor 的输入恰好相同时，指纹也必须不同。
//
// internal/search 的 fingerprint 把 processor 名编进了哈希，
// internal/understanding 的 fingerprintOf 也是。这条测试是那个设计的靶子：
// 不编进去的话，「把一格的指纹抄进另一格」在库里看不出来 ——
// 两格值一样，而它们本来就可能一样。
func TestDifferentProcessorsNeverShareAFingerprint(t *testing.T) {
	// 造一个让 text_embedding 与 search_text 的输入退化成同一段的输入：
	// 类目名为空时，两者的输入都只剩标题与副标题。
	in := understanding.ProductInput{Title: "同一段文本", Subtitle: "副标题"}
	seen := map[string]string{}
	for _, p := range understanding.All() {
		fp := p.Fingerprint(in)
		if other, dup := seen[fp]; dup {
			t.Errorf("%s 与 %s 算出了同一格指纹 —— processor 名没有进哈希，"+
				"把一格的值抄进另一格将永远看不出来", p.Name(), other)
		}
		seen[fp] = p.Name()
	}
}

// 指纹必须是确定的：同样的输入算两次必须一样。
//
// 不确定的实现（比如把一个 map 的遍历结果拼进去）的症状是每一轮都判成
// 「输入变了」，于是全库每 30 秒重算一遍，账单一直涨而检索结果完全正常。
func TestFingerprintIsDeterministic(t *testing.T) {
	in := baseInput()
	for _, p := range understanding.All() {
		first := p.Fingerprint(in)
		if first == "" {
			t.Fatalf("%s 的指纹是空串 —— 下面那条循环在比较两个空串", p.Name())
		}
		for i := 0; i < 3; i++ {
			if got := p.Fingerprint(in); got != first {
				t.Fatalf("%s 第 %d 次算出了不同的指纹：%q vs %q", p.Name(), i, got, first)
			}
		}
	}
}

// All() 与 Names() 不能是空的，名字也不能重复。
//
// 阳性对照：上面三条全部按 All() 枚举，All() 返回空切片时它们一条都不会红。
func TestRegistryIsNotEmpty(t *testing.T) {
	if len(understanding.All()) < 3 {
		t.Fatalf("登记了 %d 个 processor —— 本轮落地的是 text_embedding / "+
			"search_text / compliance_check 三个，少了谁都意味着上面那几条"+
			"逐字段扰动的检查漏掉了它", len(understanding.All()))
	}
	names := understanding.Names()
	for i := 1; i < len(names); i++ {
		if names[i] == names[i-1] {
			t.Errorf("有两个 processor 都叫 %q —— 它们会共用 input_hashes 的同一格，"+
				"后写的那个会覆盖先写的，而两边都以为自己记住了", names[i])
		}
	}
}
