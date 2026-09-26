package service

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/keel/keel/internal/repository"
)

// 营销活动怎么算。**全仓库只有这一份实现**（数据模型 §7「营销活动」）。
//
// 试算（POST /orders/preview）、下单（POST /orders）、「本单可用券」、购物车（GET /cart）
// 四处都调 computePromotions，商品列表 / 详情的活动标签调同一个文件里的 productPromotionTags ——
// 与券的 evaluateCoupon 同一条纪律：试算说减多少，下单就减多少，不是因为两边约定好了，
// 而是根本没有第二份代码可以跑偏。
//
// 它是纯函数：不碰数据库、不看时钟（「此刻生效的活动」由调用方按同一个 now 取来），
// 于是阶梯、贪心、分摊余数、叠加规则都能在没有数据库的单元测试里逐分核对
// （promotion_calc_test.go）。
//
// # 计价顺序（与运费模板约定，写死）
//
//	门店最终价 → ① 限时折扣 / 秒杀改单价 → ② 满减 / 满折按行分摊 → ③ 券 → ④ 运费
//
// ① 改的是**价**：活动价 = min(门店价, 特价)。特价比门店价还高时活动不生效 ——
// 否则门店单独降了价，「限时特价」反倒让买家多付钱。它不进 discount_cents，
// 省下的钱体现在单价与 goods_amount_cents 里（PromotionHit 另报一个「省了多少」给人看）。
//
// ② 是**优惠**：按行分摊，一行至多参与一个满减满折（见 pickThresholdPromotions 的贪心）。
//
// ③ 券的门槛与计算基数是 ② 之后的每行金额（couponLinesOf），不在这里算。

// promoLine 是参与活动计算的一行：定价结果与挑行的素材。
type promoLine struct {
	SKUID          int64
	ProductID      int64
	BrandID        *int64
	CategoryPath   *string
	ListPriceCents int64
	Quantity       int32
}

// livePromotions 是「此刻生效」的全部活动素材，一次请求取一次（loadLivePromotions）。
type livePromotions struct {
	Promos map[int64]repository.Promotion
	Tiers  map[int64][]repository.PromotionTier
	Scopes map[int64][]repository.CouponScope
	// Offers 按 sku_id 分组的单价类报价（只含 Promos 里的活动）。
	Offers map[int64][]repository.PriceOffer
}

func (lp livePromotions) empty() bool { return len(lp.Promos) == 0 }

// PromotionHit 是一个活动在这一单（或购物车已勾选的行）上的结果。契约的 PromotionHit。
type PromotionHit struct {
	PromotionID     int64
	Name            string
	Type            int16
	Applied         bool
	DiscountCents   int64
	StackWithCoupon bool
	ThresholdUnit   int16
	// ReachedThreshold 命中的那一档门槛；未命中为 nil。
	ReachedThreshold *int64
	// NextThreshold / Shortfall 离下一档（未命中时即最低一档）还差多少；已是最高档为 nil。
	NextThreshold *int64
	Shortfall     *int64
	SKUIDs        []int64
	Message       string
}

// promoLimitViolation 是一行超出了每人限购。试算与下单把它翻成 409，购物车只展示。
type promoLimitViolation struct {
	PromotionID int64
	SKUID       int64
	Limit       int32
	Bought      int32
	Asked       int32
}

// promoResult 是 computePromotions 的结论，各切片与输入的 lines 一一对应。
type promoResult struct {
	// UnitPrices 是每行的成交单价（门店价或活动价）。
	UnitPrices []int64
	// PricePromotionIDs 是改了这一行单价的活动；没有为 nil。
	PricePromotionIDs []*int64
	// LineDiscounts 是满减满折分摊到每一行的优惠，求和恒等于 DiscountCents。
	LineDiscounts []int64
	DiscountCents int64
	Hits          []PromotionHit
	// CouponBlockers 是命中了、且不与券同享的活动名。非空时这一单不能再用券。
	CouponBlockers []string
	Violations     []promoLimitViolation
}

