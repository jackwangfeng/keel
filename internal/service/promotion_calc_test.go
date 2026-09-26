package service

import (
	"testing"
	"time"

	"github.com/keel/keel/internal/repository"
)

// computePromotions 是全仓库唯一一份营销活动计算（promotion_calc.go 文件头）。这里不碰数据库，
// 把阶梯、贪心、分摊余数、单价类的取价与配额、叠加规则逐分核对。
//
// 与 coupon_calc_test.go 同一个写法：每条用例断言一个具体的分数。一个把门槛比较写成 > 的实现
// 在「刚好满 100」那一条上会红，一个让两个活动重复减同一行的实现在「范围重叠」那一条上会红，
// 一个把特价直接替代门店价的实现在「特价比门店价还贵」那一条上会红。

var promoStart = calcNow.Add(-time.Hour)
var promoEnd = calcNow.Add(time.Hour)

func promo(id int64, typ, unit int16, stack bool) repository.Promotion {
	return repository.Promotion{ID: id, Name: "活动", Type: typ, ThresholdUnit: unit,
		StackWithCoupon: stack, StartsAt: promoStart, EndsAt: promoEnd, Status: 1}
}

func tierOff(th, off int64) repository.PromotionTier {
	return repository.PromotionTier{Threshold: th, DiscountCents: off}
}

func tierRate(th int64, rate int16) repository.PromotionTier {
	return repository.PromotionTier{Threshold: th, DiscountRate: rate}
}

// lp 造一份「此刻生效」的活动素材。
func lp(ps ...repository.Promotion) livePromotions {
	out := livePromotions{
		Promos: map[int64]repository.Promotion{},
		Tiers:  map[int64][]repository.PromotionTier{},
		Scopes: map[int64][]repository.CouponScope{},
		Offers: map[int64][]repository.PriceOffer{},
	}
	for _, p := range ps {
		out.Promos[p.ID] = p
	}
	return out
}

func pline(sku, product int64, price int64, qty int32, cat string) promoLine {
	return promoLine{SKUID: sku, ProductID: product, ListPriceCents: price, Quantity: qty, CategoryPath: str(cat)}
}

func sumOf(v []int64) int64 {
	var s int64
	for _, x := range v {
		s += x
	}
	return s
}

// mustBalance：分摊恒等（各行之和 = 总优惠）且每一行不超过这一行的金额。
func mustBalance(t *testing.T, lines []promoLine, r promoResult) {
	t.Helper()
	if s := sumOf(r.LineDiscounts); s != r.DiscountCents {
		t.Fatalf("各行分摊之和 %d ≠ 总优惠 %d（分摊恒等式破了）", s, r.DiscountCents)
	}
	for i, ln := range lines {
		amount := r.UnitPrices[i] * int64(ln.Quantity)
		if r.LineDiscounts[i] > amount || r.LineDiscounts[i] < 0 {
			t.Fatalf("第 %d 行分到 %d，行金额只有 %d", i, r.LineDiscounts[i], amount)
		}
	}
}

func hitOf(t *testing.T, r promoResult, id int64) PromotionHit {
	t.Helper()
	for _, h := range r.Hits {
		if h.PromotionID == id {
			return h
		}
	}
	t.Fatalf("结果里没有活动 %d：%+v", id, r.Hits)
	return PromotionHit{}
}

// ---------------------------------------------------------------------------
// 满减阶梯
// ---------------------------------------------------------------------------

