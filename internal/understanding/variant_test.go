package understanding_test

import (
	"testing"

	"github.com/keel/keel/internal/understanding"
)

// 变体兜底那一遍的行为闸门（variant.go 文件头讲了为什么是「同音字归一」
// 而不是整词拼音化，以及为什么值域只有多字词条的首字）。
//
// 这一组要同时守住两头：
//
//	· 真在躲词表的写法要拦住（不然这个功能等于没做）；
//	· 正常中文一个都不许拦（不然它把全站商家都挡在发布之外）。
//
// 第二头的判据不是「我觉得不会」，是**改代码之前跑出来的实测**：
// 演示库 42 个商品标题/副标题 + search_logs 1102 条真实搜索词 + 下面这些
// 反例，共 156 条，误拒 0。数字先于代码，不是代码写完凑的。

// 谐音：躲词表的人换的是「最 / 第 / 绝 / 顶」这种显眼的字。
func TestVariantCatchesHomophoneSwap(t *testing.T) {
	for _, tc := range []struct {
		in           string
		term         string
		offset, size int
	}{
		{"全网醉佳咖啡壶", "最佳", 2, 2},     // 醉 → 最
		{"这款嘴便宜，闭眼入", "最便宜", 2, 3},  // 嘴 → 最
		{"决对领先同行的做工", "绝对领先", 0, 4}, // 决 → 绝
		{"鼎级材质，手感很好", "顶级", 0, 2},   // 鼎 → 顶
		{"蕞新款上市", "蕞", 0, 1},        // 单字条目：它在词表里，归一不动它
		{"全网蕞强保温杯", "蕞", 2, 1},
	} {
		got := check(t, understanding.ProductInput{Title: tc.in})
		if len(got) == 0 {
			t.Errorf("%q 一条都没命中 —— 同音字归一没生效", tc.in)
			continue
		}
		var hit *understanding.Violation
		for i := range got {
			if got[i].Term == tc.term {
				hit = &got[i]
				break
			}
		}
		if hit == nil {
			t.Errorf("%q 命中的是 %+v，里面没有 %q", tc.in, got, tc.term)
			continue
		}
		if hit.Offset != tc.offset || hit.Length != tc.size {
			t.Errorf("%q 里 %q 的位置是 [%d,+%d)，期望 [%d,+%d) —— "+
				"同音归一是一比一换字，位置不该漂；漂了说明下标表没跟着走",
				tc.in, tc.term, hit.Offset, hit.Length, tc.offset, tc.size)
		}
	}
}

// 插入装饰符号：「最·好」「最*好」。
//
// 只跨装饰符号、不跨标点，是 variant.go 第四节那条取舍；这里给它一条正例，
// TestPunctuationIsNotSkipped 给它一条反例，两头都钉住。
func TestVariantStripsDecorativeFillers(t *testing.T) {
	for _, tc := range []struct {
		in           string
		term         string
		offset, size int
	}{
		{"全网最·好用的壶", "最好", 2, 3}, // 长度含那个被跨过去的符号
		{"全网最*好用的壶", "最好", 2, 3},
		{"全网最※好用的壶", "最好", 2, 3},
	} {
		got := check(t, understanding.ProductInput{Title: tc.in})
		if len(got) != 1 {
			t.Errorf("%q 命中 %d 条，期望 1 条：%+v", tc.in, len(got), got)
			continue
		}
		if got[0].Term != tc.term || got[0].Offset != tc.offset || got[0].Length != tc.size {
			t.Errorf("%q 命中的是 %q @[%d,+%d)，期望 %q @[%d,+%d)",
				tc.in, got[0].Term, got[0].Offset, got[0].Length,
				tc.term, tc.offset, tc.size)
		}
	}
}

// **已知拦不住的两类，在这里写成断言。**
//
// 它们不是 bug，是 variant.go 第三节明写的取舍：要挡住就得放开值域，
// 而放开值域的代价是误拒（「神器 / 神奇」在拼音空间里是同一个串）。
//
// 写成断言是为了防另一种事：哪天有人顺手放宽了规则、这两类忽然拦住了，
// 那条改动看着是「变强了」，实际上是误拒面被打开了 —— 这条测试会红，
// 逼他把误拒那一头也重跑一遍。
func TestVariantKnownGapsStayOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"纯拼音写法", "zuihao 的一款咖啡壶"},
		{"非首字的谐音", "遥遥领衔的做工"}, // 衔 不是词条首字，不归一
		{"连字符不跨", "全网最-好用的壶"}, // '-' 在型号里到处都是，刻意不跨
	} {
		if got := check(t, understanding.ProductInput{Title: tc.in}); len(got) != 0 {
			t.Errorf("%s：%q 被拦下了（%+v）—— 这是已知的、有意留下的缺口。"+
				"若你有意把它补上，先重跑误拒那一头的实测，再来改这条断言",
				tc.name, tc.in, got)
		}
	}
}

// 真实语料不许误拒。
//
// 前 12 条是演示库里**真实存在**的商品标题，后 12 条是 search_logs 里
// 真实出现过的搜索词 —— 都不是我编的。用它们当判据，是因为「正常的商品文案」
// 长什么样，靠想是想不全的。
func TestVariantNoFalsePositivesOnRealCorpus(t *testing.T) {
	for _, s := range []string{
		// 演示库商品标题
		"亚麻四件套", "香薰蜡烛", "室内扩香藤条", "羊毛地毯",
		"雪纺碎花连衣裙", "真丝吊带长裙", "羊毛混纺大衣", "高腰阔腿牛仔裤",
		"针织开衫外套", "黑巧克力礼盒", "冻干草莓脆", "明前龙井茶",
		// 真实搜索词
		"连衣裙", "巧克力", "大衣", "登山鞋", "耳机", "蜡烛",
		"咖啡", "草莓", "手冲", "杯", "瑜伽垫", "冷萃咖啡液 6 支",
		// 同音字最容易踩到的那几个正常词
		"一级品大豆油", "等级品特价清仓", "神奇的清洁力", "决定权在你手上",
		"低价不等于低质", "第七天回访", "顶层带阁楼",
	} {
		if got := check(t, understanding.ProductInput{Title: s}); len(got) != 0 {
			t.Errorf("%q 被拦下了（%+v）—— 正常文案被判违规会让商家发不了货，"+
				"这比放过一个故意躲词表的人贵得多", s, got)
		}
	}
}
