package understanding

import (
	"crypto/sha256"
	"encoding/hex"
)

// 广告法违禁词表。编译进二进制的理由（以及被否决的「一张 shared-reference 表」
// 那条路的代价）写在 compliance.go 的文件头第二节，不在这里重复。
//
// ===========================================================================
// 词表的边界：为什么没有裸「最」，也没有裸「第一」
// ===========================================================================
//
// 《广告法》第九条禁的是**绝对化用语**，不是那两个字。
//
//	最近 / 最后 / 最初 / 最多可放 5 件   —— 全是正常中文
//	第一次 / 第一天 / 第一季            —— 同上
//
// 把裸字列进词表，全站商家的标题会成片被拒，而且没有任何出口。所以词表里的
// 每一条都是**短语**。compliance_test.go 里有一组反例断言专门盯这一条
// （TestCommonChineseWordsAreNotFlagged）—— 它是这份词表唯一的刹车：
// 没有它，把「最」加进来是一个看上去更严格、也不会让任何测试变红的动作。
//
// 代价诚实说：短语词表挡不住「本店第一」「价格最低哦」这类没被枚举到的说法。
// 那是 §2 说要用小模型兜底的那一半，而它这一轮没有做
// （compliance.go 文件头第三节）。
//
// ===========================================================================
// 匹配是在**规范化之后**做的，所以词条写成规范形态
// ===========================================================================
//
// 规范化 = 全角转半角 + 转小写 + 丢掉空白（scanField 的注释）。所以：
//
//	· 词条一律小写半角："no.1" 而不是 "No.1" / "ＮＯ．１"；
//	· 词条里不要有空格 —— 有的话永远匹配不上，而且不会有任何东西报错。
//	  TestWordlistEntriesAreNormalized 盯着这一条。

// wordlistVersion 进指纹。改词表必须改它。
//
// 与 search.TemplateVersion 同一个道理：指纹回答的是「要不要重算」，
// 而词表改了之后同一段文案的结论可能不同 —— 输入的字面没变，该重算的事实变了。
// 不把它编进去的话，加一个违禁词之后存量商品一件也不会被重新检查。
//
// （今天这一格指纹还没有任何写入点，见 ComplianceCheck.Fingerprint 的挂账。
// 先把规矩立对，比将来补一个已经错了的版本号便宜。）
//
// v1 → v2：加了变体那一遍（同音字归一 + 去装饰符号）。词表一个字没改，
// 但**同一段文案的结论可能不同**（「醉佳」从放行变成拦下），而指纹回答的
// 正是「要不要重算」。改的是匹配规则不是词表，版本号照样得动 —— 它记的是
// 「这套规则的版本」，不是「这张表的版本」。
const wordlistVersion = "adlaw-v2"

type wordlistEntry struct {
	term string
	rule string
}

// fingerprintOf 与 internal/search 的 fingerprint 是同一套做法
// （processor 名 + 版本 + 输入，以 \x00 分隔再 sha256）。
//
// 没有复用那一份：它在 internal/search 里是未导出的，而把它导出意味着
// internal/search 要认识「合规检查」这件与检索无关的事。两处各七行，
// 而它们真正要一致的是**形状**（分隔符 + 版本进哈希），不是同一段代码 ——
// 这两条性质各自有测试盯着。
func fingerprintOf(processor, input string) string {
	h := sha256.New()
	h.Write([]byte(processor))
	h.Write([]byte{0})
	h.Write([]byte(wordlistVersion))
	h.Write([]byte{0})
	h.Write([]byte(input))
	return hex.EncodeToString(h.Sum(nil))
}

// wordlist 是那份词表。
//
// 分三组，对应 Violation.Rule 的三个取值。分组的用处是给商家看的那句话里
// 有一个说得出口的分类 —— 「属于绝对化用语」比「命中违禁词 #37」有用得多。
var wordlist = []wordlistEntry{
	// ---------------------------------------------------------------
	// 一、绝对化用语（第九条第（三）项：「国家级」「最高级」「最佳」等）
	// ---------------------------------------------------------------
	{"国家级", ruleAbsolute},
	{"最高级", ruleAbsolute},
	{"最佳", ruleAbsolute},
	{"最好", ruleAbsolute},
	{"最优", ruleAbsolute},
	{"最优秀", ruleAbsolute},
	{"最强", ruleAbsolute},
	{"最先进", ruleAbsolute},
	{"最便宜", ruleAbsolute},
	{"最低价", ruleAbsolute},
	{"最高档", ruleAbsolute},
	{"最奢侈", ruleAbsolute},
	{"最流行", ruleAbsolute},
	{"最受欢迎", ruleAbsolute},
	{"最畅销", ruleAbsolute},
	{"最热销", ruleAbsolute},
	{"最划算", ruleAbsolute},
	{"世界第一", ruleAbsolute},
	{"全球第一", ruleAbsolute},
	{"中国第一", ruleAbsolute},
	{"全国第一", ruleAbsolute},
	{"全网第一", ruleAbsolute},
	{"行业第一", ruleAbsolute},
	{"销量第一", ruleAbsolute},
	{"第一品牌", ruleAbsolute},
	{"独一无二", ruleAbsolute},
	{"绝无仅有", ruleAbsolute},
	{"史无前例", ruleAbsolute},
	{"空前绝后", ruleAbsolute},
	{"前无古人", ruleAbsolute},
	{"顶级", ruleAbsolute},
	{"极品", ruleAbsolute},
	{"绝对领先", ruleAbsolute},
	{"遥遥领先", ruleAbsolute},
	{"全球领先", ruleAbsolute},
	{"世界领先", ruleAbsolute},
	{"领导品牌", ruleAbsolute},
	{"领袖品牌", ruleAbsolute},
	{"王牌", ruleAbsolute},
	{"永久", ruleAbsolute},
	{"万能", ruleAbsolute},
	{"绝对", ruleAbsolute},

	// ---------------------------------------------------------------
	// 二、国家机关名义与免检背书
	//     （第九条第（一）（二）项；「国家免检」制度 2008 年已废止，
	//      任何宣称都是虚假广告）
	// ---------------------------------------------------------------
	{"国家免检", ruleAuthority},
	{"免检产品", ruleAuthority},
	{"国家质量免检", ruleAuthority},
	{"国家领导人推荐", ruleAuthority},
	{"国家机关推荐", ruleAuthority},
	{"政府推荐", ruleAuthority},
	{"央视上榜品牌", ruleAuthority},
	{"特供", ruleAuthority},
	{"专供", ruleAuthority},
	{"军供", ruleAuthority},

	// ---------------------------------------------------------------
	// 三、变体写法
	//
	// §2 点名的三个例子就在这里：蕞 / no.1 / 巅峰之作。
	// **它们是被列进来的，不是被识别出来的** —— 词表之外的新变体
	// （冣 / zui 好 / 拆字谐音）今天一个也拦不住，那是小模型兜底的活，
	// 本轮没有做（compliance.go 文件头第三节）。
	//
	// 「蕞」单字进词表是个例外：它是「最」的异体写法，在现代汉语商品文案里
	// 没有任何正常用法 —— 它出现在标题里，目的只可能是躲词表。
	// ---------------------------------------------------------------
	{"蕞", ruleVariant},
	{"no.1", ruleVariant},
	{"no1", ruleVariant},
	{"top1", ruleVariant},
	{"第1品牌", ruleVariant},
	{"销量第1", ruleVariant},
	{"巅峰之作", ruleVariant},
	{"神器", ruleVariant},
	{"秒杀全场", ruleVariant},
}