func TestFullReductionTiers(t *testing.T) {
	m := lp(promo(1, repository.PromoFullReduction, repository.ThresholdByAmount, true))
	m.Tiers[1] = []repository.PromotionTier{tierOff(10000, 1000), tierOff(20000, 3000)}

	cases := []struct {
		amount    int64
		off       int64
		applied   bool
		shortfall *int64
	}{
		{9999, 0, false, i64(1)},        // 差 1 分满 100
		{10000, 1000, true, i64(10000)}, // 刚好满 100：命中第一档，离第二档还差 100 元
		{19999, 1000, true, i64(1)},
		{20000, 3000, true, nil}, // 最高档：没有下一档
		{50000, 3000, true, nil}, // 阶梯不是循环满减
	}
	for _, c := range cases {
		lines := []promoLine{pline(1, 1, c.amount, 1, "/1/")}
		r := computePromotions(lines, m, anyStore, nil)
		mustBalance(t, lines, r)
		if r.DiscountCents != c.off {
			t.Fatalf("%d 分：减了 %d，期望 %d", c.amount, r.DiscountCents, c.off)
		}
		h := hitOf(t, r, 1)
		if h.Applied != c.applied {
			t.Fatalf("%d 分：applied=%v，期望 %v", c.amount, h.Applied, c.applied)
		}
		switch {
		case c.shortfall == nil && h.Shortfall != nil:
			t.Fatalf("%d 分：已是最高档却报了还差 %d", c.amount, *h.Shortfall)
		case c.shortfall != nil && (h.Shortfall == nil || *h.Shortfall != *c.shortfall):
			t.Fatalf("%d 分：还差 %v，期望 %d", c.amount, h.Shortfall, *c.shortfall)
		}
	}
}

func TestFullDiscountByQuantity(t *testing.T) {
	// 满 2 件 9 折、满 3 件 8.5 折。
	m := lp(promo(1, repository.PromoFullDiscount, repository.ThresholdByQty, true))
	m.Tiers[1] = []repository.PromotionTier{tierRate(2, 900), tierRate(3, 850)}

	one := []promoLine{pline(1, 1, 3333, 1, "/1/")}
	r := computePromotions(one, m, anyStore, nil)
	if r.DiscountCents != 0 || hitOf(t, r, 1).Shortfall == nil || *hitOf(t, r, 1).Shortfall != 1 {
		t.Fatalf("1 件：应当不命中、还差 1 件，实得减 %d / %+v", r.DiscountCents, hitOf(t, r, 1))
	}

	// 2 件 × 33.33 = 66.66 元，9 折减 6.666 → 向下取整 666 分（对商家有利，同券的折扣）。
	two := []promoLine{pline(1, 1, 3333, 2, "/1/")}
	r = computePromotions(two, m, anyStore, nil)
	mustBalance(t, two, r)
	if r.DiscountCents != 666 {
		t.Fatalf("2 件 66.66 元打 9 折：减 %d，期望 666", r.DiscountCents)
	}
	if got := hitOf(t, r, 1).Message; got != "已减 6.66 元，再买 1 件 可享 8.5折" {
		t.Fatalf("提示语：%q", got)
	}
}

// ---------------------------------------------------------------------------
// 分摊与余数：与券同一份规则（allocateDiscount）
// ---------------------------------------------------------------------------

func TestPromotionAllocationRemainderSpillsOver(t *testing.T) {
	// 34 / 33 / 33 分三行，满 1 分减 99 分：按比例 33 / 32 / 32，余 2 分。
	// 全给最大行就是 35 > 34 —— 那一行将来一分钱都退不了（chk_item_refund）。
	// 正确结果：最大行补到 34，余下 1 分顺延给次大行（金额相同按行序）。
	m := lp(promo(1, repository.PromoFullReduction, repository.ThresholdByAmount, true))
	m.Tiers[1] = []repository.PromotionTier{tierOff(1, 99)}
	lines := []promoLine{pline(1, 1, 34, 1, "/1/"), pline(2, 2, 33, 1, "/1/"), pline(3, 3, 33, 1, "/1/")}
	r := computePromotions(lines, m, anyStore, nil)
	mustBalance(t, lines, r)
	want := []int64{34, 33, 32}
	for i := range want {
		if r.LineDiscounts[i] != want[i] {
			t.Fatalf("分摊 %v，期望 %v", r.LineDiscounts, want)
		}
	}
}

