package understanding

// 违禁词的**变体兜底**（商品理解服务设计 §2 那句「小模型兜底变体」的另一条路）。
//
// ===========================================================================
// 一、为什么不是用模型做
// ===========================================================================
//
// compliance.go 文件头第三节写着变体为什么没做：快路径预算 200 ms，一次
// 27B 生成远超这个数，而按第一条纪律（回答不出来就拒绝发布），「引擎忙」会
// 直接变成「商家发不了货」。
//
// 这一轮换了一条**不进引擎**的路：同音字归一。它挡的是谐音那一类
// （蕞 / 冣 / 醉 / 嘴 替「最」），成本是一次 O(N) 的字符替换。
//
// ===========================================================================
// 二、做法，以及为什么值域窄到「多字词条的首字」
// ===========================================================================
//
//	文本 → normalize → 去装饰符号 → 同音字归一 → **原有的精确词表匹配**
//
// 归一是一比一的换字，长度不变，所以下标表不用重算 —— 位置天然是对的，
// 客户端高亮照旧盖得住商家看到的那一段。
//
// 开头试过更宽的那一版（整词拼音化），**实测当场撞出误拒**，所以收窄了：
//
//   · 词表里唯一一条单字条目「蕞」在拼音空间里就是一个 zui 音节，
//     于是「最近」「最后」「最初」「最多」全被当成「蕞」拦下；
//   · 「神器」与「神奇」在拼音空间里是同一个串 shen|qi，
//     「神奇的清洁力」这种再正常不过的文案会被判违规。
//
// 根子是中文同音字太多，两音节的词在拼音空间里碰撞是常态。所以只替换
// **多字词条的第一个字**：躲词表的人换的是「最 / 第 / 绝 / 顶」这种显眼的字，
// 不是「好 / 佳」这种中性字。收窄之后「奇」「级」连表都进不去，上面两条误拒
// 自动消失。
//
// 实测（演示库 42 个商品标题/副标题 + search_logs 1102 条真实搜索词 +
// 项目刹车测试那几条反例，共 156 条）：**误拒 0 条**，同时「醉佳」「嘴便宜」
// 「决对领先」「鼎级」四类真变体都拦得住。数字是在改代码之前跑出来的，
// 不是改完之后凑的。
//
// ===========================================================================
// 三、拦不住的两类，以及为什么接受
// ===========================================================================
//
//	· 纯拼音写法（zuihao）  —— 汉字 → 汉字的表碰不到 ASCII 串；
//	· 非首字的谐音（遥遥领衔 不配 遥遥领先）。
//
// 要挡它们就得放开值域，而放开值域的代价是误拒。**误拒会让所有商家发不了货，
// 漏拦只放过一个故意躲的人**，方向不能选错。这两条是已知的、有意的缺口。
//
// ===========================================================================
// 四、装饰符号：只跨这几个，标点一个都不跨
// ===========================================================================
//
// 让「价格最。好评如潮」拼出「最好」是一次误拒，而标点在中文里本来就是语义
// 边界（TestPunctuationIsNotSkipped 盯的就是这条）。所以跨的**只有装饰性符号**：
// 它们出现在词中间几乎只有一种理由 —— 躲词表。「。」「，」「、」一个都不在里面。
//
// 连字符 '-' 也没进：型号里到处都是（"iphone-15"），而两侧都是汉字的「最-好」
// 远没有「最·好」常见。宁可漏这一小类，也不要让一整批正常型号被拦。

// variantFillers 是允许跨过去的装饰性符号。
//
// 全角形态（＊、～等）不必另列：进到这里之前已经过 foldRune，全角 ASCII
// 折回了半角。中圆点 '·'（U+00B7）不是 ASCII，单独列着。
var variantFillers = map[rune]bool{
	'*': true, '·': true, '★': true, '☆': true, '▲': true, '△': true,
	'●': true, '○': true, '◆': true, '◇': true, '■': true, '□': true,
	'※': true, '^': true, '~': true, '_': true,
}

// maxFillerRun 是最多允许连着跨几个符号。
//
// 「最·好」是一个；「最***好」也能见到。超过三个连着的装饰符在真实文案里没见过，
// 而放开上限会让一段纯符号被整体吃掉、两端的词被拼到一起。
const maxFillerRun = 3

