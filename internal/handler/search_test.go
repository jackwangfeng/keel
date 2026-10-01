package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/handler"
	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/search"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// POST /search 这条链路的行为闸门。
//
// 夹具与那个**语义可控**的引擎替身在 search_fixture_test.go，
// 为什么替身必须可控（而不是「确定性就够」）写在那个文件的头上。

// searchResp 是契约里 /search 的 200 响应。
//
// 用 map 收 items 而不是直接解成 api.SearchHit，是因为有两条断言要看
// **某个键在不在**（scores.rerank / scores.business / trace_id），
// 而解成结构体之后「字段缺席」和「字段是零值」就分不开了 ——
// 那正是这两条断言要区分的东西。
type searchResp struct {
	Items     []map[string]any `json:"items"`
	LatencyMs int              `json:"latency_ms"`
	Total     int              `json:"total"`
	Strategy  string           `json:"strategy"`
	raw       map[string]any
}

func searchOn(t *testing.T, engine http.Handler, host string, body string) (*httptest.ResponseRecorder, searchResp) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/search", strings.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	var out searchResp
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("响应不是预期结构: %v\n%s", err, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out.raw); err != nil {
			t.Fatal(err)
		}
	}
	return w, out
}

func doSearch(t *testing.T, host, body string) (*httptest.ResponseRecorder, searchResp) {
	t.Helper()
	return searchOn(t, testEngine, host, body)
}

func titlesOf(r searchResp) []string {
	out := make([]string, 0, len(r.Items))
	for _, it := range r.Items {
		out = append(out, it["title"].(string))
	}
	return out
}

