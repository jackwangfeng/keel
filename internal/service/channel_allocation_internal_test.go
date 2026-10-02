package service

import (
	"testing"

	"github.com/keel/keel/internal/channel"
)

// 基线建议（spec §4.1）的三条规则 + 两条「不给建议」，纯函数。

func allocBinding(id int64, ratio, safety int32, velocity float64, net int64) AllocationChannel {
	return AllocationChannel{BindingID: &id, Name: "渠道", Rule: &channel.StockRule{RatioBP: ratio, SafetyQty: safety},
		RuleLevel: channel.RuleLevelBinding, DailyVelocity: velocity, UnitNetCents: net}
}

func allocSelf(velocity float64, net int64) AllocationChannel {
	return AllocationChannel{Name: "自营", DailyVelocity: velocity, UnitNetCents: net}
}

func onlySuggestion(t *testing.T, got []AllocationSuggestion) AllocationSuggestion {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("期望恰好一条建议，实得 %+v", got)
	}
	return got[0]
}

func TestSuggestAllocationHeldZeroRaisesRatio(t *testing.T) {
	a := allocBinding(7, 8000, 0, 1.5, 3000)
	a.HeldZeroHours = 30
	sku := AllocationSKU{SKUID: 11, Available: 100, Channels: []AllocationChannel{allocSelf(1, 5000), a}}
	s := onlySuggestion(t, suggestAllocation(sku, 14))
	if s.BindingID != 7 || s.SKUID == nil || *s.SKUID != 11 || s.RatioBP != 9000 || s.SafetyQty != 0 || s.Why == "" {
		t.Fatalf("挂零 30 小时且有卖：比例应 80%%→90%%（SKU 级），实得 %+v", s)
	}

	// 不到 24 小时、或这个渠道一件没卖：不动。
	a.HeldZeroHours = 23
	if got := suggestAllocation(AllocationSKU{SKUID: 11, Available: 100, Channels: []AllocationChannel{a}}, 14); len(got) != 0 {
		t.Fatalf("挂零 23 小时不该有建议：%+v", got)
	}
	a.HeldZeroHours, a.DailyVelocity = 30, 0
	if got := suggestAllocation(AllocationSKU{SKUID: 11, Available: 100, Channels: []AllocationChannel{a}}, 14); len(got) != 0 {
		t.Fatalf("没卖过不该有建议：%+v", got)
	}

	// 比例封顶 100%。
	b := allocBinding(7, 9500, 0, 1, 3000)
	b.HeldZeroHours = 48
	if s := onlySuggestion(t, suggestAllocation(AllocationSKU{SKUID: 11, Available: 100, Channels: []AllocationChannel{b}}, 14)); s.RatioBP != 10000 {
		t.Fatalf("95%% 上调应封顶 100%%：%+v", s)
	}

	// 已经 100%：改为把安全库存降到「其它渠道日均 × 0.5」取整。
	c := allocBinding(7, 10000, 5, 1, 3000)
	c.HeldZeroHours = 48
	s = onlySuggestion(t, suggestAllocation(AllocationSKU{SKUID: 11, Available: 100,
		Channels: []AllocationChannel{allocSelf(3, 5000), c}}, 14))
	if s.RatioBP != 10000 || s.SafetyQty != 1 {
		t.Fatalf("比例已 100%%：安全库存应 5→1（其它渠道日均 3 × 0.5 取整），实得 %+v", s)
	}
}

func TestSuggestAllocationRejectsRaiseSafety(t *testing.T) {
	a := allocBinding(7, 10000, 1, 1, 3000)
	a.StockoutRejects = 3
	s := onlySuggestion(t, suggestAllocation(AllocationSKU{SKUID: 11, Available: 100, Channels: []AllocationChannel{a}}, 14))
	// 14 天 = 2 周，ceil(3 / 2) = 2。
	if s.SafetyQty != 3 || s.RatioBP != 10000 {
		t.Fatalf("两周 3 件缺货拒单：安全库存应 1→3，实得 %+v", s)
	}
}

func TestSuggestAllocationShortageLowersLowestNet(t *testing.T) {
	// 可售 15；三个渠道日均各 2 → 3 天需求 18 > 15。净收入：自营 5000 > A 4000 > B 3000。
	// 先保自营与 A（各 6），B 只剩 3 件 → 比例 floor(3 × 10000 / 15) = 2000。
	sku := AllocationSKU{SKUID: 11, Available: 15, Channels: []AllocationChannel{
		allocSelf(2, 5000), allocBinding(1, 10000, 0, 2, 4000), allocBinding(2, 10000, 0, 2, 3000)}}
	s := onlySuggestion(t, suggestAllocation(sku, 14))
	if s.BindingID != 2 || s.RatioBP != 2000 {
		t.Fatalf("货不够分：净收入最低的 B 应下调到 20%%，实得 %+v", s)
	}
	if got := channel.PublishedQty(15, channel.StockRule{RatioBP: s.RatioBP}); got != 3 {
		t.Fatalf("下调之后 B 对外 %d 件，期望 3", got)
	}

	// 够分：不动。
	sku.Available = 40
	if got := suggestAllocation(sku, 14); len(got) != 0 {
		t.Fatalf("可售 40 够分，不该有建议：%+v", got)
	}
}

func TestSuggestAllocationNoneWhenKeelOutOfStock(t *testing.T) {
	a := allocBinding(7, 5000, 0, 2, 3000)
	a.HeldZeroHours, a.StockoutRejects = 100, 5
	if got := suggestAllocation(AllocationSKU{SKUID: 11, Available: 0, StockoutDays: 3,
		Channels: []AllocationChannel{allocSelf(2, 5000), a}}, 14); len(got) != 0 {
		t.Fatalf("keel 自己没货：缺的是货不是分配，不该有建议（Review Focus 4）：%+v", got)
	}
}

func TestSuggestAllocationNeverForSelf(t *testing.T) {
	self := allocSelf(2, 5000)
	self.HeldZeroHours, self.StockoutRejects = 100, 5
	if got := suggestAllocation(AllocationSKU{SKUID: 11, Available: 3, Channels: []AllocationChannel{self}}, 14); len(got) != 0 {
		t.Fatalf("自营没有规则，不给建议：%+v", got)
	}
}

func TestAllocationVelocity(t *testing.T) {
	cases := []struct {
		sold      int64
		days      int
		zeroHours float64
		want      float64
	}{
		{14, 14, 0, 1},
		{14, 14, 7 * 24, 2}, // 挂零 7 天：分母 7
		{5, 14, 14 * 24, 5}, // 整窗挂零：分母下限 1
		{0, 14, 0, 0},
	}
	for _, c := range cases {
		if got := allocationVelocity(c.sold, c.days, c.zeroHours); got != c.want {
			t.Errorf("allocationVelocity(%d, %d, %v) = %v，期望 %v", c.sold, c.days, c.zeroHours, got, c.want)
		}
	}
}

func TestAllocationUnitNet(t *testing.T) {
	// 100 元、佣金 15%、成本 60 元 → 25 元；成本 0 不减。
	if got := allocationUnitNet(10000, 1500, 6000); got != 2500 {
		t.Fatalf("单件净收入 %d，期望 2500", got)
	}
	if got := allocationUnitNet(10000, 0, 0); got != 10000 {
		t.Fatalf("没佣金没成本的单件净收入 %d，期望 10000", got)
	}
}
