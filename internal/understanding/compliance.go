package understanding

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// 合规检查（商品理解服务设计 §2 的快路径，§1 的 compliance_check）。
//
// ===========================================================================
// 一、它为什么是同步的，以及这带来的唯一一处「宁可误拒」
// ===========================================================================
//
// §2 原话：「广告法违禁词在中国是硬约束，单次处罚起点二十万。这一步必须同步
// 阻塞，不能放异步。」而 §7 的降级表里，合规检查超时是**全系统唯一**一处
// 宁可误拒的地方（docs/ai-capabilities.md「三条纪律」第三条重复了这句）。
//
// 「宁可误拒」这四个字落到代码上是一条具体的规矩：**这个检查回答不出来的时候，
// 商品不许上架**。它不是 fail-open，也不是「跳过检查记一条日志」。
// 落点在 internal/service/admin_catalog.go 的 checkCompliance —— 预算、
// 超时、以及超时之后拒绝，都在那里；这个文件只负责「给定文本，命中了什么」。
//
// ===========================================================================
// 二、词表编译进二进制，不是一张 shared-reference 表 —— 这是一个要解释的决定
// ===========================================================================
//
// 两条路都真实可行，代价不同：
//
//	              热更新        每次检查的成本          落地面
//	  数据库表      可以         多一次查询 + 一次反序列化  要进 db/tenancy.json，
//	                                                    要一套后台维护接口
//	  编译进二进制   要发版       零                      一个 Go 切片
//
// **选了编译进去**，四条理由，从强到弱：
//
//  1. **「热更新」这个优势在这个仓库里基本不存在。** 广告法违禁词是全国性的、
//     全租户共用的静态参考数据，那就是 db/tenancy.json 的 shared-reference 类，
//     而那一类的 GRANT 面是 **keel_app 只读**（理由写在那一类的说明里：
//     任何租户都能改状态机，是个比跨租户读取更难发现的洞）。于是「热更新」
//     实际要走管理员角色 —— 也就是一次迁移，也就是一次发版。
//     换来的热更新能力是假的，而多出来的查询是真的。
//  2. **它在同步路径上，而那条路径的预算是 200 ms。** 多一次查询本身不贵，
//     贵的是它把「能不能发布商品」挂到了一次数据库往返的可用性上 ——
//     而按第一节那条规矩，查不出来就是拒绝。一次锁等待会变成一次发布失败。
//     编译进去之后，这个检查在任何数据库故障下都照常工作。
//  3. **词表是这套规则的测试固件本身。** 加一个词是一次会被评审看见的 diff，
//     配一条测试；而一张表里的一行数据改了，没有任何 diff、没有任何测试。
//     这条与「表结构的唯一真相源是数据模型文档」是同一条脾气。
//  4. 少一张表、少一条 tenancy.json 条目、少一套后台维护接口。
//
// **被否决的那条路，代价也写下来**：改词要发版。对一个真的在运营的部署，
// 「监管新点名了一个词，今天就要拦住」会变成一次紧急发布。真到那一天，
// 正确的做法是加一张 shared-reference 表**覆盖**这份内置词表（内置的作为兜底，
// 表里的作为增量），而不是把内置这份搬走 —— 那样第 2 条的可用性性质还在。
// 这一步本轮不做，因为没有第二个数据点说明它是需要的。
//
// ===========================================================================
// 三、这一轮**只有规则词表**，小模型兜底没有做
// ===========================================================================
//
// §2 写的是「规则词表打底 + 小模型兜底变体（"蕞""No.1""巅峰之作"）」。
// 这一轮落地的是前半句。后半句没有做，理由与代价都说清楚：
//
//   - 引擎侧的 /v1/generate 有 schema 约束解码（§6），能力是够的。但快路径的
//     预算是 200 ms，而一次 27B 模型的生成在共享 GPU 上远超这个数 ——
//     把它放进同步路径，按第一节那条规矩，超时就是拒绝发布，
//     于是「引擎忙」会直接变成「商家发不了货」。
//   - 那三个例子本身是**列进词表**的（见下面 wordlist 里的 variant 分组）：
//     蕞 / No.1 / 巅峰之作 现在就拦得住。拦不住的是**词表之外的新变体** ——
//     "冣好"、"zui 好"、拆字、谐音。那一半今天不存在。
//
// **所以这里没有「已实现」的谎**：文档（设计 §十一、ai-capabilities 第 6 项）
// 与这段注释说的是同一件事 —— 变体兜底还没有。
//
// ===========================================================================
// 四、类目资质那一半也没有做，而且是因为库里没有那个东西
// ===========================================================================
//
// §2 说快路径还要挡「类目资质缺失」。数据模型里**没有任何一张表记录商家资质**
// （§3 商品域只有 categories / products / skus / product_images）。
// 没有数据就没有判据，硬写一条只能是永远为真或永远为假。
//
// 这件事直接影响了词表的边界：**医疗 / 保健用语（治疗、疗效、根治、特效）
// 刻意不在词表里**。它们的合法性取决于类目与资质 —— 一家卖血压计的店
// 说「用于高血压治疗」是合规的，而在没有资质数据的前提下把它们列进词表，
// 等于让那家店永远发不出商品，且没有任何豁免出口。
// 宁可漏拦一类需要语境判断的词，也不要造一条无解的拦截。