func TestPromotionAllocationIsProportional(t *testing.T) {
	// 满 300 减 50：100 / 200 两行按 1:2 分，16 / 33，余 1 分给大行 → 16 / 34。
	m := lp(promo(1, repository.PromoFullReduction, repository.ThresholdByAmount, true))
	m.Tiers[1] = []repository.PromotionTier{tierOff(30000, 5000)}
	lines := []promoLine{pline(1, 1, 10000, 1, "/1/"), pline(2, 2, 20000, 1, "/1/")}
	r := computePromotions(lines, m, anyStore, nil)
	mustBalance(t, lines, r)
	if r.LineDiscounts[0] != 1666 || r.LineDiscounts[1] != 3334 {
		t.Fatalf("分摊 %v，期望 [1666 3334]", r.LineDiscounts)
	}
}

// ---------------------------------------------------------------------------
// 一行至多参与一个满减满折：贪心
// ---------------------------------------------------------------------------

func overlapPromos() livePromotions {
	// 1：全场满 200 减 30；2：服装（分类 /1/2/ 含子孙）满 100 减 20。
	m := lp(promo(1, repository.PromoFullReduction, repository.ThresholdByAmount, true),
		promo(2, repository.PromoFullReduction, repository.ThresholdByAmount, true))
	m.Tiers[1] = []repository.PromotionTier{tierOff(20000, 3000)}
	m.Tiers[2] = []repository.PromotionTier{tierOff(10000, 2000)}
	m.Scopes[2] = []repository.CouponScope{{ScopeType: repository.ScopeCategory, TargetID: i64(2),
		Include: true, CategoryPath: str("/1/2/")}}
	return m
}

func TestOverlappingPromotionsNeverDiscountALineTwice(t *testing.T) {
	// 连衣裙 150（服装）+ 零食 60：全场 210 ≥ 200 减 30；服装 150 ≥ 100 减 20。
	// 两个都减就是 50 —— 连衣裙被减了两次。贪心挑减得多的全场（30），它占走全部行。
	lines := []promoLine{pline(1, 1, 15000, 1, "/1/2/3/"), pline(2, 2, 6000, 1, "/9/")}
	r := computePromotions(lines, overlapPromos(), anyStore, nil)
	mustBalance(t, lines, r)
	if r.DiscountCents != 3000 {
		t.Fatalf("范围重叠时减了 %d，期望只减全场的 3000", r.DiscountCents)
	}
	if !hitOf(t, r, 1).Applied {
		t.Fatalf("全场满减应当命中")
	}
	for _, h := range r.Hits {
		if h.PromotionID == 2 {
			t.Fatalf("服装满减的行全被占走了，不该再报它（报「还差多少」只是噪音）：%+v", h)
		}
	}
}

func TestGreedyFallsBackToTheSmallerPromotionOnTheRestOfTheLines(t *testing.T) {
	// 连衣裙 150 + 零食 40 = 190，全场不满 200；服装 150 满 100 减 20 命中，占走连衣裙。
	// 剩下的零食 40 再评估全场：不满 → 报「还差 160 元」（按剩下的行算，不是按整单）。
	lines := []promoLine{pline(1, 1, 15000, 1, "/1/2/3/"), pline(2, 2, 4000, 1, "/9/")}
	r := computePromotions(lines, overlapPromos(), anyStore, nil)
	mustBalance(t, lines, r)
	if r.DiscountCents != 2000 || r.LineDiscounts[0] != 2000 || r.LineDiscounts[1] != 0 {
		t.Fatalf("减 %d、分摊 %v，期望服装满减 2000 全落在连衣裙上", r.DiscountCents, r.LineDiscounts)
	}
	h := hitOf(t, r, 1)
	if h.Applied || h.Shortfall == nil || *h.Shortfall != 16000 {
		t.Fatalf("全场满减应当未命中、在剩下的 40 元上还差 160 元：%+v", h)
	}
}

