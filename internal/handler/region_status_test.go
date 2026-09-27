package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
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

// 商品列表按门店标出有没有货（2026-09-27：之前列表不读库存，in_stock 恒缺席，
// 买家端列表上无货的商品照样挂「＋」，点进去才知道没货）。
//
// 连衣裙在北京门店清零、广州门店不动：同一件商品按门店给出不同的 in_stock ——
// 证明它是按这家店算的，而不是全局的「还有没有」。
func TestProductListMarksOutOfStockPerStore(t *testing.T) {
	cs := newCouponShop(t)
	setStoreStock(t, cs.adminShop, cs.NorthStore, cs.DressSKU, 0)

	inStock := func(storeID int64) map[int64]*bool {
		var out struct {
			Items []api.ProductSummary `json:"items"`
		}
		decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/products?store_id=%d", storeID), ""),
			http.StatusOK, "商品列表", &out)
		m := map[int64]*bool{}
		for _, it := range out.Items {
			m[it.Id] = it.InStock
		}
		return m
	}
	north, south := inStock(cs.NorthStore), inStock(cs.SouthStore)
	for _, c := range []struct {
		what string
		got  *bool
		want bool
	}{
		{"北京门店的连衣裙", north[cs.DressProduct], false},
		{"北京门店的衬衫", north[cs.ShirtProduct], true},
		{"广州门店的连衣裙", south[cs.DressProduct], true},
	} {
		if c.got == nil {
			t.Fatalf("%s：in_stock 缺席 —— 列表没有读库存", c.what)
		}
		if *c.got != c.want {
			t.Errorf("%s：in_stock=%v，期望 %v", c.what, *c.got, c.want)
		}
	}
}

// 商品列表有货在前（2026-09-27，00087 product_store_stock）。
//
// 夹具里衬衫比连衣裙晚建，默认顺序是衬衫在前。
//
//  ① 后台把北京门店的衬衫清零（后台改库存那条会立刻刷标记）：北京列表连衣裙在前、衬衫在后；
//     广州不受影响，衬衫仍在前 —— 证明排序按门店。
//  ② 绕过后台，用下单 SAGA 的库存分支把北京门店的连衣裙扣到 0（这条路 core 不逐笔知道水位），
//     全量刷新一轮之后两件都无货，回到默认顺序；再补回衬衫库存，衬衫排回前面。
func TestProductListPutsInStockFirst(t *testing.T) {
	cs := newCouponShop(t)
	order := func(storeID int64) []int64 {
		var out struct {
			Items []api.ProductSummary `json:"items"`
		}
		decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/products?store_id=%d", storeID), ""),
			http.StatusOK, "商品列表", &out)
		var ids []int64
		for _, it := range out.Items {
			if it.Id == cs.DressProduct || it.Id == cs.ShirtProduct {
				ids = append(ids, it.Id)
			}
		}
		return ids
	}
	want := func(what string, got []int64, first, second int64) {
		t.Helper()
		if len(got) != 2 || got[0] != first || got[1] != second {
			t.Fatalf("%s：顺序是 %v，期望 [%d %d]（连衣裙 %d，衬衫 %d）", what, got, first, second,
				cs.DressProduct, cs.ShirtProduct)
		}
	}
	want("初始（都有货）", order(cs.NorthStore), cs.ShirtProduct, cs.DressProduct)

	setStoreStock(t, cs.adminShop, cs.NorthStore, cs.ShirtSKU, 0)
	want("北京衬衫清零后", order(cs.NorthStore), cs.DressProduct, cs.ShirtProduct)
	want("广州不受影响", order(cs.SouthStore), cs.ShirtProduct, cs.DressProduct)

	deductViaSaga(t, cs.MerchantID, cs.NorthStore, cs.DressSKU, int32(availableAt(t, cs.NorthStore, cs.DressSKU)))
	job := service.NewStockFlagService(repository.New(testPool), localInventory(), 0, nil)
	if err := job.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	want("下单扣光连衣裙 + 全量刷新后（两件都无货）", order(cs.NorthStore), cs.ShirtProduct, cs.DressProduct)

	setStoreStock(t, cs.adminShop, cs.NorthStore, cs.ShirtSKU, 5)
	want("补回衬衫后", order(cs.NorthStore), cs.ShirtProduct, cs.DressProduct)
}
