package handler_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/keel/keel/internal/api"
)

// 门店那一组测试的夹具。
//
// 全部走**真实的 HTTP 接口**建数据，不直接写库 —— 与 newAdminShop 那一处
// 相反，而理由也相反：那一个造的是「测试的前置条件」（一家店、一个会话），
// 这几个造的正是**被测的东西**。用 SQL 造一个大区，等于把
// 「POST /admin/regions 真的能建出一个可用的大区」从判据里摘掉。
//
// 例外是 newAdminShop 自己播的那家默认门店：它是前置条件（没有它，
// CreateSKU 建不出库存行），不是这一组在验的东西。

// createCategory 建一个根类目，返回 id。
func createCategory(t *testing.T, sh adminShop, name string) int64 {
	t.Helper()
	var cat api.AdminCategory
	decodeInto(t, post(t, sh.Host, "/api/v1/admin/categories",
		fmt.Sprintf(`{"name":%q,"sort_order":1}`, name+" "+sh.Suffix), sh.Token),
		http.StatusCreated, "建类目", &cat)
	return cat.Id
}

// createPublishedSKU 建一件**已上架**的商品并给它一个 SKU，返回两个 id。
//
// 上架这一步不能省：买家侧那几条读路径只看 products.status = 1 的行，
// 草稿商品在 /products 与 /products/{id} 上都是查不到 —— 于是
// 「门店定价有没有生效」会被一个 404 掩盖掉，而 404 指向的是完全错的方向。
func createPublishedSKU(t *testing.T, sh adminShop, categoryID int64,
	title string, priceCents int64) (productID, skuID int64) {
	t.Helper()

	var p api.AdminProduct
	decodeInto(t, post(t, sh.Host, "/api/v1/admin/products",
		fmt.Sprintf(`{"category_id":%d,"title":%q}`, categoryID, title+" "+sh.Suffix), sh.Token),
		http.StatusCreated, "建商品", &p)

	var sku api.AdminSku
	decodeInto(t, post(t, sh.Host, fmt.Sprintf("/api/v1/admin/products/%d/skus", p.Id),
		fmt.Sprintf(`{"sku_code":"SKU-%s-%d","price_cents":%d,"available_qty":50}`,
			sh.Suffix, priceCents, priceCents), sh.Token),
		http.StatusCreated, "建 SKU", &sku)

	wantStatus(t, post(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/products/%d/publication", p.Id),
		`{"action":"publish"}`, sh.Token), http.StatusOK, "上架")

	return p.Id, sku.Id
}

// createRegion 走 POST /admin/regions 建一个大区。
func createRegion(t *testing.T, sh adminShop, code, name string) int64 {
	t.Helper()
	var r api.AdminRegion
	decodeInto(t, post(t, sh.Host, "/api/v1/admin/regions",
		fmt.Sprintf(`{"code":%q,"name":%q}`, code+"-"+sh.Suffix, name), sh.Token),
		http.StatusCreated, "建大区", &r)
	if r.StoreCount != 0 {
		t.Fatalf("新建大区的 store_count 是 %d，期望 0", r.StoreCount)
	}
	return r.Id
}

// createStore 走 POST /admin/stores 建一家**非默认**门店，带坐标。
//
// 坐标必给：distance_m 按它算，而一家没有坐标的店在按距离排序里会变成
// 「距离未知」，永远排在最后 —— 那会让「取最近的一家」这条断言在
// 一个错误的实现上也能绿。
func createStore(t *testing.T, sh adminShop, regionID int64, code, name string,
	lng, lat float64) int64 {
	t.Helper()
	var s api.AdminStore
	decodeInto(t, post(t, sh.Host, "/api/v1/admin/stores",
		fmt.Sprintf(`{"region_id":%d,"code":%q,"name":%q,"lng":%v,"lat":%v}`,
			regionID, code+"-"+sh.Suffix, name, lng, lat), sh.Token),
		http.StatusCreated, "建门店", &s)
	if s.Fence != nil {
		t.Fatal("建店时不该带围栏 —— 契约把围栏留给 PUT .../fence，" +
			"因为建店与画围栏是两个人在两个时刻做的事")
	}
	if s.IsDefault {
		t.Fatal("没传 is_default 却建出了一家默认门店")
	}
	return s.Id
}

