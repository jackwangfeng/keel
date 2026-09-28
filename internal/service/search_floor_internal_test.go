package service

import (
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
