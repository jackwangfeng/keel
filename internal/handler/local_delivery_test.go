package handler_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// 同城配送（00110）主路径：有围栏的门店按距离分档收配送费、不读运费模板；默认门店照旧按模板。
//
// 北京门店画上围栏、配上「1 公里内 3 元 / 50 公里内 8 元、起送 20 元」。同一个买家、同一件连衣裙（60 元）：
//   - 地址就在门店坐标上 → 距离 0 → 3 元；全店默认模板（北京首件 8 元）在这里不生效；
//   - 地址没有坐标 → 算不出距离 → 按最后一档 8 元；
//   - 满 50 免配送费 → 0；
//   - 起送价调到 100 元 → 试算与下单 422 below-minimum-order，购物车照常显示差额 40 元；
//   - 默认门店（全国兜底）照旧按模板收 8 元（阳性对照：模板本身是好的）。
func TestLocalDeliveryForFencedStores(t *testing.T) {
	cs := newCouponShop(t)
	cs.createFreight(t, nationwideFreight)
	setFence(t, cs.adminShop, cs.NorthStore, 116.30, 39.80, 116.50, 40.00)
	t.Cleanup(func() {
		adminExec(t, `DELETE FROM cart_items WHERE merchant_id = $1`, cs.MerchantID)
		adminExec(t, `DELETE FROM carts WHERE merchant_id = $1`, cs.MerchantID)
	})
	putCfg := func(body string) api.AdminLocalDelivery {
		t.Helper()
		var out api.AdminLocalDelivery
		decodeInto(t, putAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/stores/%d/local-delivery", cs.NorthStore), body, cs.Token),
			http.StatusOK, "配同城配送", &out)
		return out
	}

	// 没配过：全 0，active（有围栏、不是默认店）。
	var got api.AdminLocalDelivery
	decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/stores/%d/local-delivery", cs.NorthStore), cs.Token),
		http.StatusOK, "读同城配送", &got)
	if !got.Active || got.MinOrderCents != 0 || len(got.FeeTiers) != 0 || got.UpdatedAt != nil {
		t.Fatalf("没配过的围栏店应是 active 且全 0：%+v", got)
	}
	cfg := putCfg(`{"min_order_cents":2000,"free_over_cents":0,
		"fee_tiers":[{"within_m":1000,"fee_cents":300},{"within_m":50000,"fee_cents":800}]}`)
	if !cfg.Active || len(cfg.FeeTiers) != 2 || cfg.UpdatedAt == nil {
		t.Fatalf("保存后：%+v", cfg)
	}
	// 档不递增 → 422。
	w := putAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/stores/%d/local-delivery", cs.NorthStore),
		`{"min_order_cents":0,"free_over_cents":0,"fee_tiers":[{"within_m":3000,"fee_cents":300},{"within_m":3000,"fee_cents":500}]}`,
		cs.Token)
	if p := problemOf(t, w, http.StatusUnprocessableEntity); p.Type != problem.TypeInvalidRequest {
		t.Fatalf("档不递增应 422 invalid-request，实得 %s", p.Type)
	}

	b := cs.newBuyer(t, "local")
	var lat, lng float64
	if err := admin(t).QueryRow(t.Context(),
		`SELECT ST_Y(location::geometry), ST_X(location::geometry) FROM stores WHERE id = $1`, cs.NorthStore).
		Scan(&lat, &lng); err != nil {
		t.Fatal(err)
	}
	adminExec(t, `UPDATE user_addresses SET lat = $2, lng = $3 WHERE id = $1`, b.Address, lat, lng)
	body := orderBodyAt(b.Address, cs.NorthStore, [][2]int64{{cs.DressSKU, 1}}, nil)

	pv := cs.mustPreview(t, b, body)
	l := pv.Freight.Local
	if pv.Freight.Mode == nil || *pv.Freight.Mode != api.Local || l == nil || l.DistanceM == nil || *l.DistanceM != 0 ||
		pv.FreightCents != 300 || len(pv.Freight.Groups) != 0 || l.MinOrderCents != 2000 || l.ShortfallCents != 0 {
		t.Fatalf("地址就在门店上：应同城配送 3 元（不读模板），实得 freight=%+v local=%+v", pv.Freight, l)
	}
	var o api.Order
	decodeInto(t, createOrder(t, cs.Host, body, b.Token, "ld-"+uniqueKey()), http.StatusCreated, "下单", &o)
	if o.FreightCents == nil || *o.FreightCents != 300 {
		t.Fatalf("订单上的配送费应是 3 元：%+v", o.FreightCents)
	}

	adminExec(t, `UPDATE user_addresses SET lat = NULL, lng = NULL WHERE id = $1`, b.Address)
	pv = cs.mustPreview(t, b, body)
	if pv.Freight.Local == nil || pv.Freight.Local.DistanceM != nil || pv.FreightCents != 800 {
		t.Fatalf("地址没有坐标应按最后一档 8 元：%+v", pv.Freight.Local)
	}

	putCfg(`{"min_order_cents":2000,"free_over_cents":5000,
		"fee_tiers":[{"within_m":1000,"fee_cents":300},{"within_m":50000,"fee_cents":800}]}`)
	pv = cs.mustPreview(t, b, body)
	if pv.FreightCents != 0 || pv.Freight.Local.FreeReason == nil || pv.Freight.Local.TierFeeCents != 800 {
		t.Fatalf("满 50 应免配送费：%+v", pv.Freight.Local)
	}

	putCfg(`{"min_order_cents":10000,"free_over_cents":0,"fee_tiers":[{"within_m":50000,"fee_cents":800}]}`)
	_, w = cs.preview(t, b, body)
	if p := problemOf(t, w, http.StatusUnprocessableEntity); p.Type != problem.TypeBelowMinimumOrder {
		t.Fatalf("没到起送价试算应 422 below-minimum-order，实得 %s", p.Type)
	}
	cw := createOrder(t, cs.Host, body, b.Token, "ld-"+uniqueKey())
	if p := problemOf(t, cw, http.StatusUnprocessableEntity); p.Type != problem.TypeBelowMinimumOrder {
		t.Fatalf("没到起送价下单应 422 below-minimum-order，实得 %s", p.Type)
	}
	wantStatus(t, postWithKey(t, cs.Host, fmt.Sprintf("/api/v1/cart/items?store_id=%d", cs.NorthStore),
		fmt.Sprintf(`{"sku_id":%d,"quantity":1}`, cs.DressSKU), b.Token, freshIdemKey()), http.StatusOK, "加购")
	var cart api.Cart
	decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/cart?store_id=%d&address_id=%d", cs.NorthStore, b.Address), b.Token),
		http.StatusOK, "购物车", &cart)
	if cart.Freight == nil || cart.Freight.Local == nil || cart.Freight.Local.ShortfallCents != 4000 {
		t.Fatalf("购物车应显示还差 40 元起送：%+v", cart.Freight)
	}

	// 默认门店不走同城配送：照旧按模板（北京首件 8 元），配置上 active = false。
	def := adminQueryInt64(t, `SELECT id FROM stores WHERE merchant_id = $1 AND is_default AND deleted_at IS NULL`,
		cs.MerchantID)
	setStoreStock(t, cs.adminShop, def, cs.DressSKU, 10)
	pv = cs.mustPreview(t, b, orderBodyAt(b.Address, def, [][2]int64{{cs.DressSKU, 1}}, nil))
	if pv.Freight.Mode == nil || *pv.Freight.Mode != api.Express || pv.Freight.Local != nil || len(pv.Freight.Groups) != 1 {
		t.Fatalf("默认门店应按运费模板：%+v", pv.Freight)
	}
	decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/stores/%d/local-delivery", def), cs.Token),
		http.StatusOK, "读默认店", &got)
	if got.Active {
		t.Fatal("默认门店的同城配送配置不该 active")
	}
}
