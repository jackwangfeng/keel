package searcheval

import (
	"bytes"
	"math"
	"testing"
)

func sim(v float64) *float64 { return &v }

// 手算一份：两条查询。
//
//	「复古地毯」：A 关键词命中（不进混淆矩阵）；B 0.70 相关、C 0.45 不相关、D 0.35 相关 —— 三件只被向量路捞到。
//	「量子计算机」：E 0.43、F 0.41，都不相关、都只被向量路捞到 —— 本该「猜你想要」。
//
// 下限 0.40：保留 B C D? 不——D 0.35 < 0.40 被切掉。保留 B（TP）C（FP）E F（FP）；切掉 D（FN）。
//
//	TP=1 FP=3 FN=1 → P=0.25 R=0.5；量子计算机有可信命中 → 没 fallback（FallbackRight=0）。
//
// 下限 0.50：保留 B（TP）；C E F 切掉；D 切掉（FN）。TP=1 FP=0 FN=1 → P=1 R=0.5；
// 量子计算机一条可信都没有 → fallback 判对（FallbackRight=1）；复古地毯有关键词命中 → 不 fallback（Wrong=0）。
func TestCalibrateHandComputed(t *testing.T) {
	pairs := []Pair{
		{MerchantID: 1, Query: "复古地毯", ProductID: 1, VectorRank: 0, KeywordAnd: true, KeywordOr: true, Similarity: sim(0.80)},
		{MerchantID: 1, Query: "复古地毯", ProductID: 2, VectorRank: 1, Similarity: sim(0.70)},
		{MerchantID: 1, Query: "复古地毯", ProductID: 3, VectorRank: 2, Similarity: sim(0.45)},
		{MerchantID: 1, Query: "复古地毯", ProductID: 4, VectorRank: 3, Similarity: sim(0.35)},
		{MerchantID: 1, Query: "量子计算机", ProductID: 5, VectorRank: 1, Similarity: sim(0.43)},
		{MerchantID: 1, Query: "量子计算机", ProductID: 6, VectorRank: 2, Similarity: sim(0.41)},
		{MerchantID: 1, Query: "量子计算机", ProductID: 7, VectorRank: 3, Similarity: sim(0.30)}, // 没标签
	}
	labels := []Label{
		{MerchantID: 1, Query: "复古地毯", ProductID: 1, Relevance: 1, Judge: "human"},
		{MerchantID: 1, Query: "复古地毯", ProductID: 2, Relevance: 0.9, Judge: "kev"},
		{MerchantID: 1, Query: "复古地毯", ProductID: 3, Relevance: 0.1, Judge: "kev"},
		{MerchantID: 1, Query: "复古地毯", ProductID: 4, Relevance: 1, Judge: "human"},
		{MerchantID: 1, Query: "量子计算机", ProductID: 5, Relevance: 0.2, Judge: "kev"},
		// 同一个键两条标签取平均：(0.6 + 0.2) / 2 = 0.4 < 0.5 → 不相关。
		{MerchantID: 1, Query: "量子计算机", ProductID: 6, Relevance: 0.6, Judge: "kev"},
		{MerchantID: 1, Query: "量子计算机", ProductID: 6, Relevance: 0.2, Judge: "human"},
	}
	rep := Calibrate(pairs, labels, Options{From: 0.40, To: 0.50, Step: 0.10})

	if rep.Pairs != 6 || rep.Unlabeled != 1 || rep.VectorOnly != 5 || rep.Relevant != 2 ||
		rep.Queries != 2 || rep.NothingRelevant != 1 {
		t.Fatalf("计数不对：%+v", rep)
	}
	if len(rep.Rows) != 2 {
		t.Fatalf("扫描档数 = %d，期望 2", len(rep.Rows))
	}
	want := []Row{
		{Floor: 0.40, TP: 1, FP: 3, FN: 1, Precision: 0.25, Recall: 0.5, FallbackRight: 0, FallbackWrong: 0},
		{Floor: 0.50, TP: 1, FP: 0, FN: 1, Precision: 1, Recall: 0.5, FallbackRight: 1, FallbackWrong: 0},
	}
	for i, w := range want {
		g := rep.Rows[i]
		if g.Floor != w.Floor || g.TP != w.TP || g.FP != w.FP || g.FN != w.FN ||
			!near(g.Precision, w.Precision) || !near(g.Recall, w.Recall) ||
			!near(g.FallbackRight, w.FallbackRight) || !near(g.FallbackWrong, w.FallbackWrong) {
			t.Errorf("下限 %.2f：得到 %+v，期望 %+v", w.Floor, g, w)
		}
	}
	if rep.BestF1 == nil || rep.BestF1.Floor != 0.50 {
		t.Errorf("F1 最高的应当是 0.50：%+v", rep.BestF1)
	}
	if rep.AtPrecision == nil || rep.AtPrecision.Floor != 0.50 {
		t.Errorf("精确率 ≥0.9 的最低下限应当是 0.50：%+v", rep.AtPrecision)
	}
	if len(rep.Judges) != 2 || rep.Judges[0] != "human" || rep.Judges[1] != "kev" {
		t.Errorf("标签来源 = %v", rep.Judges)
	}
}

// 有相关候选、却在高下限下一条可信都没有的查询，记作误判的「猜你想要」。
func TestCalibrateCountsWrongFallback(t *testing.T) {
	pairs := []Pair{{MerchantID: 1, Query: "提神的饮品", ProductID: 1, VectorRank: 1, Similarity: sim(0.55)}}
	labels := []Label{{MerchantID: 1, Query: "提神的饮品", ProductID: 1, Relevance: 1}}
	rep := Calibrate(pairs, labels, Options{From: 0.60, To: 0.60, Step: 0.01})
	if len(rep.Rows) != 1 || rep.Rows[0].FallbackWrong != 1 || rep.Rows[0].FN != 1 {
		t.Fatalf("得到 %+v", rep.Rows)
	}
}

func TestJSONLRoundTrip(t *testing.T) {
	in := []Pair{{MerchantID: 3, Query: "连衣裙", ProductID: 9, Title: "雪纺<碎花>", Similarity: sim(0.5), VectorRank: 1}}
	var buf bytes.Buffer
	if err := WriteJSONL(&buf, in); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte(`\u003c`)) {
		t.Errorf("尖括号被转义了，打标的人读不顺：%s", buf.String())
	}
	out, err := ReadJSONL[Pair](&buf)
	if err != nil || len(out) != 1 || out[0].Title != in[0].Title || *out[0].Similarity != 0.5 {
		t.Fatalf("往返不一致：%v %+v", err, out)
	}
	if _, err := ReadJSONL[Pair](bytes.NewBufferString("{}\nnot json\n")); err == nil {
		t.Error("坏行应当报错")
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCalibrateRecommendsNothingWhenNothingIsRelevant(t *testing.T) {
	pairs := []Pair{{MerchantID: 1, Query: "量子计算机", ProductID: 1, VectorRank: 1, Similarity: sim(0.45)}}
	labels := []Label{{MerchantID: 1, Query: "量子计算机", ProductID: 1, Relevance: 0}}
	rep := Calibrate(pairs, labels, Options{})
	if rep.BestF1 != nil || rep.AtPrecision != nil {
		t.Errorf("一个相关的都没有时不该推荐下限：best=%+v at=%+v", rep.BestF1, rep.AtPrecision)
	}
}
