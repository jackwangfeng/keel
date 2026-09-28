package service

import (
	"context"
	"time"

	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
)

// 商品列表 / 详情上的活动标签（契约 ProductSummary.promotion_tags、Sku.promo_price_cents）。
//
// 标签按**这家门店、此刻**算，判据与试算是同一份（promotion_calc.go 的
// productPromotionTags / skuPromoPrices → pickPriceOffer、lineEligible、storeAllowed）：
// 列表上写着「满100减10」，把这件商品加到 100 元去试算，就真的减 10。
//
// 没有任何生效活动时只花一条查询（ListLivePromotions），列表页不为活动多付代价。
//
// ### 两段：事务里取素材，事务外问配额再算（微服务拆分阶段 1b）
//
// 秒杀的「配额够不够」决定挑不挑那个报价（pickPriceOffer），而配额 00075 起在库存服务。
// 问库存服务不能在事务里（单体下是整池互等，拆分下是攥着连接等网络），所以拆成两段：
// loadPromotionTags 在事务里取素材（活动、报价、商品的品牌与分类），finish 在事务之后
// ——有单价类报价时——向库存服务批量问一次配额，再按同一套判据算。没有报价的页面不问。

// promoTagMaterial 是事务里取好的素材。
type promoTagMaterial struct {
	sc         repository.StoreScope
	productIDs []int64
	lp         livePromotions
	byProduct  map[int64][]repository.PriceOffer
	facts      map[int64]repository.ProductPromotionFacts
}

// loadPromotionTags 在事务里取一批商品在这家店此刻的活动素材。
func loadPromotionTags(ctx context.Context, tx repository.Tx, sc repository.StoreScope,
	productIDs []int64, now time.Time) (promoTagMaterial, error) {
	m := promoTagMaterial{sc: sc, productIDs: productIDs, byProduct: map[int64][]repository.PriceOffer{}}
	if len(productIDs) == 0 {
		return m, nil
	}
	lp, err := loadLivePromotions(ctx, tx, nil, now, nil)
	if err != nil || lp.empty() {
		m.lp = lp
		return m, err
	}
	m.lp = lp
	offers, err := tx.ListLivePriceOffersForProducts(ctx, sc.StoreID, productIDs, now)
	if err != nil {
		return promoTagMaterial{}, err
	}
	if m.facts, err = tx.ListProductPromotionFacts(ctx, productIDs); err != nil {
		return promoTagMaterial{}, err
	}
	for _, o := range offers {
		if _, ok := lp.Promos[o.PromotionID]; ok {
			m.byProduct[o.ProductID] = append(m.byProduct[o.ProductID], o)
		}
	}
	return m, nil
}

// finish 在事务之后算标签与各 SKU 的活动价。有单价类报价时先向库存服务问配额：
// 问不到时 degrade 为真（列表）就按「配额未知」算（秒杀与限时折扣的报价都不生效，标签里只剩满减满折）；
// 为假（详情）就把 inventory.ErrUnavailable 交回去（详情整页 503，与水位同一个口径）。
func (m promoTagMaterial) finish(ctx context.Context, inv inventory.Service,
	degrade bool) (map[int64][]ProductPromotionTag, map[int64]SkuPromoPrice, error) {
	tags := map[int64][]ProductPromotionTag{}
	prices := map[int64]SkuPromoPrice{}
	if len(m.productIDs) == 0 || m.lp.empty() {
		return tags, prices, nil
	}
	var skus []int64
	seen := map[int64]bool{}
	for _, os := range m.byProduct {
		for _, o := range os {
			if !seen[o.SKUID] {
				seen[o.SKUID] = true
				skus = append(skus, o.SKUID)
			}
		}
	}
	q := &activityQuotas{asked: true, m: map[inventory.ActivityKey]inventory.Activity{}}
	if len(skus) > 0 {
		got, err := inv.ActivityStock(ctx, inventory.ActivityQuery{SKUIDs: skus})
		switch {
		case err == nil:
			q.m = got
		case degrade && inventory.IsUnavailable(err):
		default:
			return nil, nil, err
		}
	}
	for _, id := range m.productIDs {
		offers := append([]repository.PriceOffer(nil), m.byProduct[id]...)
		if err := q.apply(offers); err != nil {
			return nil, nil, err
		}
		f, ok := m.facts[id]
		if !ok {
			f = repository.ProductPromotionFacts{ProductID: id}
		}
		if t := productPromotionTags(f, offers, m.lp, m.sc); len(t) > 0 {
			tags[id] = t
		}
		for sku, p := range skuPromoPrices(offers, m.lp, m.sc) {
			prices[sku] = p
		}
	}
	return tags, prices, nil
}

// promoFloors 从 finish 算出的各 SKU 活动价里，按商品取最低的那个（列表卡片上的活动价）。
func (m promoTagMaterial) promoFloors(prices map[int64]SkuPromoPrice) map[int64]int64 {
	out := map[int64]int64{}
	for pid, offers := range m.byProduct {
		for _, o := range offers {
			p, ok := prices[o.SKUID]
			if !ok {
				continue
			}
			if cur, seen := out[pid]; !seen || p.PriceCents < cur {
				out[pid] = p.PriceCents
			}
		}
	}
	return out
}

// promoMinPriceOf 是契约 ProductSummary.promo_min_price_cents：活动价里最低的比门店最低价
// 还低时才给（这件商品此刻实际能买到的最低单价）；不低（特价落在较贵的规格上）时缺席，
// 卡片照旧只显示门店价。
func promoMinPriceOf(floors map[int64]int64, productID, minPriceCents int64) *int64 {
	if p, ok := floors[productID]; ok && p < minPriceCents {
		return &p
	}
	return nil
}
