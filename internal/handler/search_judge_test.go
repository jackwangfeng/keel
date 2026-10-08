package handler_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 相关度预判（00230）在检索里的效果：只被向量路捞到的候选，判过的按预判留或去，不再看余弦下限。
//
// 「连衣裙」：真丝吊带长裙一个二元组都不沾，只从向量路进来（相似度 ≈0.99，余弦下限下一定留）；
// 手冲咖啡壶在另一根概念轴上（相似度很低，余弦下限下一定去）。预判把两件反过来判，结果也该跟着反过来。
// 查询前后多打空白，验证读写两边走的是同一个归一化。
func TestSearchHonorsRelevanceJudgments(t *testing.T) {
	fx := newSearchFixture(t)
	skirt, coffee := fx.IDsA[fxSkirt.Title], fx.IDsA[fxCoffee.Title]

	_, before := doSearch(t, fx.HostA, `{"query":"连衣裙"}`)
	if !contains(titlesOf(before), fxSkirt.Title) || contains(titlesOf(before), fxCoffee.Title) {
		t.Fatalf("前提不成立：没有预判时应当有长裙、没有咖啡壶：%v", titlesOf(before))
	}

	if _, err := admin(t).Exec(context.Background(),
		`INSERT INTO search_relevance_judgments (merchant_id, query, product_id, relevance, judge)
		 VALUES ($1, '连衣裙', $2, 0.05, 'test@1'), ($1, '连衣裙', $3, 0.9, 'test@1')`,
		fx.MerchantA, skirt, coffee); err != nil {
		t.Fatal(err)
	}
	_, after := doSearch(t, fx.HostA, `{"query":"  连衣裙 "}`)
	got := titlesOf(after)
	if contains(got, fxSkirt.Title) {
		t.Errorf("长裙被判不相关（0.05），不该再出现：%v", got)
	}
	if !contains(got, fxCoffee.Title) {
		t.Errorf("咖啡壶被判相关（0.9），余弦再低也该留下：%v", got)
	}
	if !contains(got, fxDress.Title) {
		t.Errorf("关键词命中的雪纺碎花连衣裙不受预判影响，应当还在：%v", got)
	}

	// B 店不受 A 店预判影响（RLS）。
	_, b := doSearch(t, fx.HostB, `{"query":"连衣裙"}`)
	if len(b.Items) == 0 {
		t.Error("B 店「连衣裙」应当照常有结果")
	}
}

// fakeJudge 是判别模型的替身：标题里带「裙」的判相关，其余判不相关；记下每次请求的题数。
type fakeJudge struct {
	mu      sync.Mutex
	version string
	calls   []int
}

func (f *fakeJudge) Decide(_ context.Context, r inference.SystemOneRequest) (*inference.SystemOneResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, len(r.Questions))
	resp := &inference.SystemOneResponse{Model: "fake-kev", ModelVersion: f.version, Answers: map[string]inference.SystemOneAnswer{}}
	for id, q := range r.Questions {
		v := 0.02
		if s, _ := q.Instructions.(string); containsRune(s, '裙') {
			v = 0.95
		}
		resp.Answers[id] = inference.SystemOneAnswer{Type: inference.QuestionNoul, Noul: &v}
	}
	return resp, nil
}

func containsRune(s string, r rune) bool { return slices.Contains([]rune(s), r) }

// 后台预判一轮：热词（≥ MinHits）才判；只判向量独有的候选（关键词 AND 命中的不判）；判过且新鲜的下一轮跳过；
// 换了判别模型版本则全部重判。
func TestSearchJudgeServiceJudgesHotQueriesOnce(t *testing.T) {
	fx := newSearchFixture(t)
	for range 3 {
		doSearch(t, fx.HostA, `{"query":"连衣裙"}`)
	}
	doSearch(t, fx.HostA, `{"query":"咖啡壶"}`) // 只搜了一次，不够热

	fj := &fakeJudge{version: "v1"}
	svc, err := service.NewSearchJudgeService(repository.New(testPool), conceptEmbedder{}, fj,
		service.SearchJudgeConfig{MinHits: 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Judge != "fake-kev@v1" || rep.Judged == 0 || rep.Failures != 0 {
		t.Fatalf("第一轮：%+v", rep)
	}

	type row struct {
		query   string
		product int64
		rel     float64
	}
	rows := func() []row {
		t.Helper()
		rs, err := adminSession(t).Query(context.Background(),
			`SELECT query, product_id, relevance FROM search_relevance_judgments WHERE merchant_id = $1 ORDER BY product_id`, fx.MerchantA)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		var out []row
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.query, &r.product, &r.rel); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	got := rows()
	var products []int64
	for _, r := range got {
		if r.query != "连衣裙" {
			t.Errorf("只该判热词「连衣裙」，判了 %q", r.query)
		}
		products = append(products, r.product)
	}
	if !slices.Contains(products, fx.IDsA[fxSkirt.Title]) {
		t.Errorf("向量独有的长裙应当被判：%v", got)
	}
	if slices.Contains(products, fx.IDsA[fxDress.Title]) {
		t.Errorf("关键词 AND 命中的雪纺碎花连衣裙不该判（检索里它一律可信）：%v", got)
	}
	if rep.Judged != len(got) {
		t.Errorf("报告说判了 %d 对，表里有 %d 行", rep.Judged, len(got))
	}

	// 第二轮：全都新鲜，一对都不判（只有探测那一次请求）。
	fj.calls = nil
	rep2, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Judged != 0 || rep2.Skipped != len(got) || len(fj.calls) != 1 {
		t.Errorf("第二轮应当全部跳过、只发探测：%+v，请求 %v", rep2, fj.calls)
	}

	// 换模型版本：全部重判。
	fj.version = "v2"
	rep3, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep3.Judged != len(got) {
		t.Errorf("换了判别模型应当全部重判：%+v", rep3)
	}
}