func TestDisjointPromotionsBothApply(t *testing.T) {
	// 服装满 100 减 10、食品满 50 减 5，范围不重叠：两个都命中，各减各的。
	m := lp(promo(1, repository.PromoFullReduction, repository.ThresholdByAmount, true),
		promo(2, repository.PromoFullReduction, repository.ThresholdByAmount, true))
	m.Tiers[1] = []repository.PromotionTier{tierOff(10000, 1000)}
	m.Tiers[2] = []repository.PromotionTier{tierOff(5000, 500)}
	m.Scopes[1] = []repository.CouponScope{{ScopeType: repository.ScopeCategory, TargetID: i64(2), Include: true, CategoryPath: str("/1/2/")}}
	m.Scopes[2] = []repository.CouponScope{{ScopeType: repository.ScopeCategory, TargetID: i64(9), Include: true, CategoryPath: str("/9/")}}
	lines := []promoLine{pline(1, 1, 12000, 1, "/1/2/"), pline(2, 2, 6000, 1, "/9/")}
	r := computePromotions(lines, m, anyStore, nil)
	mustBalance(t, lines, r)
	if r.DiscountCents != 1500 || r.LineDiscounts[0] != 1000 || r.LineDiscounts[1] != 500 {
		t.Fatalf("减 %d、分摊 %v，期望 1500 = [1000 500]", r.DiscountCents, r.LineDiscounts)
	}
}

func TestPromotionTieBreaksBySmallerID(t *testing.T) {
	// 两个活动在同一批行上减得一样多：取 id 小的。结果必须确定，试算与下单才挑同一个。
	m := lp(promo(7, repository.PromoFullReduction, repository.ThresholdByAmount, true),
		promo(3, repository.PromoFullReduction, repository.ThresholdByAmount, false))
	m.Tiers[7] = []repository.PromotionTier{tierOff(100, 10)}
	m.Tiers[3] = []repository.PromotionTier{tierOff(100, 10)}
	for i := 0; i < 20; i++ { // map 遍历顺序是随机的：跑几遍
		r := computePromotions([]promoLine{pline(1, 1, 1000, 1, "/1/")}, m, anyStore, nil)
		if len(r.Hits) != 1 || r.Hits[0].PromotionID != 3 || !r.Hits[0].Applied {
			t.Fatalf("同额时应当取 id 小的活动 3（7 的行被占光，不再报它）：%+v", r.Hits)
		}
	}
}

func TestPromotionStoreScope(t *testing.T) {
	// 华北大区（21）以外不生效；排除优先。
	m := lp(promo(1, repository.PromoFullReduction, repository.ThresholdByAmount, true))
	m.Tiers[1] = []repository.PromotionTier{tierOff(100, 10)}
	m.Scopes[1] = []repository.CouponScope{{ScopeType: repository.ScopeRegion, TargetID: i64(21), Include: true}}
	lines := []promoLine{pline(1, 1, 1000, 1, "/1/")}
	if r := computePromotions(lines, m, repository.StoreScope{StoreID: 11, RegionID: 21}, nil); r.DiscountCents != 10 {
		t.Fatalf("华北门店应当命中，实减 %d", r.DiscountCents)
	}
	if r := computePromotions(lines, m, repository.StoreScope{StoreID: 12, RegionID: 22}, nil); r.DiscountCents != 0 || len(r.Hits) != 0 {
		t.Fatalf("华南门店不该命中也不该有提示：%+v", r)
	}
}

// ---------------------------------------------------------------------------
// 单价类：活动价 = min(门店价, 特价)
// ---------------------------------------------------------------------------

func offer(promoID, sku, price int64, rate int16, limit, stock, sold int32) repository.PriceOffer {
	return repository.PriceOffer{PromotionID: promoID, SKUID: sku, PromoPriceCents: price,
		DiscountRate: rate, PerUserLimit: limit, StockQty: stock, SoldQty: sold}
}

