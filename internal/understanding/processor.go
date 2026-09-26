// Package understanding 是商品理解服务的能力抽象（商品理解服务设计 §1）。
//
// 每个能力是一个 processor：它声明自己叫什么、关心商品的哪几个字段，
// 并据此算出一格**输入指纹**。指纹落在 product_understanding.input_hashes 的
// 同名键上，而那一列是「这个 processor 的输入变没变」的唯一真相源
// （数据模型 §8「增量重算的指纹：只认一处」）。
//
// ===========================================================================
// 一、这个包为什么存在：InputFields 不是装饰，它是那条「只重跑受影响的能力」
// ===========================================================================
//
// 设计 §3 给的目标是逐条的：
//
//	标题变更   → 仅重跑 text_embedding / category_classify / compliance_check
//	主图更换   → 仅重跑 image_embedding / image_quality
//	价格变更   → 不触发任何 processor
//
// 在 M3 里这件事是**隐式**的：internal/search/derive.go 的两个 Fingerprint
// 各自决定往哈希里喂什么，而「谁关心哪几个字段」这句话只存在于那个文件的注释里。
// 两个 processor 的时候还读得完；八个的时候，「改主图触发了文本向量重算」
// 这种错只会表现为账单变高，没有任何东西报错。
//
// 所以 InputFields() 被提成接口的一部分，并由 processor_test.go 的
// TestFingerprintDependsExactlyOnInputFields 逐个 processor × 逐个字段核对：
// **声明关心的字段改了，指纹必须变；没声明的字段改了，指纹必须不变。**
// 那条测试是这个包存在的理由，接口只是它的落点。
//
// ===========================================================================
// 二、与设计 §1 那段接口的两处偏离，以及为什么
// ===========================================================================
//
// §1 给的是：
//
//	type Processor interface {
//	    Name() string
//	    InputFields() []string
//	    Process(ctx context.Context, in *ProductInput) (*ProcessorResult, error)
//	}
//
// 这里的 Processor **只有前两个半**（Name / InputFields / Fingerprint），
// Process 不在里面。两条理由：
//
//   - **快慢两路的输出没有任何共同结构。** compliance_check 产出的是
//     「哪个字段第几个字命中了哪条违禁词」，text_embedding 产出的是 1024 个
//     float32 加一个模型名。一个共同的 *ProcessorResult 只能是 any 加一次
//     类型断言 —— 那不是抽象，是把 type switch 从调用点挪进了结构体，
//     而且调用点仍然要写。落点也不同：一个进同步响应，一个进 product_text_vectors。
//   - **慢路径不许一条一条调。** 设计 §6 的批处理要求是硬的：「索引侧批大小
//     32–64；禁止循环单条调用」。一个 `Process(ctx, *ProductInput)` 的签名
//     从形状上就鼓励 for 循环里调一次 —— 而那正是要禁的东西。
//     慢路径的执行入口因此是各自的批量方法（TextEmbedding.Contents +
//     inference.Embedder.Embed），不长成 Processor 的一个方法。
//
// 于是这个接口收敛到「所有能力真正共有的那一半」：身份与指纹。
// 共有的东西放接口里，不共有的东西放各自的具体类型上 —— 而不是发明一个
// 谁都不满足的公共形状再靠断言拆开。
//
// ===========================================================================
// 三、指纹的算法仍然在 internal/search，这里不搬家
// ===========================================================================
//
// search.ProductText 的 EmbedFingerprint / SearchTextFingerprint 与
// TemplateVersion 一起住在那个包里，因为**索引侧与查询侧共用同一套切分与拼接**
// （那个包的文件头讲得很清楚：两边用的若不是同一个函数，漂移的表现形式是
// 「某些词搜不到」）。把它们搬到这里会让查询侧去 import 一个叫 understanding
// 的包，而查询侧与商品理解无关。
//
// 这个包做的是**把它们包装成 processor**，并给每一格指纹配上一份可机械检查的
// 「我关心哪些字段」的声明。字面量（"text_embedding" / "search_text"）
// 仍然只在 internal/search 里出现一次。
package understanding

import (
	"slices"

	"github.com/keel/keel/internal/search"
)

// ProductInput 是全部 processor 看得见的商品输入的**并集**。
//
// 它是并集而不是「每个 processor 自己的输入结构体」，因为调用方只读一次商品行
// （db/queries/semantic.sql 那三条 SELECT 的列一模一样），然后把同一份原料
// 分给若干个 processor。每个 processor 从中取自己 InputFields() 声明的那几格。
//
// **价格、库存、销量刻意不在这里，而且不是忘了**：语义检索层 §2.1 那张
// 「必须排除的字段」表说的是高频变动字段进了输入会让指纹天天变、全量天天重算。
// 用一个字段有限的结构体承载输入，等于让「不小心把 sales_count 拼进去」
// 这件事需要先改类型定义 —— 那是一个会被评审看见的动作。
type ProductInput struct {
	ProductID int64

	Title        string
	Subtitle     string
	Description  string
	CategoryName string
}

// 字段名常量。InputFields() 返回的就是这些串，而
// TestFingerprintDependsExactlyOnInputFields 拿它们去逐个字段做扰动 ——
// 打错一个字母的症状是那个字段从此不被检查，测试照绿。所以只在这里写一次。
const (
	FieldTitle        = "title"
	FieldSubtitle     = "subtitle"
	FieldDescription  = "description"
	FieldCategoryName = "category_name"
)

