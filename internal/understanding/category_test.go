package understanding

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/keel/keel/internal/inference"
)

// keywordEmbedder 按关键词给向量：文本里含第 i 个关键词就在第 i 维上加 1。
// 它让「哪个类目离哪条标题近」可预测，同时记下每次调用送了哪些文本 ——
// 缓存那几条断言靠它数「类目向量有没有被重算」。
type keywordEmbedder struct {
	words   []string
	version string
	calls   [][]string
	err     error
}

func (e *keywordEmbedder) Embed(_ context.Context, texts []string) (*inference.Result, error) {
	e.calls = append(e.calls, append([]string(nil), texts...))
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, inference.Dim)
		for j, w := range e.words {
			if strings.Contains(t, w) {
				v[j] = 1
			}
		}
		v[inference.Dim-1] = 0.05 // 保证非零
		out[i] = v
	}
	return &inference.Result{Vectors: out, Model: inference.ModelName, ModelVersion: e.version}, nil
}

var testOptions = []CategoryOption{
	{ID: 1, PathName: "服装 > 女装 > 连衣裙"},
	{ID: 2, PathName: "食品饮料 > 咖啡"},
	{ID: 3, PathName: "数码 > 耳机"},
	{ID: 4, PathName: "家居 > 香薰蜡烛"},
}