// computePromotions 算出这些行在这些活动下的单价、分摊与命中明细。
//
// bought 是这个买家在各（活动 × SKU）上已经买了几件（每人限购的判据）；购物车传 nil。
func computePromotions(lines []promoLine, lp livePromotions, store repository.StoreScope,
	bought map[repository.PurchaseKey]int32) promoResult {

	res := promoResult{
		UnitPrices:        make([]int64, len(lines)),
		PricePromotionIDs: make([]*int64, len(lines)),
		LineDiscounts:     make([]int64, len(lines)),
		Hits:              []PromotionHit{},
	}
	priceHits := map[int64]*PromotionHit{}
	var priceOrder []int64

	// ---- ① 单价类：活动价 = min(门店价, 特价) ----
	for i, ln := range lines {
		res.UnitPrices[i] = ln.ListPriceCents
		best, bestPrice := pickPriceOffer(ln, lp, store)
		if best == nil {
			continue
		}
		p := lp.Promos[best.PromotionID]
		if best.PerUserLimit > 0 && bought != nil {
			had := bought[repository.PurchaseKey{PromotionID: best.PromotionID, SKUID: ln.SKUID}]
			if had+ln.Quantity > best.PerUserLimit {
				res.Violations = append(res.Violations, promoLimitViolation{
					PromotionID: best.PromotionID, SKUID: ln.SKUID,
					Limit: best.PerUserLimit, Bought: had, Asked: ln.Quantity,
				})
			}
		}
		res.UnitPrices[i] = bestPrice
		id := best.PromotionID
		res.PricePromotionIDs[i] = &id

		h, ok := priceHits[id]
		if !ok {
			h = &PromotionHit{
				PromotionID: id, Name: p.Name, Type: p.Type, Applied: true,
				StackWithCoupon: p.StackWithCoupon, SKUIDs: []int64{},
			}
			priceHits[id] = h
			priceOrder = append(priceOrder, id)
		}
		h.DiscountCents += (ln.ListPriceCents - bestPrice) * int64(ln.Quantity)
		h.SKUIDs = append(h.SKUIDs, ln.SKUID)
	}
	for _, id := range priceOrder {
		h := priceHits[id]
		verb := "限时特价"
		if h.Type == repository.PromoFlashSale {
			verb = "秒杀价"
		}
		h.Message = fmt.Sprintf("%s，已省 %s 元", verb, yuan(h.DiscountCents))
		res.Hits = append(res.Hits, *h)
	}

	// ---- ② 满减 / 满折：按活动价算的行金额 ----
	amounts := make([]int64, len(lines))
	for i, ln := range lines {
		amounts[i] = res.UnitPrices[i] * int64(ln.Quantity)
	}
	thr := pickThresholdPromotions(lines, amounts, lp, store)
	res.LineDiscounts = thr.LineDiscounts
	res.DiscountCents = thr.DiscountCents
	res.Hits = append(res.Hits, thr.Hits...)

	// ---- 叠加：命中了不与券同享的活动，这一单不能再用券 ----
	for _, h := range res.Hits {
		if h.Applied && !h.StackWithCoupon {
			res.CouponBlockers = append(res.CouponBlockers, h.Name)
		}
	}
	return res
}

// pickPriceOffer 在一行身上此刻生效的报价里挑价最低的那个。
//
// 不参与的报价：活动不在 Promos 里（未上线 / 不在有效期）、门店不在范围、特价不低于门店价
// （活动价 = min(门店价, 特价)，特价更贵时活动就是不生效）、秒杀配额不够这一行的件数
// （配额不够就按门店价报价 —— 与其报一个下单时注定扣不到配额的价，不如现在就说实话）。
// 同价取活动 id 小的：结果必须确定，试算与下单才会挑中同一个。
func pickPriceOffer(ln promoLine, lp livePromotions, store repository.StoreScope) (*repository.PriceOffer, int64) {
	var best *repository.PriceOffer
	bestPrice := ln.ListPriceCents
	for k := range lp.Offers[ln.SKUID] {
		o := &lp.Offers[ln.SKUID][k]
		p, ok := lp.Promos[o.PromotionID]
		if !ok || (p.Type != repository.PromoLimitedPrice && p.Type != repository.PromoFlashSale) {
			continue
		}
		if !storeAllowed(lp.Scopes[o.PromotionID], store) {
			continue
		}
		if o.StockQty > 0 && o.SoldQty+ln.Quantity > o.StockQty {
			continue
		}
		price := offerPrice(*o, ln.ListPriceCents)
		if price >= ln.ListPriceCents {
			continue
		}
		if best == nil || price < bestPrice || (price == bestPrice && o.PromotionID < best.PromotionID) {
			best, bestPrice = o, price
		}
	}
	return best, bestPrice
}

// offerPrice 是一个报价在某个门店价上的活动单价。
//
// 折扣类：⌈门店价 × rate / 1000⌉，**向上取整到分** —— 与券「折扣向下取整」是同一个方向
// （误差对商家有利、每件不到 1 分）：券算的是减免（向下），这里算的是价（向上）。
func offerPrice(o repository.PriceOffer, list int64) int64 {
	if o.PromoPriceCents > 0 {
		return o.PromoPriceCents
	}
	return list - rateDiscount(list, o.DiscountRate)
}

