package service

import (
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/repository"
)

// evaluateCoupon 是全仓库唯一一份券计算（coupon_calc.go 文件头）。这里不碰数据库，
// 把四种券型的边界、范围的每一条规则、分摊的恒等式逐分核对。
//
// 每条用例都在断言一个具体的分数，而不是「能用 / 不能用」：一个把门槛比较写成 >
// 的实现在「刚好满门槛」那一条上会红，一个把折扣向上取整的实现在「9.99 × 8.5 折」
// 那一条上会红 —— 这些是本文件存在的理由。

var calcNow = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

func i64(v int64) *int64   { return &v }
func str(v string) *string { return &v }

func couponOfRule(r repository.CouponRule) repository.UserCoupon {
	return repository.UserCoupon{
		ID: 1, Status: repository.UserCouponUnused,
		ValidStartAt: calcNow.Add(-time.Hour), ValidEndAt: calcNow.Add(time.Hour),
		Rule: r,
	}
}

func fullReduction(threshold, off int64) repository.UserCoupon {
	return couponOfRule(repository.CouponRule{TemplateID: 9, CouponType: couponTypeFullReduction,
		ThresholdCents: threshold, DiscountCents: off})
}

func rateCoupon(rate int16, threshold, cap int64) repository.UserCoupon {
	return couponOfRule(repository.CouponRule{TemplateID: 9, CouponType: couponTypeRateDiscount,
		DiscountRate: rate, ThresholdCents: threshold, MaxDiscountCents: cap})
}

func instant(off int64) repository.UserCoupon {
	return couponOfRule(repository.CouponRule{TemplateID: 9, CouponType: couponTypeInstant, DiscountCents: off})
}

var anyStore = repository.StoreScope{StoreID: 11, RegionID: 21}

func line(product, amount int64) couponLine {
	return couponLine{ProductID: product, AmountCents: amount, CategoryPath: str("/1/")}
}

func mustApply(t *testing.T, v couponVerdict) couponVerdict {
	t.Helper()
	if !v.Applicable {
		t.Fatalf("期望可用，实际不可用：%s", v.Reason)
	}
	var sum int64
	for _, d := range v.LineDiscounts {
		sum += d
	}
	if sum != v.DiscountCents {
		t.Fatalf("各行分摊之和 %d ≠ 总减免 %d（分摊恒等式破了）", sum, v.DiscountCents)
	}
	return v
}

func mustReject(t *testing.T, v couponVerdict, reasonHas string) {
	t.Helper()
	if v.Applicable {
		t.Fatalf("期望不可用（%s），实际可用，减 %d", reasonHas, v.DiscountCents)
	}
	if !strings.Contains(v.Reason, reasonHas) {
		t.Fatalf("不可用的原因是 %q，期望包含 %q", v.Reason, reasonHas)
	}
}

// ---------------------------------------------------------------------------
// 1 满减
// ---------------------------------------------------------------------------

func TestFullReductionExactlyAtThreshold(t *testing.T) {
	v := mustApply(t, evaluateCoupon(fullReduction(10000, 2000), nil, anyStore,
		[]couponLine{line(1, 10000)}, calcNow))
	if v.DiscountCents != 2000 {
		t.Fatalf("刚好满 100 元，应减 2000 分，实得 %d", v.DiscountCents)
	}
}

func TestFullReductionOneCentShort(t *testing.T) {
	mustReject(t, evaluateCoupon(fullReduction(10000, 2000), nil, anyStore,
		[]couponLine{line(1, 9999)}, calcNow), "还差 1 分")
}

func TestFullReductionComparesEligibleSubtotalNotWholeOrder(t *testing.T) {
	// 整单 150 元，但只有 60 元在范围内：满 100 不成立。
	scopes := []repository.CouponScope{{ScopeType: repository.ScopeProduct, TargetID: i64(1), Include: true}}
	mustReject(t, evaluateCoupon(fullReduction(10000, 2000), scopes, anyStore,
		[]couponLine{line(1, 6000), line(2, 9000)}, calcNow), "还差 4000 分")
}

// ---------------------------------------------------------------------------
// 2 折扣
// ---------------------------------------------------------------------------

