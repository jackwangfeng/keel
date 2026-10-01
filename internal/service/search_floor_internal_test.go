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

	got, fb := applyFloor([]search.Ranked{r(1, 1, 0), r(2, 2, 0), r(3, 3, 1)}, byID, nil, 0.40)
	if fb || len(got) != 2 || got[0].ID != 1 || got[1].ID != 3 {
		t.Fatalf("有可信命中：应留 [1 3]（关键词命中的 3 不看相似度）、fallback=false，实得 %v %v", ids(got), fb)
	}
	got, fb = applyFloor([]search.Ranked{r(2, 1, 0), r(4, 2, 0)}, byID, nil, 0.40)
	if !fb || len(got) != 2 {
		t.Fatalf("一条可信命中都没有：原样返回、fallback=true，实得 %v %v", ids(got), fb)
	}
	got, fb = applyFloor([]search.Ranked{r(2, 1, 0), r(4, 2, 0)}, byID, nil, -1)
	if fb || len(got) != 2 {
		t.Fatalf("floor < 0 关闭下限：原样、fallback=false，实得 %v %v", ids(got), fb)
	}
	if got, fb = applyFloor(nil, byID, nil, 0.40); fb || len(got) != 0 {
		t.Fatalf("没有候选不是 fallback：%v %v", ids(got), fb)
	}

	// 有相关度预判的按预判，不看余弦：1（0.80）被判不相关 → 去；4（0.30）被判相关 → 留；2 没判过 → 照旧按下限去。
	judged := map[int64]float64{1: 0.05, 4: JudgedRelevantAt}
	got, fb = applyFloor([]search.Ranked{r(1, 1, 0), r(2, 2, 0), r(4, 3, 0)}, byID, judged, 0.40)
	if fb || fmt.Sprint(ids(got)) != "[4]" {
		t.Fatalf("预判优先于余弦：应只留 [4]，实得 %v %v", ids(got), fb)
	}
	// 关键词命中的不受预判影响（3 被判 0 也留）。
	got, _ = applyFloor([]search.Ranked{r(3, 1, 1)}, byID, map[int64]float64{3: 0}, 0.40)
	if fmt.Sprint(ids(got)) != "[3]" {
		t.Fatalf("关键词命中的不看预判：%v", ids(got))
	}
	// 下限关了（floor < 0）也照样按预判去掉判不相关的。
	got, fb = applyFloor([]search.Ranked{r(1, 1, 0), r(2, 2, 0)}, byID, map[int64]float64{1: 0.1}, -1)
	if fb || fmt.Sprint(ids(got)) != "[2]" {
		t.Fatalf("floor < 0 时预判仍生效：应留 [2]，实得 %v %v", ids(got), fb)
	}
}

func TestCoversPageUsesJudgments(t *testing.T) {
	vec := []repository.SearchHit{{ID: 1, Distance: 0.2}, {ID: 2, Distance: 0.7}, {ID: 3, Distance: 0.7}}
	// 无预判：只有 1 过 0.40 → 凑不够 2 件。
	if coversPage(nil, vec, nil, 0.40, 2) {
		t.Error("无预判时只有 1 件可信，不该算凑够")
	}
	// 预判把 2、3 判成相关、1 判成不相关 → 2 件可信，凑够。
	if !coversPage(nil, vec, map[int64]float64{1: 0, 2: 0.9, 3: 0.9}, 0.40, 2) {
		t.Error("预判后有 2 件可信，应当凑够")
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