func hitByTitle(r searchResp, title string) map[string]any {
	for _, it := range r.Items {
		if it["title"] == title {
			return it
		}
	}
	return nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// 双路召回真的各自捞到了东西，而且捞到的不是同一批。
//
// 判据分三段，各挡一类失效：
//
//	① 搜「连衣裙」能找到「雪纺碎花连衣裙」—— 最基本的那条；
//	② 也能找到「真丝吊带长裙」，而它与查询词**一个二元组都不共享**
//	   （「连衣裙」切出「连衣 衣裙」，「真丝吊带长裙」切出「真丝 丝吊 吊带 带长 长裙」），
//	   所以它只可能是向量那一路捞回来的 —— 这一条是「向量召回真的在跑」
//	   的唯一证据。把 recallByVector 整个删掉，红的就是它；
//	③ 两件裙子都**排在**「手冲咖啡壶」前面。
//
// 第 ③ 条为什么是「排在前面」而不是「不在结果里」：**召回层没有相似度阈值，
// 这是刻意的**。ANN 返回的是最近的 N 条，不是「足够近的那些」；而定一个
// 「余弦距离小于多少才算相关」的阈值需要 §9.1 的离线评测集，那个东西还不存在，
// 现在拍一个数就是 §9 开头点名的「盲调」。管相关度的是 Reranker 精排，本轮没有；
// M5 接上的业务重排只管「该不该卖」，不替召回层挡不相关的东西。
//
// 代价在小店上最刺眼：一家只有 4 件商品的店，搜什么都会把 4 件全返回，
// 只是顺序不同。这条测试把它写成断言而不是假装看不见。
func TestHybridRecallFindsBothLexicalAndSemanticMatches(t *testing.T) {
	fx := newSearchFixture(t)

	w, body := doSearch(t, fx.HostA, `{"query":"连衣裙","explain":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("检索返回 %d：%s", w.Code, w.Body.String())
	}
	titles := titlesOf(body)

	if !contains(titles, fxDress.Title) {
		t.Fatalf("搜「连衣裙」没找到 %q —— 两路召回都没捞到它。结果：%v", fxDress.Title, titles)
	}
	if !contains(titles, fxSkirt.Title) {
		t.Fatalf("搜「连衣裙」没找到 %q。它与查询词一个二元组都不共享"+
			"（查询切出 %q，它的 search_text 是 %q），关键词那一路本来就捞不到它 —— "+
			"捞不到它就说明**向量召回这一路没有在跑**。结果：%v",
			fxSkirt.Title, search.TSQueryOr("连衣裙"),
			search.ProductText{Title: fxSkirt.Title, Subtitle: fxSkirt.Subtitle}.SearchText(),
			titles)
	}
	rank := map[string]int{}
	for i, x := range titles {
		rank[x] = i
	}
	if c, ok := rank[fxCoffee.Title]; ok {
		if rank[fxDress.Title] > c || rank[fxSkirt.Title] > c {
			t.Fatalf("搜「连衣裙」时 %q 排在裙子前面 —— 排序没有任何区分力。结果：%v",
				fxCoffee.Title, titles)
		}
		if got := hitByTitle(body, fxCoffee.Title)["recall_source"]; got != string(search.SourceVector) {
			t.Errorf("%q 的 recall_source 是 %v，期望 vector —— "+
				"它与「连衣裙」没有共享二元组，只可能是无阈值的 ANN 召回捞回来的", fxCoffee.Title, got)
		}
	}

	// recall_source 要真的分得开。全填 both（或者全填一个值）的实现
	// 能让上面三条全绿，而契约里这个字段存在的理由正是
	// 「某类查询长期只由单路命中，说明另一路在这类查询上失效了」。
	dress := hitByTitle(body, fxDress.Title)
	skirt := hitByTitle(body, fxSkirt.Title)
	if got := dress["recall_source"]; got != string(search.SourceBoth) {
		t.Errorf("%q 的 recall_source 是 %v，期望 both —— "+
			"它字面命中（search_text 里有「连衣」「衣裙」）也语义命中", fxDress.Title, got)
	}
	if got := skirt["recall_source"]; got != string(search.SourceVector) {
		t.Errorf("%q 的 recall_source 是 %v，期望 vector —— "+
			"它与查询词没有共享的二元组，关键词那一路不可能捞到它", fxSkirt.Title, got)
	}

	t.Logf("搜「连衣裙」→ %v；tsquery = %q", titles, search.TSQueryOr("连衣裙"))
}

// 关键词那一路真的走的是 bigram，而不是整句匹配。
//
// 「衣裙」是「雪纺碎花连衣裙」中间的两个字，它既不是前缀也不是完整的词。
// `to_tsvector('simple', title)` 会把整个标题当成一个 token，搜「衣裙」零结果
// —— 那正是语义检索层 §3 开头说的「完全不可用」。
//
// 这一条同时守住**查询侧与索引侧用的是同一个切分**：把 TSQueryOr 改成
// 「按空格切」（也就是查询侧另写一份），「衣裙」会变成一个整词 token，
// 而索引侧的 search_text 里只有二元组，匹配不上 —— 红的就是它。
//
// 为了让它只由关键词那一路负责，请求里把向量那一路的贡献排除掉：
// 断言的是 recall_source 含 keyword，而不只是「找到了」。
func TestKeywordRecallMatchesMidWordBigram(t *testing.T) {
	fx := newSearchFixture(t)

	w, body := doSearch(t, fx.HostA, `{"query":"衣裙","explain":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("检索返回 %d：%s", w.Code, w.Body.String())
	}
	hit := hitByTitle(body, fxDress.Title)
	if hit == nil {
		t.Fatalf("搜「衣裙」没找到 %q。它是标题正中间的两个字 —— "+
			"搜不到说明关键词那一路不是按二元组匹配的（语义检索层 §3）。结果：%v",
			fxDress.Title, titlesOf(body))
	}
	src := fmt.Sprint(hit["recall_source"])
	if src != string(search.SourceKeyword) && src != string(search.SourceBoth) {
		t.Fatalf("%q 的 recall_source 是 %q，里面没有 keyword —— "+
			"那它就不是关键词那一路捞回来的，这条测试没在测 bigram", fxDress.Title, src)
	}
	t.Logf("「衣裙」→ tsquery %q，命中 %q（recall_source=%s）",
		search.TSQueryOr("衣裙"), fxDress.Title, src)
}

// 单字查询由关键词那一路召回（深度审查 2026-09-27：「杯」「咖」无结果）。
//
// 查询侧「裙」切出来就是「裙」；索引侧过去只有二元组（「长裙」），整词对不上，
// 这件商品只能靠向量那一路碰运气。索引侧追加单字之后（search.IndexTerms），
// 关键词那一路必须命中它 —— 断言的是 recall_source 含 keyword，不只是「找到了」：
// 无阈值的 ANN 什么都会返回，只断言找到会恒绿。
func TestKeywordRecallMatchesSingleCharacter(t *testing.T) {
	fx := newSearchFixture(t)

	w, body := doSearch(t, fx.HostA, `{"query":"裙","explain":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("检索返回 %d：%s", w.Code, w.Body.String())
	}
	for _, title := range []string{fxDress.Title, fxSkirt.Title} {
		hit := hitByTitle(body, title)
		if hit == nil {
			t.Fatalf("搜「裙」没找到 %q。结果：%v", title, titlesOf(body))
		}
		src := fmt.Sprint(hit["recall_source"])
		if src != string(search.SourceKeyword) && src != string(search.SourceBoth) {
			t.Errorf("%q 的 recall_source 是 %q，里面没有 keyword —— "+
				"索引侧没有单字，单字查询在关键词那一路对不上", title, src)
		}
	}
	if hit := hitByTitle(body, fxCoffee.Title); hit != nil {
		if src := fmt.Sprint(hit["recall_source"]); src != string(search.SourceVector) {
			t.Errorf("%q 标题里没有「裙」，recall_source 却是 %q", fxCoffee.Title, src)
		}
	}
}

// 跨租户：A 店搜不到 B 店的同名商品。
//
// 这是 M3 唯一的新读路径，而且它走的是**索引扫描**（HNSW / GIN），
// 与之前验过的顺序扫描不是同一条。
//
// 两家店里那件商品**标题完全一样**，所以「A 的结果里没有 B 的那条」
// 只可能是 RLS 挡的，不可能是「它本来就不匹配」。判据用 id 而不是标题：
// 标题一样，只有 id 分得开它们。
//
// 阳性对照必须有：先确认 B 店自己搜得到那件商品。少了它，RLS 失效以外
// 的任何原因（夹具没播进去、派生数据没写上、Host 解析错了）都会让
// 「A 搜不到 B 的」这句话为真 —— 那正是本仓库反复抓到的那类假绿。
func TestSearchDoesNotLeakAcrossTenants(t *testing.T) {
	fx := newSearchFixture(t)
	idA, idB := fx.IDsA[fxDress.Title], fx.IDsB[fxDress.Title]
	if idA == 0 || idB == 0 || idA == idB {
		t.Fatalf("夹具坏了：A=%d B=%d", idA, idB)
	}

	// 阳性对照：B 店自己搜得到。
	_, bodyB := doSearch(t, fx.HostB, `{"query":"连衣裙"}`)
	if idsOf(bodyB)[idB] != true {
		t.Fatalf("阳性对照不成立：B 店搜「连衣裙」没搜到自己那件 %q（id=%d）—— "+
			"下面那条「A 搜不到 B 的」因此证明不了任何事。B 的结果：%v",
			fxDress.Title, idB, bodyB.Items)
	}

	// 两种过滤条件各查一次，而且**两次都要**断言。
	//
	// 不是凑数：in_stock_only=true 那次带着一条对 skus / inventories 的
	// EXISTS —— 那两张表有它们自己的 RLS 策略。也就是说那一次即使 products 与
	// product_text_vectors 的策略双双失效，别家商品也会被 skus 的策略挡在
	// EXISTS 里（变异验证实测：把那两条策略都改成 `OR true`，只有带 EXISTS
	// 的那一次仍然是绿的）。不带 EXISTS 的那一次（默认，in_stock_only=false）
	// 让断言真的落在检索自己那两张表的策略上。
	for _, tc := range []struct{ name, body string }{
		{"默认（in_stock_only=false）", `{"query":"连衣裙"}`},
		{"in_stock_only=true", `{"query":"连衣裙","filters":{"in_stock_only":true}}`},
	} {
		_, bodyA := doSearch(t, fx.HostA, tc.body)
		got := idsOf(bodyA)
		if !got[idA] {
			t.Fatalf("[%s] A 店搜「连衣裙」没搜到自己那件（id=%d）", tc.name, idA)
		}
		if got[idB] {
			t.Fatalf("[%s] **A 店搜到了 B 店的商品 id=%d** —— "+
				"检索这条索引扫描路径上 RLS 没挡住。A 的结果 id：%v", tc.name, idB, got)
		}
	}
	t.Logf("A(id=%d) 与 B(id=%d) 同名商品，两种过滤条件下各自都只搜得到自己的", idA, idB)
}

func idsOf(r searchResp) map[int64]bool {
	out := map[int64]bool{}
	for _, it := range r.Items {
		out[int64(it["id"].(float64))] = true
	}
	return out
}

// 降级链：引擎**真的打不通**时，检索仍然返回结果，而且是关键词那一路的结果。
//
// §8 原话：「任何一环故障，搜索都必须仍能返回结果」。
//
// 这里用的是一个真的 inference.Client，指向一个**本机上真的没人监听的端口**
// （net.Listen 拿到一个端口再立刻关掉，所以它确定是空的），不是一个
// 「Embed 直接 return err」的替身。两者的差别是实打实的：替身跑不到
// 客户端的超时、连接错误、错误分类（ErrUnavailable / ErrProtocol）那一段，
// 而降级链要判断的恰恰是「这是哪一类错误」。
//
// 阳性对照同样必须有：同一个夹具、同一个查询，在**引擎好的**那套路由上
// 必须能捞到那件只有向量路才找得到的商品。少了它，「降级之后还有结果」
// 在一个向量路本来就没捞到任何东西的查询上永远为真。
func TestSearchDegradesToKeywordWhenEngineIsDown(t *testing.T) {
	fx := newSearchFixture(t)

	// 阳性对照：引擎好的时候，向量路捞得到「真丝吊带长裙」。
	_, healthy := doSearch(t, fx.HostA, `{"query":"连衣裙"}`)
	if !contains(titlesOf(healthy), fxSkirt.Title) {
		t.Fatalf("阳性对照不成立：引擎好的时候都没捞到 %q —— "+
			"下面那条「引擎挂了之后它消失了」证明不了向量路真的停了。结果：%v",
			fxSkirt.Title, titlesOf(healthy))
	}

	down := routerWithDeadEngine(t)
	w, body := searchOn(t, down, fx.HostA, `{"query":"连衣裙","explain":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("引擎挂了之后检索返回 %d，期望 200 —— "+
			"§8：任何一环故障，搜索都必须仍能返回结果。响应：%s", w.Code, w.Body.String())
	}
	titles := titlesOf(body)
	if len(titles) == 0 {
		t.Fatal("引擎挂了之后返回了空列表 —— 关键词那一路被一起拖下水了（§8 的降级链没生效）")
	}
	if !contains(titles, fxDress.Title) {
		t.Fatalf("引擎挂了之后没搜到 %q，而它是字面命中的 —— 关键词那一路也没跑。结果：%v",
			fxDress.Title, titles)
	}
	if contains(titles, fxSkirt.Title) {
		t.Fatalf("引擎挂了之后还搜到了 %q —— 它只有向量那一路捞得到，"+
			"这说明向量路并没有真的停，这条测试测的不是降级。结果：%v", fxSkirt.Title, titles)
	}
	for _, it := range body.Items {
		if got := it["recall_source"]; got != nil && got != string(search.SourceKeyword) {
			t.Errorf("降级之后有一条的 recall_source 是 %v，期望全是 keyword", got)
		}
	}
	t.Logf("引擎打不通时仍返回 %d 条，全部来自关键词路：%v", len(titles), titles)
}

// deadEngineClient 建一个指着「真的连不上的地址」的引擎客户端。
//
// 不用「Embed 直接 return err」的替身：替身跑不到客户端的超时、连接错误、
// 错误分类（ErrUnavailable / ErrProtocol）那一段，而降级链要判断的恰恰是
// 「这是哪一类错误」。
func deadEngineClient(t *testing.T) inference.Embedder {
	t.Helper()

	// 拿一个端口再立刻还回去：这样它在本机上确定是空的，
	// 而不是「挑一个大概没人用的数字」——后者会在某台机器上偶然撞上一个
	// 真的服务，那时这条测试测的就是别的东西了。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	client, err := inference.New(inference.Config{
		Endpoint: "http://" + addr,
		// 超时给小一点：这条测试等的是一次 connection refused，
		// 本机上它是立刻返回的；给 5 秒（客户端默认）只会让测试在
		// 出别的岔子时挂很久。
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// 降级这件事在系统里真的留下了痕迹：SearchResult.Degraded 与一条 WARN 日志。
//
// service/search.go 写着那条 WARN「**必须**出现在日志里 —— 否则『搜索质量
// 怎么突然变差了』这件事在任何地方都没有痕迹」，SearchResult.Degraded 的注释
// 写着「它的去处是日志与测试」。在这条测试之前，两句话都是假的：
// grep Degraded 只有定义、注释、赋值三处，没有任何 handler 或测试读它；
// 那条 WarnContext 也没有任何断言 —— 整段删掉，全绿。
//
// 两个方向都锁住，这是这条测试的形状，不是凑数：
//
//	· **引擎挂了 ⇒ 必须有**。删掉那条 WarnContext（或者把级别降成 Debug /
//	  Info）红的是下半段。
//	· **引擎好着 ⇒ 必须没有**。少了这一半，一条无条件打印的日志也能让上半段
//	  变绿，而那时「有 WARN」就不再说明任何事情了。上半段同时读 Degraded
//	  必须是 false —— 这个字段在两个方向上都被读到。
//
// 用真的连不上的地址而不是「Embed 直接 return err」的替身，理由见
// deadEngineClient：降级链要判断的是错误的**类别**，替身跑不到那一段。
func TestDegradationIsMarkedOnTheResultAndLeavesAWarnInTheLog(t *testing.T) {
	fx := newSearchFixture(t)
	ctx := tenant.NewContext(t.Context(), fx.MerchantA)
	req := service.SearchRequest{
		Query:   "连衣裙",
		Filters: service.SearchFilters{InStockOnly: true},
	}

	// 阴性对照：引擎好着。
	okRes, okLog := searchCapturingLog(t, conceptEmbedder{}, ctx, req)
	if okRes.Degraded {
		t.Fatalf("引擎好着却报了降级 —— 下半段那条「降级时有 WARN」因此"+
			"证明不了任何事。日志：%q", okLog)
	}
	if len(okRes.Items) == 0 {
		t.Fatal("引擎好着却一条都没召回，这条测试的前提不成立")
	}
	if strings.Contains(okLog, degradeLogMarker) {
		t.Fatalf("引擎好着的时候也打了那条降级 WARN —— 它是无条件打印的，"+
			"于是「降级时日志里有痕迹」这件事在任何情况下都为真，等于没有。日志：%q", okLog)
	}

	// 降级：引擎真的连不上。
	badRes, badLog := searchCapturingLog(t, deadEngineClient(t), ctx, req)
	if !badRes.Degraded {
		t.Fatalf("引擎连不上，SearchResult.Degraded 却是 false —— "+
			"「这一次是纯关键词结果」这件事上层读不出来。日志：%q", badLog)
	}
	if len(badRes.Items) == 0 {
		t.Fatal("引擎连不上之后返回了空列表 —— 关键词那一路被一起拖下水了" +
			"（语义检索层 §8 的降级链没生效）")
	}
	if !strings.Contains(badLog, "level=WARN") || !strings.Contains(badLog, degradeLogMarker) {
		t.Fatalf("向量召回整路失败，日志里却没有那条 WARN —— "+
			"「搜索质量怎么突然变差了」这件事在系统里没有任何痕迹，"+
			"而 service/search.go 那句「必须出现在日志里」就是假的。实际日志：%q", badLog)
	}
	t.Logf("降级时的日志：%s", strings.TrimSpace(badLog))
}

// degradeLogMarker 是那条降级 WARN 里一段稳定的话。
//
// 匹配一段话而不是只匹配 level=WARN：这条链路上别的地方也可能打 WARN，
// 那时「有 WARN」就不等于「降级被记下来了」。
const degradeLogMarker = "退化为纯关键词召回"

// searchCapturingLog 用给定的引擎跑一次检索，并把这期间的 slog 输出收下来。
//
// slog.Default() 必须在 **NewSearchService 之前**换掉：那个构造函数在 log 为
// nil 时把当时的 slog.Default() 存进结构体，之后再换默认 logger 就晚了 ——
// 而「晚了」的症状是一个永远抓不到日志的空缓冲区，看上去和「那条日志不存在」
// 一模一样。
func searchCapturingLog(t *testing.T, emb inference.Embedder, ctx context.Context,
	req service.SearchRequest) (service.SearchResult, string) {
	t.Helper()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	svc := service.NewSearchService(repository.New(testPool), localInventory(), emb, service.SearchConfig{}, nil)
	res, err := svc.Search(ctx, req)
	if err != nil {
		t.Fatalf("检索报错：%v", err)
	}
	return res, buf.String()
}

// routerWithDeadEngine 装一套接着「真的连不上的引擎」的路由。
func routerWithDeadEngine(t *testing.T) http.Handler {
	t.Helper()
	client := deadEngineClient(t)
	return app.Router(testPool,
		tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}),
		auth.NewSigner([]byte("keel-test-secret-key-32-bytes-long!!")),
		testOrders, service.PaymentConfig{Sandbox: true}, client)
}

// explain=true 时，scores 里**恰好**是这一次真正跑过的那几个阶段。
//
// 三种形态各验一次，每一种都同时断言「该有的在」与「不该有的不在」：
//
//	· 默认策略、引擎好着：vector / keyword / rrf / business / final，没有 rerank；
//	· strategy = rrf-v1：没有 business（那条流水线不做业务重排），final = rrf；
//	· 引擎挂了（降级）：没有 vector —— 向量路没跑成，相似度是零值，填进去
//	  就是「算过、分数是 0」。
//
// 这是 contract_test.go 那张 NotYetImplementedStage 挂账的反向锁：精排哪天
// 真的接上了、开始往 scores 里填 rerank，这条测试就红，逼人回来把挂账划掉。
// 它同时断言挂账**真的还挂在那儿**（少了这半句，把挂账删掉之后照样绿），
// 并且断言「业务重排」已经**不在**挂账里 —— 实现了还挂着，是另一种不诚实。
func TestExplainListsExactlyTheStagesThatRan(t *testing.T) {
	r := routeOf(t, http.MethodPost, "/search")
	if _, ok := r.NotYetImplementedStage["Reranker 精排"]; !ok {
		t.Fatal("contract_test.go 的 routes 表里 /search 没有挂「Reranker 精排」这笔账 —— " +
			"要么精排真的实现了（那这条测试该改），要么挂账被删了" +
			"（那「契约写四层、这里只跑三层」这件事就没人记着了）")
	}
	if why, ok := r.NotYetImplementedStage["业务重排"]; ok {
		t.Fatalf("「业务重排」还挂在 NotYetImplementedStage 上（%s），但它已经实现了", why)
	}

	fx := newSearchFixture(t)
	check := func(name string, engine http.Handler, body string, want, absent []string) []map[string]any {
		t.Helper()
		w, resp := searchOn(t, engine, fx.HostA, body)
		if w.Code != http.StatusOK {
			t.Fatalf("[%s] 检索返回 %d：%s", name, w.Code, w.Body.String())
		}
		if len(resp.Items) == 0 {
			t.Fatalf("[%s] 结果是空的，下面的断言没有对象", name)
		}
		for _, it := range resp.Items {
			scores, ok := it["scores"].(map[string]any)
			if !ok {
				t.Fatalf("[%s] explain=true 但 %v 没有 scores", name, it["title"])
			}
			for _, k := range want {
				if _, ok := scores[k]; !ok {
					t.Errorf("[%s] %v 的 scores 里没有 %q —— 跑过的阶段必须有分",
						name, it["title"], k)
				}
			}
			for _, k := range absent {
				if v, ok := scores[k]; ok {
					t.Errorf("[%s] %v 的 scores 里出现了 %q（值 %v）—— 这一次没跑这一层，"+
						"缺席才是实话，0 与「没算过」是两件事", name, it["title"], k, v)
				}
			}
		}
		return resp.Items
	}

	items := check("默认策略", testEngine, `{"query":"连衣裙","explain":true}`,
		[]string{"vector", "keyword", "rrf", "business", "final"}, []string{"rerank"})
	// final 真的是 rrf × business，business 真的是库存因子。
	sawSoldOut := false
	for _, it := range items {
		sc := it["scores"].(map[string]any)
		rrf, ok1 := sc["rrf"].(float64)
		biz, ok2 := sc["business"].(float64)
		fin, ok3 := sc["final"].(float64)
		if !ok1 || !ok2 || !ok3 {
			continue // 缺键上面已经报过了；这里只验数值
		}
		wantBiz := search.StockFactorInStock
		if it["in_stock"] == false {
			wantBiz = search.StockFactorOutOfStock
			sawSoldOut = true
		}
		if math.Abs(biz-wantBiz) > 1e-6 {
			t.Errorf("%v（in_stock=%v）的 business = %v，期望 %v", it["title"], it["in_stock"], biz, wantBiz)
		}
		if math.Abs(fin-rrf*biz) > 1e-6 {
			t.Errorf("%v 的 final = %v，期望 rrf × business = %v", it["title"], fin, rrf*biz)
		}
	}
	if !sawSoldOut {
		t.Error("结果里没有断货商品，business = 0.05 那一档没被验到")
	}

	for _, it := range check("strategy=rrf-v1", testEngine,
		`{"query":"连衣裙","explain":true,"strategy":"rrf-v1"}`,
		[]string{"vector", "keyword", "rrf", "final"}, []string{"rerank", "business"}) {
		sc := it["scores"].(map[string]any)
		if sc["final"] != sc["rrf"] {
			t.Errorf("rrf-v1 下 %v 的 final = %v，期望等于 rrf = %v", it["title"], sc["final"], sc["rrf"])
		}
	}

	check("引擎挂了", routerWithDeadEngine(t), `{"query":"连衣裙","explain":true}`,
		[]string{"keyword", "rrf", "business", "final"}, []string{"rerank", "vector"})

	// explain=false 时连 scores 都不该出现（契约：「explain=true 时返回」）。
	_, plain := doSearch(t, fx.HostA, `{"query":"连衣裙"}`)
	for _, it := range plain.Items {
		if _, ok := it["scores"]; ok {
			t.Errorf("explain 没开，%v 却带着 scores", it["title"])
		}
		if _, ok := it["recall_source"]; ok {
			t.Errorf("explain 没开，%v 却带着 recall_source", it["title"])
		}
	}
}

// 缺货商品默认**出现**在结果里，但排在所有有货商品之后（语义检索层 §6，
// §11 阶段 4 的验收标准「缺货商品不再出现在首屏」）。
//
// 阳性对照是同一个查询走 rrf-v1（不做业务重排）：那时断货的「亚麻直筒连衣裙」
// 两路都命中（字面有「连衣 衣裙」，语义也是裙子），必须排在至少一件有货商品
// 前面。少了这一半，「它排在最后」也可能只是它本来就相关度最低 ——
// 那样把业务重排整段删掉，这条测试照样绿。
func TestOutOfStockIsDemotedNotDropped(t *testing.T) {
	fx := newSearchFixture(t)

	posOf := func(titles []string, want string) int {
		for i, x := range titles {
			if x == want {
				return i
			}
		}
		return -1
	}

	_, plain := doSearch(t, fx.HostA, `{"query":"连衣裙","strategy":"rrf-v1"}`)
	pt := titlesOf(plain)
	sp := posOf(pt, fxSoldOut.Title)
	if sp < 0 {
		t.Fatalf("rrf-v1 下没搜到断货的 %q，对照不成立。结果：%v", fxSoldOut.Title, pt)
	}
	if sp == len(pt)-1 {
		t.Fatalf("rrf-v1 下断货的 %q 本来就排在最后（%v）—— 下面那条「业务重排把它压到最后」"+
			"因此证明不了任何事，夹具需要一件相关度低于它的有货商品", fxSoldOut.Title, pt)
	}

	_, body := doSearch(t, fx.HostA, `{"query":"连衣裙"}`)
	if body.Strategy != service.DefaultStrategy {
		t.Fatalf("默认策略回显 %q，期望 %q", body.Strategy, service.DefaultStrategy)
	}
	titles := titlesOf(body)
	so := posOf(titles, fxSoldOut.Title)
	if so < 0 {
		t.Fatalf("默认检索没搜到断货的 %q —— 业务重排是降权，不是删除"+
			"（删除是 in_stock_only=true 的事）。结果：%v", fxSoldOut.Title, titles)
	}
	for i, it := range body.Items {
		if it["in_stock"] == true && i > so {
			t.Errorf("有货的 %v 排在断货的 %q 后面（结果：%v）—— 业务重排没有把缺货压下去",
				it["title"], fxSoldOut.Title, titles)
		}
	}
	t.Logf("rrf-v1：%v；默认（业务重排）：%v", pt, titles)
}

// strategy 回显的是**真的跑过的**那条流水线，不是回显请求里那个字符串。
//
// 语义检索层 §9.3：strategy_id 写进检索日志、按策略分组对比线上指标。
// 回显请求值的话，精排上线前后两个月的 "default" 指的是两条不同的流水线，
// 而那份对比表会把它们当成同一桶。
func TestStrategyEchoesThePipelineThatActuallyRan(t *testing.T) {
	fx := newSearchFixture(t)
	for _, body := range []string{
		`{"query":"连衣裙"}`,
		`{"query":"连衣裙","strategy":"default"}`,
		`{"query":"连衣裙","strategy":"rrf-biz-v1"}`,
		`{"query":"连衣裙","strategy":"with-reranker-someday"}`,
	} {
		w, got := doSearch(t, fx.HostA, body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s → %d：%s", body, w.Code, w.Body.String())
		}
		if got.Strategy != service.DefaultStrategy {
			t.Errorf("%s → strategy=%q，期望 %q（请求值不是回显值："+
				"回的是真的跑过的那条流水线）", body, got.Strategy, service.DefaultStrategy)
		}
	}
	// 唯一被认出来的另一条：rrf-v1，M3 那条不做业务重排的流水线。
	// 它必须回显它自己 —— 业务重排上线之后，这个名字仍然指同一条流水线。
	if w, got := doSearch(t, fx.HostA, `{"query":"连衣裙","strategy":"rrf-v1"}`); w.Code != http.StatusOK ||
		got.Strategy != service.StrategyRRFOnly {
		t.Errorf("strategy=rrf-v1 → %d / %q，期望 200 / %q", w.Code, got.Strategy, service.StrategyRRFOnly)
	}
	// 一个没人认得的策略不该被拒 —— 契约里它是可选参数，
	// 客户端从回显里就看得出自己没落到想要的那一桶。
	w, _ := doSearch(t, fx.HostA, `{"query":"连衣裙","strategy":"with-reranker-someday"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("不认得的 strategy 返回了 %d，期望 200", w.Code)
	}
}

// filters 真的在筛，而且 in_stock_only 默认是 false。
//
// 默认值那一条单独断言：契约把 SearchFilters.in_stock_only 的 default 定成
// false（M3 独立验收 I10），也就是**不传 filters 时断货商品照常出现**，
// 由业务重排压到有货商品之后（TestOutOfStockIsDemotedNotDropped 盯排序那一半）。
// handler 此前一直按 true 在跑，与契约相反，而这条测试当时断言的也是 true ——
// 测试照着实现写，于是两者一起偏离了契约，谁也没红。
func TestSearchFiltersApply(t *testing.T) {
	fx := newSearchFixture(t)

	// 一个字都不传 filters：断货的那件要出现。
	_, def := doSearch(t, fx.HostA, `{"query":"连衣裙"}`)
	if !contains(titlesOf(def), fxSoldOut.Title) {
		t.Errorf("没传 filters 时没搜到断货的 %q —— in_stock_only 的默认值不是 false"+
			"（契约 SearchFilters.in_stock_only 的 default）。结果：%v",
			fxSoldOut.Title, titlesOf(def))
	}
	// 显式打开之后它必须消失，否则 in_stock_only 这个开关本身没在筛。
	_, only := doSearch(t, fx.HostA, `{"query":"连衣裙","filters":{"in_stock_only":true}}`)
	if contains(titlesOf(only), fxSoldOut.Title) {
		t.Fatalf("显式 in_stock_only=true 还搜到了断货的 %q。结果：%v",
			fxSoldOut.Title, titlesOf(only))
	}

	// 类目筛选。
	// 限定到咖啡类目：两件裙子必须消失。**不能断言「结果为空」** ——
	// 召回层没有相似度阈值（见 TestHybridRecall 那段），
	// 咖啡壶自己会被 ANN 捞回来，那是对的。
	_, coffee := doSearch(t, fx.HostA,
		fmt.Sprintf(`{"query":"连衣裙","filters":{"category_id":%d}}`, fx.CategoryCoffeeA))
	for _, gone := range []string{fxDress.Title, fxSkirt.Title} {
		if contains(titlesOf(coffee), gone) {
			t.Errorf("把类目限定到咖啡器具之后还搜出了 %q —— category_id 没生效。结果：%v",
				gone, titlesOf(coffee))
		}
	}
	_, dress := doSearch(t, fx.HostA,
		fmt.Sprintf(`{"query":"连衣裙","filters":{"category_id":%d}}`, fx.CategoryDressA))
	if !contains(titlesOf(dress), fxDress.Title) {
		t.Fatalf("限定到女装类目之后反而搜不到 %q —— 上面那条断言"+
			"因此可能只是 category_id 把一切都筛掉了。结果：%v", fxDress.Title, titlesOf(dress))
	}
	if contains(titlesOf(dress), fxCoffee.Title) {
		t.Errorf("限定到女装类目却搜出了 %q。结果：%v", fxCoffee.Title, titlesOf(dress))
	}

	// 价格区间：真丝吊带长裙 45900，连衣裙 19900。
	_, cheap := doSearch(t, fx.HostA, `{"query":"连衣裙","filters":{"max_price_cents":30000}}`)
	if contains(titlesOf(cheap), fxSkirt.Title) {
		t.Errorf("max_price_cents=30000 还搜出了 %d 分的 %q", fxSkirt.Cents, fxSkirt.Title)
	}
	if !contains(titlesOf(cheap), fxDress.Title) {
		t.Errorf("max_price_cents=30000 把 %d 分的 %q 也筛掉了", fxDress.Cents, fxDress.Title)
	}
	_, pricey := doSearch(t, fx.HostA, `{"query":"连衣裙","filters":{"min_price_cents":30000}}`)
	if !contains(titlesOf(pricey), fxSkirt.Title) {
		t.Errorf("min_price_cents=30000 没搜到 %d 分的 %q", fxSkirt.Cents, fxSkirt.Title)
	}
	if contains(titlesOf(pricey), fxDress.Title) {
		t.Errorf("min_price_cents=30000 还搜出了 %d 分的 %q", fxDress.Cents, fxDress.Title)
	}
}

// 草稿与软删除的商品，两路召回都不许交出来。
//
// db/queries/search.sql 的文件头写着「两条查询的过滤条件必须逐字一致……
// 且没有任何东西会红」。这条测试就是来当那句话的执行者的 —— 对照
// db/queries/products.sql 那条同名纪律，那边的执行者是
// TestDraftAndDeletedProductsAreInvisible。
//
// 判据的三段，缺一段这条测试就会变成空转：
//
//	① 两件反例**带着完整的派生数据**（search_text + 向量），由夹具自证
//	   （newSearchFixture 末尾那段）。没有派生数据的商品是被「没有数据」
//	   挡住的，那时把 `AND p.status = 1` 整个删掉它照样不出现。
//	② **阳性对照**：同一次查询里，一件字面几乎相同、只是 status = 1 的商品
//	   必须出现。少了它，「反例没出现」也可能只是这个查询什么都没召回。
//	③ **四种 filters 组合**都验。两路的过滤条件是分别写的，而 RRF 融合的是
//	   两路的并集 —— 任何一路把草稿放进来，它就会出现在最终结果里。
//	   组合刻意选成「除了 status / deleted_at 之外没有任何条件会挡住反例」：
//	   反例是女装、有货、19900 分，四组条件对它全部成立。
func TestDraftAndDeletedProductsAreInvisibleInSearch(t *testing.T) {
	fx := newSearchFixture(t)

	cases := []struct {
		name string
		body string
	}{
		{"不带 filters（in_stock_only 默认 false）", `{"query":"连衣裙"}`},
		{"in_stock_only=true", `{"query":"连衣裙","filters":{"in_stock_only":true}}`},
		{"限定女装类目", fmt.Sprintf(`{"query":"连衣裙","filters":{"category_id":%d}}`, fx.CategoryDressA)},
		{"价格区间 10000-30000", `{"query":"连衣裙","filters":{"min_price_cents":10000,"max_price_cents":30000}}`},
	}

	for _, c := range cases {
		w, body := doSearch(t, fx.HostA, c.body)
		if w.Code != http.StatusOK {
			t.Fatalf("[%s] 检索返回 %d：%s", c.name, w.Code, w.Body.String())
		}
		titles := titlesOf(body)

		// ② 阳性对照先跑：这一组条件下，在架的那件裙子必须在。
		if !contains(titles, fxDress.Title) {
			t.Fatalf("[%s] 阳性对照不成立：在架的 %q 都没搜到 —— "+
				"下面那两条「草稿 / 已删除没出现」因此证明不了任何事，"+
				"它们可能只是这一组条件把一切都筛掉了。结果：%v",
				c.name, fxDress.Title, titles)
		}
		for _, bad := range []searchProduct{fxDraft, fxDeleted} {
			if contains(titles, bad.Title) {
				t.Errorf("[%s] 搜到了 %q（%s）—— 两路召回里的 "+
					"`AND p.status = 1` / `AND p.deleted_at IS NULL` 有一路漏了。"+
					"RRF 融合的是两路的并集，任何一路放它进来它就会出现在最终结果里。"+
					"结果：%v", c.name, bad.Title, bad.Subtitle, titles)
			}
		}
	}
}

// 查询词里切不出任何可检索的内容时回 422，不是 200 + 空列表。
//
// 「这串东西搜不了」与「这家店没有」对用户是两件事：前者该提示换个词，
// 后者该提示这家店没有。回空列表把前者伪装成了后者。
func TestSearchRejectsQueriesWithNothingSearchable(t *testing.T) {
	fx := newSearchFixture(t)
	for _, q := range []string{`""`, `"   "`, `"!!!,。；"`} {
		w, _ := doSearch(t, fx.HostA, `{"query":`+q+`}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("query=%s 返回 %d，期望 422：%s", q, w.Code, w.Body.String())
		}
	}
	// 阳性对照：一个真能切出词的查询必须是 200，否则上面那些 422
	// 可能只是这条路由整个坏了。
	w, _ := doSearch(t, fx.HostA, `{"query":"连衣裙"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("正常查询返回 %d，上面那些 422 因此说明不了什么", w.Code)
	}
}

// 契约给 query 写了 maxLength: 200，handler 要真的挡住它。
//
// 为什么这是正确性而不是洁癖：向量那一路给引擎的等待上限是**按字数算的**
// （service.PerRuneEmbedBudget），没有长度闸门它就没有上界 —— 一条请求体
// 大小闸门之内完全放得下的超长 query（8 KiB 能装两千多个汉字）会让一个
// **公开无鉴权**的接口占着引擎跑很久。
//
// 数的是字不是字节：200 个汉字在 UTF-8 里是 600 字节。按字节挡的话，
// 边界会落在 66 个汉字上 —— 契约允许长度的三分之一，而且没有任何东西会红。
// 下面三个用例正是为此：200 个汉字必须过，201 个必须被拒。
func TestSearchRejectsQueriesLongerThanTheContractAllows(t *testing.T) {
	fx := newSearchFixture(t)

	// 刚好卡在契约上限上：200 个汉字（UTF-8 600 字节）必须是 200。
	// 这一条是阳性对照 —— 没有它，一个把上限当成字节数的实现照样绿。
	atLimit := strings.Repeat("连", service.MaxQueryRunes)
	if n := len([]rune(atLimit)); n != 200 {
		t.Fatalf("用例自己就不对：%d 个字", n)
	}
	w, _ := doSearch(t, fx.HostA, fmt.Sprintf(`{"query":%q}`, atLimit))
	if w.Code != http.StatusOK {
		t.Errorf("正好 %d 个字（契约的 maxLength）被拒了，返回 %d：%s —— "+
			"上限被当成字节数了？200 个汉字是 %d 字节",
			service.MaxQueryRunes, w.Code, w.Body.String(), len(atLimit))
	}

	// 超一个字就该 422，与「切不出词」同形。
	tooLong := strings.Repeat("连", service.MaxQueryRunes+1)
	w, _ = doSearch(t, fx.HostA, fmt.Sprintf(`{"query":%q}`, tooLong))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("%d 个字（超过契约的 maxLength: %d）返回 %d，期望 422 —— "+
			"handler 对这个上限一个字都没校验，而向量那一路的等待上限是按字数算的，"+
			"没有这道闸门它就没有上界。响应：%s",
			service.MaxQueryRunes+1, service.MaxQueryRunes, w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("422 的 Content-Type 是 %q，期望 application/problem+json", ct)
	}
}

// 引擎的等待上限跟着查询长度走，长查询不再必然静默降级。
//
// 这条测试守的是 service.DefaultQueryEmbedTimeout 上那段实测：embedding 的
// 耗时随长度线性涨，而上一版的上限是一个常数 250 ms。验收直接打引擎量到
// 150 字就要 0.258 s —— 也就是说在**契约允许长度的 75%** 处，向量那一路
// 已经必然超时，端到端 latency_ms 稳定停在 250，返回纯关键词结果，
// 而响应是一个正常的 200、strategy 照样回 "rrf-v1"。
//
// 这里不依赖真引擎（那是 make test-engine 的事），而是拿一个**按输入长度
// 真的睡觉**的替身：它把「耗时随长度涨」这条性质做进夹具，于是
// 「上限是不是也随长度涨」这件事就能被断言。斜率取得比生产机器慢
// （2 ms/字 对实测的 1.6 ms/字），这样断言不会因为 CI 机器快一点就失去意义。
func TestEmbedTimeoutGrowsWithQueryLength(t *testing.T) {
	fx := newSearchFixture(t)
	ctx := tenant.NewContext(t.Context(), fx.MerchantA)

	// 60 字与 200 字（契约上限）。前者在旧的常数上限下也能过，
	// 后者在旧上限下必然超时 —— 两条一起断言，才说明变的是「随长度」
	// 而不是「把常数调大了」。
	for _, n := range []int{60, service.MaxQueryRunes} {
		query := strings.Repeat("连衣裙", n/3)[:0] + strings.Repeat("裙", n)
		svc := service.NewSearchService(repository.New(testPool), localInventory(),
			slowByLengthEmbedder{perRune: 2 * time.Millisecond}, service.SearchConfig{}, nil)
		res, err := svc.Search(ctx, service.SearchRequest{
			Query:   query,
			Filters: service.SearchFilters{InStockOnly: true},
		})
		if err != nil {
			t.Fatalf("%d 字的查询报错：%v", n, err)
		}
		if res.Degraded {
			t.Errorf("%d 字的查询降级了 —— 引擎只用了 %v，而契约允许这个长度。"+
				"给引擎的等待上限没有跟着长度走，长查询会**必然**静默降级："+
				"响应仍是 200、strategy 仍是 rrf-v1，访客看不出任何异常",
				n, time.Duration(n)*2*time.Millisecond)
		}
	}

	// 阴性对照：慢到超出上限时仍然要降级（而不是变成无限等待）。
	// 少了它，「不降级」也可能只是因为超时被取消掉了。
	svc := service.NewSearchService(repository.New(testPool), localInventory(),
		slowByLengthEmbedder{perRune: 40 * time.Millisecond}, service.SearchConfig{}, nil)
	res, err := svc.Search(ctx, service.SearchRequest{
		Query:   strings.Repeat("裙", 60),
		Filters: service.SearchFilters{InStockOnly: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Degraded {
		t.Error("引擎慢到 2.4 秒也没有降级 —— 上限没有上界了，" +
			"一个人正盯着的搜索框前面会一直等下去")
	}
}

// slowByLengthEmbedder 按输入长度睡觉，模拟真引擎「耗时随字数线性涨」。
type slowByLengthEmbedder struct{ perRune time.Duration }

func (e slowByLengthEmbedder) Embed(ctx context.Context, texts []string) (*inference.Result, error) {
	n := 0
	for _, t := range texts {
		n += len([]rune(t))
	}
	select {
	case <-time.After(time.Duration(n) * e.perRune):
	case <-ctx.Done():
		return nil, fmt.Errorf("%w：%v", inference.ErrUnavailable, ctx.Err())
	}
	return conceptEmbedder{}.Embed(ctx, texts)
}

// 请求体大小闸门。
//
// /search 是 security: []（公开无鉴权），而在此之前全仓库唯一一处
// MaxBytesReader 在 handler/webhook.go。一个公开的、每次请求都要在 CPU 上
// 跑模型推理的接口，连读多少字节都不限。
func TestSearchRejectsAnOversizedBody(t *testing.T) {
	fx := newSearchFixture(t)

	// 闸门之内：一个正常请求必须照常 200。阳性对照。
	w, _ := doSearch(t, fx.HostA, `{"query":"连衣裙"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("正常请求返回 %d，下面那条 413 因此说明不了什么：%s", w.Code, w.Body.String())
	}

	// 闸门之外：塞一个超长的 strategy（它不受 query 的长度闸门管），
	// 把请求体顶到 MaxSearchBodyBytes 之上。
	//
	// 刻意不用超长的 query：那会被长度闸门先挡成 422，于是这条测试
	// 测的就是那一条，而不是大小闸门。
	body := fmt.Sprintf(`{"query":"连衣裙","strategy":%q}`,
		strings.Repeat("x", handler.MaxSearchBodyBytes+1))
	w, _ = doSearch(t, fx.HostA, body)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d 字节的请求体返回 %d，期望 413 —— 大小闸门没挂上。响应：%s",
			len(body), w.Code, w.Body.String())
	}
}

