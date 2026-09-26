package service

import (
	"context"
	"time"

	"github.com/keel/keel/internal/repository"
)

// 商品列表 / 详情上的活动标签（契约 ProductSummary.promotion_tags、Sku.promo_price_cents）。
//
// 标签按**这家门店、此刻**算，判据与试算是同一份（promotion_calc.go 的
// productPromotionTags / skuPromoPrices → pickPriceOffer、lineEligible、storeAllowed）：
// 列表上写着「满100减10」，把这件商品加到 100 元去试算，就真的减 10。
//
// 没有任何生效活动时只花一条查询（ListLivePromotions），列表页不为活动多付代价。

// promotionTagsFor 取一批商品在这家店此刻的活动标签，以及它们各 SKU 的活动价。
func promotionTagsFor(ctx context.Context, tx repository.Tx, sc repository.StoreScope,
	productIDs []int64, now time.Time) (map[int64][]ProductPromotionTag, map[int64]SkuPromoPrice, error) {
	tags := map[int64][]ProductPromotionTag{}
	prices := map[int64]SkuPromoPrice{}
	if len(productIDs) == 0 {
		return tags, prices, nil
	}
	lp, err := loadLivePromotions(ctx, tx, nil, now)
	if err != nil || lp.empty() {
		return tags, prices, err
	}
	offers, err := tx.ListLivePriceOffersForProducts(ctx, sc.StoreID, productIDs, now)
	if err != nil {
		return nil, nil, err
	}
	facts, err := tx.ListProductPromotionFacts(ctx, productIDs)
	if err != nil {
		return nil, nil, err
	}
	byProduct := map[int64][]repository.PriceOffer{}
	for _, o := range offers {
		if _, ok := lp.Promos[o.PromotionID]; ok {
			byProduct[o.ProductID] = append(byProduct[o.ProductID], o)
		}
	}
	for _, id := range productIDs {
		f, ok := facts[id]
		if !ok {
			f = repository.ProductPromotionFacts{ProductID: id}
		}
		if t := productPromotionTags(f, byProduct[id], lp, sc); len(t) > 0 {
			tags[id] = t
		}
		for sku, p := range skuPromoPrices(byProduct[id], lp, sc) {
			prices[sku] = p
		}
	}
	return tags, prices, nil
}
