// Package search 放「商品文本怎么变成可检索的东西」这件事，索引侧与查询侧共用。
//
// 眼下有两样：
//
//	① bigram 切分（本文件）——「羊毛衫」→「羊毛 毛衫」。语义检索层 §3 定的
//	   一期关键词召回方案：分词发生在**应用层**，数据库只用 simple 配置按空格切，
//	   不引入 pg_jieba / zhparser 之类要自打镜像的扩展。
//	② 送进 embedding 的拼接文本与两个 processor 的输入指纹（derive.go）。
//
// # 为什么切分放在这里，而不是放在索引任务里
//
// 因为**两侧必须切得一模一样**。索引侧把「红色连衣裙」切成
// 「红色 色连 连衣 衣裙」写进 products.search_text，查询侧把用户输入的
// 「连衣裙」切成「连衣 衣裙」再去 to_tsquery —— 两边用的若不是同一个函数，
// 漂移的表现形式是「某些词搜不到」，而且只对某一类字符串成立
// （比如一边把英文转小写、另一边没转）。没有任何测试会因为「两处实现不同」
// 而红，只有召回率会悄悄掉。
//
// 查询侧（Task 5 的 RRF 召回）直接调本包，不要再写一份。
package search

import (
	"strings"
	"unicode"
)

// Bigram 把一段文本切成可以交给 to_tsvector('simple', ...) 的串。
//
// 规则就两条（语义检索层 §3）：
//
//	· 中日韩表意文字：切**二元组**。"红色连衣裙" → "红色 色连 连衣 衣裙"。
//	  连续的一段 n 个汉字出 n-1 个二元组；只有一个字时出那个字本身
//	  （否则「裙」这种单字词会整个消失）。
//	· 英文与数字：**不做 bigram**，按空格与字符类边界正常切词。型号、规格
//	  （"205/55R16"、"iPhone 15"）做 bigram 只会制造噪声 —— simple 配置
//	  对拉丁字母本来就分得开，这正是 §3 说「英文和数字按空格与边界正常切词」的理由。
//
// 其余字符（标点、空白、emoji）一律当分隔符：它们不进 tsvector，留着只会
// 让二元组跨过一个本来就该断开的边界（"连衣裙，女装" 不该切出「裙女」）。
//
// 输出统一小写。to_tsvector('simple', ...) 自己也会折大小写，但查询侧要拿同一个
// 函数的输出去手工拼 tsquery，那一步没有人替它折 —— 两侧的大小写一致必须由
// 这个函数负责，不能指望数据库。
func Bigram(s string) string {
	var out []string
	var cjk []rune  // 当前这一段连续的表意文字
	var word []rune // 当前这一段连续的字母数字

	flushWord := func() {
		if len(word) > 0 {
			out = append(out, string(word))
			word = word[:0]
		}
	}
	flushCJK := func() {
		switch {
		case len(cjk) == 0:
		case len(cjk) == 1:
			// 单字成段。丢掉它的话「裙」「茶」这类单字词从关键词路彻底消失。
			out = append(out, string(cjk))
		default:
			for i := 0; i+1 < len(cjk); i++ {
				out = append(out, string(cjk[i:i+2]))
			}
		}
		cjk = cjk[:0]
	}

	for _, r := range s {
		switch {
		case isIdeograph(r):
			flushWord()
			cjk = append(cjk, r)
		case isWordRune(r):
			flushCJK()
			word = append(word, unicode.ToLower(r))
		default:
			flushCJK()
			flushWord()
		}
	}
	flushCJK()
	flushWord()
	return strings.Join(out, " ")
}

// isIdeograph 判断这个字符要不要走二元组。
//
// 范围取表意文字本身（汉字、日文假名、谚文音节），不含 CJK 标点
// （unicode.Han 之外的 \p{P} 落在 default 分支当分隔符）。假名与谚文一起进来，
// 是因为它们与汉字有同一个毛病：simple 配置对它们同样不分词，
// 而商品标题里出现日文韩文并不稀奇（跨境店铺）。
func isIdeograph(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

// isWordRune 判断这个字符属不属于「按空格切就够」的那一类：
// 拉丁字母、数字，以及夹在型号中间的字符类里最常见的那些。
//
// 刻意**不**把 '-' '/' '.' 算进来："205/55R16" 切成 205 / 55r16 三段之后，
// 查询侧输入 "55R16" 仍然命中 55r16 那一段；而把它们并进 token 的话，
// 用户少打一个斜杠就一条也召不回。宁可切碎一点。
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}