// thresholdOutcome 是满减满折那一步的结论。
type thresholdOutcome struct {
	LineDiscounts []int64
	DiscountCents int64
	Hits          []PromotionHit
}

// thresholdEval 是一个满减满折活动在一组行上的评估。
type thresholdEval struct {
	Promo    repository.Promotion
	Eligible []bool
	Subtotal int64 // 参与行的金额之和（分）
	Measure  int64 // 门槛比的那个量：金额或件数
	Tier     *repository.PromotionTier
	Off      int64
}

// pickThresholdPromotions 选出这一单命中的满减满折，并把优惠分摊到行。
//
// # 一行至多参与一个满减满折：贪心
//
// 两个活动范围重叠（全场满 200 减 30、服装满 100 减 20）时，同一件衣服不能被减两次 ——
// 那是按活动的字面意思都说不通的叠加，而且把「商家最多让利多少」变成了一个配置组合爆炸的问题。
// 规则：
//
//  1. 在还没被占用的行上逐个评估活动，挑**减得最多**的那一个（同额取 id 小的，结果必须确定）；
//  2. 它范围内的行全部归它（包括凑单但不再影响门槛的行），按金额比例分摊（allocateDiscount，
//     与券同一份分摊与余数规则）；
//  3. 在剩下的行上重复，直到没有活动还能命中。
//
// 范围不重叠时（服装满 100 减 10、食品满 50 减 5）两个都命中，各减各的 —— 那正是买家预期的。
// 贪心不保证全局最优（极端配置下换一种分法可能多减几分钱），换来的是规则一句话能讲清、
// 结果确定、试算与下单不可能分叉。
func pickThresholdPromotions(lines []promoLine, amounts []int64, lp livePromotions,
	store repository.StoreScope) thresholdOutcome {

	out := thresholdOutcome{LineDiscounts: make([]int64, len(lines)), Hits: []PromotionHit{}}
	var candidates []repository.Promotion
	for _, p := range lp.Promos {
		if p.Type != repository.PromoFullReduction && p.Type != repository.PromoFullDiscount {
			continue
		}
		if len(lp.Tiers[p.ID]) == 0 || !storeAllowed(lp.Scopes[p.ID], store) {
			continue
		}
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		return out
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })

	taken := make([]bool, len(lines))
	applied := map[int64]bool{}
	for {
		var best *thresholdEval
		for _, p := range candidates {
			if applied[p.ID] {
				continue
			}
			ev := evalThreshold(p, lp, lines, amounts, taken)
			if ev.Off <= 0 {
				continue
			}
			if best == nil || ev.Off > best.Off {
				e := ev
				best = &e
			}
		}
		if best == nil {
			break
		}
		applied[best.Promo.ID] = true
		cl := make([]couponLine, len(lines))
		for i := range lines {
			cl[i] = couponLine{AmountCents: amounts[i]}
		}
		alloc := allocateDiscount(cl, best.Eligible, best.Subtotal, best.Off)
		skuIDs := []int64{}
		for i := range lines {
			if best.Eligible[i] {
				taken[i] = true
				out.LineDiscounts[i] += alloc[i]
				skuIDs = append(skuIDs, lines[i].SKUID)
			}
		}
		out.DiscountCents += best.Off
		out.Hits = append(out.Hits, thresholdHit(*best, lp.Tiers[best.Promo.ID], skuIDs, true))
	}

	// 没命中的活动：在还没被占用、且在它范围内的行上报「还差多少」。一行都没有就不报 ——
	// 它范围内的东西全被别的活动占了，或者这一单里本来就没有它的商品，说「还差 100 元」只是噪音。
	for _, p := range candidates {
		if applied[p.ID] {
			continue
		}
		ev := evalThreshold(p, lp, lines, amounts, taken)
		if ev.Subtotal <= 0 {
			continue
		}
		skuIDs := []int64{}
		for i := range lines {
			if ev.Eligible[i] {
				skuIDs = append(skuIDs, lines[i].SKUID)
			}
		}
		out.Hits = append(out.Hits, thresholdHit(ev, lp.Tiers[p.ID], skuIDs, false))
	}
	return out
}

