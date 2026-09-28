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

// 同城配送模板（00111）：没配过的围栏店按默认模板收；门店可以引用别的模板；改模板即生效；
// 在用的模板与默认模板删不掉；重名 409；设新默认会取消旧默认。
func TestLocalDeliveryTemplates(t *testing.T) {
	cs := newCouponShop(t)
	setFence(t, cs.adminShop, cs.NorthStore, 116.30, 39.80, 116.50, 40.00)
	b := cs.newBuyer(t, "ldtpl")
	var lat, lng float64
	if err := admin(t).QueryRow(t.Context(),
		`SELECT ST_Y(location::geometry), ST_X(location::geometry) FROM stores WHERE id = $1`, cs.NorthStore).
		Scan(&lat, &lng); err != nil {
		t.Fatal(err)
	}
	adminExec(t, `UPDATE user_addresses SET lat = $2, lng = $3 WHERE id = $1`, b.Address, lat, lng)
	body := orderBodyAt(b.Address, cs.NorthStore, [][2]int64{{cs.DressSKU, 1}}, nil)
	fee := func() int64 { t.Helper(); return int64(cs.mustPreview(t, b, body).FreightCents) }
	createTpl := func(body string) api.LocalDeliveryTemplate {
		t.Helper()
		var out api.LocalDeliveryTemplate
		decodeInto(t, postIdem(t, cs.Host, "/api/v1/admin/local-delivery-templates", body, cs.Token),
			http.StatusCreated, "建同城配送模板", &out)
		return out
	}
	storeCfg := func() api.AdminLocalDelivery {
		t.Helper()
		var out api.AdminLocalDelivery
		decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/stores/%d/local-delivery", cs.NorthStore), cs.Token),
			http.StatusOK, "读门店同城配送", &out)
		return out
	}
	storePath := fmt.Sprintf("/api/v1/admin/stores/%d/local-delivery", cs.NorthStore)

	if c := storeCfg(); c.Source != api.LocalDeliverySourceNone || fee() != 0 {
		t.Fatalf("没有任何模板时应是 none、配送费 0：%+v", c)
	}
	std := createTpl(`{"name":"市区标准","is_default":true,"min_order_cents":0,"free_over_cents":0,
		"fee_tiers":[{"within_m":3000,"fee_cents":300}]}`)
	if c := storeCfg(); c.Source != api.LocalDeliverySourceDefaultTemplate || c.TemplateId == nil || *c.TemplateId != std.Id ||
		fee() != 300 {
		t.Fatalf("没配过的围栏店应跟随默认模板收 3 元：%+v", c)
	}
	far := createTpl(`{"name":"远郊","is_default":false,"min_order_cents":0,"free_over_cents":0,
		"fee_tiers":[{"within_m":50000,"fee_cents":900}]}`)
	var c api.AdminLocalDelivery
	decodeInto(t, putAs(t, cs.Host, storePath, fmt.Sprintf(`{"template_id":%d}`, far.Id), cs.Token), http.StatusOK, "选模板", &c)
	if c.Source != api.LocalDeliverySourceTemplate || c.TemplateName == nil || *c.TemplateName != "远郊" || fee() != 900 {
		t.Fatalf("引用「远郊」应收 9 元：%+v", c)
	}
	wantStatus(t, putAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/local-delivery-templates/%d", far.Id),
		`{"name":"远郊","is_default":false,"min_order_cents":0,"free_over_cents":0,"fee_tiers":[{"within_m":50000,"fee_cents":700}]}`,
		cs.Token), http.StatusOK, "改模板")
	if fee() != 700 {
		t.Fatal("改了模板，引用它的门店应立即按新规则收 7 元")
	}

	// 在用 / 默认的删不掉；重名 409。
	for _, id := range []int64{far.Id, std.Id} {
		w := deleteAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/local-delivery-templates/%d", id), cs.Token)
		if p := problemOf(t, w, http.StatusConflict); p.Type != problem.TypeLocalDeliveryTemplateInUse {
			t.Fatalf("模板 %d 在用 / 是默认，删除应 409 in-use，实得 %s", id, p.Type)
		}
	}
	w := postIdem(t, cs.Host, "/api/v1/admin/local-delivery-templates",
		`{"name":"远郊","is_default":false,"min_order_cents":0,"free_over_cents":0,"fee_tiers":[]}`, cs.Token)
	if p := problemOf(t, w, http.StatusConflict); p.Type != problem.TypeLocalDeliveryTemplateConflict {
		t.Fatalf("重名应 409 conflict，实得 %s", p.Type)
	}

	// 改回跟随默认 → 3 元；「远郊」没人用了，能删。
	decodeInto(t, deleteAs(t, cs.Host, storePath, cs.Token), http.StatusOK, "改回跟随默认", &c)
	if c.Source != api.LocalDeliverySourceDefaultTemplate || fee() != 300 {
		t.Fatalf("改回跟随默认后应按「市区标准」3 元：%+v", c)
	}
	wantStatus(t, deleteAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/local-delivery-templates/%d", far.Id), cs.Token),
		http.StatusNoContent, "删没人用的模板")

	// 设新默认：旧的自动取消；列表的 store_count 数的是跟随默认的围栏店。
	night := createTpl(`{"name":"夜间","is_default":true,"min_order_cents":0,"free_over_cents":0,
		"fee_tiers":[{"within_m":3000,"fee_cents":600}]}`)
	var list struct {
		Items []api.LocalDeliveryTemplate `json:"items"`
	}
	decodeInto(t, getAs(t, cs.Host, "/api/v1/admin/local-delivery-templates", cs.Token), http.StatusOK, "模板列表", &list)
	for _, it := range list.Items {
		switch it.Id {
		case std.Id:
			if it.IsDefault {
				t.Error("设了新默认，「市区标准」应不再是默认")
			}
		case night.Id:
			if !it.IsDefault || it.StoreCount == nil || *it.StoreCount < 1 {
				t.Errorf("「夜间」应是默认且至少有北京门店跟随：%+v", it)
			}
		}
	}
	if fee() != 600 {
		t.Fatal("跟随默认的门店应按新默认「夜间」收 6 元")
	}
}