// setFence 给一家门店画一个**矩形**围栏（经纬度的轴对齐矩形）。
//
// 环按逆时针给出并闭合。矩形足够了：这一组测试验的是「点在不在里面」与
// 「哪一家更近」，而不是 PostGIS 的几何实现 —— 自交那一种由
// TestSelfIntersectingFenceIsRejectedWithTheReason 单独验。
func setFence(t *testing.T, sh adminShop, storeID int64, lng0, lat0, lng1, lat1 float64) {
	t.Helper()
	body := fmt.Sprintf(
		`{"fence":{"type":"Polygon","coordinates":[[[%v,%v],[%v,%v],[%v,%v],[%v,%v],[%v,%v]]]}}`,
		lng0, lat0, lng1, lat0, lng1, lat1, lng0, lat1, lng0, lat0)
	var s api.AdminStore
	decodeInto(t, putAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d/fence", storeID),
		body, sh.Token), http.StatusOK, "配围栏", &s)
	if s.Fence == nil {
		t.Fatal("配完围栏读回来是 null —— ST_AsGeoJSON 那一侧或者 decodeFence 出了问题")
	}
	if len(s.Fence.Coordinates) != 1 || len(s.Fence.Coordinates[0]) != 5 {
		t.Fatalf("读回来的围栏形状是 %+v，期望一个 5 点闭环", s.Fence.Coordinates)
	}
}

// resolveAt 按坐标打一次 GET /stores/resolve。**不带令牌**：契约里它是
// security: []，而带上令牌会让「它公开不公开」这件事没有靶子。
func resolveAt(t *testing.T, host string, lng, lat float64) api.StoreResolveResult {
	t.Helper()
	return decodeResolve(t, getAs(t, host,
		fmt.Sprintf("/api/v1/stores/resolve?lat=%v&lng=%v", lat, lng), ""))
}

// resolveNoCoord 不带任何坐标打一次 —— 「拒绝授权定位」在服务端看到的
// 就是这个形状。
func resolveNoCoord(t *testing.T, host string) api.StoreResolveResult {
	t.Helper()
	return decodeResolve(t, getAs(t, host, "/api/v1/stores/resolve", ""))
}

func decodeResolve(t *testing.T, w *httptest.ResponseRecorder) api.StoreResolveResult {
	t.Helper()
	var res api.StoreResolveResult
	decodeInto(t, w, http.StatusOK, "解析门店", &res)
	return res
}

// assertProductPrice 按指定门店读买家侧的商品详情，断言生效价。
//
// 走 **GET /products/{id}?store_id=**，不走后台那条 —— 这一组测试真正要
// 证明的是「买家看到的价按门店走」，而后台那条读的是同一张视图，
// 拿它来断言等于自己证明自己。
func assertProductPrice(t *testing.T, host string, storeID, productID, want int64) {
	t.Helper()
	w := getAs(t, host, fmt.Sprintf("/api/v1/products/%d?store_id=%d", productID, storeID), "")
	var d api.ProductDetail
	decodeInto(t, w, http.StatusOK, "买家侧商品详情", &d)
	if int64(d.MinPriceCents) != want {
		t.Fatalf("按门店 %d 算，商品 %d 的 min_price_cents 是 %d，期望 %d —— "+
			"三层定价（基准价 → 大区价 → 门店价）必须经由 sku_prices_by_store 视图，"+
			"那是全仓库唯一一处写那条 COALESCE 的地方",
			storeID, productID, d.MinPriceCents, want)
	}
	// 只有一个 SKU 时区间退化成一个点。顺带核一次 max —— 把 min() 写成
	// max() 的实现在单 SKU 下看不出来，但漏填 max 看得出来。
	if d.MaxPriceCents == nil || int64(*d.MaxPriceCents) != want {
		t.Fatalf("max_price_cents 是 %v，期望 %d", d.MaxPriceCents, want)
	}
	// 详情的响应体里**没有** store —— 契约只给 GET /products 与 POST /search
	// 定了那个字段。这里不补一个：契约是单一真相源，handler 多回一个字段
	// 与少回一个同样是漂移。「按哪家店算的」在这条路径上由请求参数回答，
	// 而这个断言正是在验那件事。
}

// newAdminShopWithoutDefaultStore 造一家**没有默认门店**的店。
//
// 做法是把 newAdminShop 播的那一家软删掉，走真实的
// DELETE /admin/stores/{id} —— 而不是造一个不播门店的平行夹具：
// 平行夹具会和 newAdminShop 各自漂移，而它们漂开的那天，
// 「没配默认店」这条路径验的就不再是同一家店的同一种状态。
func newAdminShopWithoutDefaultStore(t *testing.T) adminShop {
	t.Helper()
	sh := newAdminShop(t)
	wantStatus(t, deleteAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/stores/%d", sh.StoreID), sh.Token),
		http.StatusNoContent, "软删默认门店")

	// 确认真的没有了。少了这一句，哪天软删不再把默认店从
	// GetDefaultStore 的视野里拿掉，这条测试会静默地变成
	// 「有默认店时也回 none」——一个反过来的 bug，而它同样是绿的。
	var list api.AdminStoreList
	decodeInto(t, getAs(t, sh.Host, "/api/v1/admin/stores", sh.Token),
		http.StatusOK, "列门店", &list)
	if list.HasDefault {
		t.Fatal("软删了唯一一家默认门店，has_default 仍是 true")
	}
	sh.StoreID = 0
	return sh
}