func TestLimitedPriceIsMinOfStorePriceAndSpecial(t *testing.T) {
	m := lp(promo(5, repository.PromoLimitedPrice, 0, true))
	m.Offers[1] = []repository.PriceOffer{offer(5, 1, 3990, 0, 0, 0, 0)}

	// 门店价 59.90 > 特价 39.90：按特价卖，省下的 20 × 2 件只在单价里，不进 discount_cents。
	lines := []promoLine{pline(1, 1, 5990, 2, "/1/")}
	r := computePromotions(lines, m, anyStore, nil)
	if r.UnitPrices[0] != 3990 || r.PricePromotionIDs[0] == nil || *r.PricePromotionIDs[0] != 5 {
		t.Fatalf("应当按特价 3990 卖：单价 %d 活动 %v", r.UnitPrices[0], r.PricePromotionIDs[0])
	}
	if r.DiscountCents != 0 {
		t.Fatalf("单价类活动不该进 discount_cents，实得 %d", r.DiscountCents)
	}
	if h := hitOf(t, r, 5); !h.Applied || h.DiscountCents != 4000 {
		t.Fatalf("命中明细里的「省了多少」应当是 (5990-3990)×2 = 4000：%+v", h)
	}

	// 门店单独降价到 29.90，比特价还便宜：活动不生效，按门店价卖 —— 否则「特价」让人多付钱。
	lines = []promoLine{pline(1, 1, 2990, 1, "/1/")}
	r = computePromotions(lines, m, anyStore, nil)
	if r.UnitPrices[0] != 2990 || r.PricePromotionIDs[0] != nil || len(r.Hits) != 0 {
		t.Fatalf("特价比门店价贵时应当按门店价 2990：单价 %d 活动 %v 命中 %+v",
			r.UnitPrices[0], r.PricePromotionIDs[0], r.Hits)
	}
}

func TestRateOfferRoundsPriceUp(t *testing.T) {
	// 9.99 元打 8.5 折 = 8.4915 元：价向上取整到 8.50（与券「减免向下取整」同一个方向）。
	m := lp(promo(5, repository.PromoLimitedPrice, 0, true))
	m.Offers[1] = []repository.PriceOffer{offer(5, 1, 0, 850, 0, 0, 0)}
	r := computePromotions([]promoLine{pline(1, 1, 999, 1, "/1/")}, m, anyStore, nil)
	if r.UnitPrices[0] != 850 {
		t.Fatalf("9.99 × 8.5 折的活动价 %d，期望 850（向上取整）", r.UnitPrices[0])
	}
}

func TestCheapestOfferWinsAndTiesBreakByID(t *testing.T) {
	m := lp(promo(8, repository.PromoLimitedPrice, 0, true), promo(4, repository.PromoFlashSale, 0, true),
		promo(6, repository.PromoLimitedPrice, 0, true))
	m.Offers[1] = []repository.PriceOffer{
		offer(8, 1, 4000, 0, 0, 0, 0),
		offer(4, 1, 3000, 0, 0, 10, 0),
		offer(6, 1, 3000, 0, 0, 0, 0),
	}
	r := computePromotions([]promoLine{pline(1, 1, 5000, 1, "/1/")}, m, anyStore, nil)
	if r.UnitPrices[0] != 3000 || *r.PricePromotionIDs[0] != 4 {
		t.Fatalf("应当取最低价 3000，同价取 id 小的 4：%d / %d", r.UnitPrices[0], *r.PricePromotionIDs[0])
	}
}

func TestFlashSaleQuotaShortFallsBackToStorePrice(t *testing.T) {
	// 秒杀配额 10、已售 9：买 1 件按秒杀价，买 2 件配额不够 → 按门店价报价（下单时注定扣不到）。
	m := lp(promo(4, repository.PromoFlashSale, 0, true))
	m.Offers[1] = []repository.PriceOffer{offer(4, 1, 990, 0, 0, 10, 9)}
	if r := computePromotions([]promoLine{pline(1, 1, 5000, 1, "/1/")}, m, anyStore, nil); r.UnitPrices[0] != 990 {
		t.Fatalf("配额还剩 1 件，买 1 件应当按秒杀价 990，实得 %d", r.UnitPrices[0])
	}
	if r := computePromotions([]promoLine{pline(1, 1, 5000, 2, "/1/")}, m, anyStore, nil); r.UnitPrices[0] != 5000 {
		t.Fatalf("配额只剩 1 件，买 2 件应当按门店价 5000，实得 %d", r.UnitPrices[0])
	}
}