// Violation 是一次命中。
//
// **Offset / Length 按 Unicode 码点计，从 0 开始**，不是字节，也不是「第几个字」
// 的自然语言说法（那个从 1 开始）。选码点是因为它是唯一一个前后端都能对上的
// 单位：Go 这一侧 []rune 就是它，客户端（uni-app x / TS）拿到的是 UTF-16 码元，
// 而商品文案里的字符几乎全在 BMP 内，两者逐字相等。
// 真出现 emoji（非 BMP）时 UTF-16 会多算一个码元 —— 这条偏差写在契约的
// 字段描述里，不假装它不存在。
type Violation struct {
	// Field 是契约里的字段名："title" / "subtitle" / "description"。
	Field  string
	Offset int
	Length int

	// Term 是命中的词（词表里那一条，规范化之后的形态）。
	Term string

	// Rule 是规则分组，见 ruleAbsolute 等常量。客户端不该按它分支
	// （那是 Problem.type 的活），它是给人看的分类。
	Rule string
}

// Message 是给商家看的那句话。
//
// 位置从 1 开始数：Offset 是给程序用的（客户端要据此高亮），
// 这句话是给人看的，而没有人会说「第 0 个字」。
func (v Violation) Message() string {
	return fmt.Sprintf("「%s」（第 %d 个字起）属于%s，《广告法》禁止在商品文案中使用",
		v.Term, v.Offset+1, v.Rule)
}

// 三个规则分组。它们进 Violation.Rule，也是给商家看的那句话里的分类名。
const (
	ruleAbsolute  = "绝对化用语"
	ruleAuthority = "国家机关名义或免检背书"
	ruleVariant   = "绝对化用语的变体写法"
)

// ComplianceCheck 是 §1 的 compliance_check，快路径上那一个。
type ComplianceCheck struct{}

func (ComplianceCheck) Name() string { return ProcessorComplianceCheck }

// ProcessorComplianceCheck 是它在 input_hashes 里的键名。
//
// 它与另外两个 processor 的键名不住在一起（那两个在 internal/search，
// 因为索引侧与查询侧共用那个包）—— 合规检查与检索无关，把它的名字塞进
// internal/search 只会让那个包多认识一件不相干的事。
const ProcessorComplianceCheck = "compliance_check"

// InputFields：**全部对外可见的文案**，而且只有文案。
//
// 类目名不在里面：违禁词是商家自己写进标题/副标题/详情的，类目名由平台维护，
// 一家店改不了它。把它算进指纹只会让「运营改一次类目名」触发全类目商品重新
// 跑合规检查 —— 而那个检查的结论不会因此改变。
func (ComplianceCheck) InputFields() []string {
	return []string{FieldTitle, FieldSubtitle, FieldDescription}
}

// Fingerprint 是这一格指纹。
//
// **今天没有任何地方写它**，这一格在 product_understanding.input_hashes 里
// 恒为缺失。挂账写在 internal/service/admin_catalog.go 的 checkCompliance 上：
// 合规结论不落库，因为写 product_understanding 会把索引触发点的水位线推掉，
// 让刚发布的商品静默退出候选集（db/queries/semantic.sql 的 MarkProductIndexed
// 上有同一段实测）。
//
// 那为什么还要有这个方法：因为 Processor 的契约是「声明了哪些字段就恰好依赖
// 哪些字段」，而 TestFingerprintDependsExactlyOnInputFields 逐个 processor 核对
// 这一条。一个没有指纹的 processor 会是那条检查唯一的盲区，而它恰好是
// 唯一一个输入里带 description 的 —— 将来真要把结论落库时，指纹已经是对的。
func (ComplianceCheck) Fingerprint(in ProductInput) string {
	return fingerprintOf(ProcessorComplianceCheck,
		strings.TrimSpace(in.Title)+"\x00"+
			strings.TrimSpace(in.Subtitle)+"\x00"+
			strings.TrimSpace(in.Description))
}