func TestRateDiscountRoundsDown(t *testing.T) {
	// 999 分打 8.5 折：减 999 × 150 / 1000 = 149.85 → 向下取整 149。
	v := mustApply(t, evaluateCoupon(rateCoupon(850, 0, 0), nil, anyStore,
		[]couponLine{line(1, 999)}, calcNow))
	if v.DiscountCents != 149 {
		t.Fatalf("999 分 8.5 折应减 149 分（向下取整），实得 %d", v.DiscountCents)
	}
}

func TestRateDiscountCap(t *testing.T) {
	// 1000 元 8 折本该减 200 元，封顶 50 元。
	v := mustApply(t, evaluateCoupon(rateCoupon(800, 0, 5000), nil, anyStore,
		[]couponLine{line(1, 100000)}, calcNow))
	if v.DiscountCents != 5000 {
		t.Fatalf("封顶 5000 分，实得 %d", v.DiscountCents)
	}
	// 封顶之下不受影响：100 元 8 折减 20 元。
	v = mustApply(t, evaluateCoupon(rateCoupon(800, 0, 5000), nil, anyStore,
		[]couponLine{line(1, 10000)}, calcNow))
	if v.DiscountCents != 2000 {
		t.Fatalf("没到封顶时应减 2000 分，实得 %d", v.DiscountCents)
	}
}

func TestRateDiscountThreshold(t *testing.T) {
	mustReject(t, evaluateCoupon(rateCoupon(900, 5000, 0), nil, anyStore,
		[]couponLine{line(1, 4999)}, calcNow), "还差 1 分")
	v := mustApply(t, evaluateCoupon(rateCoupon(900, 5000, 0), nil, anyStore,
		[]couponLine{line(1, 5000)}, calcNow))
	if v.DiscountCents != 500 {
		t.Fatalf("刚好满 50 元 9 折应减 500 分，实得 %d", v.DiscountCents)
	}
}

func TestRateDiscountThatRoundsToZeroIsNotApplicable(t *testing.T) {
	// 1 分钱打 9.99 折：减 0.001 分 → 0。一张「用了但一分没减」的券不该被锁、被核销。
	mustReject(t, evaluateCoupon(rateCoupon(999, 0, 0), nil, anyStore,
		[]couponLine{line(1, 1)}, calcNow), "减不了钱")
}

func TestRateDiscountDoesNotOverflow(t *testing.T) {
	// 小计接近 int64 上限时，朴素的 subtotal*off 会溢出成负数。
	big := int64(9_000_000_000_000_000_000)
	got := rateDiscount(big, 850)
	want := big/1000*150 + (big%1000)*150/1000
	if got != want || got <= 0 {
		t.Fatalf("大额折扣算成了 %d，期望 %d", got, want)
	}
}

// ---------------------------------------------------------------------------
// 3 立减
// ---------------------------------------------------------------------------

func TestInstantCappedAtEligibleSubtotal(t *testing.T) {
	// 立减 50 元，适用商品只有 30 元：减 30 元，应付不为负。
	v := mustApply(t, evaluateCoupon(instant(5000), nil, anyStore,
		[]couponLine{line(1, 3000)}, calcNow))
	if v.DiscountCents != 3000 {
		t.Fatalf("减免应封顶在适用小计 3000 分，实得 %d", v.DiscountCents)
	}
	if v.LineDiscounts[0] != 3000 {
		t.Fatalf("唯一一行应分摊 3000 分，实得 %d", v.LineDiscounts[0])
	}
}

func TestInstantCapIsPerEligibleNotWholeOrder(t *testing.T) {
	// 整单 100 元，范围内只有 30 元：立减 50 只减 30，不在范围外那一行上多减。
	scopes := []repository.CouponScope{{ScopeType: repository.ScopeProduct, TargetID: i64(1), Include: true}}
	v := mustApply(t, evaluateCoupon(instant(5000), scopes, anyStore,
		[]couponLine{line(1, 3000), line(2, 7000)}, calcNow))
	if v.DiscountCents != 3000 || v.LineDiscounts[1] != 0 {
		t.Fatalf("应只在适用行上减 3000 分，实得总 %d、范围外那行 %d", v.DiscountCents, v.LineDiscounts[1])
	}
}

// ---------------------------------------------------------------------------
// 4 包邮：本期拒绝
// ---------------------------------------------------------------------------

func TestFreeShippingIsNeverApplicable(t *testing.T) {
	c := couponOfRule(repository.CouponRule{CouponType: couponTypeFreeShipping})
	mustReject(t, evaluateCoupon(c, nil, anyStore, []couponLine{line(1, 10000)}, calcNow), "不计运费")
}

