package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/handler"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// POST /search/events 的行为闸门（规则在 service/search_event.go 的文件头）。
//
// 每条测试都**不带令牌**打：这条接口在契约里是 security: []，与 /search 一样。
// 哪天有人给它挂上 Bearer，这一整个文件都会红成 401。

// postEventOn 往 engine 上发一次回传。
func postEventOn(t *testing.T, engine http.Handler, host, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/search/events", strings.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func postEvent(t *testing.T, host, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postEventOn(t, testEngine, host, body)
}

func eventBody(trace, event string, productID int64) string {
	return fmt.Sprintf(`{"trace_id":%q,"event":%q,"product_id":%d}`, trace, event, productID)
}

// behaviorRow 是 search_logs 一行的三个行为列。
type behaviorRow struct{ Clicked, Carted, Ordered *int64 }

func (b behaviorRow) String() string {
	f := func(p *int64) string {
		if p == nil {
			return "NULL"
		}
		return fmt.Sprint(*p)
	}
	return fmt.Sprintf("clicked=%s carted=%s ordered=%s", f(b.Clicked), f(b.Carted), f(b.Ordered))
}

// behaviorOf 用管理员连接（绕过 RLS）读 trace_id 那一行的三个行为列。
// 用管理员连接是因为跨租户那条断言要看的正是「别家店的请求有没有碰到这一行」。
func behaviorOf(t *testing.T, trace string) behaviorRow {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var b behaviorRow
	if err := admin.QueryRow(ctx,
		`SELECT clicked_id, carted_id, ordered_id FROM search_logs WHERE trace_id = $1`,
		trace).Scan(&b.Clicked, &b.Carted, &b.Ordered); err != nil {
		t.Fatalf("读 trace_id = %s 那一行失败：%v", trace, err)
	}
	return b
}

func eqID(p *int64, want int64) bool { return p != nil && *p == want }

// searchForTrace 搜一次，返回 trace_id 与这次返回的商品 id（按顺序）。
func searchForTrace(t *testing.T, engine http.Handler, host, body string) (string, []int64) {
	t.Helper()
	w, resp := searchOn(t, engine, host, body)
	if w.Code != http.StatusOK {
		t.Fatalf("检索返回 %d：%s", w.Code, w.Body.String())
	}
	trace, _ := resp.raw["trace_id"].(string)
	if !traceIDShape.MatchString(trace) {
		t.Fatalf("响应里的 trace_id = %v，期望 32 个十六进制字符。响应：%s", resp.raw["trace_id"], w.Body.String())
	}
	return trace, responseIDs(resp)
}

// /search 回的 trace_id 就是它那一行日志的 trace_id —— 回传拿着它能找到那一行。
//
// 这条是 contract_test.go 里那笔 trace_id 挂账被划掉之后的反向锁：
// 哪天响应里又不回它（或者回的是另一个随手生成的 id），这里红。
func TestSearchReturnsTheTraceIDOfItsLogRow(t *testing.T) {
	fx := newSearchFixture(t)
	t1, _ := searchForTrace(t, testEngine, fx.HostA, `{"query":"连衣裙"}`)
	t2, _ := searchForTrace(t, testEngine, fx.HostA, `{"query":"连衣裙"}`)
	if t1 == t2 {
		t.Fatalf("两次检索回了同一个 trace_id %s", t1)
	}
	logs := searchLogsOf(t, fx.MerchantA)
	if len(logs) != 2 || logs[0].TraceID != t1 || logs[1].TraceID != t2 {
		var got []string
		for _, l := range logs {
			got = append(got, l.TraceID)
		}
		t.Fatalf("响应里的 trace_id 是 %s / %s，search_logs 里是 %v —— 两边必须是同一个值", t1, t2, got)
	}
}