// 一个 IP 打洪水会被挡下来，别的 IP 不受牵连。
//
// M3 验收实测（BGE-M3 跑在 CPU 上）：20 个并发的 198 字查询（契约允许的长度）
// 打进去之后，同时发的 5 个正常短查询全部从 n=5 lat=55 变成 n=1 lat=250 ——
// 整站掉到纯关键词。M4 换 GPU 之后这条曲线整体右移（拐点从 8 挪到 16），
// 默认配额跟着重算过，两组前提与实测表都在 app/ratelimit.go 的文件头。
//
// 这条测试真的把配额打满并断言被拒，而不是断言「配置读进来了」。
// 它自己设 EnvSearchRateLimit / EnvSearchRateBurst，所以**不依赖那两个默认值**
// —— 默认值重算时这条测试不该跟着变，它守的是「限流这件事在不在」。
//
// 它自己装一套路由（走的是同一个 app.Router），因为包级那套把配额调到了
// 测试打不穿的数 —— 理由写在 main_test.go。也就是说这条测试同时证明了
// 「限流真的挂在 /search 这条路由上」。
func TestSearchRateLimitsAFloodFromOneIP(t *testing.T) {
	fx := newSearchFixture(t)

	const burst = 3
	t.Setenv(app.EnvSearchRateLimit, "0.001") // 慢到这次测试里补不回一个额度
	t.Setenv(app.EnvSearchRateBurst, fmt.Sprint(burst))
	limited := app.Router(testPool,
		tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}),
		auth.NewSigner([]byte("keel-test-secret-key-32-bytes-long!!")),
		testOrders, service.PaymentConfig{Sandbox: true}, conceptEmbedder{})

	const flood = burst + 5
	var ok, rejected int
	var lastRejected *httptest.ResponseRecorder
	for i := 0; i < flood; i++ {
		w, _ := searchOn(t, limited, fx.HostA, `{"query":"连衣裙"}`)
		switch w.Code {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			rejected++
			lastRejected = w
		default:
			t.Fatalf("第 %d 个请求返回 %d，既不是 200 也不是 429：%s",
				i, w.Code, w.Body.String())
		}
	}
	if ok != burst {
		t.Errorf("放行了 %d 个，期望正好 %d（瞬时额度）", ok, burst)
	}
	if rejected != flood-burst {
		t.Fatalf("只挡下了 %d 个（共打了 %d 个，额度 %d）—— "+
			"限流没生效。单机一条 for 循环就能让一个公开 Demo 的语义搜索"+
			"变成关键词搜索，而访客看不出任何异常", rejected, flood, burst)
	}
	if got := lastRejected.Header().Get("Retry-After"); got == "" {
		t.Error("429 没带 Retry-After —— 客户端能做的只有立刻重试，" +
			"而那正好是限流要挡的行为")
	}
	if ct := lastRejected.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("429 的 Content-Type 是 %q，期望 application/problem+json", ct)
	}
	if !strings.Contains(lastRejected.Body.String(), "problems/rate-limited") {
		t.Errorf("429 的 type 不是 rate-limited：%s", lastRejected.Body.String())
	}

	// 别的 IP 不受牵连 —— 否则这就不是「按 IP 限流」，是一个全局开关，
	// 一个攻击者能把所有人一起关在门外。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/search",
		strings.NewReader(`{"query":"连衣裙"}`))
	req.Host = fx.HostA
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.7:4321"
	other := httptest.NewRecorder()
	limited.ServeHTTP(other, req)
	if other.Code != http.StatusOK {
		t.Fatalf("另一个 IP 也被挡了（%d）—— 这不是按 IP 限流，"+
			"一个攻击者能把所有人一起关在门外：%s", other.Code, other.Body.String())
	}
}