func TestRecommendRanksByCosineAndCutsTopK(t *testing.T) {
	emb := &keywordEmbedder{words: []string{"裙", "咖啡", "耳机", "蜡烛"}, version: "v1"}
	rec := NewCategoryRecommender(emb)
	res, err := rec.Recommend(context.Background(),
		[]string{CategoryQueryText("雪纺连衣裙", "夏季"), CategoryQueryText("挂耳咖啡", "")}, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || len(res[0]) != CategoryTopK || len(res[1]) != CategoryTopK {
		t.Fatalf("每条查询应当给 %d 个候选：%v", CategoryTopK, res)
	}
	if res[0][0].ID != 1 || res[1][0].ID != 2 {
		t.Errorf("Top-1 不对：%v / %v", res[0][0], res[1][0])
	}
	if !(res[0][0].Score > res[0][1].Score) {
		t.Errorf("候选应当按分数降序：%v", res[0])
	}
	if !(CategoryGate{MinScore: 0.9, MinMargin: 0.1}).Confident(res[0]) {
		t.Errorf("分数与分差都够，应当自动选：%v", res[0])
	}
	if (CategoryGate{MinScore: 1.01}).Confident(res[0]) {
		t.Error("分数不够不能自动选")
	}
	if (CategoryGate{}).Confident(nil) {
		t.Error("没有候选不能自动选")
	}
	// 一次推荐只打一次引擎：查询与类目合成一批（设计 §6 禁止循环单条调用）。
	if len(emb.calls) != 1 || len(emb.calls[0]) != 2+len(testOptions) {
		t.Errorf("应当一次调用送 %d 条，实际 %v", 2+len(testOptions), emb.calls)
	}
}

func TestCategoryVectorsAreCachedByPathText(t *testing.T) {
	emb := &keywordEmbedder{words: []string{"裙", "咖啡", "耳机", "蜡烛"}, version: "v1"}
	rec := NewCategoryRecommender(emb)
	ctx := context.Background()
	if _, err := rec.Recommend(ctx, []string{"q1"}, testOptions); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Recommend(ctx, []string{"q2"}, testOptions); err != nil {
		t.Fatal(err)
	}
	if got := emb.calls[1]; len(got) != 1 || got[0] != "q2" {
		t.Fatalf("第二次推荐不该重算任何类目向量，实际送了 %v", got)
	}

	// 改名 = 路径文本变了 = 缓存键变了：只重算改了名的那一个。
	renamed := append([]CategoryOption(nil), testOptions...)
	renamed[2] = CategoryOption{ID: 3, PathName: "数码 > 耳机音箱"}
	if _, err := rec.Recommend(ctx, []string{"q3"}, renamed); err != nil {
		t.Fatal(err)
	}
	if got := emb.calls[2]; len(got) != 2 || got[1] != "数码 > 耳机音箱" {
		t.Fatalf("改名之后应当只补算新名字，实际送了 %v", got)
	}
}

func TestModelVersionChangeInvalidatesCache(t *testing.T) {
	emb := &keywordEmbedder{words: []string{"裙"}, version: "v1"}
	rec := NewCategoryRecommender(emb)
	ctx := context.Background()
	if _, err := rec.Recommend(ctx, []string{"q1"}, testOptions); err != nil {
		t.Fatal(err)
	}
	emb.version = "v2"
	if _, err := rec.Recommend(ctx, []string{"q2"}, testOptions); err != nil {
		t.Fatal(err)
	}
	// 第二次：先送查询（此时以为类目都在缓存里），发现版本变了 → 类目整批按新模型重算。
	if len(emb.calls) != 3 {
		t.Fatalf("换模型之后应当补一次调用重算全部类目，实际 %d 次：%v", len(emb.calls), emb.calls)
	}
	if got := emb.calls[2]; len(got) != len(testOptions) {
		t.Errorf("重算的应当是全部 %d 个类目，实际 %v", len(testOptions), got)
	}
	if rec.version != "v2" {
		t.Errorf("缓存版本应当跟到 v2，实际 %q", rec.version)
	}
}

func TestRecommendDegradesWithoutEngine(t *testing.T) {
	if _, err := NewCategoryRecommender(nil).Recommend(context.Background(), []string{"x"}, testOptions); !errors.Is(err, ErrNoEmbedder) {
		t.Errorf("没配引擎应当是 ErrNoEmbedder，得到 %v", err)
	}
	var nilRec *CategoryRecommender
	if _, err := nilRec.Recommend(context.Background(), []string{"x"}, testOptions); !errors.Is(err, ErrNoEmbedder) {
		t.Errorf("nil 推荐器应当是 ErrNoEmbedder，得到 %v", err)
	}
	emb := &keywordEmbedder{err: fmt.Errorf("%w: connection refused", inference.ErrUnavailable)}
	_, err := NewCategoryRecommender(emb).Recommend(context.Background(), []string{"x"}, testOptions)
	if !errors.Is(err, inference.ErrUnavailable) {
		t.Errorf("引擎挂了应当原样带出 ErrUnavailable，得到 %v", err)
	}
	// 没有类目可选时不打引擎。
	emb2 := &keywordEmbedder{version: "v1"}
	res, err := NewCategoryRecommender(emb2).Recommend(context.Background(), []string{"x"}, nil)
	if err != nil || len(res) != 1 || len(res[0]) != 0 || len(emb2.calls) != 0 {
		t.Errorf("没有类目时应当返回空候选且不调引擎：%v %v %v", res, err, emb2.calls)
	}
}

func TestQueryTextCarriesInstructionAndSubtitle(t *testing.T) {
	q := CategoryQueryText(" 连衣裙 ", " 夏季 ")
	if !strings.HasPrefix(q, "Instruct: ") || !strings.HasSuffix(q, "连衣裙 夏季") {
		t.Errorf("查询文本形状不对：%q", q)
	}
	if CategoryQueryText("连衣裙", "") != categoryQueryInstruct+"连衣裙" {
		t.Error("副标题为空时不该多一个空格")
	}
}

func TestGateNeedsBothScoreAndMargin(t *testing.T) {
	g := CategoryGate{MinScore: 0.5, MinMargin: 0.03}
	for _, c := range []struct {
		cands []CategoryCandidate
		want  bool
	}{
		{[]CategoryCandidate{{Score: 0.70}, {Score: 0.60}}, true},
		{[]CategoryCandidate{{Score: 0.70}, {Score: 0.68}}, false}, // 分差不够：挤在一起
		{[]CategoryCandidate{{Score: 0.45}, {Score: 0.10}}, false}, // 分数不够：矮子里拔将军
		{[]CategoryCandidate{{Score: 0.55}}, true},                 // 只有一个类目，只看分数
		{[]CategoryCandidate{{Score: 0.40}}, false},
	} {
		if got := g.Confident(c.cands); got != c.want {
			t.Errorf("Confident(%v) = %v，期望 %v", c.cands, got, c.want)
		}
	}
}
