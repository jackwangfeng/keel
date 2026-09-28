package handler_test

import (
	"net/http"
	"testing"

	"github.com/keel/keel/internal/problem"
)

// 下单 / 试算：收货地址带坐标时必须落在所选门店的围栏内（2026-09-28）。
//
// 北京门店围栏 116.30–116.50 × 39.80–40.00。同一个买家、同一条地址，只改坐标：
// 没坐标（老地址）照常；坐标在围栏内照常；挪到上海 → 试算与下单都 422 address-out-of-range。
// 默认门店是全国兜底，地址在上海也能在它那里下单（阳性对照：拦住北京的是围栏，不是地址坏了）。
func TestOrderAddressMustBeInsideStoreFence(t *testing.T) {
	cs := newCouponShop(t)
	setFence(t, cs.adminShop, cs.NorthStore, 116.30, 39.80, 116.50, 40.00)
	b := cs.newBuyer(t, "fence")
	setCoord := func(lat, lng any) {
		t.Helper()
		if _, err := admin(t).Exec(t.Context(), `UPDATE user_addresses SET lat = $2, lng = $3 WHERE id = $1`,
			b.Address, lat, lng); err != nil {
			t.Fatal(err)
		}
	}
	north := cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 1, nil)

	if _, w := cs.preview(t, b, north); w.Code != http.StatusOK {
		t.Fatalf("地址没坐标时应照常试算（判不了就不拦）：%d %s", w.Code, w.Body.String())
	}
	setCoord(39.90, 116.40)
	if _, w := cs.preview(t, b, north); w.Code != http.StatusOK {
		t.Fatalf("地址在围栏内试算失败：%d %s", w.Code, w.Body.String())
	}

	setCoord(31.23, 121.47)
	_, w := cs.preview(t, b, north)
	if p := problemOf(t, w, http.StatusUnprocessableEntity); p.Type != problem.TypeAddressOutOfRange {
		t.Fatalf("地址在围栏外试算应 422 address-out-of-range，实得 %s", p.Type)
	}
	cw := createOrder(t, cs.Host, north, b.Token, "fence-"+uniqueKey())
	if p := problemOf(t, cw, http.StatusUnprocessableEntity); p.Type != problem.TypeAddressOutOfRange {
		t.Fatalf("地址在围栏外下单应 422 address-out-of-range，实得 %s", p.Type)
	}

	def := adminQueryInt64(t, `SELECT id FROM stores WHERE merchant_id = $1 AND is_default AND deleted_at IS NULL`,
		cs.MerchantID)
	setStoreStock(t, cs.adminShop, def, cs.DressSKU, 10)
	if _, w := cs.preview(t, b, cs.orderJSON(b, def, cs.DressSKU, 1, nil)); w.Code != http.StatusOK {
		t.Fatalf("默认门店是全国兜底，围栏外的地址也应能试算：%d %s", w.Code, w.Body.String())
	}
}
