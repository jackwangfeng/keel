package handler_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// 大区停用 = 名下门店一律按停业处理（2026-09-27：之前 regions.status 哪条查询都没读，
// 停用一个大区对买家毫无影响）。
//
// 停用华北：北京门店的围栏命中不到它、选店列表里没有它、在它下单 / 试算 409 store-unavailable；
// 华南照常（阳性对照：证明拦住北京的是大区状态，不是夹具坏了）。重新启用后北京恢复。
// 另外：停用大区里的门店不能被设为默认门店。
func TestDisabledRegionTakesItsStoresOffline(t *testing.T) {
	cs := newCouponShop(t)
	setFence(t, cs.adminShop, cs.NorthStore, 116.30, 39.80, 116.50, 40.00)
	b := cs.newBuyer(t, "region")

	openIDs := func() map[int64]bool {
		var out struct {
			Items []api.Store `json:"items"`
		}
		decodeInto(t, getAs(t, cs.Host, "/api/v1/stores", ""), http.StatusOK, "选店列表", &out)
		ids := map[int64]bool{}
		for _, s := range out.Items {
			ids[s.Id] = true
		}
		return ids
	}
	hitsNorth := func() bool {
		for _, s := range resolveAt(t, cs.Host, 116.40, 39.90).Stores {
			if s.Id == cs.NorthStore {
				return true
			}
		}
		return false
	}
	setRegion := func(status int) {
		wantStatus(t, patchAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/regions/%d", cs.NorthRegion),
			fmt.Sprintf(`{"status":%d}`, status), cs.Token), http.StatusOK, "改大区状态")
	}

	if !openIDs()[cs.NorthStore] || !hitsNorth() {
		t.Fatal("停用之前北京门店就不在选店列表 / 围栏命中里 —— 夹具不对，这条测试没在测任何东西")
	}
	if _, w := cs.preview(t, b, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 1, nil)); w.Code != http.StatusOK {
		t.Fatalf("停用之前北京门店试算就失败：%d %s", w.Code, w.Body.String())
	}

	setRegion(0)

	if openIDs()[cs.NorthStore] {
		t.Error("华北停用后，北京门店还在 GET /stores 里")
	}
	if hitsNorth() {
		t.Error("华北停用后，站在北京门店围栏里还能命中它")
	}
	_, w := cs.preview(t, b, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 1, nil))
	if p := problemOf(t, w, http.StatusConflict); p.Type != problem.TypeStoreUnavailable {
		t.Fatalf("华北停用后在北京门店试算应 409 store-unavailable，实得 %s", p.Type)
	}
	cw := createOrder(t, cs.Host, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 1, nil), b.Token, "rg-"+uniqueKey())
	if p := problemOf(t, cw, http.StatusConflict); p.Type != problem.TypeStoreUnavailable {
		t.Fatalf("华北停用后在北京门店下单应 409 store-unavailable，实得 %s", p.Type)
	}
	w = putAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/stores/%d/default", cs.NorthStore), "", cs.Token)
	if p := problemOf(t, w, http.StatusConflict); p.Type != problem.TypeStoreUnavailable {
		t.Fatalf("停用大区里的门店设为默认应 409 store-unavailable，实得 %s", p.Type)
	}
	// 阳性对照：华南不受影响。
	if !openIDs()[cs.SouthStore] {
		t.Error("停用华北把华南的广州门店也带下线了")
	}
	if _, w := cs.preview(t, b, cs.orderJSON(b, cs.SouthStore, cs.DressSKU, 1, nil)); w.Code != http.StatusOK {
		t.Fatalf("停用华北后广州门店试算失败：%d %s", w.Code, w.Body.String())
	}

	setRegion(1)
	if !openIDs()[cs.NorthStore] || !hitsNorth() {
		t.Error("华北重新启用后，北京门店没有回到选店列表 / 围栏命中里")
	}
	if _, w := cs.preview(t, b, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 1, nil)); w.Code != http.StatusOK {
		t.Fatalf("华北重新启用后北京门店试算失败：%d %s", w.Code, w.Body.String())
	}
}

// 停业的门店不能下单（之前只挡了定位与选店，下单那条只看门店存不存在）。
func TestClosedStoreRejectsOrders(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "closed")
	wantStatus(t, patchAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/stores/%d", cs.NorthStore), `{"status":0}`, cs.Token),
		http.StatusOK, "门店停业")
	_, w := cs.preview(t, b, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 1, nil))
	if p := problemOf(t, w, http.StatusConflict); p.Type != problem.TypeStoreUnavailable {
		t.Fatalf("停业门店试算应 409 store-unavailable，实得 %s", p.Type)
	}
}
