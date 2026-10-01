package search

import "strings"

// 查询侧：把用户输入的一串字变成 to_tsquery 吃得下的东西（M3 Task 4）。
//
// 它和索引侧用**同一个** Bigram，这是这个包存在的全部理由（见 bigram.go 的
// 包注释）。两侧各写一份的话，漂移的表现形式是「某些词搜不到」，
// 而且只对某一类字符串成立 —— 没有任何测试会因为「两处实现不同」而红，
// 只有召回率会悄悄掉。

// TSQueryOr 用 " | " 把 Bigram 的输出拼成一条 tsquery 串。
// 切不出任何词时返回空串 —— 调用方必须判空，理由见下面「为什么不兜底」。
//
// # 为什么是 OR 而不是 AND
//
// 这一层是**召回**，不是排序。「红色连衣裙」切出「红色 色连 连衣 衣裙」四个
// 二元组：AND 要求一件商品同时含这四个，那基本只剩标题一字不差的那几件，
// 而「碎花连衣裙」会被整个漏掉 —— 漏在召回层的东西，后面任何一层都救不回来。
// OR 把它们都捞进来，命中几个的区分度交给 ts_rank_cd（覆盖密度：命中的二元组
// 挨得越近、越多，分越高），再交给 RRF 与（将来的）精排。
//
// 代价是明确的：OR 会捞回一批只命中一个二元组的噪声。一期认这个代价，
// 因为召回层宁滥勿缺。
//
// # 2026-10 起：先 AND、不够再 OR
//
// 上面那笔代价后来被压测量出来了（docs/性能压测-2026-10.md 第六节 ③）：长尾词
// 「栖木复古地毯」切成五个二元组，OR 捞回 9980 件，绝大多数只命中「地毯」或「复古」
// 一个 —— 不光是噪声，召回 SQL 还要一件件给它们算排序分。
//
// 所以多词查询现在**先**用 TSQueryAnd（全部二元组都命中），够一页就只用它；不够再用
// 这一条 OR 补齐（service/search.go 的 recallByKeyword）。「碎花连衣裙」那类只命中
// 一部分的商品仍然捞得回来 —— 只是排在全部命中的那批后面，且只在全部命中的不够时。
// 单词查询 AND 与 OR 是同一个集合，只跑这一条，与改之前逐字相同。
//
// # 为什么不兜底：切不出词时返回空串，而不是返回一条"永不命中"的 tsquery
//
// `to_tsquery('simple', '')` 是**语法错误**，不是空结果。返回一条像
// "zzz_never_match" 那样的假串能让调用方省一个 if，但它把「用户搜了一串标点」
// 这件事伪装成了「搜到了 0 条」，而这两件事的处置不同：前者该走「换个词试试」，
// 后者该走「这家店真的没有」。
func TSQueryOr(s string) string {
	b := Bigram(s)
	if b == "" {
		return ""
	}
	return strings.Join(strings.Fields(b), " | ")
}

// TSQueryAnd 用 " & " 把 Bigram 的输出（去重后）拼成一条 tsquery 串，并返回去重后的词数。
// 切不出任何词时返回 ("", 0)。
//
// 词数 ≤ 1 时 AND 与 OR 是同一个命中集合，调用方应当只跑 TSQueryOr 那一条（单词查询
// 与改之前逐字相同）。去重是为了让「哈哈哈」（切出两个「哈哈」）也被认作单词查询。
//
// 安全性与 TSQueryOr 相同：每个词都来自 Bigram，只含字母、数字与表意文字，
// tsquery 的元字符（& | ! ( ) : * ' <->）一个都活不下来；拼进去的 " & " 是唯一的算子。
func TSQueryAnd(s string) (string, int) {
	b := Bigram(s)
	if b == "" {
		return "", 0
	}
	seen := make(map[string]bool)
	terms := make([]string, 0, 8)
	for _, w := range strings.Fields(b) {
		if !seen[w] {
			seen[w] = true
			terms = append(terms, w)
		}
	}
	return strings.Join(terms, " & "), len(terms)
}