func TestPerUserLimitViolation(t *testing.T) {
	// 每人限购 2 件、已买 1 件：再买 1 件可以，再买 2 件超限（试算与下单报 409）。
	m := lp(promo(5, repository.PromoLimitedPrice, 0, true))
	m.Offers[1] = []repository.PriceOffer{offer(5, 1, 3990, 0, 2, 0, 0)}
	bought := map[repository.PurchaseKey]int32{{PromotionID: 5, SKUID: 1}: 1}
	if r := computePromotions([]promoLine{pline(1, 1, 5990, 1, "/1/")}, m, anyStore, bought); len(r.Violations) != 0 {
		t.Fatalf("1 + 1 ≤ 2，不该超限：%+v", r.Violations)
	}
	r := computePromotions([]promoLine{pline(1, 1, 5990, 2, "/1/")}, m, anyStore, bought)
	if len(r.Violations) != 1 || r.Violations[0].Limit != 2 || r.Violations[0].Bought != 1 {
		t.Fatalf("1 + 2 > 2，应当超限：%+v", r.Violations)
	}
	// 购物车不传已购件数：不判限购（那是下单前的一道闸）。
	if r := computePromotions([]promoLine{pline(1, 1, 5990, 5, "/1/")}, m, anyStore, nil); len(r.Violations) != 0 {
		t.Fatalf("没传已购件数时不该判限购：%+v", r.Violations)
	}
}

func TestThresholdIsMeasuredOnActivityPrice(t *testing.T) {
	// 门店价 120 元、特价 90 元、满 100 减 10：按活动价算是 90，不满 100 —— 不能按门店价凑门槛。
	m := lp(promo(5, repository.PromoLimitedPrice, 0, true),
		promo(1, repository.PromoFullReduction, repository.ThresholdByAmount, true))
	m.Offers[1] = []repository.PriceOffer{offer(5, 1, 9000, 0, 0, 0, 0)}
	m.Tiers[1] = []repository.PromotionTier{tierOff(10000, 1000)}
	r := computePromotions([]promoLine{pline(1, 1, 12000, 1, "/1/")}, m, anyStore, nil)
	if r.DiscountCents != 0 {
		t.Fatalf("活动价 90 元不满 100，满减不该命中，实减 %d", r.DiscountCents)
	}
	if h := hitOf(t, r, 1); h.Shortfall == nil || *h.Shortfall != 1000 {
		t.Fatalf("应当报还差 10 元：%+v", h)
	}
}

// ---------------------------------------------------------------------------
// 与券：叠加开关、券按活动后金额算
// ---------------------------------------------------------------------------

func TestNonStackablePromotionBlocksCoupons(t *testing.T) {
	m := lp(promo(1, repository.PromoFullReduction, repository.ThresholdByAmount, false))
	m.Tiers[1] = []repository.PromotionTier{tierOff(100, 10)}
	r := computePromotions([]promoLine{pline(1, 1, 1000, 1, "/1/")}, m, anyStore, nil)
	if len(r.CouponBlockers) != 1 {
		t.Fatalf("命中了不与券同享的活动，应当挡住券：%+v", r.CouponBlockers)
	}
	// 没命中（不满门槛）就不挡。
	r = computePromotions([]promoLine{pline(1, 1, 50, 1, "/1/")}, m, anyStore, nil)
	if len(r.CouponBlockers) != 0 {
		t.Fatalf("没命中的活动不该挡券：%+v", r.CouponBlockers)
	}
}

