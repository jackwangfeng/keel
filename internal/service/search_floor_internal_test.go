package service

import (
	"fmt"
	"testing"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/search"
)

func TestApplyFloor(t *testing.T) {
	r := func(id int64, vec, kw int) search.Ranked {
		return search.Ranked{Fused: search.Fused{ID: id, VectorRank: vec, KeywordRank: kw}}
	}
	byID := map[int64]repository.SearchHit{
		1: {ID: 1, Distance: 0.2},  // 0.80
		2: {ID: 2, Distance: 0.65}, // 0.35
		3: {ID: 3, Distance: 0.9},  // 0.10，但关键词也捞到了
		4: {ID: 4, Distance: 0.7},  // 0.30
	}
	ids := func(rs []search.Ranked) []int64 {
		var out []int64
		for _, x := range rs {
			out = append(out, x.ID)
		}
		return out
	}

	got, fb := applyFloor([]search.Ranked{r(1, 1, 0), r(2, 2, 0), r(3, 3, 1)}, byID, 0.40)
	if fb || len(got) != 2 || got[0].ID != 1 || got[1].ID != 3 {
		t.Fatalf("有可信命中：应留 [1 3]（关键词命中的 3 不看相似度）、fallback=false，实得 %v %v", ids(got), fb)
	}
	got, fb = applyFloor([]search.Ranked{r(2, 1, 0), r(4, 2, 0)}, byID, 0.40)
	if !fb || len(got) != 2 {
		t.Fatalf("一条可信命中都没有：原样返回、fallback=true，实得 %v %v", ids(got), fb)
	}
	got, fb = applyFloor([]search.Ranked{r(2, 1, 0), r(4, 2, 0)}, byID, -1)
	if fb || len(got) != 2 {
		t.Fatalf("floor < 0 关闭下限：原样、fallback=false，实得 %v %v", ids(got), fb)
	}
	if got, fb = applyFloor(nil, byID, 0.40); fb || len(got) != 0 {
		t.Fatalf("没有候选不是 fallback：%v %v", ids(got), fb)
	}
}

func TestExactTitleFirst(t *testing.T) {
	r := func(id int64) search.Ranked { return search.Ranked{Fused: search.Fused{ID: id}} }
	byID := map[int64]repository.SearchHit{
		18: {ID: 18, Title: "陶瓷马克杯 两只装", InStock: true},
		24: {ID: 24, Title: "陶瓷马克杯", InStock: true},
		30: {ID: 30, Title: "陶瓷 马克杯", InStock: false},
	}
	ids := func(rs []search.Ranked) []int64 {
		var out []int64
		for _, x := range rs {
			out = append(out, x.ID)
		}
		return out
	}
	got := ids(exactTitleFirst([]search.Ranked{r(18), r(30), r(24)}, byID, " 陶瓷马克杯 ", true))
	if fmt.Sprint(got) != "[24 18 30]" {
		t.Fatalf("精确同名且有货的挪到最前、其余顺序不变：%v", got)
	}
	// 无货的同名不挪（30 忽略空白后也同名，但缺货）。
	if got := ids(exactTitleFirst([]search.Ranked{r(18), r(30)}, byID, "陶瓷马克杯", true)); fmt.Sprint(got) != "[18 30]" {
		t.Fatalf("缺货的同名不挪：%v", got)
	}
	// 库存不知道时按有货。
	if got := ids(exactTitleFirst([]search.Ranked{r(18), r(30)}, byID, "陶瓷马克杯", false)); fmt.Sprint(got) != "[30 18]" {
		t.Fatalf("库存不知道时同名的照样挪：%v", got)
	}
	if got := ids(exactTitleFirst([]search.Ranked{r(18)}, byID, "  ", true)); fmt.Sprint(got) != "[18]" {
		t.Fatalf("空查询原样：%v", got)
	}
}
