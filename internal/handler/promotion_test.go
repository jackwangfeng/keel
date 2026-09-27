package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 营销活动（数据模型 §7·二，迁移 00058）的端到端测试。夹具是 couponShop：一家新开的店，
// 连衣裙 60 元（服装 / 连衣裙）、衬衫 50 元（服装），华北、华南门店各 50 件。
// 活动全部走后台接口建、上线；买家侧全部走买家接口 —— 测的是装配好的整条链路。

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// relTime 是相对现在的 RFC3339 时间：活动的有效期一律用相对时间，用例不会过期失效。
func relTime(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }

// livePromotion 建一个活动并立刻上线。body 是不带 name / 有效期的那部分 JSON 字段。
func (cs couponShop) livePromotion(t *testing.T, name, fields string) api.AdminPromotion {
	t.Helper()
	p := cs.createPromotion(t, name, fields)
	var out api.AdminPromotion
	decodeInto(t, cs.patchPromotion(t, p.Id, `{"status":1}`), http.StatusOK, "上线活动", &out)
	if out.Phase != "running" {
		t.Fatalf("上线之后活动阶段是 %q，期望 running", out.Phase)
	}
	return out
}

func (cs couponShop) createPromotion(t *testing.T, name, fields string) api.AdminPromotion {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"starts_at":%q,"ends_at":%q,%s}`, name, relTime(-time.Hour), relTime(48*time.Hour), fields)
	var out api.AdminPromotion
	decodeInto(t, postIdem(t, cs.Host, "/api/v1/admin/promotions", body, cs.Token), http.StatusCreated, "建活动", &out)
	if out.Status != 0 || out.Phase != "offline" {
		t.Fatalf("新建的活动应当是下线的：status=%d phase=%s", out.Status, out.Phase)
	}
	return out
}

func (cs couponShop) patchPromotion(t *testing.T, id int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	return reqAs(t, http.MethodPatch, cs.Host, fmt.Sprintf("/api/v1/admin/promotions/%d", id), body, cs.Token)
}

// fullReduction100 是全场「满 100 减 10、满 200 减 30」。
const fullReduction100 = `"promotion_type":1,"threshold_unit":1,"stack_with_coupon":true,` +
	`"tiers":[{"threshold":10000,"discount_cents":1000,"discount_rate":0},{"threshold":20000,"discount_cents":3000,"discount_rate":0}]`

// twoLineJSON：连衣裙 × 2（120 元）+ 衬衫 × 1（50 元），华北门店。
func (cs couponShop) twoLineJSON(b couponBuyer, couponID *int64) string {
	s := fmt.Sprintf(`{"items":[{"sku_id":%d,"quantity":2},{"sku_id":%d,"quantity":1}],"address_id":%d,"store_id":%d`,
		cs.DressSKU, cs.ShirtSKU, b.Address, cs.NorthStore)
	if couponID != nil {
		s += fmt.Sprintf(`,"user_coupon_id":%d`, *couponID)
	}
	return s + "}"
}

func previewItem(t *testing.T, pv api.OrderPreview, sku int64) api.OrderPreviewItem {
	t.Helper()
	for _, it := range pv.Items {
		if it.SkuId == sku {
			return it
		}
	}
	t.Fatalf("试算里没有 sku %d", sku)
	return api.OrderPreviewItem{}
}

func previewHit(t *testing.T, hits []api.PromotionHit, id int64) api.PromotionHit {
	t.Helper()
	for _, h := range hits {
		if h.PromotionId == id {
			return h
		}
	}
	t.Fatalf("结果里没有活动 %d：%+v", id, hits)
	return api.PromotionHit{}
}

// ---------------------------------------------------------------------------
// 满减：试算、下单、快照、分摊，与券叠加，退款净额恒等
// ---------------------------------------------------------------------------

// 两件连衣裙（120）+ 一件衬衫（50）= 170 元。满减活动命中「满 100 减 10」：
//
//	活动分摊：连衣裙 floor(1000 × 12000 / 17000) = 705，衬衫 floor(1000 × 5000 / 17000) = 294，
//	          余 1 分给大行 → 706 / 294。
//
// 再带一张满 100 减 20 的券，门槛与基数是活动后金额：11294 + 4706 = 16000 ≥ 10000，
//
//	券分摊：连衣裙 floor(2000 × 11294 / 16000) = 1411，衬衫 floor(2000 × 4706 / 16000) = 588，
//	        余 1 分给大行 → 1412 / 588。
//
// 每行全部优惠：连衣裙 706 + 1412 = 2118，衬衫 294 + 588 = 882；应付 17000 − 3000 = 14000。
// 全部退完：连衣裙 12000 − 2118 + 衬衫 5000 − 882 = 14000 = 实付，逐分相等。
func TestFullReductionStacksWithCouponAndRefundsToTheCent(t *testing.T) {
	cs := newCouponShop(t)
	promo := cs.livePromotion(t, "满100减10", fullReduction100)
	b := cs.newBuyer(t, "promo-stack")
	coupon := cs.wholeStoreCoupon(t, b)

	// 不带券：只有活动。
	pv, w := cs.preview(t, b, cs.twoLineJSON(b, nil))
	wantStatus(t, w, http.StatusOK, "试算（只有活动）")
	if pv.PromotionDiscountCents != 1000 || pv.CouponDiscountCents != 0 || *pv.DiscountCents != 1000 ||
		pv.PayableCents != 16000 {
		t.Fatalf("只有活动时：活动 %d 券 %d 合计 %d 应付 %d，期望 1000 / 0 / 1000 / 16000",
			pv.PromotionDiscountCents, pv.CouponDiscountCents, *pv.DiscountCents, pv.PayableCents)
	}
	h := previewHit(t, pv.Promotions, promo.Id)
	if !h.Applied || h.DiscountCents != 1000 || h.NextThreshold == nil || *h.NextThreshold != 20000 ||
		h.Shortfall == nil || *h.Shortfall != 3000 || h.Message != "已减 10 元，再买 30 元 可减 30 元" {
		t.Fatalf("命中明细不对（应当命中第一档、离第二档还差 30 元）：%+v", h)
	}
	if d, s := previewItem(t, pv, cs.DressSKU), previewItem(t, pv, cs.ShirtSKU); d.PromotionDiscountCents != 706 ||
		s.PromotionDiscountCents != 294 || d.DiscountCents != 706 {
		t.Fatalf("活动分摊 %d / %d，期望 706 / 294（余数给大行）", d.PromotionDiscountCents, s.PromotionDiscountCents)
	}
	// 本单可用券按活动后金额（160 元）判门槛：满 100 减 20 可用、能减 20。
	if len(*pv.ApplicableCoupons) != 1 || (*pv.ApplicableCoupons)[0].ApplicableDiscountCents != 2000 {
		t.Fatalf("本单可用券：%+v", pv.ApplicableCoupons)
	}

	// 带券：活动 + 券。
	pv, w = cs.preview(t, b, cs.twoLineJSON(b, &coupon.Id))
	wantStatus(t, w, http.StatusOK, "试算（活动 + 券）")
	if pv.PromotionDiscountCents != 1000 || pv.CouponDiscountCents != 2000 || *pv.DiscountCents != 3000 ||
		pv.PayableCents != 14000 {
		t.Fatalf("活动 + 券：活动 %d 券 %d 合计 %d 应付 %d，期望 1000 / 2000 / 3000 / 14000",
			pv.PromotionDiscountCents, pv.CouponDiscountCents, *pv.DiscountCents, pv.PayableCents)
	}
	if d, s := previewItem(t, pv, cs.DressSKU), previewItem(t, pv, cs.ShirtSKU); d.DiscountCents != 2118 ||
		s.DiscountCents != 882 || d.PromotionDiscountCents != 706 {
		t.Fatalf("每行全部优惠 %d / %d，期望 2118 / 882", d.DiscountCents, s.DiscountCents)
	}

	// 下单：与试算逐分相等（expected_payable_cents 就是试算的应付）。
	body := cs.twoLineJSON(b, &coupon.Id)
	body = body[:len(body)-1] + `,"expected_payable_cents":14000}`
	var o api.Order
	decodeInto(t, createOrder(t, cs.Host, body, b.Token, "pr-"+uniqueKey()), http.StatusCreated, "下单", &o)
	if o.PayableCents != 14000 || *o.DiscountCents != 3000 || *o.PromotionDiscountCents != 1000 ||
		len(*o.Promotions) != 1 || (*o.Promotions)[0].PromotionId != promo.Id || (*o.Promotions)[0].DiscountCents != 1000 {
		t.Fatalf("订单金额或活动快照不对：%+v", o)
	}
	_, lines := cs.lines(t, b, o.OrderNo)
	dress, shirt := lines[cs.DressSKU], lines[cs.ShirtSKU]
	if *dress.DiscountCents != 2118 || *dress.PromotionDiscountCents != 706 || *shirt.DiscountCents != 882 ||
		*shirt.PromotionDiscountCents != 294 || *dress.ListPriceCents != 6000 || dress.PricePromotionId != nil {
		t.Fatalf("订单行的分摊快照不对：连衣裙 %+v 衬衫 %+v", dress, shirt)
	}

	// 活动之后改名、下线：订单上的快照不动。
	wantStatus(t, cs.patchPromotion(t, promo.Id, `{"status":0}`), http.StatusOK, "下线")
	wantStatus(t, cs.patchPromotion(t, promo.Id, `{"name":"改过的名字"}`), http.StatusOK, "改名")
	d, _ := cs.lines(t, b, o.OrderNo)
	if (*d.Promotions)[0].Name != "满100减10" || *d.PromotionDiscountCents != 1000 {
		t.Fatalf("订单详情里的活动快照被后来的修改影响了：%+v", d.Promotions)
	}

	// 付款、发货、全部退完：退款总额 = 实付，每行已退 = 净额（活动 + 券都已扣掉）。
	cs.pay(t, o.OrderNo, o.PayableCents)
	wantStatus(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusCreated, "发货")
	r := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{dress.Id, 2}, [2]int64{shirt.Id, 1}))
	if r.AmountCents != 14000 {
		t.Fatalf("整单退款 %d，期望 14000 = 实付（连衣裙 9882 + 衬衫 4118）", r.AmountCents)
	}
	cs.mustApprove(t, r.RefundNo)
	if m := orderMoneyOf(t, o.OrderNo); m.RefundStatus != 3 || m.Refunded != m.Paid || m.Paid != 14000 {
		t.Fatalf("退完之后订单是 %+v，期望 refunded = paid = 14000", m)
	}
	var mismatched int64
	if err := admin(t).QueryRow(context.Background(), `
		SELECT count(*) FROM order_items oi JOIN orders o ON o.id = oi.order_id
		 WHERE o.order_no = $1 AND oi.refunded_cents <> oi.amount_cents - oi.discount_cents`,
		o.OrderNo).Scan(&mismatched); err != nil {
		t.Fatal(err)
	}
	if mismatched != 0 {
		t.Fatalf("%d 行的已退金额 ≠ 净额（amount − 活动 − 券）", mismatched)
	}
}

// 券的门槛按活动后金额判：两件衬衫 100 元，满减活动减 10 → 90 元，满 100 减 20 的券不可用。
// 00058 之前券按 100 元判门槛、这张券可用 —— 那是在已经打过折的钱上再凑一次门槛。
func TestCouponThresholdIsCheckedAfterPromotions(t *testing.T) {
	cs := newCouponShop(t)
	cs.livePromotion(t, "满100减10", fullReduction100)
	b := cs.newBuyer(t, "promo-threshold")
	coupon := cs.wholeStoreCoupon(t, b)

	_, w := cs.preview(t, b, cs.orderJSON(b, cs.NorthStore, cs.ShirtSKU, 2, &coupon.Id))
	if p := problemOf(t, w, http.StatusConflict); p.Type != problem.TypeCouponNotApplicable {
		t.Fatalf("活动后 90 元不满 100，带券应当 409 coupon-not-applicable，实得 %+v", p)
	}
	pv, w := cs.preview(t, b, cs.orderJSON(b, cs.NorthStore, cs.ShirtSKU, 2, nil))
	wantStatus(t, w, http.StatusOK, "不带券试算")
	if len(*pv.ApplicableCoupons) != 0 {
		t.Fatalf("活动后 90 元，本单可用券应当为空：%+v", pv.ApplicableCoupons)
	}
}

// 不与券同享的活动命中之后，这一单不能再用券；没命中（不满门槛）就不挡。
func TestNonStackablePromotionBlocksCoupon(t *testing.T) {
	cs := newCouponShop(t)
	p := cs.createPromotion(t, "独享满减", `"promotion_type":1,"threshold_unit":1,"stack_with_coupon":false,`+
		`"tiers":[{"threshold":10000,"discount_cents":1000,"discount_rate":0}]`)
	wantStatus(t, cs.patchPromotion(t, p.Id, `{"status":1}`), http.StatusOK, "上线")
	b := cs.newBuyer(t, "promo-exclusive")
	coupon := cs.wholeStoreCoupon(t, b)

	_, w := cs.preview(t, b, cs.twoLineJSON(b, &coupon.Id))
	pb := problemOf(t, w, http.StatusConflict)
	if pb.Type != problem.TypeCouponNotApplicable || pb.Detail == nil {
		t.Fatalf("命中独享活动时带券应当 409 coupon-not-applicable：%+v", pb)
	}
	pv, _ := cs.preview(t, b, cs.twoLineJSON(b, nil))
	if len(*pv.ApplicableCoupons) != 0 || pv.PromotionDiscountCents != 1000 {
		t.Fatalf("命中独享活动：可用券应为空、活动照减：%+v / %d", pv.ApplicableCoupons, pv.PromotionDiscountCents)
	}
	// 一件衬衫 50 元：活动没命中，券照常（虽然这张券也不满门槛，但原因应当是券自己的门槛）。
	_, w = cs.preview(t, b, cs.orderJSON(b, cs.NorthStore, cs.ShirtSKU, 1, &coupon.Id))
	if pb := problemOf(t, w, http.StatusConflict); pb.Detail == nil || !strings.Contains(*pb.Detail, "还差") {
		t.Fatalf("活动没命中时拒券的原因应当是券的门槛：%+v", pb)
	}
}

// ---------------------------------------------------------------------------
// 限时折扣：活动价 = min(门店价, 特价)、每人限购、标签、购物车
// ---------------------------------------------------------------------------

func TestLimitedPriceWithPerUserLimit(t *testing.T) {
	cs := newCouponShop(t)
	promo := cs.livePromotion(t, "连衣裙限时特价", fmt.Sprintf(`"promotion_type":3,`+
		`"skus":[{"sku_id":%d,"promo_price_cents":3990,"per_user_limit":2}]`, cs.DressSKU))
	b := cs.newBuyer(t, "limited")

	// 商品列表与详情：标签与 SKU 活动价。
	var list struct {
		Items []api.ProductSummary `json:"items"`
	}
	decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/products?store_id=%d", cs.NorthStore), ""),
		http.StatusOK, "商品列表", &list)
	var dressTags []api.PromotionTag
	for _, it := range list.Items {
		if it.Id == cs.DressProduct {
			dressTags = *it.PromotionTags
		} else if it.PromotionTags == nil || len(*it.PromotionTags) != 0 {
			t.Fatalf("衬衫不在活动里，不该有标签：%+v", it.PromotionTags)
		}
	}
	if len(dressTags) != 1 || dressTags[0].Label != "限时特价 ¥39.9" || dressTags[0].PromotionId != promo.Id {
		t.Fatalf("连衣裙的标签：%+v", dressTags)
	}
	var detail api.ProductDetail
	decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/products/%d?store_id=%d", cs.DressProduct, cs.NorthStore), ""),
		http.StatusOK, "商品详情", &detail)
	if detail.Skus[0].PromoPriceCents == nil || *detail.Skus[0].PromoPriceCents != 3990 ||
		detail.Skus[0].PriceCents != 6000 {
		t.Fatalf("详情里的 SKU：门店价 %d 活动价 %v", detail.Skus[0].PriceCents, detail.Skus[0].PromoPriceCents)
	}

	// 购物车：单价是活动价、门店价划线，合计按活动价 —— 与试算的 goods_amount_cents 逐分相等。
	// 车挂着买家与 SKU 的外键，要排在夹具清理之前删掉（t.Cleanup 后进先出）。
	t.Cleanup(func() {
		adminExec(t, `DELETE FROM cart_items WHERE merchant_id = $1`, cs.MerchantID)
		adminExec(t, `DELETE FROM carts WHERE merchant_id = $1`, cs.MerchantID)
	})
	wantStatus(t, postWithKey(t, cs.Host, fmt.Sprintf("/api/v1/cart/items?store_id=%d", cs.NorthStore),
		fmt.Sprintf(`{"sku_id":%d,"quantity":2}`, cs.DressSKU), b.Token, freshIdemKey()), http.StatusOK, "加购")
	var cart api.Cart
	decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/cart?store_id=%d", cs.NorthStore), b.Token),
		http.StatusOK, "购物车", &cart)
	if len(cart.Items) != 1 || *cart.Items[0].PriceCents != 3990 || cart.Items[0].ListPriceCents == nil ||
		*cart.Items[0].ListPriceCents != 6000 || cart.SelectedTotalCents != 7980 {
		t.Fatalf("购物车：%+v", cart)
	}

	// 试算：超出限购 409；限购之内按活动价。
	_, w := cs.preview(t, b, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 3, nil))
	if p := problemOf(t, w, http.StatusConflict); p.Type != problem.TypePromotionLimitExceeded {
		t.Fatalf("买 3 件超出限购 2，应当 409 promotion-limit-exceeded：%+v", p)
	}
	pv, w := cs.preview(t, b, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 2, nil))
	wantStatus(t, w, http.StatusOK, "限购之内试算")
	it := previewItem(t, pv, cs.DressSKU)
	if it.PriceCents != 3990 || it.ListPriceCents != 6000 || it.PricePromotionId == nil || *it.PricePromotionId != promo.Id ||
		pv.GoodsAmountCents != 7980 || *pv.DiscountCents != 0 || pv.GoodsAmountCents != cart.SelectedTotalCents {
		t.Fatalf("限时特价的试算：%+v / goods %d", it, pv.GoodsAmountCents)
	}
	if h := previewHit(t, pv.Promotions, promo.Id); !h.Applied || h.DiscountCents != 4020 {
		t.Fatalf("命中明细（省 (6000-3990)×2 = 4020）：%+v", h)
	}

	// 下单占掉 2 件限购额度；再买 1 件就超了。
	var o api.Order
	decodeInto(t, createOrder(t, cs.Host, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 2, nil), b.Token, "lp-"+uniqueKey()),
		http.StatusCreated, "按特价下单", &o)
	if o.PayableCents != 7980 {
		t.Fatalf("应付 %d，期望 7980", o.PayableCents)
	}
	if got := adminQueryInt64(t, `SELECT sold FROM activity_stocks WHERE promotion_id = $1`, promo.Id); got != 2 {
		t.Fatalf("活动已售 %d，期望 2", got)
	}
	_, w = cs.preview(t, b, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 1, nil))
	if p := problemOf(t, w, http.StatusConflict); p.Type != problem.TypePromotionLimitExceeded {
		t.Fatalf("已买 2 件再买 1 件应当 409：%+v", p)
	}
	// 别的买家不受影响。
	other := cs.newBuyer(t, "limited-other")
	_, w = cs.preview(t, other, cs.orderJSON(other, cs.NorthStore, cs.DressSKU, 2, nil))
	wantStatus(t, w, http.StatusOK, "别的买家的限购额度是自己的")

	// 取消订单：限购额度与活动已售件数放回，又能按特价买 2 件。
	wantStatus(t, orderAction(t, cs.Host, o.OrderNo, "cancel", b.Token, "c-"+uniqueKey()), http.StatusOK, "取消")
	if got := adminQueryInt64(t, `SELECT sold FROM activity_stocks WHERE promotion_id = $1`, promo.Id); got != 0 {
		t.Fatalf("取消之后活动已售 %d，期望放回到 0", got)
	}
	_, w = cs.preview(t, b, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 2, nil))
	wantStatus(t, w, http.StatusOK, "取消之后限购额度回来了")
}

// 特价比门店价还高（门店单独降价）：活动不生效，按门店价卖。
func TestSpecialPriceAboveStorePriceIsIgnored(t *testing.T) {
	cs := newCouponShop(t)
	cs.livePromotion(t, "贵的特价", fmt.Sprintf(`"promotion_type":3,"skus":[{"sku_id":%d,"promo_price_cents":5500}]`, cs.DressSKU))
	// 北京门店把连衣裙单独调到 49 元（三层定价的门店价）。
	wantStatus(t, putAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/price", cs.NorthStore, cs.DressSKU),
		`{"price_cents":4900}`, cs.Token), http.StatusOK, "门店调价")
	b := cs.newBuyer(t, "special-above")
	pv, w := cs.preview(t, b, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 1, nil))
	wantStatus(t, w, http.StatusOK, "北京试算")
	if it := previewItem(t, pv, cs.DressSKU); it.PriceCents != 4900 || it.PricePromotionId != nil || len(pv.Promotions) != 0 {
		t.Fatalf("北京门店价 49 < 特价 55，应当按 49 卖、不挂活动：%+v", it)
	}
	pv, _ = cs.preview(t, b, cs.orderJSON(b, cs.SouthStore, cs.DressSKU, 1, nil))
	if it := previewItem(t, pv, cs.DressSKU); it.PriceCents != 5500 || it.PricePromotionId == nil {
		t.Fatalf("广州门店价 60 > 特价 55，应当按 55 卖：%+v", it)
	}
}

// ---------------------------------------------------------------------------
// 秒杀：配额不超卖、每人限购不被并发击穿
// ---------------------------------------------------------------------------

func TestFlashSaleDoesNotOversellUnderConcurrency(t *testing.T) {
	cs := newCouponShop(t)
	const quota, buyers = 5, 12
	promo := cs.livePromotion(t, "衬衫秒杀", fmt.Sprintf(`"promotion_type":4,`+
		`"skus":[{"sku_id":%d,"promo_price_cents":990,"stock_qty":%d}]`, cs.ShirtSKU, quota))
	stockBefore := availableAt(t, cs.NorthStore, cs.ShirtSKU)

	bs := make([]couponBuyer, buyers)
	for i := range bs {
		bs[i] = cs.newBuyer(t, fmt.Sprintf("flash-%d", i))
	}
	var wg sync.WaitGroup
	codes := make([]int, buyers)
	types := make([]string, buyers)
	for i := range bs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := createOrder(t, cs.Host, cs.orderJSON(bs[i], cs.NorthStore, cs.ShirtSKU, 1, nil), bs[i].Token,
				fmt.Sprintf("fl-%d-%s", i, uniqueKey()))
			codes[i] = w.Code
			if w.Code != http.StatusCreated {
				var p api.Problem
				_ = json.Unmarshal(w.Body.Bytes(), &p)
				types[i] = p.Type
			}
		}(i)
	}
	wg.Wait()

	sold := adminQueryInt64(t, `SELECT sold FROM activity_stocks WHERE promotion_id = $1`, promo.Id)
	atFlash := adminQueryInt64(t, `
		SELECT COALESCE(sum(oi.quantity), 0) FROM order_items oi JOIN orders o ON o.id = oi.order_id
		 WHERE oi.price_promotion_id = $1 AND o.status = 10`, promo.Id)
	if sold != quota || atFlash != quota {
		t.Fatalf("秒杀配额 %d：已售计数 %d、按秒杀价成交 %d 件 —— 超卖或少卖了", quota, sold, atFlash)
	}
	created := 0
	for i, c := range codes {
		switch {
		case c == http.StatusCreated:
			created++
		case c == http.StatusConflict && (types[i] == problem.TypePromotionSoldOut ||
			types[i] == problem.TypeIdempotencyKeyInFlight):
		default:
			t.Fatalf("第 %d 个买家：%d %s（只允许成功或 409 promotion-sold-out）", i, c, types[i])
		}
	}
	// 配额抢光之后才下单的人按门店价成交（试算看得到配额不够，报的就是门店价），
	// 所以成功的单数 ≥ 配额；但门店库存扣掉的件数必须正好等于成功的件数（失败的都补偿回去了）。
	if created < quota {
		t.Fatalf("成功 %d 单，少于配额 %d", created, quota)
	}
	if got := availableAt(t, cs.NorthStore, cs.ShirtSKU); stockBefore-got != created {
		t.Fatalf("门店库存扣了 %d 件，成功 %d 单 —— 秒杀失败的单没把门店库存放回去", stockBefore-got, created)
	}
}

func TestFlashSalePerUserLimitHoldsUnderConcurrency(t *testing.T) {
	cs := newCouponShop(t)
	promo := cs.livePromotion(t, "衬衫秒杀限购", fmt.Sprintf(`"promotion_type":4,`+
		`"skus":[{"sku_id":%d,"promo_price_cents":990,"stock_qty":40,"per_user_limit":1}]`, cs.ShirtSKU))
	b := cs.newBuyer(t, "flash-limit")

	const tries = 6
	var wg sync.WaitGroup
	codes := make([]int, tries)
	for i := 0; i < tries; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = createOrder(t, cs.Host, cs.orderJSON(b, cs.NorthStore, cs.ShirtSKU, 1, nil), b.Token,
				fmt.Sprintf("fu-%d-%s", i, uniqueKey())).Code
		}(i)
	}
	wg.Wait()
	bought := adminQueryInt64(t, `SELECT COALESCE(sum(qty), 0) FROM promotion_purchases WHERE promotion_id = $1`, promo.Id)
	atFlash := adminQueryInt64(t, `
		SELECT COALESCE(sum(oi.quantity), 0) FROM order_items oi JOIN orders o ON o.id = oi.order_id
		 WHERE oi.price_promotion_id = $1 AND o.status = 10`, promo.Id)
	if bought != 1 || atFlash != 1 {
		t.Fatalf("每人限购 1 件：限购计数 %d、按秒杀价成交 %d 件（并发击穿了限购）", bought, atFlash)
	}
	ok := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			ok++
		} else if c != http.StatusConflict {
			t.Fatalf("同一买家并发下单只允许成功或 409，实得 %d", c)
		}
	}
	if ok != 1 {
		t.Fatalf("同一买家并发 %d 单，成功 %d 单，期望恰好 1 单", tries, ok)
	}
}

// 每人限购与活动配额的最终仲裁（试算与建单前的计价只是预告）。微服务拆分阶段 1b 起两道闸分在两处：
// 每人限购在建单分支（0 → 10 的同一个事务里累计，core），活动配额在库存分支（库存服务，
// 与门店库存同一个事务）。这里把两条竞态**确定性地**造出来：
//
//   - 限购：b 已经按秒杀价买走了限购额度，再造一笔 b 的、按秒杀价成交的草稿单直接调建单分支 ——
//     两笔并发订单里后到的那一笔在建单分支里看到的世界。必须 Failure，订单停在 0，限购计数不动。
//   - 配额：把库存服务里的配额收紧到已售数，再用一笔按秒杀价成交的载荷直接调库存分支 ——
//     必须拒绝（提交一行 sold_out），门店库存与已售一件不动；收尾分支据此 Failure。
func TestStockBranchIsTheFinalArbiterOfQuotaAndLimit(t *testing.T) {
	cs := newCouponShop(t)
	p := cs.createPromotion(t, "秒杀仲裁", fmt.Sprintf(`"promotion_type":4,`+
		`"skus":[{"sku_id":%d,"promo_price_cents":990,"stock_qty":2,"per_user_limit":1}]`, cs.ShirtSKU))
	b := cs.newBuyer(t, "arbiter-b")
	orderB := cs.placeOrder(t, b, cs.NorthStore, cs.ShirtSKU, 1, nil) // 活动还没上线：门店价
	wantStatus(t, cs.patchPromotion(t, p.Id, `{"status":1}`), http.StatusOK, "上线")
	orderA := cs.placeOrder(t, b, cs.NorthStore, cs.ShirtSKU, 1, nil) // 秒杀价，占掉 b 的限购
	if orderA.PayableCents != 990 {
		t.Fatalf("A 应当按秒杀价 990 成交，实得 %d", orderA.PayableCents)
	}
	sold := func() int64 {
		return adminQueryInt64(t, `SELECT sold FROM activity_stocks WHERE promotion_id = $1`, p.Id)
	}
	purchases := func() int64 {
		return adminQueryInt64(t, `SELECT COALESCE(sum(qty), 0) FROM promotion_purchases WHERE promotion_id = $1`, p.Id)
	}
	stock := availableAt(t, cs.NorthStore, cs.ShirtSKU)

	// 限购：照着 B 造一笔 status = 0、按秒杀价成交的草稿单（同一个买家 b）。
	draftNo := orderB.OrderNo + "d"
	adminExec(t, `INSERT INTO orders (merchant_id, order_no, user_id, status, goods_amount_cents, payable_cents,
	                                  receiver_snapshot, expire_at, store_id, region_id, store_snapshot)
	              SELECT merchant_id, $2, user_id, 0, 990, 990, receiver_snapshot, expire_at, store_id, region_id, store_snapshot
	                FROM orders WHERE order_no = $1`, orderB.OrderNo, draftNo)
	adminExec(t, `INSERT INTO order_items (merchant_id, order_id, sku_id, product_id, title_snapshot, spec_snapshot,
	                                       price_cents, list_price_cents, quantity, amount_cents, price_promotion_id)
	              SELECT oi.merchant_id, (SELECT id FROM orders WHERE order_no = $2), oi.sku_id, oi.product_id,
	                     oi.title_snapshot, oi.spec_snapshot, 990, oi.list_price_cents, 1, 990, $3
	                FROM order_items oi JOIN orders o ON o.id = oi.order_id WHERE o.order_no = $1`,
		orderB.OrderNo, draftNo, p.Id)
	if got := branchOf(t, service.BranchOrderCreate)(gidFor(t, cs.MerchantID, draftNo), "01", "action"); got != dtm.Failure {
		t.Fatalf("超出每人限购，建单分支应当返回 Failure，实得 %d", got)
	}
	if st := orderStatusOf(t, draftNo); st != 0 || purchases() != 1 {
		t.Fatalf("超限的建单分支动了账：订单 %d、限购计数 %d", st, purchases())
	}

	// 配额：收紧到已售数，再按秒杀价扣一件。
	adminExec(t, `UPDATE activity_stocks SET quota = sold WHERE promotion_id = $1`, p.Id)
	gid := gidFor(t, cs.MerchantID, draftNo)
	payload, err := inventory.EncodeDeductPayload(inventory.DeductPayload{OrderNo: draftNo, StoreID: cs.NorthStore,
		Lines: []inventory.OrderLine{{SKUID: cs.ShirtSKU, Qty: 1, PromotionID: &p.Id}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := invBranchOf(t, inventory.BranchDeduct)(gid, "03", "action", payload); got != dtm.Success {
		t.Fatalf("配额已满的库存分支返回 %d，期望 Success（一行 sold_out 拒绝）", got)
	}
	if sold() != 1 || availableAt(t, cs.NorthStore, cs.ShirtSKU) != stock {
		t.Fatalf("被拒的库存分支动了账：已售 %d、门店库存 %d→%d", sold(), stock, availableAt(t, cs.NorthStore, cs.ShirtSKU))
	}
	logs := inventoryLogsOf(t, draftNo)
	if len(logs) != 1 || logs[0].BizType != inventory.BizOrderRejected {
		t.Fatalf("期望一行拒绝流水，实得 %+v", logs)
	}
	adminExec(t, `UPDATE orders SET status = 10 WHERE order_no = $1`, draftNo)
	if got := branchOf(t, service.BranchOrderFinish)(gid, "04", "action"); got != dtm.Failure {
		t.Fatalf("收尾分支读到 sold_out 应当 Failure，实得 %d", got)
	}
	adminExec(t, `UPDATE orders SET status = 90 WHERE order_no = $1`, draftNo)
}

// ---------------------------------------------------------------------------
// 新人礼
// ---------------------------------------------------------------------------

func TestNewBuyerGiftIsGrantedOnceBeforeTheFirstOrder(t *testing.T) {
	cs := newCouponShop(t)
	tpl := cs.createTemplate(t, `{"name":"新人券","coupon_type":3,"discount_cents":500,"valid_mode":2,"valid_days":7}`)
	gift := cs.livePromotion(t, "新人礼", fmt.Sprintf(`"promotion_type":5,"gift_coupon_template_id":%d`, tpl.Id))

	b := cs.newBuyer(t, "newbie")
	hash, err := auth.HashPassword("secret-123")
	if err != nil {
		t.Fatal(err)
	}
	adminExec(t, `UPDATE users SET password_hash = $1 WHERE id = $2`, hash, b.UserID)
	login := func() {
		t.Helper()
		wantStatus(t, post(t, cs.Host, "/api/v1/auth/login",
			fmt.Sprintf(`{"phone":%q,"password":"secret-123"}`, b.Phone), ""), http.StatusOK, "登录")
	}
	count := func() int64 {
		return adminQueryInt64(t, `SELECT count(*) FROM user_coupons WHERE user_id = $1 AND template_id = $2 AND source = 3`,
			b.UserID, tpl.Id)
	}

	login()
	if count() != 1 {
		t.Fatalf("首单前登录应当发 1 张新人券，实得 %d", count())
	}
	login() // 再登录一次：一人一张
	if count() != 1 {
		t.Fatalf("再次登录不该重复发，实得 %d", count())
	}
	var got api.AdminPromotion
	decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/promotions/%d", gift.Id), cs.Token), http.StatusOK, "活动详情", &got)
	if got.GiftGrantedCount == nil || *got.GiftGrantedCount != 1 {
		t.Fatalf("后台的已发张数：%v", got.GiftGrantedCount)
	}

	// 下过单的买家不再是新人。
	old := cs.newBuyer(t, "veteran")
	adminExec(t, `UPDATE users SET password_hash = $1 WHERE id = $2`, hash, old.UserID)
	cs.placeOrder(t, old, cs.NorthStore, cs.ShirtSKU, 1, nil)
	wantStatus(t, post(t, cs.Host, "/api/v1/auth/login",
		fmt.Sprintf(`{"phone":%q,"password":"secret-123"}`, old.Phone), ""), http.StatusOK, "老买家登录")
	if n := adminQueryInt64(t, `SELECT count(*) FROM user_coupons WHERE user_id = $1 AND source = 3`, old.UserID); n != 0 {
		t.Fatalf("下过单的买家不该拿到新人礼，实得 %d 张", n)
	}
}

// ---------------------------------------------------------------------------
// 后台：规则校验、上线中不许改规则、上线核完整性
// ---------------------------------------------------------------------------

func TestAdminPromotionRulesAndLifecycle(t *testing.T) {
	cs := newCouponShop(t)
	bad := []string{
		`"promotion_type":1,"threshold_unit":1,"tiers":[{"threshold":10000,"discount_cents":20000,"discount_rate":0}]`,                                                            // 满 100 减 200
		`"promotion_type":1,"threshold_unit":1,"tiers":[{"threshold":10000,"discount_cents":2000,"discount_rate":0},{"threshold":20000,"discount_cents":1000,"discount_rate":0}]`, // 高档减得少
		`"promotion_type":2,"threshold_unit":2,"tiers":[{"threshold":2,"discount_cents":0,"discount_rate":1000}]`,                                                                 // 折扣越界
		fmt.Sprintf(`"promotion_type":4,"skus":[{"sku_id":%d,"promo_price_cents":990}]`, cs.ShirtSKU),                                                                             // 秒杀没配额
		fmt.Sprintf(`"promotion_type":3,"skus":[{"sku_id":%d,"promo_price_cents":990,"discount_rate":800}]`, cs.ShirtSKU),                                                         // 二选一
		`"promotion_type":3,"skus":[{"sku_id":999999999,"promo_price_cents":990}]`,                                                                                                // SKU 查不到
		`"promotion_type":5`, // 新人礼没券模板
	}
	for i, f := range bad {
		body := fmt.Sprintf(`{"name":"坏活动","starts_at":%q,"ends_at":%q,%s}`, relTime(-time.Hour), relTime(time.Hour), f)
		if p := problemOf(t, postIdem(t, cs.Host, "/api/v1/admin/promotions", body, cs.Token),
			http.StatusUnprocessableEntity); p.Type != problem.TypeInvalidRequest {
			t.Fatalf("第 %d 个坏配置应当 422 invalid-request：%+v", i, p)
		}
	}

	// 没有阶梯的满减可以建（草稿），但上不了线。
	draft := cs.createPromotion(t, "草稿", `"promotion_type":1,"threshold_unit":1`)
	if p := problemOf(t, cs.patchPromotion(t, draft.Id, `{"status":1}`), http.StatusUnprocessableEntity); p.Type != problem.TypeInvalidRequest {
		t.Fatalf("没有阶梯的活动上线应当 422：%+v", p)
	}

	// 上线中只能改名、下线；改规则 409。
	live := cs.livePromotion(t, "上线的", fullReduction100)
	if p := problemOf(t, cs.patchPromotion(t, live.Id, `{"tiers":[{"threshold":5000,"discount_cents":500,"discount_rate":0}]}`),
		http.StatusConflict); p.Type != problem.TypePromotionOnline {
		t.Fatalf("上线中改规则应当 409 promotion-online：%+v", p)
	}
	wantStatus(t, cs.patchPromotion(t, live.Id, `{"name":"改个名字"}`), http.StatusOK, "上线中改名")
	wantStatus(t, cs.patchPromotion(t, live.Id, `{"status":0}`), http.StatusOK, "下线")
	var edited api.AdminPromotion
	decodeInto(t, cs.patchPromotion(t, live.Id, `{"tiers":[{"threshold":5000,"discount_cents":500,"discount_rate":0}]}`),
		http.StatusOK, "下线后改规则", &edited)
	if len(edited.Tiers) != 1 || edited.Tiers[0].Threshold != 5000 || edited.Phase != "offline" {
		t.Fatalf("下线后改规则：%+v", edited)
	}

	// 列表按类型筛。
	var page struct {
		Items []api.AdminPromotion `json:"items"`
		Total int                  `json:"total"`
	}
	decodeInto(t, getAs(t, cs.Host, "/api/v1/admin/promotions?promotion_type=1&status=0", cs.Token), http.StatusOK, "列表", &page)
	if page.Total != 2 {
		t.Fatalf("下线的满减活动应当有 2 个（草稿 + 下线的），实得 %d", page.Total)
	}

	// 卖出过的 SKU 不能移出活动。
	flash := cs.livePromotion(t, "秒杀", fmt.Sprintf(`"promotion_type":4,"skus":[{"sku_id":%d,"promo_price_cents":990,"stock_qty":3}]`, cs.ShirtSKU))
	b := cs.newBuyer(t, "sold")
	cs.placeOrder(t, b, cs.NorthStore, cs.ShirtSKU, 1, nil)
	wantStatus(t, cs.patchPromotion(t, flash.Id, `{"status":0}`), http.StatusOK, "下线秒杀")
	if p := problemOf(t, cs.patchPromotion(t, flash.Id, fmt.Sprintf(`{"skus":[{"sku_id":%d,"promo_price_cents":990,"stock_qty":3}]}`, cs.DressSKU)),
		http.StatusUnprocessableEntity); p.Type != problem.TypeInvalidRequest {
		t.Fatalf("卖出过的 SKU 移出活动应当 422：%+v", p)
	}
}
