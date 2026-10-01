package handler_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/keel/keel/internal/handler"
)

// ② 先 AND、不够再 OR。
//
//	· size = 2、全部词都命中的有 2 件：只用 AND（match=and），只命中一个词的两件**不在**召回里
//	  （search_logs.recall_ids 是融合的全部候选，比响应里截到 size 的那几条更能说明问题）；
//	· size = 20：AND 只有 2 件不够一页，用 OR 补齐（match=and+or），全部命中的排在只命中一部分的前面；
//	· 单词查询：match=single。
//	· 全程 B 店那件「复古地毯」不出现（跨店隔离在 AND 那条上同样成立）。
//
// 走了哪一条从 explain 的诊断头与 search_logs 的三列（00210）两处看。
func TestKeywordRecallIsAndFirstThenOr(t *testing.T) {
	fx := newSearchFixture(t)
	idsA := addKeywordProducts(t, fx, fx.MerchantA, []kwProduct{
		{Title: "栖木复古地毯", Subtitle: "客厅", Cents: 19900},
		{Title: "栖木复古地毯 加厚", Subtitle: "卧室", Cents: 25900},
		{Title: "复古台灯", Subtitle: "书房", Cents: 9900},
		{Title: "羊毛地毯", Subtitle: "客厅", Cents: 39900},
	})
	idsB := addKeywordProducts(t, fx, fx.MerchantB, []kwProduct{
		{Title: "复古地毯 B店", Subtitle: "客厅", Cents: 19900},
	})
	full := []int64{idsA["栖木复古地毯"], idsA["栖木复古地毯 加厚"]}
	partial := []int64{idsA["复古台灯"], idsA["羊毛地毯"]}
	other := idsB["复古地毯 B店"]

	type logRow struct {
		recall      []int64
		match       *string
		limit, hits *int32
	}
	run := func(query string, size int) (string, []int64, logRow) {
		t.Helper()
		w, body := doSearch(t, fx.HostA, fmt.Sprintf(`{"query":%q,"size":%d,"explain":true}`, query, size))
		if w.Code != 200 {
			t.Fatalf("%q size=%d：%d %s", query, size, w.Code, w.Body.String())
		}
		var got []int64
		for _, it := range body.Items {
			got = append(got, int64(it["id"].(float64)))
		}
		trace, _ := body.raw["trace_id"].(string)
		if trace == "" {
			t.Fatal("检索没有回 trace_id")
		}
		var lr logRow
		if err := admin(t).QueryRow(context.Background(),
			`SELECT recall_ids, keyword_match, keyword_limit, keyword_hits FROM search_logs WHERE trace_id = $1`,
			trace).Scan(&lr.recall, &lr.match, &lr.limit, &lr.hits); err != nil {
			t.Fatal(err)
		}
		return w.Header().Get(handler.SearchKeywordExplainHeader), got, lr
	}
	str := func(p *string) string {
		if p == nil {
			return "<NULL>"
		}
		return *p
	}
	i32 := func(p *int32) int32 {
		if p == nil {
			return -1
		}
		return *p
	}

	// AND 够一页。
	hdr, got, lr := run("复古地毯", 2)
	if want := "match=and; limit=6; and_hits=2; hits=2"; hdr != want {
		t.Errorf("AND 够一页时诊断头 = %q，期望 %q", hdr, want)
	}
	if str(lr.match) != "and" || i32(lr.limit) != 6 || i32(lr.hits) != 2 {
		t.Errorf("search_logs 记的是 match=%s limit=%d hits=%d，期望 and / 6 / 2", str(lr.match), i32(lr.limit), i32(lr.hits))
	}
	if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(full))) {
		t.Errorf("AND 够一页时返回 %v，期望正好是全部命中的那两件 %v", got, full)
	}
	for _, id := range append(slices.Clone(partial), other) {
		if slices.Contains(lr.recall, id) {
			t.Errorf("AND 够一页时召回里混进了 %d（只命中部分词，或是 B 店的）：recall_ids=%v", id, lr.recall)
		}
	}

	// AND 不够一页，OR 补齐。
	hdr, got, lr = run("复古地毯", 20)
	// hits 不写死：夹具里原有的商品（裙子的副标题）也可能命中「复古」，那也是 OR 该补进来的。
	if want := "match=and+or; limit=60; and_hits=2; hits="; !strings.HasPrefix(hdr, want) {
		t.Errorf("AND 不够时诊断头 = %q，期望以 %q 开头", hdr, want)
	}
	if str(lr.match) != "and+or" || i32(lr.limit) != 60 || i32(lr.hits) < 4 {
		t.Errorf("search_logs 记的是 match=%s limit=%d hits=%d，期望 and+or / 60 / ≥4", str(lr.match), i32(lr.limit), i32(lr.hits))
	}
	pos := func(id int64) int { return slices.Index(got, id) }
	for _, p := range partial {
		if pos(p) < 0 {
			t.Errorf("AND 不够时 OR 应当把只命中部分词的 %d 补进来：%v", p, got)
			continue
		}
		for _, f := range full {
			if pos(f) < 0 || pos(f) > pos(p) {
				t.Errorf("全部命中的 %d 应当排在只命中部分词的 %d 前面：%v", f, p, got)
			}
		}
	}
	if slices.Contains(got, other) || slices.Contains(lr.recall, other) {
		t.Errorf("A 店搜到了 B 店的 %d", other)
	}

	// 单词查询。
	hdr, _, lr = run("地毯", 20)
	if !strings.HasPrefix(hdr, "match=single; limit=60;") || str(lr.match) != "single" {
		t.Errorf("单词查询诊断头 = %q、search_logs.keyword_match = %s，期望 single", hdr, str(lr.match))
	}

	// 不 explain 时没有这个头（它只给排查的人看）。
	w, _ := doSearch(t, fx.HostA, `{"query":"复古地毯"}`)
	if h := w.Header().Get(handler.SearchKeywordExplainHeader); h != "" {
		t.Errorf("explain=false 时不该回诊断头，回了 %q", h)
	}
}