// ---------------------------------------------------------------------------
// 状态与有效期
// ---------------------------------------------------------------------------

func TestExpiredAndNotYetValidAndNonUnusedCoupons(t *testing.T) {
	lines := []couponLine{line(1, 10000)}

	c := instant(100)
	c.ValidEndAt = calcNow // 边界：valid_end_at 那一刻已经过期（区间是左闭右开）
	mustReject(t, evaluateCoupon(c, nil, anyStore, lines, calcNow), "已过期")

	c = instant(100)
	c.ValidStartAt = calcNow.Add(time.Minute)
	mustReject(t, evaluateCoupon(c, nil, anyStore, lines, calcNow), "才开始可用")

	c = instant(100)
	c.Status = repository.UserCouponLocked
	mustReject(t, evaluateCoupon(c, nil, anyStore, lines, calcNow), "占用")

	c = instant(100)
	c.Status = repository.UserCouponUsed
	mustReject(t, evaluateCoupon(c, nil, anyStore, lines, calcNow), "用过")

	c = instant(100)
	c.ValidEndAt = calcNow.Add(time.Nanosecond) // 还差一纳秒过期：可用
	mustApply(t, evaluateCoupon(c, nil, anyStore, lines, calcNow))
}

// ---------------------------------------------------------------------------
// 适用范围
// ---------------------------------------------------------------------------

func TestCategoryScopeIncludesDescendants(t *testing.T) {
	// 券限「服装」(/1/)，商品在「服装 / 女装 / 连衣裙」(/1/5/9/)：适用。
	// 另一件在「食品」(/2/)：不适用。
	scopes := []repository.CouponScope{{ScopeType: repository.ScopeCategory, TargetID: i64(1),
		CategoryPath: str("/1/"), Include: true}}
	lines := []couponLine{
		{ProductID: 1, AmountCents: 4000, CategoryPath: str("/1/5/9/")},
		{ProductID: 2, AmountCents: 6000, CategoryPath: str("/2/")},
	}
	v := mustApply(t, evaluateCoupon(instant(1000), scopes, anyStore, lines, calcNow))
	if v.EligibleSubtotal != 4000 || v.LineDiscounts[1] != 0 {
		t.Fatalf("子孙分类应适用、别的分类不适用：小计 %d，食品那行分到 %d", v.EligibleSubtotal, v.LineDiscounts[1])
	}
}

func TestCategoryPrefixIsIDBoundedNotStringPrefix(t *testing.T) {
	// /1/ 不能匹配 /12/：path 带着两侧的斜杠，前缀判定才等于子树判定。
	scopes := []repository.CouponScope{{ScopeType: repository.ScopeCategory, TargetID: i64(1),
		CategoryPath: str("/1/"), Include: true}}
	lines := []couponLine{{ProductID: 1, AmountCents: 4000, CategoryPath: str("/12/")}}
	mustReject(t, evaluateCoupon(instant(1000), scopes, anyStore, lines, calcNow), "没有适用")
}

func TestDeletedCategoryMatchesNothing(t *testing.T) {
	// 分类规则的目标已软删（CategoryPath 为 nil）：包含规则不给任何行折扣。
	scopes := []repository.CouponScope{{ScopeType: repository.ScopeCategory, TargetID: i64(1), Include: true}}
	mustReject(t, evaluateCoupon(instant(1000), scopes, anyStore,
		[]couponLine{line(1, 4000)}, calcNow), "没有适用")
}

func TestExcludeWinsOverInclude(t *testing.T) {
	// 全场可用，但分类 /1/5/ 排除；商品 3 被单独包含也捞不回来。
	scopes := []repository.CouponScope{
		{ScopeType: repository.ScopeAll, Include: true},
		{ScopeType: repository.ScopeProduct, TargetID: i64(3), Include: true},
		{ScopeType: repository.ScopeCategory, TargetID: i64(5), CategoryPath: str("/1/5/"), Include: false},
	}
	lines := []couponLine{
		{ProductID: 3, AmountCents: 5000, CategoryPath: str("/1/5/7/")}, // 被排除的子类
		{ProductID: 4, AmountCents: 2000, CategoryPath: str("/1/6/")},
	}
	v := mustApply(t, evaluateCoupon(instant(100000), scopes, anyStore, lines, calcNow))
	if v.EligibleSubtotal != 2000 || v.LineDiscounts[0] != 0 {
		t.Fatalf("排除应优先于包含：小计 %d，被排除那行分到 %d", v.EligibleSubtotal, v.LineDiscounts[0])
	}
}

