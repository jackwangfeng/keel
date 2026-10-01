package service

import (
	"slices"
	"testing"

	"github.com/keel/keel/internal/repository"
)

// mergeAndThenOr：AND 的原样在前，OR 里重复的跳过、其余按原顺序接上，总数截到 limit。
func TestMergeAndThenOr(t *testing.T) {
	hits := func(ids ...int64) []repository.SearchHit {
		out := make([]repository.SearchHit, 0, len(ids))
		for _, id := range ids {
			out = append(out, repository.SearchHit{ID: id})
		}
		return out
	}
	idsOf := func(hs []repository.SearchHit) []int64 {
		out := make([]int64, 0, len(hs))
		for _, h := range hs {
			out = append(out, h.ID)
		}
		return out
	}
	cases := []struct {
		name     string
		and, or  []repository.SearchHit
		limit    int
		expected []int64
	}{
		{"OR 里含 AND 的那几件", hits(5, 3), hits(3, 9, 5, 1), 10, []int64{5, 3, 9, 1}},
		{"截到 limit", hits(5, 3), hits(9, 8, 7), 4, []int64{5, 3, 9, 8}},
		{"AND 为空", nil, hits(9, 8), 10, []int64{9, 8}},
		{"AND 自己就超过 limit", hits(1, 2, 3), hits(4), 2, []int64{1, 2}},
	}
	for _, c := range cases {
		if got := idsOf(mergeAndThenOr(c.and, c.or, c.limit)); !slices.Equal(got, c.expected) {
			t.Errorf("[%s] = %v，期望 %v", c.name, got, c.expected)
		}
	}
}