// evalThreshold 评估一个满减满折活动在「未被占用、且在范围内」的行上能减多少。
func evalThreshold(p repository.Promotion, lp livePromotions, lines []promoLine, amounts []int64,
	taken []bool) thresholdEval {
	ev := thresholdEval{Promo: p, Eligible: make([]bool, len(lines))}
	scopes := lp.Scopes[p.ID]
	var qty int64
	for i, ln := range lines {
		if taken[i] || amounts[i] <= 0 {
			continue
		}
		if !lineEligible(scopes, couponLine{
			ProductID: ln.ProductID, BrandID: ln.BrandID, CategoryPath: ln.CategoryPath,
		}) {
			continue
		}
		ev.Eligible[i] = true
		ev.Subtotal += amounts[i]
		qty += int64(ln.Quantity)
	}
	if ev.Subtotal <= 0 {
		return ev
	}
	ev.Measure = ev.Subtotal
	if p.ThresholdUnit == repository.ThresholdByQty {
		ev.Measure = qty
	}
	tiers := lp.Tiers[p.ID] // 按门槛升序（查询保证）
	for k := len(tiers) - 1; k >= 0; k-- {
		if tiers[k].Threshold <= ev.Measure {
			t := tiers[k]
			ev.Tier = &t
			break
		}
	}
	if ev.Tier == nil {
		return ev
	}
	switch p.Type {
	case repository.PromoFullReduction:
		ev.Off = ev.Tier.DiscountCents
	case repository.PromoFullDiscount:
		ev.Off = rateDiscount(ev.Subtotal, ev.Tier.DiscountRate)
	}
	// 不超过参与行的金额：应付因此不可能为负（与券第 3 步同一条）。
	if ev.Off > ev.Subtotal {
		ev.Off = ev.Subtotal
	}
	return ev
}

// thresholdHit 把一次评估装成 PromotionHit，连同「离下一档还差多少」。
func thresholdHit(ev thresholdEval, tiers []repository.PromotionTier, skuIDs []int64, applied bool) PromotionHit {
	p := ev.Promo
	h := PromotionHit{
		PromotionID: p.ID, Name: p.Name, Type: p.Type, Applied: applied,
		StackWithCoupon: p.StackWithCoupon, ThresholdUnit: p.ThresholdUnit, SKUIDs: skuIDs,
	}
	if applied {
		h.DiscountCents = ev.Off
		th := ev.Tier.Threshold
		h.ReachedThreshold = &th
	}
	var next *repository.PromotionTier
	for k := range tiers {
		if tiers[k].Threshold > ev.Measure {
			next = &tiers[k]
			break
		}
	}
	if next != nil {
		nt := next.Threshold
		short := next.Threshold - ev.Measure
		h.NextThreshold, h.Shortfall = &nt, &short
	}

	var b strings.Builder
	if applied {
		fmt.Fprintf(&b, "已减 %s 元", yuan(ev.Off))
		if next != nil {
			fmt.Fprintf(&b, "，再买 %s 可%s", measureText(p.ThresholdUnit, *h.Shortfall), tierBenefit(p.Type, *next))
		}
	} else if next != nil {
		fmt.Fprintf(&b, "还差 %s %s", measureText(p.ThresholdUnit, *h.Shortfall), tierBenefit(p.Type, *next))
	}
	h.Message = b.String()
	return h
}

func measureText(unit int16, v int64) string {
	if unit == repository.ThresholdByQty {
		return fmt.Sprintf("%d 件", v)
	}
	return yuan(v) + " 元"
}

// tierBenefit 是一档的好处，用在「还差 50 元 减 30 元」「再买 1 件 享 9 折」这类句子里。
func tierBenefit(typ int16, t repository.PromotionTier) string {
	if typ == repository.PromoFullDiscount {
		return "享 " + rateText(t.DiscountRate) + "折"
	}
	return "减 " + yuan(t.DiscountCents) + " 元"
}

// tierLabel 是一档的标签：「满100减10」「满3件减20」「满2件9折」「满200享8.5折」。
func tierLabel(typ, unit int16, t repository.PromotionTier) string {
	th := yuan(t.Threshold)
	if unit == repository.ThresholdByQty {
		th = fmt.Sprintf("%d件", t.Threshold)
	}
	if typ == repository.PromoFullDiscount {
		if unit == repository.ThresholdByQty {
			return "满" + th + rateText(t.DiscountRate) + "折"
		}
		return "满" + th + "享" + rateText(t.DiscountRate) + "折"
	}
	return "满" + th + "减" + yuan(t.DiscountCents)
}

// yuan 把分写成给人看的元：1000 → "10"，1050 → "10.5"，1005 → "10.05"。不经过浮点。
func yuan(cents int64) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	s := fmt.Sprintf("%d", cents/100)
	if f := cents % 100; f != 0 {
		s += strings.TrimRight(fmt.Sprintf(".%02d", f), "0")
	}
	if neg {
		return "-" + s
	}
	return s
}