// size 的钳制与 total 的语义。
func TestSearchSizeIsClampedAndTotalIsTheReturnedCount(t *testing.T) {
	fx := newSearchFixture(t)
	w, one := doSearch(t, fx.HostA, `{"query":"连衣裙","size":1}`)
	if w.Code != http.StatusOK {
		t.Fatalf("检索返回 %d：%s", w.Code, w.Body.String())
	}
	if len(one.Items) != 1 {
		t.Fatalf("size=1 返回了 %d 条", len(one.Items))
	}
	if one.Total != 1 {
		t.Errorf("size=1 时 total=%d，期望 1（契约：本次召回并排序后的结果总数，"+
			"上限即 size；一期不支持翻页）", one.Total)
	}
	// 越界的 size 钳制而不是报错，与 /products 的分页同一条纪律。
	for _, body := range []string{`{"query":"连衣裙","size":0}`, `{"query":"连衣裙","size":100000}`} {
		w, got := doSearch(t, fx.HostA, body)
		if w.Code != http.StatusOK {
			t.Errorf("%s 返回 %d，期望钳制而不是报错", body, w.Code)
		}
		if len(got.Items) == 0 {
			t.Errorf("%s 返回空列表", body)
		}
	}
}

// 未登录也能搜（契约里 /search 是 security: []）。
func TestSearchIsPublic(t *testing.T) {
	fx := newSearchFixture(t)
	w, _ := doSearch(t, fx.HostA, `{"query":"连衣裙"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("不带令牌检索返回 %d，期望 200（契约里这条接口是 security: []）", w.Code)
	}
}

// 响应里那几个字段真的能被按契约生成的类型解出来。
//
// 这一条盯的是「手写的响应结构体与契约漂移」——searchResponse 的外层四个字段
// 是手写的（契约里它是内联 schema，生成器没出类型），而 items 用的是生成的
// api.SearchHit。把 items 解回 api.SearchHit 至少让后者这一半有编译期之外的
// 一道检查。
func TestSearchResponseDecodesIntoGeneratedTypes(t *testing.T) {
	fx := newSearchFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/search",
		strings.NewReader(`{"query":"连衣裙","explain":true}`))
	req.Host = fx.HostA
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	testEngine.ServeHTTP(w, req)

	var body struct {
		Items     []api.SearchHit `json:"items"`
		LatencyMs int             `json:"latency_ms"`
		Total     int             `json:"total"`
		Strategy  string          `json:"strategy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应解不进生成的类型: %v\n%s", err, w.Body.String())
	}
	if len(body.Items) == 0 {
		t.Fatal("结果是空的")
	}
	for _, it := range body.Items {
		if it.Id == 0 || it.Title == "" {
			t.Errorf("命中缺 id 或 title: %+v", it)
		}
		if !it.Status.Valid() || it.Status != 1 {
			t.Errorf("%q 的 status 是 %d，检索只该返回在架商品", it.Title, it.Status)
		}
		if it.RecallSource == nil || !it.RecallSource.Valid() {
			t.Errorf("%q 的 recall_source 不是契约枚举里的值: %v", it.Title, it.RecallSource)
		}
		if it.InStock == nil {
			t.Errorf("%q 没有 in_stock 字段", it.Title)
		}
		if it.Scores == nil || it.Scores.Business == nil || it.Scores.Final == nil {
			t.Errorf("%q 的 scores 解不出 business / final：%+v", it.Title, it.Scores)
		}
	}
	if body.LatencyMs < 0 {
		t.Errorf("latency_ms = %d", body.LatencyMs)
	}
	t.Logf("latency_ms=%d total=%d strategy=%s", body.LatencyMs, body.Total, body.Strategy)
}