func TestBrandScope(t *testing.T) {
	scopes := []repository.CouponScope{{ScopeType: repository.ScopeBrand, TargetID: i64(77), Include: true}}
	lines := []couponLine{
		{ProductID: 1, AmountCents: 1000, BrandID: i64(77)},
		{ProductID: 2, AmountCents: 9000},
	}
	v := mustApply(t, evaluateCoupon(instant(5000), scopes, anyStore, lines, calcNow))
	if v.DiscountCents != 1000 {
		t.Fatalf("只有品牌 77 那一行适用，应减 1000 分，实得 %d", v.DiscountCents)
	}
}

func TestRegionAndStoreScopes(t *testing.T) {
	lines := []couponLine{line(1, 10000)}
	north := []repository.CouponScope{{ScopeType: repository.ScopeRegion, TargetID: i64(21), Include: true}}

	// 华北门店（大区 21）：可用。
	mustApply(t, evaluateCoupon(instant(100), north, repository.StoreScope{StoreID: 11, RegionID: 21}, lines, calcNow))
	// 华南门店（大区 22）：不可用。
	mustReject(t, evaluateCoupon(instant(100), north, repository.StoreScope{StoreID: 12, RegionID: 22}, lines, calcNow),
		"不能在这家门店")

	// 限大区 21，但其中门店 11 排除：11 不可用、同大区的 13 可用。
	withExclude := append(append([]repository.CouponScope{}, north...),
		repository.CouponScope{ScopeType: repository.ScopeStore, TargetID: i64(11), Include: false})
	mustReject(t, evaluateCoupon(instant(100), withExclude, repository.StoreScope{StoreID: 11, RegionID: 21}, lines, calcNow),
		"不能在这家门店")
	mustApply(t, evaluateCoupon(instant(100), withExclude, repository.StoreScope{StoreID: 13, RegionID: 21}, lines, calcNow))

	// 只限门店 14（新店开业券）：商品维度按全场，别的门店不可用。
	newStore := []repository.CouponScope{{ScopeType: repository.ScopeStore, TargetID: i64(14), Include: true}}
	mustApply(t, evaluateCoupon(instant(100), newStore, repository.StoreScope{StoreID: 14, RegionID: 22}, lines, calcNow))
	mustReject(t, evaluateCoupon(instant(100), newStore, repository.StoreScope{StoreID: 11, RegionID: 21}, lines, calcNow),
		"不能在这家门店")
}

// ---------------------------------------------------------------------------
// 分摊
// ---------------------------------------------------------------------------

func TestAllocationRemainderGoesToLargestAndSpillsOver(t *testing.T) {
	// 34 / 33 / 33 分减 99 分：比例向下取整得 33 / 32 / 32，余 2。
	// 全给最大行会变成 35 > 34；应给到 34，剩下 1 分顺延给次大行（按订单行顺序第 2 行）。
	lines := []couponLine{line(1, 34), line(2, 33), line(3, 33)}
	v := mustApply(t, evaluateCoupon(instant(99), nil, anyStore, lines, calcNow))
	want := []int64{34, 33, 32}
	for i := range want {
		if v.LineDiscounts[i] != want[i] {
			t.Fatalf("分摊是 %v，期望 %v", v.LineDiscounts, want)
		}
		if v.LineDiscounts[i] > lines[i].AmountCents {
			t.Fatalf("第 %d 行分摊 %d 超过了它的金额 %d", i+1, v.LineDiscounts[i], lines[i].AmountCents)
		}
	}
}

func TestAllocationIsProportional(t *testing.T) {
	// 满 300 减 50，三行 100 / 100 / 100：各 16，余 2 给最大行（金额相同取第一行）→ 18 / 16 / 16。
	lines := []couponLine{line(1, 10000), line(2, 10000), line(3, 10000)}
	v := mustApply(t, evaluateCoupon(fullReduction(30000, 50), nil, anyStore, lines, calcNow))
	want := []int64{18, 16, 16}
	for i := range want {
		if v.LineDiscounts[i] != want[i] {
			t.Fatalf("分摊是 %v，期望 %v", v.LineDiscounts, want)
		}
	}
}
