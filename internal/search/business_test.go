package search_test

import (
	"math"
	"testing"

	"github.com/keel/keel/internal/search"
)

// 业务重排（语义检索层 §6）的纯函数闸门。

// 缺货的那条即便在两路里都是第一名，也要排到有货的后面，
// 而且分数是**乘**出来的（final = rrf × 0.05），不是减出来的。
func TestOutOfStockSinksBelowInStockMultiplicatively(t *testing.T) {
	// 10 两路都第一；20、30 各只在一路里。
	fused := search.FuseRRF([]int64{10, 20}, []int64{10, 30})
	got, missing := search.RerankByBusiness(fused, map[int64]search.BusinessSignals{
		10: {InStock: false},
		20: {InStock: true},
		30: {InStock: true},
	})
	if missing != 0 {
		t.Fatalf("missing = %d，期望 0", missing)
	}
	var order []int64
	for _, r := range got {
		order = append(order, r.ID)
	}
	if order[len(order)-1] != 10 {
		t.Fatalf("顺序 %v：两路第一但缺货的 10 应当排在最后", order)
	}
	for _, r := range got {
		want := r.Score * search.StockFactorInStock
		if r.ID == 10 {
			want = r.Score * search.StockFactorOutOfStock
			if r.Business != search.StockFactorOutOfStock {
				t.Errorf("10 的 business = %v，期望 %v", r.Business, search.StockFactorOutOfStock)
			}
		}
		if math.Abs(r.Final-want) > 1e-12 {
			t.Errorf("%d 的 final = %v，期望 rrf×business = %v", r.ID, r.Final, want)
		}
	}
}

// 文件头第一节那笔账：在契约允许的最大召回面上（size=100 × RecallMultiplier=3，
// 每一路 300 条），缺货因子要把**最好的**缺货候选压到**最差的**有货候选之下。
// 哪天有人把 0.05 调大、或者把召回倍数调大，这条先红，逼人重算。
func TestStockFactorBeatsTheWholeRRFRange(t *testing.T) {
	const perRoute = 300
	best := 2.0 / float64(search.RRFK+1)         // 两路都第一
	worst := 1.0 / float64(search.RRFK+perRoute) // 单路最后一名
	if best*search.StockFactorOutOfStock >= worst*search.StockFactorInStock {
		t.Fatalf("最好的缺货候选 %.5f ≥ 最差的有货候选 %.5f —— 缺货不再保证排在有货之后",
			best*search.StockFactorOutOfStock, worst)
	}
}

// 乘子相同的候选之间，重排不改变融合时的先后。
func TestBusinessRerankKeepsFusedOrderWhenFactorsTie(t *testing.T) {
	fused := search.FuseRRF([]int64{5, 4, 3, 2, 1}, nil)
	sig := map[int64]search.BusinessSignals{}
	for _, f := range fused {
		sig[f.ID] = search.BusinessSignals{InStock: true}
	}
	got, _ := search.RerankByBusiness(fused, sig)
	for i := range fused {
		if got[i].ID != fused[i].ID {
			t.Fatalf("第 %d 位：融合是 %d，重排后成了 %d —— 全部有货时顺序不该变",
				i, fused[i].ID, got[i].ID)
		}
	}
}