// 关键词那一路挂了、向量那一路好着时，检索仍然返回结果。
//
// §8 说的是「任何一环」，不只是引擎那一环。这条从另一侧验降级链：
// 用一个会让关键词查询报错的 tsquery 是做不到的（它由应用拼，拼得出的东西
// 永远合法），所以这里直接在 service 层注入一个只让关键词那一路失败的仓储。
func TestSearchStillReturnsWhenKeywordRouteFails(t *testing.T) {
	fx := newSearchFixture(t)

	svc := service.NewSearchService(
		keywordBrokenRepo{inner: repository.New(testPool)}, localInventory(), conceptEmbedder{},
		service.SearchConfig{}, nil)
	res, err := svc.Search(tenant.NewContext(t.Context(), fx.MerchantA),
		service.SearchRequest{Query: "连衣裙", Filters: service.SearchFilters{InStockOnly: true}})
	if err != nil {
		t.Fatalf("关键词那一路挂了就整个失败了：%v —— "+
			"§8：任何一环故障，搜索都必须仍能返回结果", err)
	}
	if len(res.Items) == 0 {
		t.Fatal("关键词那一路挂了之后返回了空列表")
	}
	for _, it := range res.Items {
		if it.Source != search.SourceVector {
			t.Errorf("%q 的 recall_source 是 %s，期望全是 vector", it.Title, it.Source)
		}
	}
	t.Logf("关键词路失败时仍返回 %d 条，全部来自向量路", len(res.Items))
}

