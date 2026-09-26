package search_test

import (
	"strings"
	"testing"

	"github.com/keel/keel/internal/search"
)

// 两格指纹**互相独立**：换类目只动 text_embedding，不动 search_text。
//
// 这是 00016 文件头第四节点名要的粒度，也是「为什么不在向量表放一个
// content_hash」的全部理由（数据模型 §8）。做错了不会报错：两格指纹绑在一起时，
// 一次类目调整会把全类目商品的 bigram 串重写一遍 —— 结果完全正确，
// 只是白写，而且 products.updated_at 会跟着前进，把下一轮的候选集撑满。
func TestFingerprintsPerProcessorAreIndependent(t *testing.T) {
	base := search.ProductText{Title: "红色连衣裙", Subtitle: "夏季新款", CategoryName: "女装"}
	cat := base
	cat.CategoryName = "裙装" // 只换类目

	if base.EmbedFingerprint() == cat.EmbedFingerprint() {
		t.Error("换了类目而 text_embedding 的指纹没变 —— 类目在 embedding 的输入里" +
			"（§2.1 的模板有「类目：」这一行），指纹必须跟着变，否则向量永远停在旧类目上")
	}
	if base.SearchTextFingerprint() != cat.SearchTextFingerprint() {
		t.Error("换了类目而 search_text 的指纹跟着变了 —— bigram 串的输入只有" +
			"标题与副标题，换一次类目不该逼着全类目商品重写 search_text")
	}
	// 阳性对照：改标题时**两格都要变**。没有它，上面第二条断言在
	// 「SearchTextFingerprint 返回一个常量」时照样绿。
	title := base
	title.Title = "蓝色长裙"
	if title.SearchTextFingerprint() == base.SearchTextFingerprint() {
		t.Fatal("改了标题而 search_text 的指纹没变 —— 那它根本没在看输入")
	}
	if title.EmbedFingerprint() == base.EmbedFingerprint() {
		t.Fatal("改了标题而 text_embedding 的指纹没变")
	}
	// 两格的值本身也不许相等：输入恰好相同时（将来模板一改就可能），
	// 把一格的指纹抄进另一格要看得出来。
	if base.EmbedFingerprint() == base.SearchTextFingerprint() {
		t.Error("两个 processor 的指纹是同一个值 —— processor 名没进哈希")
	}
}

// 送进 embedding 的文本里**没有**价格、库存、销量、详情。
//
// 语义检索层 §2.1 那张「必须排除的字段」表：把高频变动字段放进 embedding，
// 指纹天天变、全量天天重算，而重算 embedding 的钱是真花出去的。
//
// 这条断言的形状刻意是「ProductText 只有三个字段」而不是「拼出来的字符串里
// 没有数字」—— 后者是一条永远绿的断言（标题里本来就可能有数字）。
// 真正挡住这件事的是类型：想把 sales_count 拼进去，得先改结构体定义。
func TestEmbedContentCarriesOnlyTheStableFields(t *testing.T) {
	p := search.ProductText{Title: "红色连衣裙", Subtitle: "夏季新款", CategoryName: "女装"}
	got := p.EmbedContent()

	// §2.1 的模板：标题重复一次，提升标题权重。
	if n := strings.Count(got, "红色连衣裙"); n != 2 {
		t.Errorf("拼接文本里标题出现 %d 次，期望 2 次（§2.1：标题重复一次提升权重）：\n%s", n, got)
	}
	for _, want := range []string{"副标题：夏季新款", "类目：女装"} {
		if !strings.Contains(got, want) {
			t.Errorf("拼接文本里缺 %q：\n%s", want, got)
		}
	}

	// 空的副标题 / 类目不许留下一行空模板：那会让两件商品的指纹
	// 因为一行「副标题：」而永远不同于它们本该相同的样子，
	// 也会给模型喂一串没有信息的噪声。
	bare := search.ProductText{Title: "T恤"}
	if strings.Contains(bare.EmbedContent(), "副标题") ||
		strings.Contains(bare.EmbedContent(), "类目") {
		t.Errorf("副标题与类目为空时不该留下模板行：\n%s", bare.EmbedContent())
	}
}

// search_text 是 bigram 串，而且标题与副标题之间不许跨界切。
func TestSearchTextIsBigramAndDoesNotCrossTitleBoundary(t *testing.T) {
	p := search.ProductText{Title: "连衣裙", Subtitle: "夏季新款", CategoryName: "女装"}
	got := p.SearchText()
	if got != "连衣 衣裙 夏季 季新 新款" {
		t.Fatalf("SearchText() = %q", got)
	}
	// 类目不许进去：进去之后同类目的商品共享一批二元组，关键词路的区分度被摊平。
	if strings.Contains(got, "女装") {
		t.Error("类目名进了 search_text")
	}
}

// 指纹是**钉死的值**，改模板就会红。
//
// 这条测试不是在验哈希算法对不对（那是 crypto/sha256 的事），它守的是
// 「改了拼接模板一定有人看见」这件事。指纹的输入里编着 TemplateVersion，
// 而 TemplateVersion 的用途是让改模板之后全库被判成过期 ——
// 忘了改它的后果是：库里的向量全部按老模板算，新写进去的按新模板算，
// 两套混在同一个向量空间里，检索照常返回结果，没有任何东西报错。
//
// 所以红的时候正确的动作是：确认你**同时**改了 TemplateVersion，
// 然后把下面两个值更新掉。只改这两个值而不改版本号，就是上面那个坏结局。
func TestFingerprintsAreNailedDown(t *testing.T) {
	p := search.ProductText{Title: "红色连衣裙", Subtitle: "夏季新款", CategoryName: "女装"}
	const (
		wantEmbed  = "dc3dcea1bedd6ca4a5a9c28095ac7d0011729715b5e546c565a494503bee11ce"
		wantSearch = "9af5373e5e0045f653d30f988a86a6e187c5f15745eab0f1a293bd2921cfa20e"
	)
	if got := p.EmbedFingerprint(); got != wantEmbed {
		t.Errorf("text_embedding 指纹变了：%s（原 %s）—— "+
			"拼接模板或 TemplateVersion 改了。两者要一起改，理由见本测试的注释",
			got, wantEmbed)
	}
	if got := p.SearchTextFingerprint(); got != wantSearch {
		t.Errorf("search_text 指纹变了：%s（原 %s）", got, wantSearch)
	}
	if search.TemplateVersion == "" {
		t.Error("TemplateVersion 是空的 —— 它进指纹，空着等于没有版本这件事")
	}
}

// 指纹对输入的每一段都敏感，而且分隔符不许被输入里的空格伪造。
//
// ("ab","c") 与 ("a","bc") 如果拼出同一个串，那是一次真实的指纹碰撞：
// 两件不同的商品被判成「输入没变」，其中一件的向量永远停在另一件的文本上。
func TestFingerprintHasNoSeparatorCollision(t *testing.T) {
	a := search.ProductText{Title: "ab", Subtitle: "c"}
	b := search.ProductText{Title: "a", Subtitle: "bc"}
	if a.SearchTextFingerprint() == b.SearchTextFingerprint() {
		t.Fatal("(\"ab\",\"c\") 与 (\"a\",\"bc\") 的 search_text 指纹相同 —— 分隔符被输入伪造了")
	}
	if a.EmbedFingerprint() == b.EmbedFingerprint() {
		t.Fatal("(\"ab\",\"c\") 与 (\"a\",\"bc\") 的 text_embedding 指纹相同")
	}
}
