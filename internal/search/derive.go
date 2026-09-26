package search

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// 商品的两份派生数据是怎么算出来的，以及「它们的输入变没变」怎么回答。
//
// 两个 processor，两份输入，**两格指纹**（数据模型 §8「增量重算的指纹：只认一处」）：
//
//	processor        输入                        落点
//	---------------  --------------------------  --------------------------------
//	text_embedding   标题 / 副标题 / 类目         product_text_vectors.embedding
//	search_text      标题 / 副标题               products.search_text
//
// 类目只进前者。这不是省事，是 00016 文件头第四节点名要的粒度：
// **换一次类目不该逼着 bigram 串重写**。两格指纹各记各的，就能做到；
// 只在向量表放一个 content_hash 做不到（§8 原话）。
//
// 而反过来 —— 只动 search_text 的输入而不动 embedding 的输入 —— 在当前模板下
// 不存在，因为 {标题, 副标题} ⊂ {标题, 副标题, 类目}。这是模板的性质，不是巧合，
// 所以测试断言的是「改类目 ⇒ 只有 text_embedding 那一格变」这一个方向。

// 两个 processor 在 product_understanding.input_hashes 里的键名。
//
// 字面量只在这里出现一次。散在 SQL、服务、测试三处的话，
// 打错一个字母的症状是 `->>` 返回 NULL —— 也就是「从没算过」，
// 于是每一轮都重算一遍，钱照花，而没有任何东西报错。
const (
	ProcessorTextEmbedding = "text_embedding"
	ProcessorSearchText    = "search_text"
)

// TemplateVersion 是**拼接模板本身**的版本，它进指纹。
//
// 为什么模板要进指纹：指纹回答的是「要不要重算」，而改模板（比如标题不再重复
// 一次、类目改成拼全路径）会让同一件商品算出不同的向量 —— 输入的**字面**没变，
// 该重算的事实变了。不把模板版本编进去的话，改完模板全库一条也不会重算，
// 而且不会有任何东西报错：检索照常返回结果，只是新老两套向量混在同一个空间里。
//
// 改 EmbedContent / SearchText 的输出形状时**必须**同时改这里。
const TemplateVersion = "text-v1"

// PipelineVersion 落进 product_understanding.pipeline_version。
//
// 它与 TemplateVersion 不是一回事：那个是「文本怎么拼」，这个是「这一轮加工
// 由哪套流水线做的」——本轮的流水线只有两个 processor（文本向量 + bigram 串），
// 图像向量与属性抽取还没有实现，所以状态只写到 1（部分完成），不写 2。
const PipelineVersion = "m3-text-v1"

// ProductText 是派生数据的**全部**输入。
//
// 它刻意只有三个字段。价格、库存、销量进不来不是因为忘了，是因为
// 语义检索层 §2.1 那张「必须排除的字段」表：把高频变动字段放进 embedding，
// 会让指纹天天变、全量天天重算，而重算 embedding 的钱是真花出去的。
// 用一个只有三个字段的结构体承载输入，等于让「不小心把 sales_count 拼进去」
// 这件事需要先改类型定义 —— 那是一个会被评审看见的动作。
//
// description 同样不在：§2.1 的排除表里写着「详情富文本 HTML：噪声极大、
// 长度爆炸」。品牌 / 规格 / 适用 / 卖点四行模板本轮**留空而不是编造** ——
// 这个 schema 里 products.brand_id 没有对应的品牌表，规格散在 skus.spec_values
// 的 JSONB 里、一件商品几十个 SKU 拼起来就是一堆噪声，卖点根本没有列。
// 硬拼出来的话，指纹会被一堆空字符串撑住，看上去很完整而实际没有信息。
type ProductText struct {
	Title        string
	Subtitle     string
	CategoryName string
}

// EmbedContent 是真正送进 /v1/embed 的那段文本，也是写进
// product_text_vectors.content 的那一份（留作调试与将来精排的输入）。
//
// 形状照语义检索层 §2.1 的模板，标题重复一次以提升它的权重。
func (p ProductText) EmbedContent() string {
	var b strings.Builder
	t := strings.TrimSpace(p.Title)
	b.WriteString(t)
	b.WriteByte('\n')
	b.WriteString(t) // §2.1：标题重复一次，提升标题权重
	if s := strings.TrimSpace(p.Subtitle); s != "" {
		b.WriteString("\n副标题：")
		b.WriteString(s)
	}
	if c := strings.TrimSpace(p.CategoryName); c != "" {
		b.WriteString("\n类目：")
		b.WriteString(c)
	}
	return b.String()
}

// SearchText 是写进 products.search_text 的 bigram 串。
//
// 输入只有标题与副标题 —— 类目名进来的话，同类目的商品会共享一批二元组，
// 关键词路的区分度被摊平；而且换类目就要重写全类目商品的 search_text。
//
// 标题与副标题之间用空格隔开，二元组不许跨过这个边界：拼成一整串的话
// 「连衣裙」+「夏季新款」会切出一个「裙夏」，那是一个不存在的词。
func (p ProductText) SearchText() string {
	return Bigram(strings.TrimSpace(p.Title) + " " + strings.TrimSpace(p.Subtitle))
}

// EmbedFingerprint 是 text_embedding 这一格的指纹。
func (p ProductText) EmbedFingerprint() string {
	return fingerprint(ProcessorTextEmbedding, p.EmbedContent())
}

// SearchTextFingerprint 是 search_text 这一格的指纹。
//
// 它按**原始输入**算，不按 Bigram 的输出算。两者在「输入没变则指纹不变」上
// 等价，但按输出算会让「切分算法改了」这件事悄悄溜过去 —— 输出变了、指纹跟着变、
// 于是会重算，听起来没问题；可切分算法一改，**没被触发点扫到的商品**
// 永远不会被重新判定，库里从此新老两套切分混着。按输入算 + TemplateVersion
// 进指纹，改算法时只要顺手改一次版本号，全库就都会被判成过期。
func (p ProductText) SearchTextFingerprint() string {
	return fingerprint(ProcessorSearchText,
		strings.TrimSpace(p.Title)+"\x00"+strings.TrimSpace(p.Subtitle))
}

// fingerprint 把 processor 名、模板版本与输入一起哈希。
//
// processor 名也进哈希：两个 processor 的输入恰好相同时（眼下不会，
// 将来模板一改就可能），它们的指纹不该是同一个值 —— 否则把一格的指纹
// 抄进另一格也看不出来。
//
// 分隔符用 \x00 而不是空格或换行：文本里出现空格是常态，
// ("ab", "c") 与 ("a", "bc") 拼出来一样，那是一次真实的指纹碰撞。
func fingerprint(processor, input string) string {
	h := sha256.New()
	h.Write([]byte(processor))
	h.Write([]byte{0})
	h.Write([]byte(TemplateVersion))
	h.Write([]byte{0})
	h.Write([]byte(input))
	return hex.EncodeToString(h.Sum(nil))
}