// rateText 把千分比写成「几折」：900 → "9"，850 → "8.5"，875 → "8.75"。
func rateText(rate int16) string {
	s := fmt.Sprintf("%d", rate/100)
	if f := rate % 100; f != 0 {
		s += strings.TrimRight(fmt.Sprintf(".%02d", f), "0")
	}
	return s
}

// ProductPromotionTag 是商品列表 / 详情上的一个活动标签。契约的 PromotionTag。
type ProductPromotionTag struct {
	PromotionID int64
	Type        int16
	Label       string
	EndsAt      *time.Time
}

// productPromotionTags 算一件商品在这家店此刻的活动标签。与 computePromotions 同一套判据：
// 满减满折按范围（商品 / 分类含子孙 / 品牌 + 门店维度）判参不参与，单价类按 pickPriceOffer 挑价。
//
// offers 是这件商品全部 SKU 的报价（带门店价）。标签给的是**最低**的活动价（「限时特价 ¥39.9」
// 说的是这件商品起售的那个价），各 SKU 自己的活动价在 skuPromoPrices 里。
func productPromotionTags(f repository.ProductPromotionFacts, offers []repository.PriceOffer,
	lp livePromotions, store repository.StoreScope) []ProductPromotionTag {
	tags := []ProductPromotionTag{}

	// 单价类：按 SKU 挑出各自的活动价，再按活动取最低价。
	type best struct {
		price int64
		typ   int16
	}
	byPromo := map[int64]best{}
	var order []int64
	for _, price := range skuPromoPrices(offers, lp, store) {
		b, ok := byPromo[price.PromotionID]
		if !ok {
			order = append(order, price.PromotionID)
		}
		if !ok || price.PriceCents < b.price {
			byPromo[price.PromotionID] = best{price: price.PriceCents, typ: lp.Promos[price.PromotionID].Type}
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	for _, id := range order {
		b := byPromo[id]
		label := "限时特价 ¥" + yuan(b.price)
		if b.typ == repository.PromoFlashSale {
			label = "秒杀 ¥" + yuan(b.price)
		}
		end := lp.Promos[id].EndsAt
		tags = append(tags, ProductPromotionTag{PromotionID: id, Type: b.typ, Label: label, EndsAt: &end})
	}

	// 满减满折：范围命中即出标签，标签写最低一档。
	var ids []int64
	for id, p := range lp.Promos {
		if (p.Type == repository.PromoFullReduction || p.Type == repository.PromoFullDiscount) &&
			len(lp.Tiers[id]) > 0 {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		p := lp.Promos[id]
		if !storeAllowed(lp.Scopes[id], store) {
			continue
		}
		if !lineEligible(lp.Scopes[id], couponLine{
			ProductID: f.ProductID, BrandID: f.BrandID, CategoryPath: f.CategoryPath,
		}) {
			continue
		}
		end := p.EndsAt
		tags = append(tags, ProductPromotionTag{
			PromotionID: id, Type: p.Type, Label: tierLabel(p.Type, p.ThresholdUnit, lp.Tiers[id][0]),
			EndsAt: &end,
		})
	}
	return tags
}

// SkuPromoPrice 是一个 SKU 在这家店此刻的活动价与给出它的活动。
type SkuPromoPrice struct {
	PromotionID int64
	PriceCents  int64
}

// skuPromoPrices 按 SKU 挑出活动价（与下单同一个 pickPriceOffer，按 1 件判秒杀配额）。
// 没有生效报价、或特价不低于门店价的 SKU 不在结果里。
func skuPromoPrices(offers []repository.PriceOffer, lp livePromotions,
	store repository.StoreScope) map[int64]SkuPromoPrice {
	bySKU := map[int64][]repository.PriceOffer{}
	list := map[int64]int64{}
	for _, o := range offers {
		bySKU[o.SKUID] = append(bySKU[o.SKUID], o)
		list[o.SKUID] = o.StorePriceCents
	}
	out := map[int64]SkuPromoPrice{}
	for sku, os := range bySKU {
		view := livePromotions{Promos: lp.Promos, Scopes: lp.Scopes,
			Offers: map[int64][]repository.PriceOffer{sku: os}}
		best, price := pickPriceOffer(promoLine{SKUID: sku, ListPriceCents: list[sku], Quantity: 1}, view, store)
		if best != nil {
			out[sku] = SkuPromoPrice{PromotionID: best.PromotionID, PriceCents: price}
		}
	}
	return out
}