// wordlistChars 是词表里出现过的全部字符。
//
// 它们在归一这一步**保持原样**，理由很具体：词表里有一条单字条目「蕞」，
// 它靠的就是「这个字在商品文案里没有任何正常用法」。若把它也归一成「最」，
// 那条目就永远命不中，而它单独出现时恰恰是最明确的躲词表信号。
var wordlistChars = func() map[rune]bool {
	m := make(map[rune]bool, 256)
	for _, e := range wordlist {
		for _, r := range e.term {
			m[r] = true
		}
	}
	return m
}()

// canonicalNorm 把同音字换成规范字。**一比一，长度不变**，所以调用方
// 手里的下标表继续有效 —— 这是选「换字」而不是「转拼音」的直接好处之一。
func canonicalNorm(norm []rune) []rune {
	out := make([]rune, len(norm))
	changed := false
	for i, r := range norm {
		if wordlistChars[r] {
			out[i] = r
			continue
		}
		if c, ok := homophoneCanonical[r]; ok {
			out[i] = c
			changed = true
			continue
		}
		out[i] = r
	}
	if !changed {
		return norm // 没动过就别多分配一次
	}
	return out
}

// stripFillers 去掉夹在两个汉字之间的装饰符号，返回新序列与平行下标。
//
// 「两侧都是汉字」这个条件是刻意的：它挡住「-15℃」这种符号在词首的情形，
// 也保证一段纯符号不会被整体吃掉。
func stripFillers(norm []rune, idx []int) ([]rune, []int) {
	out := make([]rune, 0, len(norm))
	oidx := make([]int, 0, len(idx))
	for i := 0; i < len(norm); {
		if !variantFillers[norm[i]] {
			out = append(out, norm[i])
			oidx = append(oidx, idx[i])
			i++
			continue
		}
		j := i
		for j < len(norm) && variantFillers[norm[j]] && j-i < maxFillerRun {
			j++
		}
		prevIsHan := len(out) > 0 && isHan(out[len(out)-1])
		nextIsHan := j < len(norm) && isHan(norm[j])
		if prevIsHan && nextIsHan {
			i = j // 跨过去：这一段不进 out
			continue
		}
		for ; i < j; i++ {
			out = append(out, norm[i])
			oidx = append(oidx, idx[i])
		}
	}
	return out, oidx
}

func isHan(r rune) bool {
	return r >= 0x4E00 && r <= 0x9FFF
}

// scanVariants 是变体那一遍：在「去符号 + 同音归一」之后的串上按词表精确匹配。
//
// 它**只补精确那一遍没报到的起点**。写成「补」而不是「并列」是有原因的：
// 一段文案写成「最好」时两遍都会命中同一处，报两次会让商家以为
// 「我改了一个它还在」。
//
// Rule 一律报 ruleVariant：商家看到的是「这个写法被识别成了变体」，
// 而 Term 报词表里那条（"最好"），不是他写的那个谐音字 —— 他要改的是意思，
// 不是字形。
func scanVariants(field string, norm []rune, idx []int, already map[int]bool) []Violation {
	alt, aidx := stripFillers(norm, idx)
	alt = canonicalNorm(alt)

	var hits []Violation
	for _, e := range wordlist {
		term := []rune(e.term)
		for i := 0; i+len(term) <= len(alt); i++ {
			if !runesEqual(alt[i:i+len(term)], term) {
				continue
			}
			start := aidx[i]
			if already[start] {
				continue
			}
			end := aidx[i+len(term)-1]
			hits = append(hits, Violation{
				Field:  field,
				Offset: start,
				Length: end - start + 1,
				Term:   e.term,
				Rule:   ruleVariant,
			})
		}
	}
	return hits
}

// variantOffsets 把已报命中的起点收成一个集合。
//
// 只记**起点**：变体那一遍与精确那一遍对同一处的命中长度相同，起点不同才是另一处。
func variantOffsets(hits []Violation) map[int]bool {
	m := make(map[int]bool, len(hits))
	for _, h := range hits {
		m[h.Offset] = true
	}
	return m
}
