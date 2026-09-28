package service

import (
	"testing"
	"time"
)

// 活动复盘窗口的纯函数（docs/AI经营-M10M11设计.md §2）：当前窗口 = [starts_at, min(ends_at, now))，
// 前一个窗口紧邻其前、等长。
func TestPromotionReviewWindows(t *testing.T) {
	starts := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	ends := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

	t.Run("活动已结束：当前窗口是完整的 starts_at..ends_at", func(t *testing.T) {
		now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		curStart, curEnd, prevStart, prevEnd := promotionReviewWindows(starts, ends, now)
		if !curStart.Equal(starts) || !curEnd.Equal(ends) {
			t.Fatalf("当前窗口 %v..%v，期望 %v..%v", curStart, curEnd, starts, ends)
		}
		wantPrevStart := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC) // 14 天前
		if !prevStart.Equal(wantPrevStart) || !prevEnd.Equal(starts) {
			t.Fatalf("前一个窗口 %v..%v，期望 %v..%v", prevStart, prevEnd, wantPrevStart, starts)
		}
	})

	t.Run("活动进行中：当前窗口截到 now", func(t *testing.T) {
		now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
		curStart, curEnd, prevStart, prevEnd := promotionReviewWindows(starts, ends, now)
		if !curStart.Equal(starts) || !curEnd.Equal(now) {
			t.Fatalf("当前窗口 %v..%v，期望 %v..%v", curStart, curEnd, starts, now)
		}
		wantPrevStart := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC) // 7 天前
		if !prevStart.Equal(wantPrevStart) || !prevEnd.Equal(starts) {
			t.Fatalf("前一个窗口 %v..%v，期望 %v..%v", prevStart, prevEnd, wantPrevStart, starts)
		}
	})

	t.Run("活动还没开始：当前窗口与前一个窗口都是空的", func(t *testing.T) {
		now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
		curStart, curEnd, prevStart, prevEnd := promotionReviewWindows(starts, ends, now)
		if !curStart.Equal(starts) || !curEnd.Equal(starts) {
			t.Fatalf("当前窗口应是空的（长度 0），得到 %v..%v", curStart, curEnd)
		}
		if !prevStart.Equal(starts) || !prevEnd.Equal(starts) {
			t.Fatalf("前一个窗口应也是空的，得到 %v..%v", prevStart, prevEnd)
		}
	})
}

// 客单价：整数四舍五入到分，不经浮点（同 report.go deriveMetrics 的技巧）。
func TestPromotionReviewAOV(t *testing.T) {
	cases := []struct {
		sales, orders, want int64
	}{
		{0, 0, 0},      // 没有订单：0，不是除零
		{1000, 4, 250}, // 整除
		{1001, 4, 250}, // 250.25 → 250
		{1003, 4, 251}, // 250.75 → 251（四舍五入）
		{999, 2, 500},  // 499.5 → 500（半数进位）
	}
	for _, c := range cases {
		if got := promotionReviewAOV(c.sales, c.orders); got != c.want {
			t.Errorf("promotionReviewAOV(%d, %d) = %d，期望 %d", c.sales, c.orders, got, c.want)
		}
	}
}
