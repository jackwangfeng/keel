package handler_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/keel/keel/internal/api"
)

// 营销活动与运费的边界：满额包邮比的是活动之后的金额（计价顺序：门店价 → 活动 → 券 → 运费 →
// 包邮券抵运费，数据模型 §7「优惠计算顺序」）。
//
// 全国满 99 包邮（首件 8 元、续件 2 元）+ 全场满 100 减 10：
//
//	两件衬衫 100 元：活动减 10 → 90 元，不满 99，收 8 + 2 = 10 元运费；应付 100 − 10 + 10 = 100。
//	把活动下线再试算：100 元满 99，包邮，应付 100。
//
// 按门店价（100）判包邮的实现会给出「减 10 又包邮、应付 90」—— 商家的包邮门槛被活动悄悄打了折，
// 这条会红。限时特价同理：连衣裙特价 49 × 2 = 98 元，不满 99。购物车的预估运费走同一个口径。
func TestFreeShippingThresholdIsMeasuredAfterPromotions(t *testing.T) {
	cs := newCouponShop(t)
	cs.createFreight(t, nationwideFreight)
	t.Cleanup(func() {
		adminExec(t, `DELETE FROM cart_items WHERE merchant_id = $1`, cs.MerchantID)
		adminExec(t, `DELETE FROM carts WHERE merchant_id = $1`, cs.MerchantID)
	})
	promo := cs.livePromotion(t, "满100减10", fullReduction100)
	b := cs.newBuyer(t, "promo-freight")

	body := orderBodyAt(b.Address, cs.NorthStore, [][2]int64{{cs.ShirtSKU, 2}}, nil)
	pv := cs.mustPreview(t, b, body)
	if pv.PromotionDiscountCents != 1000 || pv.FreightCents != 1000 || pv.PayableCents != 10000 {
		t.Fatalf("活动后 90 元不满 99：活动 %d 运费 %d 应付 %d，期望 1000 / 1000 / 10000",
			pv.PromotionDiscountCents, pv.FreightCents, pv.PayableCents)
	}
	var o api.Order
	decodeInto(t, createOrder(t, cs.Host, body, b.Token, "pf-"+uniqueKey()), http.StatusCreated, "下单", &o)
	if o.PayableCents != 10000 || *o.FreightCents != 1000 || *o.PromotionDiscountCents != 1000 {
		t.Fatalf("订单：应付 %d 运费 %d 活动 %d", o.PayableCents, *o.FreightCents, *o.PromotionDiscountCents)
	}

	// 购物车：两件衬衫，预估运费同样按活动后的 90 元判。
	wantStatus(t, postWithKey(t, cs.Host, fmt.Sprintf("/api/v1/cart/items?store_id=%d", cs.NorthStore),
		fmt.Sprintf(`{"sku_id":%d,"quantity":2}`, cs.ShirtSKU), b.Token, freshIdemKey()), http.StatusOK, "加购")
	var cart api.Cart
	decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/cart?store_id=%d&address_id=%d", cs.NorthStore, b.Address), b.Token),
		http.StatusOK, "购物车", &cart)
	if cart.PromotionDiscountCents != 1000 || cart.Freight == nil || cart.Freight.FreightCents != 1000 {
		t.Fatalf("购物车：活动 %d，预估运费 %+v，期望活动 1000、运费 1000", cart.PromotionDiscountCents, cart.Freight)
	}

	// 活动下线：100 元满 99，包邮。
	wantStatus(t, cs.patchPromotion(t, promo.Id, `{"status":0}`), http.StatusOK, "下线")
	pv = cs.mustPreview(t, b, body)
	if pv.PromotionDiscountCents != 0 || pv.FreightCents != 0 || pv.PayableCents != 10000 {
		t.Fatalf("没有活动时 100 元应包邮：活动 %d 运费 %d 应付 %d", pv.PromotionDiscountCents, pv.FreightCents, pv.PayableCents)
	}

	// 限时特价：连衣裙 49 × 2 = 98 元，不满 99（门店价 120 元是满的）。
	cs.livePromotion(t, "连衣裙特价", fmt.Sprintf(`"promotion_type":3,"skus":[{"sku_id":%d,"promo_price_cents":4900}]`, cs.DressSKU))
	pv = cs.mustPreview(t, b, orderBodyAt(b.Address, cs.NorthStore, [][2]int64{{cs.DressSKU, 2}}, nil))
	if pv.GoodsAmountCents != 9800 || pv.FreightCents != 1000 || pv.PayableCents != 10800 {
		t.Fatalf("特价 98 元不满 99：商品 %d 运费 %d 应付 %d", pv.GoodsAmountCents, pv.FreightCents, pv.PayableCents)
	}
}
