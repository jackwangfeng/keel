package search_test

import (
	"strings"
	"testing"

	"github.com/keel/keel/internal/search"
)

// bigram 切分真的切了。
//
// 「红色连衣裙」→「红色 色连 连衣 衣裙」是语义检索层 §3 的那个例子，
// 逐字照抄。它守的不是一个格式，是整条中文关键词召回路成立与否：
// 不切分的话 to_tsvector('simple','红色连衣裙') 整句一个 token，
// 用户搜「连衣裙」零结果 —— 而这件事不报错，只是搜不到。
//
// internal/db/semantic_test.go 的 TestSearchVectorIsGeneratedFromSearchText
// 从数据库那一侧证明「search_vector 读的是 search_text 这一列」，
// 这里证明「写进那一列的东西真的被切过」。两条缺一不可：
// 把 search_text 直接写成未切分的标题，那一条照绿。
func TestBigramSplitsCJK(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"红色连衣裙", "红色 色连 连衣 衣裙"}, // 语义检索层 §3 的例子
		{"羊毛衫", "羊毛 毛衫"},         // 数据模型 §8 的例子
		{"裙", "裙"},               // 单字成段：丢掉它，「裙」这个词从关键词路消失
		{"", ""},
	} {
		if got := search.Bigram(tc.in); got != tc.want {
			t.Errorf("Bigram(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// 英文与数字**不做 bigram**（§3：「英文和数字按空格与边界正常切词，不做 bigram」）。
//
// 这条与上面那条方向相反，必须分开断言。只有上面那条的话，一个「把所有字符
// 都切成二元组」的实现照样全绿，而它会把 "iphone" 切成 ip/ph/ho/on/ne ——
// 精确型号查询（§11 阶段 2 的验收标准是命中率 > 95%）从此全是噪声。
func TestBigramLeavesLatinAndDigitsWhole(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"iPhone 15 Pro", "iphone 15 pro"},
		{"205/55R16", "205 55r16"}, // 斜杠是分隔符，见 isWordRune 的注释
		{"ABC", "abc"},             // 统一小写，查询侧要拿同一个输出去拼 tsquery
	} {
		if got := search.Bigram(tc.in); got != tc.want {
			t.Errorf("Bigram(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// 二元组不许跨过字符类边界与标点。
//
// 「连衣裙，女装」切出一个「裙女」的话，那是一个不存在的词，
// 而它会让搜「裙女」这种乱敲的输入命中，同时把 tsvector 撑大。
func TestBigramDoesNotCrossBoundaries(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"连衣裙，女装", "连衣 衣裙 女装"},
		{"A4纸", "a4 纸"},
		{"2024春季新款", "2024 春季 季新 新款"},
	} {
		if got := search.Bigram(tc.in); got != tc.want {
			t.Errorf("Bigram(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
	// 阳性对照：把标点换成空字符串（也就是「如果不当分隔符会怎样」），
	// 切出来一定包含那个跨边界的二元组。没有这一条，上面的期望值
	// 只是一串没有对照的字面量。
	if !strings.Contains(search.Bigram("连衣裙女装"), "裙女") {
		t.Fatal("「连衣裙女装」里居然切不出「裙女」—— 那上面那条断言证明不了" +
			"标点被当成了分隔符，它可能只是碰巧")
	}
}

// 查询侧与索引侧必须用**同一个函数**，所以它得是确定的、幂等可比的。
//
// 这条测试真正守的是「两侧切得一样」这件事在函数层面成立：
// 同一段文本切两次必须完全相同。不确定的实现（比如用了 map 遍历顺序）
// 会让索引侧写进去的串和查询侧拼出来的串偶尔不一致 ——
// 症状是「有些词有时候搜得到」，没有任何断言会红。
func TestBigramIsDeterministic(t *testing.T) {
	const s = "红色碎花连衣裙 女装 夏季新款 M码"
	first := search.Bigram(s)
	for i := 0; i < 5; i++ {
		if got := search.Bigram(s); got != first {
			t.Fatalf("第 %d 次切分结果不同：%q vs %q", i, got, first)
		}
	}
	if first == "" {
		t.Fatal("切出来是空的 —— 上面那条循环在比较两个空串")
	}
}

// 索引侧追加单字，单字查询才召得回来（深度审查 2026-09-27：「杯」「咖」无结果）。
func TestIndexTermsAppendsUnigramsAfterBigrams(t *testing.T) {
	cases := []struct{ in, want string }{
		{"陶瓷马克杯", "陶瓷 瓷马 马克 克杯 陶 瓷 马 克 杯"},
		{"裙", "裙 裙"}, // 单字成段：Bigram 已出一次，单字再记一次，无害
		{"咖啡 咖啡豆", "咖啡 咖啡 啡豆 咖 啡 豆"}, // 单字去重
		{"iPhone 15", "iphone 15"},   // 没有表意文字：与 Bigram 相同
		{"", ""},
	}
	for _, tc := range cases {
		if got := search.IndexTerms(tc.in); got != tc.want {
			t.Errorf("IndexTerms(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// 查询侧切出来的每个词，索引侧一定有 —— 两侧约定的底线。
func TestQueryTermsAreSubsetOfIndexTerms(t *testing.T) {
	title := "陶瓷马克杯 挂耳咖啡 10 包"
	idx := map[string]bool{}
	for _, w := range strings.Fields(search.IndexTerms(title)) {
		idx[w] = true
	}
	for _, q := range []string{"杯", "咖", "马克杯", "咖啡", "10", "陶瓷马克杯"} {
		for _, w := range strings.Fields(search.Bigram(q)) {
			if !idx[w] {
				t.Errorf("查询 %q 切出 %q，索引里没有 —— 这个词永远搜不到", q, w)
			}
		}
	}
}

func TestNormQuery(t *testing.T) {
	for in, want := range map[string]string{
		" 连衣裙 ": "连衣裙", "红色  连衣裙": "红色 连衣裙", "Dress\t": "dress", "红色连衣裙": "红色连衣裙",
	} {
		if got := search.NormQuery(in); got != want {
			t.Errorf("NormQuery(%q) = %q，期望 %q", in, got, want)
		}
	}
}