// Processor 是 §1 那张能力清单里所有能力共有的那一半。
//
// 为什么 Process 不在里面，见文件头第二节。
type Processor interface {
	// Name 是这个能力在 product_understanding.input_hashes 里的键名，
	// 也是它在设计 §1 那张表里的名字。
	Name() string

	// InputFields 是这个能力关心商品的哪几个字段。
	//
	// 它的语义是**恰好**：列出来的字段变了，指纹必须变；没列出来的字段变了，
	// 指纹必须不变。这两个方向都被测试盯着，缺一个都不行 ——
	// 只保「列出来的会变」的话，一个把整行商品都喂进哈希的实现照样全绿，
	// 而那正是 §3 说「单一 content_hash 太粗」要避免的东西。
	InputFields() []string

	// Fingerprint 算这一格指纹。
	Fingerprint(in ProductInput) string
}

// TextEmbedding 是 §1 的 text_embedding：标题 / 副标题 / 类目 → 1024 维向量。
//
// 它只负责「输入是什么、指纹怎么算、送进模型的文本长什么样」。**真的去调引擎
// 不在这里** —— 那要批量（§6），而批量是调用方的事（internal/service/index.go
// 攒够一批再打一次 /v1/embeddings）。
type TextEmbedding struct{}

func (TextEmbedding) Name() string { return search.ProcessorTextEmbedding }

// InputFields：类目名在里面。
//
// 这一条不是可有可无的：送进模型的文本里有类目名（Content 下面就是），
// 所以「改类目名」必须让这一格指纹变。而它同时意味着触发点那条 SQL
// 必须看 categories.updated_at —— 那半边写在 db/queries/semantic.sql 上。
// 两边少任何一边，改一次类目名就让该类目下全部商品的向量永久过期而不报错。
func (TextEmbedding) InputFields() []string {
	return []string{FieldTitle, FieldSubtitle, FieldCategoryName}
}

func (p TextEmbedding) Fingerprint(in ProductInput) string {
	return p.text(in).EmbedFingerprint()
}

// Content 是真正送进 /v1/embeddings 的那段文本，也是写进
// product_text_vectors.content 的那一份。
func (p TextEmbedding) Content(in ProductInput) string {
	return p.text(in).EmbedContent()
}

func (TextEmbedding) text(in ProductInput) search.ProductText {
	return search.ProductText{
		Title:        in.Title,
		Subtitle:     in.Subtitle,
		CategoryName: in.CategoryName,
	}
}

// SearchText 是 bigram 关键词串那一格。
//
// 设计 §1 的能力清单里没有它 —— 那张表列的是 AI 能力，而 bigram 切分一个模型
// 都不用（语义检索层 §3：一期刻意不引入 pg_jieba / zhparser）。但它在
// input_hashes 里**真的占一格**，与别的 processor 一样要回答「输入变没变」，
// 所以它在这里也是一个 processor。不给它这个身份的话，它就是唯一一格
// 没有 InputFields 声明、因此不被 TestFingerprintDependsExactlyOnInputFields
// 检查的指纹 —— 而它恰好是最容易被误加输入的那一格（把类目名拼进去很顺手）。
type SearchText struct{}

func (SearchText) Name() string { return search.ProcessorSearchText }

// InputFields：**没有类目名**，与 text_embedding 的差别就在这一格。
//
// 类目名进来的话，同类目的商品会共享一批二元组，关键词路的区分度被摊平；
// 而且换类目就要重写全类目商品的 search_text。这条差异正是 §3
// 「按能力分别记 hash」那一节的例子本身，所以它值得有一条测试
// （TestCategoryChangeTouchesOnlyTheEmbeddingFingerprint）。
func (SearchText) InputFields() []string {
	return []string{FieldTitle, FieldSubtitle}
}

func (p SearchText) Fingerprint(in ProductInput) string {
	return p.text(in).SearchTextFingerprint()
}

// Text 是写进 products.search_text 的 bigram 串。
func (p SearchText) Text(in ProductInput) string {
	return p.text(in).SearchText()
}

func (SearchText) text(in ProductInput) search.ProductText {
	return search.ProductText{Title: in.Title, Subtitle: in.Subtitle}
}

// All 是本轮落地的全部 processor。
//
// 它存在只为一件事：让 processor_test.go 那条逐字段扰动的检查能**枚举**，
// 而不是逐个手写。新加一个 processor 却忘了往这里登记，它就不在任何检查的
// 视野里 —— 这与 internal/db/migrate_test.go 从 pg_class 枚举表是同一条纪律，
// 只是这里没有系统目录可枚举，只能靠这一行。
//
// 顺序无关紧要，但它同时是「今天真的落地了哪几个能力」的清单：
// §1 那张表有八个 processor，这里有三个。剩下五个的状态写在
// docs/电商系统-商品理解服务设计.md §十一。
func All() []Processor {
	return []Processor{TextEmbedding{}, SearchText{}, ComplianceCheck{}}
}

// Names 是 All() 的名字集合，供「input_hashes 里出现了一个没人认得的键」
// 这类检查使用。
func Names() []string {
	out := make([]string, 0, len(All()))
	for _, p := range All() {
		out = append(out, p.Name())
	}
	slices.Sort(out)
	return out
}