// Check 扫一件商品的三段文案，按字段顺序、字段内按位置返回全部命中。
//
// ctx 每扫一个字段核一次：这个函数本身是纯计算、快得没有意义，
// 但**调用方要靠 ctx 的超时来兑现「宁可误拒」那条规矩**，而一个从不看 ctx
// 的实现会让那条规矩在真出问题时（词表被喂成几万条、文案被喂成几 MB）
// 静默失效。两处缺一不可：调用方设预算，这里认它。
func (c ComplianceCheck) Check(ctx context.Context, in ProductInput) ([]Violation, error) {
	var out []Violation
	for _, f := range []struct {
		name, text string
	}{
		{FieldTitle, in.Title},
		{FieldSubtitle, in.Subtitle},
		{FieldDescription, in.Description},
	} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out = append(out, scanField(f.name, f.text)...)
	}
	return out, nil
}

// scanField 在一段文本里找出全部违禁词。
//
// ### 规范化：全角转半角、大写转小写、**丢掉空白**
//
// 前两条是形态归一（"ＮＯ.1" 与 "no.1" 是同一个词）。第三条是规避对抗：
// 「最 好」「最　好」是最常见的一种躲词表写法，一个空格就能绕过。
//
// **只丢空白，不丢标点**，这是一处刻意的取舍：跨过标点去拼词会造出
// 「价格最。好评如潮」这样的误拒，而标点在中文里本来就是语义边界。
// 代价是「最·好」「最-好」这类写法拦不住 —— 那属于第三节说的「词表之外的变体」，
// 是小模型兜底的活。
//
// ### 位置怎么还原
//
// 规范化会丢字符，所以不能拿规范化后的下标当结果。idx 是一张**平行的下标表**：
// norm[i] 来自原文的第 idx[i] 个码点。命中 [i, i+len) 之后，
// Offset = idx[i]，Length = idx[i+len-1] - idx[i] + 1 —— 也就是把被丢掉的
// 空白一并算进长度，客户端高亮出来正好盖住用户看到的那一段。
func scanField(field, text string) []Violation {
	if text == "" {
		return nil
	}
	norm, idx := normalize(text)
	if len(norm) == 0 {
		return nil
	}

	var hits []Violation
	for _, e := range wordlist {
		term := []rune(e.term)
		for i := 0; i+len(term) <= len(norm); i++ {
			if !runesEqual(norm[i:i+len(term)], term) {
				continue
			}
			start := idx[i]
			end := idx[i+len(term)-1]
			hits = append(hits, Violation{
				Field:  field,
				Offset: start,
				Length: end - start + 1,
				Term:   e.term,
				Rule:   e.rule,
			})
		}
	}
	return dedupe(hits)
}

// dedupe 按位置排序，并丢掉**被别的命中完全包住**的那些。
//
// 为什么需要它：词表里既有「最好」也有「全网最好」，一段文案会同时命中两条，
// 而商家看到的是同一个地方被报了两次。留长的那一条 —— 它是更完整的证据。
//
// 完全相同的两条（同位置同长度，来自两条写法不同但规范化后相同的词条）
// 也在这里收敛成一条。
func dedupe(hits []Violation) []Violation {
	if len(hits) <= 1 {
		return hits
	}
	sort.Slice(hits, func(a, b int) bool {
		if hits[a].Offset != hits[b].Offset {
			return hits[a].Offset < hits[b].Offset
		}
		return hits[a].Length > hits[b].Length // 长的排前面，短的会被它吃掉
	})
	out := hits[:0:0]
	for _, h := range hits {
		covered := false
		for _, k := range out {
			if k.Offset <= h.Offset && h.Offset+h.Length <= k.Offset+k.Length {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, h)
		}
	}
	return out
}

// normalize 返回规范化后的码点序列，以及每个码点在原文里的下标。
func normalize(s string) ([]rune, []int) {
	rs := []rune(s)
	norm := make([]rune, 0, len(rs))
	idx := make([]int, 0, len(rs))
	for i, r := range rs {
		if unicode.IsSpace(r) || r == '　' {
			continue // 全角空格不在 unicode.IsSpace 的 Latin-1 快路径里，显式列一个
		}
		norm = append(norm, foldRune(r))
		idx = append(idx, i)
	}
	return norm, idx
}

// foldRune 把全角 ASCII 折回半角，再统一小写。
//
// 全角区间 U+FF01..U+FF5E 与 ASCII 0x21..0x7E 一一对应，差 0xFEE0。
// 这一条挡的是「ＮＯ．１」这种输入法顺手打出来的全角形态 ——
// 它在页面上和 "NO.1" 长得几乎一样，而按码点比对是两个完全不同的串。
func foldRune(r rune) rune {
	if r >= 0xFF01 && r <= 0xFF5E {
		r -= 0xFEE0
	}
	return unicode.ToLower(r)
}

func runesEqual(a, b []rune) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