// 检索日志没写进去时，响应里**不回** trace_id —— 库里没有那一行，
// 回出去的 id 只会换来之后每一次回传的 404。
func TestSearchWithoutALogRowReturnsNoTraceID(t *testing.T) {
	fx := newSearchFixture(t)
	svc := service.NewSearchService(logBrokenRepo{inner: repository.New(testPool)},
		conceptEmbedder{}, service.SearchConfig{}, nil)
	res, err := svc.Search(tenant.NewContext(t.Context(), fx.MerchantA),
		service.SearchRequest{Query: "连衣裙"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) == 0 {
		t.Fatal("阳性对照：写日志失败时检索本身也该有结果")
	}
	if res.TraceID != "" {
		t.Fatalf("检索日志写失败了，TraceID 却是 %q —— 那个 id 在库里没有落点", res.TraceID)
	}

	// 对照：日志写得进去时它有值（否则上面那条可能只是 TraceID 从来没被赋过）。
	ok := service.NewSearchService(repository.New(testPool), conceptEmbedder{}, service.SearchConfig{}, nil)
	res, err = ok.Search(tenant.NewContext(t.Context(), fx.MerchantA), service.SearchRequest{Query: "连衣裙"})
	if err != nil {
		t.Fatal(err)
	}
	if !traceIDShape.MatchString(res.TraceID) {
		t.Fatalf("日志写进去了，TraceID 却是 %q", res.TraceID)
	}
}

// 三种事件各填各的列，**每一列首次为准**：再报同一种事件（不论同一件还是另一件）
// 仍然 204，但不覆盖。
func TestSearchEventsFillTheirColumnAndTheFirstWriteWins(t *testing.T) {
	fx := newSearchFixture(t)
	trace, ranked := searchForTrace(t, testEngine, fx.HostA, `{"query":"连衣裙"}`)
	if len(ranked) < 2 {
		t.Fatalf("这条测试要至少两条结果才能验「第二次不覆盖」，只有 %v", ranked)
	}
	first, second := ranked[0], ranked[1]

	if b := behaviorOf(t, trace); b.Clicked != nil || b.Carted != nil || b.Ordered != nil {
		t.Fatalf("还没回传，行为列就有值：%v", b)
	}

	send := func(event string, pid int64) {
		t.Helper()
		if w := postEvent(t, fx.HostA, eventBody(trace, event, pid)); w.Code != http.StatusNoContent {
			t.Fatalf("%s %d 返回 %d，期望 204：%s", event, pid, w.Code, w.Body.String())
		}
	}

	send("click", first)
	if b := behaviorOf(t, trace); !eqID(b.Clicked, first) || b.Carted != nil || b.Ordered != nil {
		t.Fatalf("click %d 之后：%v —— 只该填 clicked_id", first, b)
	}

	// 幂等：同一件再报一次、另一件再报一次，都 204、都不覆盖。
	send("click", first)
	send("click", second)
	if b := behaviorOf(t, trace); !eqID(b.Clicked, first) {
		t.Fatalf("第二、三次 click 之后 clicked_id = %v，期望仍是首次的 %d —— 首次为准", b.Clicked, first)
	}

	send("add_cart", second)
	send("order", second)
	send("add_cart", first)
	send("order", first)
	b := behaviorOf(t, trace)
	if !eqID(b.Clicked, first) || !eqID(b.Carted, second) || !eqID(b.Ordered, second) {
		t.Fatalf("最终 %v，期望 clicked=%d carted=%d ordered=%d", b, first, second, second)
	}
}

// product_id 必须是这次检索**真正返回过的**商品，否则 422、一列都不写。
//
// 反例刻意挑「同一家店里真实存在、上架、有货」的商品 —— 用一个不存在的 id
// 的话，这条测试分不清挡住它的是 ranked_ids 那道闸门，还是别的什么。
func TestSearchEventsOnlyAcceptProductsTheSearchReturned(t *testing.T) {
	fx := newSearchFixture(t)
	trace, ranked := searchForTrace(t, testEngine, fx.HostA, `{"query":"连衣裙","size":1}`)
	if len(ranked) != 1 {
		t.Fatalf("size=1 返回了 %v", ranked)
	}
	returned := map[int64]bool{ranked[0]: true}
	var outsider int64
	for _, title := range []string{fxSkirt.Title, fxCoffee.Title, fxDress.Title} {
		if id := fx.IDsA[title]; !returned[id] {
			outsider = id
			break
		}
	}
	if outsider == 0 {
		t.Fatal("夹具里找不到一件没被返回的 A 店商品")
	}

	for _, ev := range []string{"click", "add_cart", "order"} {
		for name, pid := range map[string]int64{"同店没返回的": outsider, "B 店的": fx.IDsB[fxDress.Title]} {
			w := postEvent(t, fx.HostA, eventBody(trace, ev, pid))
			if w.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s %s商品 %d 返回 %d，期望 422 —— 公开接口接受任意商品 id，"+
					"任何人拿一个 trace_id 就能刷指标：%s", ev, name, pid, w.Code, w.Body.String())
			}
		}
	}
	if b := behaviorOf(t, trace); b.Clicked != nil || b.Carted != nil || b.Ordered != nil {
		t.Fatalf("被拒的回传写进了行为列：%v", b)
	}

	// 阳性对照：同一个 trace_id、返回过的那件，照常 204。
	if w := postEvent(t, fx.HostA, eventBody(trace, "click", ranked[0])); w.Code != http.StatusNoContent {
		t.Fatalf("返回过的商品 %d 回传得到 %d：%s", ranked[0], w.Code, w.Body.String())
	}
	if b := behaviorOf(t, trace); !eqID(b.Clicked, ranked[0]) {
		t.Fatalf("阳性对照没写进去：%v", b)
	}
}

