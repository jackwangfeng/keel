package search_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/keel/keel/internal/search"
)

// RRF（语义检索层 §4）与查询侧切分的闸门。纯函数，不碰数据库。

// 两路都命中的那条必须排在只有一路命中的前面 —— 这是 RRF 的全部意义。
//
// 判据用**具体的分数**而不是只看顺序：把「只对命中的那一路累加」改成
// 「两路无条件都按 1/(k+rank) 累加」（rank 为 0 时就是 1/60），
// 顺序在这组数据上不一定变，而分数一定变。上一轮学到的那一课：
// 少了「判定真的发生了」那半句，把整段注释掉测试照样绿。
func TestRRFPrefersDocumentsFoundByBothRoutes(t *testing.T) {
	// 向量路：10, 20, 30；关键词路：30, 40, 10。
	// 30 在两路里分别是第 3、第 1 名；10 是第 1、第 3 名 —— 两者分数相同，
	// 靠 id 升序分先后。20 与 40 各只有一路。
	got := search.FuseRRF([]int64{10, 20, 30}, []int64{30, 40, 10})

	wantOrder := []int64{10, 30, 20, 40}
	var gotOrder []int64
	for _, f := range got {
		gotOrder = append(gotOrder, f.ID)
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("融合顺序 %v，期望 %v", gotOrder, wantOrder)
	}

	const k = search.RRFK
	want := map[int64]float64{
		10: 1.0/(k+1) + 1.0/(k+3),
		30: 1.0/(k+3) + 1.0/(k+1),
		20: 1.0 / (k + 2),
		40: 1.0 / (k + 2),
	}
	for _, f := range got {
		if f.Score != want[f.ID] {
			t.Errorf("id=%d 的 RRF 分是 %.10f，期望 %.10f（k=%d，名次 vector=%d keyword=%d）",
				f.ID, f.Score, want[f.ID], k, f.VectorRank, f.KeywordRank)
		}
	}

	// 名次与 recall_source。
	for _, c := range []struct {
		id      int64
		vec, kw int
		source  search.RecallSource
	}{
		{10, 1, 3, search.SourceBoth},
		{30, 3, 1, search.SourceBoth},
		{20, 2, 0, search.SourceVector},
		{40, 0, 2, search.SourceKeyword},
	} {
		for _, f := range got {
			if f.ID != c.id {
				continue
			}
			if f.VectorRank != c.vec || f.KeywordRank != c.kw {
				t.Errorf("id=%d 名次 (%d,%d)，期望 (%d,%d)",
					c.id, f.VectorRank, f.KeywordRank, c.vec, c.kw)
			}
			if f.Source() != c.source {
				t.Errorf("id=%d recall_source=%s，期望 %s", c.id, f.Source(), c.source)
			}
		}
	}
}

// 只有一路有结果时，融合退化成那一路的原顺序。
//
// 这是降级链在纯逻辑这一侧的样子：引擎挂了 ⇒ 向量路是空列表 ⇒
// 结果必须还是关键词那一路，而且**顺序不变**。
func TestRRFWithOneEmptyRouteKeepsTheOtherOrder(t *testing.T) {
	kw := []int64{7, 8, 9}
	got := search.FuseRRF(nil, kw)
	if len(got) != len(kw) {
		t.Fatalf("融合出 %d 条，期望 %d 条", len(got), len(kw))
	}
	for i, f := range got {
		if f.ID != kw[i] {
			t.Fatalf("第 %d 条是 %d，期望 %d —— 单路时顺序被改了", i, f.ID, kw[i])
		}
		if f.Source() != search.SourceKeyword {
			t.Errorf("id=%d 的 recall_source 是 %s，期望 keyword", f.ID, f.Source())
		}
	}
	if len(search.FuseRRF(nil, nil)) != 0 {
		t.Error("两路都空时应该融合出 0 条")
	}
}

// 分数相同时必须有确定的次序。
//
// 两路名次完全对称的两条商品分数一模一样，而 sort.Slice 不稳定 ——
// 少了 id 这个次级键，同一个请求发两次可以拿到不同的顺序，
// §9.1 的离线评测集会从第一天起就测不出东西。
func TestRRFIsDeterministicOnTies(t *testing.T) {
	// 5 与 6 都只被关键词路捞到，且都是第 1、第 2 名 —— 换个输入顺序试试。
	a := search.FuseRRF([]int64{5, 6}, []int64{6, 5})
	b := search.FuseRRF([]int64{5, 6}, []int64{6, 5})
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("同样的输入融合出两种结果：%v / %v", a, b)
	}
	// 5 与 6 的分数必然相同（名次 (1,2) 与 (2,1)），所以顺序只能由 id 定。
	if a[0].Score != a[1].Score {
		t.Fatalf("这组数据本该同分：%v", a)
	}
	if a[0].ID != 5 {
		t.Fatalf("同分时第一条是 %d，期望 id 小的 5", a[0].ID)
	}
}

