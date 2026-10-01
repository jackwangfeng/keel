package handler_test

import (
	"slices"
	"testing"
	"time"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/search"
	"github.com/keel/keel/internal/tenant"
)

// 离线评测集的取数（repository.SearchEvalTx，cmd/keel-searcheval 用）：
// 近邻按距离排、草稿与软删不出现、关键词 AND 只要全部命中的、只被关键词捞到的也算得出距离、
// 查询按次数聚合 —— 全程只看得到本店（RLS），B 店的商品与检索日志一条都不出现。
func TestSearchEvalQueriesStayInsideTheTenant(t *testing.T) {
	fx := newSearchFixture(t)
	for range 3 {
		doSearch(t, fx.HostA, `{"query":"连衣裙"}`)
	}
	doSearch(t, fx.HostA, `{"query":"咖啡壶"}`)
	doSearch(t, fx.HostB, `{"query":"B店独有的查询"}`)

	ctx := tenant.NewContext(t.Context(), fx.MerchantA)
	repo := repository.New(testPool)
	idsB := make([]int64, 0, len(fx.IDsB))
	for _, id := range fx.IDsB {
		idsB = append(idsB, id)
	}
	vec := conceptVector("连衣裙")

	err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		qs, err := tx.SearchEvalQueries(ctx, time.Now().Add(-time.Hour), 10)
		if err != nil {
			return err
		}
		got := map[string]int64{}
		for _, q := range qs {
			got[q.Query] = q.Hits
		}
		if got["连衣裙"] != 3 || got["咖啡壶"] != 1 || qs[0].Query != "连衣裙" {
			t.Errorf("查询聚合 = %v，期望 连衣裙×3 排第一、咖啡壶×1", qs)
		}
		if _, leaked := got["B店独有的查询"]; leaked {
			t.Error("读到了 B 店的检索日志")
		}

		nb, err := tx.SearchEvalVectorNeighbors(ctx, vec, 50)
		if err != nil {
			return err
		}
		var ids []int64
		for i, c := range nb {
			ids = append(ids, c.ID)
			if i > 0 && c.Distance < nb[i-1].Distance {
				t.Errorf("近邻没按距离排：%v", nb)
			}
		}
		if len(nb) == 0 || nb[0].CategoryName != "女装" {
			t.Errorf("「连衣裙」最近的应当是女装：%+v", nb)
		}
		for _, bad := range []int64{fx.IDsA[fxDraft.Title], fx.IDsA[fxDeleted.Title]} {
			if slices.Contains(ids, bad) {
				t.Errorf("草稿 / 软删商品 %d 出现在近邻里", bad)
			}
		}
		for _, b := range idsB {
			if slices.Contains(ids, b) {
				t.Errorf("近邻里出现了 B 店的 %d", b)
			}
		}

		and, _ := search.TSQueryAnd("咖啡壶")
		kw, err := tx.SearchEvalKeywordHits(ctx, and, 50)
		if err != nil {
			return err
		}
		if len(kw) != 1 || kw[0].ID != fx.IDsA[fxCoffee.Title] || kw[0].Rank <= 0 {
			t.Errorf("「咖啡壶」AND 应当只命中手冲咖啡壶：%+v", kw)
		}

		d, err := tx.SearchEvalDistances(ctx, vec, append([]int64{fx.IDsA[fxCoffee.Title]}, idsB...))
		if err != nil {
			return err
		}
		if len(d) != 1 {
			t.Errorf("距离应当只算出本店那一件，得到 %v", d)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