// 租户隔离：A 店的 trace_id 从 B 店的域名打进来，是 404，而且那一行一列都没动。
// 与不存在的 trace_id 是同一个 404（§2：查不到即 404，不是 403）。
func TestSearchEventsAreTenantIsolated(t *testing.T) {
	fx := newSearchFixture(t)
	trace, ranked := searchForTrace(t, testEngine, fx.HostA, `{"query":"连衣裙"}`)

	w := postEvent(t, fx.HostB, eventBody(trace, "click", ranked[0]))
	if w.Code != http.StatusNotFound {
		t.Fatalf("A 店的 trace_id 从 B 店打进来返回 %d，期望 404：%s", w.Code, w.Body.String())
	}
	crossTenant := w.Body.String()
	if b := behaviorOf(t, trace); b.Clicked != nil {
		t.Fatalf("B 店的请求改到了 A 店的检索日志：%v", b)
	}

	unknown := strings.Repeat("0", 32)
	w = postEvent(t, fx.HostA, eventBody(unknown, "click", ranked[0]))
	if w.Code != http.StatusNotFound {
		t.Fatalf("不存在的 trace_id 返回 %d，期望 404：%s", w.Code, w.Body.String())
	}
	if w.Body.String() != crossTenant {
		t.Errorf("「别家店的」与「不存在的」trace_id 回的正文不一样 —— 分开报等于替调用方做跨店枚举：\n%s\n%s",
			crossTenant, w.Body.String())
	}

	// 阳性对照：同一个请求从 A 店打，204 且写进去了。
	if w := postEvent(t, fx.HostA, eventBody(trace, "click", ranked[0])); w.Code != http.StatusNoContent {
		t.Fatalf("A 店自己回传返回 %d：%s", w.Code, w.Body.String())
	}
	if b := behaviorOf(t, trace); !eqID(b.Clicked, ranked[0]) {
		t.Fatalf("阳性对照没写进去：%v", b)
	}
}

// 形状不对的请求一律 422（超长 413），一列都不写。
func TestSearchEventsRejectMalformedRequests(t *testing.T) {
	fx := newSearchFixture(t)
	trace, ranked := searchForTrace(t, testEngine, fx.HostA, `{"query":"连衣裙"}`)
	pid := ranked[0]

	cases := map[string]string{
		"event 不在枚举里":   eventBody(trace, "view", pid),
		"event 缺席":      fmt.Sprintf(`{"trace_id":%q,"product_id":%d}`, trace, pid),
		"product_id 缺席": fmt.Sprintf(`{"trace_id":%q,"event":"click"}`, trace),
		"trace_id 缺席":   fmt.Sprintf(`{"event":"click","product_id":%d}`, pid),
		"trace_id 大写":   eventBody(strings.ToUpper(trace), "click", pid),
		"trace_id 太短":   eventBody(trace[:31], "click", pid),
		"不是 JSON":       `trace_id=` + trace,
	}
	for name, body := range cases {
		if w := postEvent(t, fx.HostA, body); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("[%s] 返回 %d，期望 422：%s", name, w.Code, w.Body.String())
		}
	}
	big := fmt.Sprintf(`{"trace_id":%q,"event":"click","product_id":%d,"pad":%q}`,
		trace, pid, strings.Repeat("x", handler.MaxSearchEventBodyBytes))
	if w := postEvent(t, fx.HostA, big); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("超长请求体返回 %d，期望 413：%s", w.Code, w.Body.String())
	}
	if b := behaviorOf(t, trace); b.Clicked != nil || b.Carted != nil || b.Ordered != nil {
		t.Fatalf("被拒的请求写进了行为列：%v", b)
	}
}

// /search/events 挂着按 IP 的限流，而且与 /search **各自一只桶**：
// 搜索额度打光了，回传照样收；回传自己的额度打光了才 429。
//
// 与 TestSearchRateLimitsAFloodFromOneIP 同一个形状：自己配一个很紧的配额、
// 自己装一套路由（同一个 app.Router），所以同时证明了「闸门真的挂在这条路由上」。
func TestSearchEventsAreRateLimitedInTheirOwnBucket(t *testing.T) {
	fx := newSearchFixture(t)

	const burst = 3
	t.Setenv(app.EnvSearchRateLimit, "0.001")
	t.Setenv(app.EnvSearchRateBurst, "1")
	t.Setenv(app.EnvSearchEventRateLimit, "0.001")
	t.Setenv(app.EnvSearchEventRateBurst, fmt.Sprint(burst))
	limited := app.Router(testPool,
		tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}),
		auth.NewSigner([]byte("keel-test-secret-key-32-bytes-long!!")),
		testOrders, service.PaymentConfig{Sandbox: true}, conceptEmbedder{})

	trace, ranked := searchForTrace(t, limited, fx.HostA, `{"query":"连衣裙"}`)
	if w, _ := searchOn(t, limited, fx.HostA, `{"query":"连衣裙"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("搜索额度是 1，第二次检索返回 %d，期望 429 —— 下面「搜索额度光了回传照收」因此没有前提", w.Code)
	}

	body := eventBody(trace, "click", ranked[0])
	for i := 0; i < burst; i++ {
		if w := postEventOn(t, limited, fx.HostA, body); w.Code != http.StatusNoContent {
			t.Fatalf("第 %d 次回传返回 %d，期望 204 —— 搜索额度打光了不该连累回传"+
				"（两只桶共用的话，一个刚搜完、连点几件商品的真人会把自己下一次搜索的额度吃掉）：%s",
				i+1, w.Code, w.Body.String())
		}
	}
	w := postEventOn(t, limited, fx.HostA, body)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("回传额度 %d 用完后第 %d 次返回 %d，期望 429 —— /search/events 没挂限流",
			burst, burst+1, w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 没带 Retry-After")
	}
}