func TestCouponThresholdUsesPostPromotionAmount(t *testing.T) {
	// 商品 100 元，满减活动减 10 → 活动后 90 元；一张满 100 减 20 的券不该可用（差 10 元）。
	// 00044 之前券按 100 元判门槛，这张券能用 —— 那是在已经打过折的钱上再凑一次门槛。
	lines := []PricedLine{{ProductID: 1, AmountCents: 10000, PromotionDiscountCents: 1000, categoryPath: str("/1/")}}
	in := couponLinesOf(lines)
	if in[0].AmountCents != 9000 {
		t.Fatalf("券的计算基数应当是活动后金额 9000，实得 %d", in[0].AmountCents)
	}
	mustReject(t, evaluateCoupon(fullReduction(10000, 2000), nil, anyStore, in, calcNow), "还差 1000 分")

	// 一张立减 95 元的券：封顶在活动后金额 90 元上，于是这一行「活动 10 + 券 90」= 行金额 100，
	// 不会超过（chk_item_promotion_discount / chk_item_refund 都成立）。
	v := mustApply(t, evaluateCoupon(instant(9500), nil, anyStore, in, calcNow))
	if v.DiscountCents != 9000 || lines[0].PromotionDiscountCents+v.LineDiscounts[0] != 10000 {
		t.Fatalf("券应当封顶在 9000，活动 + 券 = 行金额：券 %d", v.DiscountCents)
	}
}

// ---------------------------------------------------------------------------
// 展示：标签与金额写法
// ---------------------------------------------------------------------------

func TestPromotionLabels(t *testing.T) {
	cases := []struct {
		typ, unit int16
		tier      repository.PromotionTier
		want      string
	}{
		{repository.PromoFullReduction, repository.ThresholdByAmount, tierOff(10000, 1000), "满100减10"},
		{repository.PromoFullReduction, repository.ThresholdByQty, tierOff(3, 2050), "满3件减20.5"},
		{repository.PromoFullDiscount, repository.ThresholdByQty, tierRate(2, 900), "满2件9折"},
		{repository.PromoFullDiscount, repository.ThresholdByAmount, tierRate(19990, 875), "满199.9享8.75折"},
	}
	for _, c := range cases {
		if got := tierLabel(c.typ, c.unit, c.tier); got != c.want {
			t.Fatalf("标签 %q，期望 %q", got, c.want)
		}
	}
	if yuan(1005) != "10.05" || yuan(1050) != "10.5" || yuan(100) != "1" || yuan(7) != "0.07" {
		t.Fatalf("yuan 写法不对：%s %s %s %s", yuan(1005), yuan(1050), yuan(100), yuan(7))
	}
}

func TestProductTagsFollowTheSameScopeRules(t *testing.T) {
	m := overlapPromos()
	m.Promos[5] = promo(5, repository.PromoFlashSale, 0, true)
	offers := []repository.PriceOffer{
		{PromotionID: 5, SKUID: 1, PromoPriceCents: 990, StockQty: 10, StorePriceCents: 5000},
		{PromotionID: 5, SKUID: 2, PromoPriceCents: 1990, StockQty: 10, StorePriceCents: 5000},
	}
	dress := repository.ProductPromotionFacts{ProductID: 1, CategoryPath: str("/1/2/3/")}
	tags := productPromotionTags(dress, offers, m, anyStore)
	want := []string{"秒杀 ¥9.9", "满200减30", "满100减20"}
	if len(tags) != len(want) {
		t.Fatalf("标签 %+v，期望 %v", tags, want)
	}
	for i := range want {
		if tags[i].Label != want[i] {
			t.Fatalf("第 %d 个标签 %q，期望 %q", i, tags[i].Label, want[i])
		}
	}
	food := repository.ProductPromotionFacts{ProductID: 2, CategoryPath: str("/9/")}
	if tags := productPromotionTags(food, nil, m, anyStore); len(tags) != 1 || tags[0].Label != "满200减30" {
		t.Fatalf("零食只该有全场满减的标签：%+v", tags)
	}
}