// 查询侧切分与索引侧是同一份。
//
// 判据是**中间那两个字**：「衣裙」既不是「雪纺碎花连衣裙」的前缀也不是后缀，
// 只有二元组切分能让它们对上。把 TSQueryOr 改成「按空格切」，
// 下面那个 strings.Contains 就不成立了。
func TestQuerySideSplitsTheSameWayAsIndexSide(t *testing.T) {
	indexed := search.ProductText{Title: "雪纺碎花连衣裙", Subtitle: "夏季新款"}.SearchText()
	q := search.TSQueryOr("衣裙")
	if q != "衣裙" {
		t.Fatalf("TSQueryOr(\"衣裙\") = %q，期望 %q", q, "衣裙")
	}
	if !strings.Contains(" "+indexed+" ", " 衣裙 ") {
		t.Fatalf("索引侧的 search_text %q 里没有 %q —— 两侧切得不一样了", indexed, q)
	}

	// 多字查询拼成 OR。
	if got, want := search.TSQueryOr("红色连衣裙"), "红色 | 色连 | 连衣 | 衣裙"; got != want {
		t.Fatalf("TSQueryOr(\"红色连衣裙\") = %q，期望 %q", got, want)
	}
	// 英文与数字不做 bigram（§3）。
	if got, want := search.TSQueryOr("iPhone 15"), "iphone | 15"; got != want {
		t.Fatalf("TSQueryOr(\"iPhone 15\") = %q，期望 %q", got, want)
	}
}

// 切不出词时返回空串，而不是一条永不命中的假 tsquery。
func TestTSQueryOrIsEmptyWhenNothingIsSearchable(t *testing.T) {
	for _, s := range []string{"", "   ", "!!!", "，。；：", "\t\n"} {
		if got := search.TSQueryOr(s); got != "" {
			t.Errorf("TSQueryOr(%q) = %q，期望空串", s, got)
		}
		if got, n := search.TSQueryAnd(s); got != "" || n != 0 {
			t.Errorf("TSQueryAnd(%q) = (%q, %d)，期望 (\"\", 0)", s, got, n)
		}
	}
}

// AND 版：去重后用 & 拼，词数是去重后的。单个词（含「哈哈哈」这种切出重复二元组的）
// 词数为 1，调用方据此只跑 OR 那一条。元字符一个都不放进来（与 OR 版同一条性质）。
func TestTSQueryAnd(t *testing.T) {
	cases := []struct {
		in   string
		want string
		n    int
	}{
		{"红色连衣裙", "红色 & 色连 & 连衣 & 衣裙", 4},
		{"iPhone 15", "iphone & 15", 2},
		{"衣裙", "衣裙", 1},
		{"哈哈哈", "哈哈", 1},
		{"a&b|c!(d):*'e<->f", "a & b & c & d & e & f", 6},
	}
	for _, c := range cases {
		got, n := search.TSQueryAnd(c.in)
		if got != c.want || n != c.n {
			t.Errorf("TSQueryAnd(%q) = (%q, %d)，期望 (%q, %d)", c.in, got, n, c.want, c.n)
		}
	}
}

// Bigram 的输出里不可能出现 tsquery 的元字符。
//
// db/queries/search.sql 把这串东西直接交给 to_tsquery，而它是一个**会解析**
// 的语法 —— `&` `|` `!` `(` `)` `:` `*` `'` `<->` 在里面都有含义。
// 这条性质是那句「没有注入面」的依据，所以它必须有一条测试，
// 而不是一句注释：Bigram 哪天放宽了字符集（比如把 '-' 或 ':' 算进 token），
// 一个搜索词就能构造出一条语法错误的 tsquery（500），
// 甚至改变查询的逻辑结构。
func TestBigramOutputCannotCarryTSQueryMetacharacters(t *testing.T) {
	const meta = `&|!():*'<>\"`
	inputs := []string{
		`连衣裙 & 咖啡`,
		`a:*`,
		`!(红色 | 蓝色)`,
		`'; DROP TABLE products; --`,
		`衣裙 <-> 咖啡`,
		`\u0000 \\ /`,
		strings.Repeat("裙&", 50),
	}
	for _, in := range inputs {
		got := search.TSQueryOr(in)
		// 输出里只允许出现 Bigram 的 token、空格，以及本函数自己加的 '|'。
		stripped := strings.ReplaceAll(got, " | ", " ")
		if strings.ContainsAny(stripped, meta) {
			t.Errorf("TSQueryOr(%q) = %q，里面带着 tsquery 元字符", in, got)
		}
	}

	// 阳性对照：这些输入真的切出了东西。全部切成空串的话，
	// 上面那个循环等于什么也没查。
	if search.TSQueryOr(`连衣裙 & 咖啡`) == "" {
		t.Fatal("带元字符的那条查询被切成了空串 —— 上面那些断言没有对象")
	}
}
