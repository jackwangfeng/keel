package searcheval

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/keel/keel/internal/inference"
)

type fakeDecider struct {
	reqs []inference.SystemOneRequest
	p    map[string]float64 // 题号 → 「是」的概率
	err  error
}

func (f *fakeDecider) Decide(_ context.Context, r inference.SystemOneRequest) (*inference.SystemOneResponse, error) {
	f.reqs = append(f.reqs, r)
	if f.err != nil {
		return nil, f.err
	}
	resp := &inference.SystemOneResponse{Model: "kev-0.8b", ModelVersion: "v1", Answers: map[string]inference.SystemOneAnswer{}}
	for id := range r.Questions {
		if v, ok := f.p[id]; ok {
			resp.Answers[id] = inference.SystemOneAnswer{Type: inference.QuestionNoul, Noul: &v}
		}
	}
	return resp, nil
}

func TestLabelQueryOneRequestPerChunkSharingTheQuery(t *testing.T) {
	pairs := []Pair{
		{MerchantID: 1, Query: "复古地毯", ProductID: 11, Title: "栖木复古地毯", Subtitle: "客厅", Category: "家纺"},
		{MerchantID: 1, Query: "复古地毯", ProductID: 12, Title: "复古台灯", Category: "灯具"},
		{MerchantID: 1, Query: "复古地毯", ProductID: 13, Title: "羊毛地毯", Category: "家纺"},
	}
	f := &fakeDecider{p: map[string]float64{"p11": 0.95, "p12": 0.05, "p13": 0.8}}
	labels, err := LabelQuery(context.Background(), f, "kev-latest", pairs, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 2 || len(f.reqs[0].Questions) != 2 || len(f.reqs[1].Questions) != 1 {
		t.Fatalf("3 件、每次 2 道题应当是 2 次请求（2 + 1），实际 %d 次", len(f.reqs))
	}
	st := f.reqs[0].State.(map[string]string)
	if st["买家搜索的词"] != "复古地毯" || f.reqs[0].Model != "kev-latest" {
		t.Errorf("状态 / 模型不对：%v %q", st, f.reqs[0].Model)
	}
	q := f.reqs[0].Questions["p11"]
	ins, _ := q.Instructions.(string)
	if q.Type != inference.QuestionNoul || !strings.Contains(ins, "栖木复古地毯") || !strings.Contains(ins, "副标题：客厅") ||
		!strings.Contains(ins, "类目：家纺") {
		t.Errorf("题面没带上商品信息：%+v", q)
	}
	if ins12, _ := f.reqs[0].Questions["p12"].Instructions.(string); strings.Contains(ins12, "副标题") {
		t.Errorf("没有副标题的商品不该写一行空的副标题：%q", ins12)
	}
	want := map[int64]float64{11: 0.95, 12: 0.05, 13: 0.8}
	for _, l := range labels {
		if l.Relevance != want[l.ProductID] || l.Judge != "kev-0.8b@v1" || l.Query != "复古地毯" || l.MerchantID != 1 {
			t.Errorf("标签不对：%+v", l)
		}
	}
	if len(labels) != 3 {
		t.Errorf("应当 3 条标签，实际 %d", len(labels))
	}
}

func TestLabelQueryRefusesMixedQueriesAndMissingAnswers(t *testing.T) {
	mixed := []Pair{{MerchantID: 1, Query: "a", ProductID: 1}, {MerchantID: 1, Query: "b", ProductID: 2}}
	if _, err := LabelQuery(context.Background(), &fakeDecider{}, "", mixed, 10); err == nil {
		t.Error("混了两条查询应当报错")
	}
	one := []Pair{{MerchantID: 1, Query: "a", ProductID: 1}}
	if _, err := LabelQuery(context.Background(), &fakeDecider{p: map[string]float64{}}, "", one, 10); err == nil {
		t.Error("缺答案应当报错，而不是记成 0")
	}
	boom := errors.New("引擎挂了")
	if _, err := LabelQuery(context.Background(), &fakeDecider{err: boom}, "", one, 10); !errors.Is(err, boom) {
		t.Errorf("引擎的错误要原样传出来：%v", err)
	}
}

func TestGroupByQueryKeepsFirstSeenOrder(t *testing.T) {
	g := GroupByQuery([]Pair{
		{MerchantID: 1, Query: "b", ProductID: 1}, {MerchantID: 1, Query: "a", ProductID: 2},
		{MerchantID: 2, Query: "b", ProductID: 3}, {MerchantID: 1, Query: "b", ProductID: 4},
	})
	if len(g) != 3 || len(g[0]) != 2 || g[0][1].ProductID != 4 || g[1][0].Query != "a" || g[2][0].MerchantID != 2 {
		t.Fatalf("分组不对：%+v", g)
	}
}