// keywordBrokenRepo 让关键词那一条查询报错，别的照常。
type keywordBrokenRepo struct{ inner *repository.Repo }

func (r keywordBrokenRepo) WithTenant(ctx context.Context, fn func(repository.Tx) error) error {
	return r.inner.WithTenant(ctx, func(tx repository.Tx) error {
		return fn(brokenKeywordTx{Tx: tx})
	})
}

type brokenKeywordTx struct{ repository.Tx }

// 签名跟着 repository.Tx 走：00020 给两路召回都加了 StoreScope。
// 这里原样接下不用 —— 这个替身的全部职责是「让关键词那一路失败」。
func (brokenKeywordTx) SearchProductsByKeyword(ctx context.Context, sc repository.StoreScope,
	tsquery string, f repository.SearchFilters, limit int32) ([]repository.SearchHit, error) {
	return nil, fmt.Errorf("注入的故障：关键词召回这一路挂了")
}

// 两路召回的过滤条件必须**逐字一致**，直接比一次。
//
// ===========================================================================
// 为什么行为测试不够，还要这一条
// ===========================================================================
//
// db/queries/search.sql 的文件头写着这条纪律。它此前的执行者有两条：
// TestSearchAppliesFilters（价格区间）与 TestDraftAndDeletedProductsAreInvisibleInSearch
// （status / deleted_at）。它们都是打 POST /search 看**融合之后**的结果，
// 而 RRF 融合的是两路的**并集** —— 也就是说它们只抓得住一个方向：
//
//	· 某一路**放行得更多**（把草稿放进来、把价格过滤删掉）→ 并集里多出东西 → 红。
//	· 某一路**放行得更少**（那一路的过滤条件更严，或者算错了价格）→
//	  并集一点没变，因为另一路照样召回了它 → **全绿**。
//
// 后一个方向不是理论问题：M4 Task 3 把价格区间改成现算之后，那个
// LEFT JOIN LATERAL 在两条查询里各写了一遍。把其中一条的 LATERAL 写岔
// （比如漏掉 COALESCE，或者 WHERE 里少一个条件），症状正是「这一路少召回
// 一些」—— 而上面那两条测试对它是全绿的。
//
// 这条测试直接比两路**自己的过滤决定**，绕开「两路召回面本来就不同」这件事：
//
//	common  := 不带筛选时两路都召回到的那批
//	assert  带上筛选之后，两路从 common 里筛掉的是**同一批**
//
// 它对两个方向都红，而且不依赖 RRF。
func TestBothRecallPathsFilterIdentically(t *testing.T) {
	fx := newSearchFixture(t)
	ctx := tenant.NewContext(t.Context(), fx.MerchantA)
	repo := repository.New(testPool)

	// 一条能把两路都撑开的查询：向量用「连衣裙 咖啡壶」这个混合概念，
	// 关键词用它的 bigram 串。两路的候选面仍然不同（向量路召回全部有向量的
	// 商品，关键词路只召回 tsquery 命中的），所以下面只在交集上比。
	const probe = "连衣裙 长裙 咖啡壶"
	emb, err := (conceptEmbedder{}).Embed(ctx, []string{probe})
	if err != nil {
		t.Fatal(err)
	}
	vec := emb.Vectors[0]
	tsq := search.TSQueryOr(search.Bigram(probe))
	if tsq == "" {
		t.Fatal("tsquery 切不出任何词 —— 这条测试没在检查任何东西")
	}

	const limit = 100
	recall := func(f repository.SearchFilters) (map[int64]bool, map[int64]bool) {
		t.Helper()
		v, k := map[int64]bool{}, map[int64]bool{}
		if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
			vh, err := tx.SearchProductsByVector(ctx, fx.ScopeA(), vec, f, limit)
			if err != nil {
				return err
			}
			for _, h := range vh {
				v[h.ID] = true
			}
			kh, err := tx.SearchProductsByKeyword(ctx, fx.ScopeA(), tsq, f, limit)
			if err != nil {
				return err
			}
			for _, h := range kh {
				k[h.ID] = true
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return v, k
	}

	baseVec, baseKw := recall(repository.SearchFilters{})
	common := map[int64]bool{}
	for id := range baseVec {
		if baseKw[id] {
			common[id] = true
		}
	}
	// 阳性对照一：两路都召回到的那批不能是空的，否则下面所有比较都在比空集。
	if len(common) < 2 {
		t.Fatalf("不带筛选时两路的交集只有 %d 件（向量路 %d、关键词路 %d）—— "+
			"这条测试没在检查任何东西", len(common), len(baseVec), len(baseKw))
	}

	// 20000 这个数是按夹具挑的：交集里四件商品的价格是 12900 / 19900 /
	// 22900 / 45900，所以上下界各自都真的会筛掉两件 —— 下面那条
	// 「这组条件一件都没筛掉」的阳性对照因此不是摆设。
	minC, maxC := int64(20000), int64(20000)
	dressCat := fx.CategoryDressA
	cases := []struct {
		name string
		f    repository.SearchFilters
	}{
		{"价格下界 20000", repository.SearchFilters{MinPriceCents: &minC}},
		{"价格上界 20000", repository.SearchFilters{MaxPriceCents: &maxC}},
		{"限定女装类目", repository.SearchFilters{CategoryID: &dressCat}},
		// 「只看有货」不在这张表里了（微服务拆分阶段 1a）：它不再是两条召回 SQL 各写一遍的
		// 过滤，而是 service 在召回之后的**一处**过滤（service/search.go 的 applyStock），
		// 两路「逐字一致」的风险随之消失。它的行为由服务层那几条 in_stock_only 的测试验。
	}
	for _, c := range cases {
		gotVec, gotKw := recall(c.f)
		var onlyVec, onlyKw []int64
		removed := 0
		for id := range common {
			v, k := gotVec[id], gotKw[id]
			if !v && !k {
				removed++
				continue
			}
			if v && !k {
				onlyVec = append(onlyVec, id)
			}
			if k && !v {
				onlyKw = append(onlyKw, id)
			}
		}
		if len(onlyVec) > 0 || len(onlyKw) > 0 {
			t.Errorf("[%s] 两路对同一批商品给出了不同的过滤结果："+
				"只有向量路放行的 %v，只有关键词路放行的 %v —— "+
				"db/queries/search.sql 那两条查询的过滤条件（含 00019 之后那个"+
				"LEFT JOIN LATERAL 与两处 COALESCE）必须逐字一致。"+
				"RRF 只看名次，它拿到的是两份对「哪些商品存在」意见不同的列表",
				c.name, onlyVec, onlyKw)
		}
		// 阳性对照二：这组条件必须**真的筛掉**交集里的一些东西。
		// 一个什么都不筛的条件下，「两路筛掉的一样」是恒真的。
		if removed == 0 {
			t.Errorf("[%s] 这组筛选条件一件都没筛掉（交集 %d 件）—— "+
				"上面那条断言在这一组上是恒真的", c.name, len(common))
		}
	}
}

// 相关度下限（2026-09-28）：只被向量路捞到、相似度低于下限的商品不算可信命中。
//
// 夹具的向量按概念轴给（search_fixture_test.go）：跨概念的余弦是 0 上下，远低于 0.40。
//
//	· 「连衣裙」有可信命中（裙子两件）：咖啡那件（跨概念，只有向量路捞到）不再挂在尾巴上，fallback = false；
//	· 「瑜伽垫」（夹具里没有这个概念，也没有字面命中）：一条可信命中都没有 → 回的是「猜你想要」，
//	  fallback = true，items 非空；search_logs 那一行 fallback 也是 true（无结果统计按它算）。
func TestSearchVectorFloorSeparatesMatchesFromGuesses(t *testing.T) {
	fx := newSearchFixture(t)

	w, body := doSearch(t, fx.HostA, `{"query":"连衣裙","explain":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("检索返回 %d：%s", w.Code, w.Body.String())
	}
	titles := titlesOf(body)
	if !contains(titles, fxDress.Title) || !contains(titles, fxSkirt.Title) {
		t.Fatalf("搜「连衣裙」应当有两件裙子：%v", titles)
	}
	if contains(titles, fxCoffee.Title) {
		t.Fatalf("搜「连衣裙」时咖啡（跨概念、只有向量路、相似度远低于下限）仍在结果里：%v", titles)
	}
	if body.raw["fallback"] != false {
		t.Fatalf("有可信命中时 fallback 应为 false：%v", body.raw["fallback"])
	}

	w, body = doSearch(t, fx.HostA, `{"query":"瑜伽垫"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("检索返回 %d：%s", w.Code, w.Body.String())
	}
	if body.raw["fallback"] != true || len(body.Items) == 0 {
		t.Fatalf("搜「瑜伽垫」应当 fallback = true 且回「猜你想要」：fallback=%v items=%v", body.raw["fallback"], titlesOf(body))
	}
	trace, _ := body.raw["trace_id"].(string)
	if trace == "" {
		t.Fatal("检索没有回 trace_id")
	}
	var fb bool
	if err := admin(t).QueryRow(context.Background(), `SELECT fallback FROM search_logs WHERE trace_id = $1`, trace).Scan(&fb); err != nil {
		t.Fatal(err)
	}
	if !fb {
		t.Fatal("「猜你想要」那一次的 search_logs.fallback 应为 true —— 否则无结果统计又数成有结果")
	}
}

// 请求体解码失败的两种该报不同的话，别再都说「不是合法的 JSON」。
//
// 后台乱点测试的真实案例：`store_id:"abc"`（契约里 store_id 是 integer）
// 被报成了「请求体不是合法的 JSON」—— 但请求体本身完全合法，商家对着一份
// 合法 JSON 看半天也看不出哪里「不合法」。problem.WriteBindError 把
// encoding/json 的 *json.UnmarshalTypeError 从真正的语法错里分出来，
// 这条测试钉住两边都不回退：真语法错还是老文案，类型不对要点名字段。
func TestSearchBodyDecodeErrorsNameTheOffendingField(t *testing.T) {
	fx := newSearchFixture(t)

	// 阴性对照：真正的语法错（少一个引号）必须还是那句通用文案，
	// 不该被新逻辑误判成某个字段的类型错误。
	w, _ := doSearch(t, fx.HostA, `{"query":"连衣裙"`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("截断的 JSON 返回 %d，期望 422：%s", w.Code, w.Body.String())
	}
	var syntaxProb api.Problem
	if err := json.Unmarshal(w.Body.Bytes(), &syntaxProb); err != nil {
		t.Fatalf("响应不是 Problem: %v\n%s", err, w.Body.String())
	}
	if syntaxProb.Title != "请求体不是合法的 JSON" {
		t.Errorf("真语法错的 title 是 %q，期望保持原文案「请求体不是合法的 JSON」", syntaxProb.Title)
	}
	if syntaxProb.Errors != nil {
		t.Errorf("真语法错不该带 errors[]（没有具体字段可指）：%v", *syntaxProb.Errors)
	}

	// 真正要修的那个案例：store_id 给成字符串。
	w, _ = doSearch(t, fx.HostA, `{"query":"连衣裙","store_id":"abc"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("store_id 给字符串返回 %d，期望 422：%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type 是 %q，期望 application/problem+json", ct)
	}
	var typeProb api.Problem
	if err := json.Unmarshal(w.Body.Bytes(), &typeProb); err != nil {
		t.Fatalf("响应不是 Problem: %v\n%s", err, w.Body.String())
	}
	if typeProb.Type != problem.TypeInvalidRequest {
		t.Errorf("Problem type 是 %q，期望 %q", typeProb.Type, problem.TypeInvalidRequest)
	}
	if typeProb.Errors == nil || len(*typeProb.Errors) != 1 {
		t.Fatalf("errors[] 是 %v，期望 1 条 —— 商家要知道是哪个字段", typeProb.Errors)
	}
	fe := (*typeProb.Errors)[0]
	if fe.Field == nil || *fe.Field != "store_id" {
		t.Errorf("errors[0].field 是 %v，期望 \"store_id\"", fe.Field)
	}
	if fe.Message == nil || !strings.Contains(*fe.Message, "store_id") || !strings.Contains(*fe.Message, "整数") {
		t.Errorf("errors[0].message 是 %v，期望指明字段名与期望类型（整数）", fe.Message)
	}
	if !strings.Contains(typeProb.Title, "store_id") {
		t.Errorf("Problem.title 是 %q，期望点名 store_id，而不是回退成通用的「不是合法的 JSON」", typeProb.Title)
	}
}
