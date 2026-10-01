package handler_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/keel/keel/internal/handler"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/search"
	"github.com/keel/keel/internal/tenant"
)

// 2026-10 复压第十节 ③：AND 不够一页时，向量路的可信命中能凑够这一页就不跑 OR（match=and+vector）；
// 向量路没跑成（引擎挂了）时 OR 照跑（match=and+or）。
//
// 「碎花真丝裙」切成「碎花 花真 真丝 丝裙」，夹具里没有一件四个都命中 → AND 为 0；
// 夹具的 conceptEmbedder 把带「裙」的都放在同一根轴上（相似度 ≈0.99，远高于 VectorFloor 0.40），
// 所以 size = 2 时向量路一定凑得够。search_logs 那一行写得进去，也顺带证明 00220 放开了约束。
func TestKeywordRecallSkipsOrWhenVectorCoversThePage(t *testing.T) {
	fx := newSearchFixture(t)
	const body = `{"query":"碎花真丝裙","size":2,"explain":true}`

	match := func(hdr string) string {
		m, _, _ := strings.Cut(hdr, ";")
		return strings.TrimPrefix(m, "match=")
	}
	logged := func(trace string) string {
		t.Helper()
		var m *string
		if err := admin(t).QueryRow(context.Background(),
			`SELECT keyword_match FROM search_logs WHERE trace_id = $1`, trace).Scan(&m); err != nil {
			t.Fatal(err)
		}
		if m == nil {
			return "<NULL>"
		}
		return *m
	}

	w, res := doSearch(t, fx.HostA, body)
	if w.Code != 200 {
		t.Fatalf("检索返回 %d：%s", w.Code, w.Body.String())
	}
	hdr := w.Header().Get(handler.SearchKeywordExplainHeader)
	if got := match(hdr); got != "and+vector" {
		t.Errorf("引擎好的时候诊断头 = %q，期望 match=and+vector（向量路凑够了一页，OR 不该跑）", hdr)
	}
	if !strings.Contains(hdr, "and_hits=0;") {
		t.Errorf("夹具前提不成立：%q 应当一件都 AND 不上，诊断头 %q", "碎花真丝裙", hdr)
	}
	if len(res.Items) != 2 {
		t.Errorf("向量路凑够一页，应当返回 2 件，实际 %v", titlesOf(res))
	}
	trace, _ := res.raw["trace_id"].(string)
	if got := logged(trace); got != "and+vector" {
		t.Errorf("search_logs.keyword_match = %s，期望 and+vector", got)
	}

	// 引擎挂了：向量路没有，OR 必须照跑，否则长尾词在降级时什么都搜不到。
	down := routerWithDeadEngine(t)
	w, res = searchOn(t, down, fx.HostA, body)
	if w.Code != 200 {
		t.Fatalf("引擎挂了之后检索返回 %d：%s", w.Code, w.Body.String())
	}
	if hdr := w.Header().Get(handler.SearchKeywordExplainHeader); match(hdr) != "and+or" {
		t.Errorf("引擎挂了之后诊断头 = %q，期望 match=and+or", hdr)
	}
	if len(res.Items) == 0 {
		t.Error("引擎挂了之后 OR 没补上任何结果")
	}
}

// OR 那条的命中集合封顶：hitCap 之内照常排序截断，超过的件数进不了内层打分。
func TestKeywordRecallHitCapBoundsTheCandidateSet(t *testing.T) {
	fx := newSearchFixture(t)
	var items []kwProduct
	for i := 0; i < 30; i++ {
		items = append(items, kwProduct{Title: fmt.Sprintf("栖木收纳盒 %02d", i), Subtitle: "卧室", Cents: 1000})
	}
	addKeywordProducts(t, fx, fx.MerchantA, items)

	ctx := tenant.NewContext(t.Context(), fx.MerchantA)
	repo := repository.New(testPool)
	count := func(hitCap int32) int {
		t.Helper()
		var n int
		if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
			hits, err := tx.SearchProductsByKeyword(ctx, fx.ScopeA(), search.TSQueryOr("栖木"),
				repository.SearchFilters{}, 100, hitCap)
			n = len(hits)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(0); n != 30 {
		t.Fatalf("不封顶应当返回全部 30 件，实际 %d", n)
	}
	if n := count(7); n != 7 {
		t.Errorf("hitCap = 7 应当只剩 7 件，实际 %d", n)
	}
}
