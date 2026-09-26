package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// 购物车（/cart）的端到端测试。规则来源：契约 Cart tag、数据模型 §10
// （upsert 合并、999 上限、不存价格快照、失效行标出来而不是删掉）与 §4（三层定价）。

func setStorePrice(t *testing.T, bs buyerShop, storeID, skuID, cents int64) {
	t.Helper()
	wantStatus(t, putAs(t, bs.Host, fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/price", storeID, skuID),
		fmt.Sprintf(`{"price_cents":%d}`, cents), bs.Token), http.StatusOK, "设门店价")
}

func setRegionPrice(t *testing.T, bs buyerShop, regionID, skuID, cents int64) {
	t.Helper()
	wantStatus(t, putAs(t, bs.Host, fmt.Sprintf("/api/v1/admin/regions/%d/skus/%d/price", regionID, skuID),
		fmt.Sprintf(`{"price_cents":%d}`, cents), bs.Token), http.StatusOK, "设大区价")
}

func unlistInStore(t *testing.T, bs buyerShop, storeID, productID int64) {
	t.Helper()
	wantStatus(t, putAs(t, bs.Host, fmt.Sprintf("/api/v1/admin/stores/%d/products/%d/listing", storeID, productID),
		`{"listed":false}`, bs.Token), http.StatusOK, "门店下架")
}

func unlistInRegion(t *testing.T, bs buyerShop, regionID, productID int64) {
	t.Helper()
	wantStatus(t, putAs(t, bs.Host, fmt.Sprintf("/api/v1/admin/regions/%d/products/%d/listing", regionID, productID),
		`{"listed":false}`, bs.Token), http.StatusOK, "大区下架")
}

func unpublish(t *testing.T, bs buyerShop, productID int64) {
	t.Helper()
	wantStatus(t, postIdem(t, bs.Host, fmt.Sprintf("/api/v1/admin/products/%d/publication", productID),
		`{"action":"unpublish"}`, bs.Token), http.StatusOK, "下架商品")
}

// 加购合并 + 价格按门店走三层定价 + store 回显。
func TestCartAddMergesAndPricesPerStore(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "加购")
	setStorePrice(t, bs, bs.NorthStore, bs.DressSKU, 5500)   // 北京门店自己的价
	setRegionPrice(t, bs, bs.SouthRegion, bs.DressSKU, 5800) // 华南大区的价

	bs.mustAdd(t, b, bs.NorthStore, bs.DressSKU, 2)
	c := bs.mustAdd(t, b, bs.NorthStore, bs.DressSKU, 3)
	if len(c.Items) != 1 {
		t.Fatalf("重复加购同一个 SKU 应合并成一行，实得 %d 行", len(c.Items))
	}
	ln := lineOf(t, c, bs.DressSKU)
	if ln.Quantity != 5 || !ln.Selected || !ln.Available || ln.Status != api.CartItemStatusAvailable {
		t.Fatalf("合并后的行不对：%+v", ln)
	}
	if ln.ProductId == nil || *ln.ProductId != bs.DressProduct || ln.Title == nil || *ln.Title == "" {
		t.Fatalf("展示素材缺了：%+v", ln)
	}
	if n := cartItemCount(t, b.UserID); n != 1 {
		t.Fatalf("库里车有 %d 行，期望 1", n)
	}

	for _, tc := range []struct {
		store int64
		want  int64
		why   string
	}{
		{bs.NorthStore, 5500, "门店价优先"},
		{bs.SouthStore, 5800, "没有门店价时取大区价"},
		{bs.StoreID, 6000, "都没有时取基准价（默认门店）"},
	} {
		got := bs.getCart(t, b, tc.store)
		ln := lineOf(t, got, bs.DressSKU)
		if ln.PriceCents == nil || *ln.PriceCents != tc.want {
			t.Fatalf("store %d（%s）的价是 %v，期望 %d", tc.store, tc.why, ln.PriceCents, tc.want)
		}
		if got.TotalCents != api.Money(tc.want*5) || got.SelectedTotalCents != api.Money(tc.want*5) {
			t.Fatalf("store %d 的合计 %d / %d，期望 %d", tc.store, got.TotalCents, got.SelectedTotalCents, tc.want*5)
		}
		if got.Store.StoreId == nil || *got.Store.StoreId != tc.store {
			t.Fatalf("store 回显不对：%+v", got.Store)
		}
	}

	// 不传 store_id 走回落链（默认门店），与 /products 同一条规则。
	var fallback api.Cart
	decodeInto(t, bs.call(t, http.MethodGet, "/api/v1/cart", "", b), http.StatusOK, "不带门店读车", &fallback)
	if fallback.Store.MatchType != api.FallbackDefault || fallback.Store.StoreId == nil || *fallback.Store.StoreId != bs.StoreID {
		t.Fatalf("不带 store_id 应回落到默认门店：%+v", fallback.Store)
	}
	// 指名一家不存在的门店：422，不静默回落。
	wantStatus(t, bs.call(t, http.MethodGet, "/api/v1/cart?store_id=999999999", "", b),
		http.StatusUnprocessableEntity, "不存在的门店")
}

// 购物车显示的钱与下单试算的钱逐分相等 —— 两边读的是同一条定价查询。
func TestCartTotalsAgreeWithOrderPreview(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "对账")
	setStorePrice(t, bs, bs.NorthStore, bs.DressSKU, 5321)
	setRegionPrice(t, bs, bs.NorthRegion, bs.ShirtSKU, 4777)

	bs.mustAdd(t, b, bs.NorthStore, bs.DressSKU, 3)
	c := bs.mustAdd(t, b, bs.NorthStore, bs.ShirtSKU, 2)

	preview := func(items string) api.OrderPreview {
		t.Helper()
		var p api.OrderPreview
		decodeInto(t, previewOrder(t, bs.Host, fmt.Sprintf(`{"items":[%s],"address_id":%d,"store_id":%d}`,
			items, b.Address, bs.NorthStore), b.Token), http.StatusOK, "试算", &p)
		return p
	}
	all := preview(fmt.Sprintf(`{"sku_id":%d,"quantity":3},{"sku_id":%d,"quantity":2}`, bs.DressSKU, bs.ShirtSKU))
	if int64(c.SelectedTotalCents) != int64(all.GoodsAmountCents) || c.TotalCents != c.SelectedTotalCents {
		t.Fatalf("购物车已选合计 %d、全车合计 %d，试算商品金额 %d —— 必须逐分相等",
			c.SelectedTotalCents, c.TotalCents, all.GoodsAmountCents)
	}
	if want := int64(5321*3 + 4777*2); int64(c.TotalCents) != want {
		t.Fatalf("全车合计 %d，期望 %d", c.TotalCents, want)
	}

	// 取消勾选衬衫：已选合计只剩连衣裙，全车合计不变；试算只带连衣裙，仍然相等。
	shirt := lineOf(t, c, bs.ShirtSKU)
	decodeInto(t, bs.call(t, http.MethodPatch, cartPath(fmt.Sprintf("/api/v1/cart/items/%d", shirt.Id), bs.NorthStore),
		`{"selected":false}`, b), http.StatusOK, "取消勾选", &c)
	onlyDress := preview(fmt.Sprintf(`{"sku_id":%d,"quantity":3}`, bs.DressSKU))
	if int64(c.SelectedTotalCents) != int64(onlyDress.GoodsAmountCents) {
		t.Fatalf("取消勾选后已选合计 %d，试算 %d", c.SelectedTotalCents, onlyDress.GoodsAmountCents)
	}
	if int64(c.TotalCents) != int64(all.GoodsAmountCents) {
		t.Fatalf("取消勾选不该改变全车合计：%d → 期望 %d", c.TotalCents, all.GoodsAmountCents)
	}
}

// 失效、这家店不卖、缺货、不够的行留在车里并按契约标出来，合计只算买得到的。
func TestCartFlagsUnavailableLinesInsteadOfDroppingThem(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "失效")
	offProd, offSKU := createPublishedSKU(t, bs.adminShop, bs.ParentCat, "将下架", 1000)
	unlistedProd, unlistedSKU := createPublishedSKU(t, bs.adminShop, bs.ParentCat, "本店不卖", 2000)
	_, emptySKU := createPublishedSKU(t, bs.adminShop, bs.ParentCat, "将售罄", 3000)
	_, lowSKU := createPublishedSKU(t, bs.adminShop, bs.ParentCat, "将不够", 4000)
	regionProd, regionSKU := createPublishedSKU(t, bs.adminShop, bs.ParentCat, "本大区不卖", 5100)
	for _, sku := range []int64{offSKU, unlistedSKU, emptySKU, lowSKU, regionSKU} {
		setStoreStock(t, bs.adminShop, bs.NorthStore, sku, 10)
	}
	bs.mustAdd(t, b, bs.NorthStore, bs.DressSKU, 1)
	bs.mustAdd(t, b, bs.NorthStore, offSKU, 1)
	bs.mustAdd(t, b, bs.NorthStore, unlistedSKU, 1)
	bs.mustAdd(t, b, bs.NorthStore, emptySKU, 1)
	bs.mustAdd(t, b, bs.NorthStore, lowSKU, 5)
	bs.mustAdd(t, b, bs.NorthStore, regionSKU, 1)

	// 加购之后世界变了：下架、本店排除、大区排除、清零、只剩 2 件。
	unpublish(t, bs, offProd)
	unlistInStore(t, bs, bs.NorthStore, unlistedProd)
	setStoreStock(t, bs.adminShop, bs.NorthStore, emptySKU, 0)
	setStoreStock(t, bs.adminShop, bs.NorthStore, lowSKU, 2)
	unlistInRegion(t, bs, bs.NorthRegion, regionProd) // 大区那一层排除，门店自己没动

	c := bs.getCart(t, b, bs.NorthStore)
	if len(c.Items) != 6 {
		t.Fatalf("失效的行不该从车里消失：实得 %d 行", len(c.Items))
	}
	for _, tc := range []struct {
		sku       int64
		status    api.CartItemStatus
		wantPrice *int64
	}{
		{bs.DressSKU, api.CartItemStatusAvailable, ptr64(6000)},
		{offSKU, api.CartItemStatusOffShelf, nil},
		{unlistedSKU, api.CartItemStatusNotSoldInStore, nil},
		{regionSKU, api.CartItemStatusNotSoldInStore, nil},
		{emptySKU, api.CartItemStatusOutOfStock, ptr64(3000)},
		{lowSKU, api.CartItemStatusInsufficientStock, ptr64(4000)},
	} {
		ln := lineOf(t, c, tc.sku)
		if ln.Status != tc.status {
			t.Errorf("sku %d 的 status 是 %s，期望 %s", tc.sku, ln.Status, tc.status)
		}
		if ln.Available != (tc.status == api.CartItemStatusAvailable) {
			t.Errorf("sku %d 的 available=%v 与 status=%s 不一致", tc.sku, ln.Available, ln.Status)
		}
		switch {
		case tc.wantPrice == nil && ln.PriceCents != nil:
			t.Errorf("sku %d（%s）不该有价，实得 %d", tc.sku, tc.status, *ln.PriceCents)
		case tc.wantPrice != nil && (ln.PriceCents == nil || *ln.PriceCents != *tc.wantPrice):
			t.Errorf("sku %d 的价是 %v，期望 %d", tc.sku, ln.PriceCents, *tc.wantPrice)
		}
	}
	if c.TotalCents != 6000 || c.SelectedTotalCents != 6000 {
		t.Fatalf("合计只该算买得到的那一行（6000），实得 %d / %d", c.TotalCents, c.SelectedTotalCents)
	}
	// price_cents 为 null 时字段要**在**（契约 required），值是 null。
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(bs.call(t, http.MethodGet, cartPath("/api/v1/cart", bs.NorthStore), "", b).Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, it := range raw.Items {
		v, ok := it["price_cents"]
		if !ok {
			t.Fatalf("有一行没有 price_cents 字段：%v", it)
		}
		if string(it["status"]) == `"off_shelf"` && string(v) != "null" {
			t.Fatalf("off_shelf 行的 price_cents 应为 null，实得 %s", v)
		}
	}

	// 换一家店看同一辆车：本店排除只在北京门店生效，广州门店照样卖（虽然没货）。
	south := bs.getCart(t, b, bs.SouthStore)
	if ln := lineOf(t, south, unlistedSKU); ln.Status != api.CartItemStatusOutOfStock {
		t.Fatalf("门店排除不该影响别的门店：广州门店里它是 %s", ln.Status)
	}
	if ln := lineOf(t, south, offSKU); ln.Status != api.CartItemStatusOffShelf {
		t.Fatalf("下架是全局的：广州门店里它是 %s", ln.Status)
	}
}


// 加购时判「卖不卖」「够不够」，各有各的 type。
func TestCartAddRejectsWhatCannotBeBought(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "拒绝")
	offProd, offSKU := createPublishedSKU(t, bs.adminShop, bs.ParentCat, "下架了", 1000)
	unpublish(t, bs, offProd)
	unlistInStore(t, bs, bs.NorthStore, bs.ShirtProduct)

	if typ := problemType(t, bs.addToCart(t, b, bs.NorthStore, bs.DressSKU, 51),
		http.StatusConflict, "超过门店库存"); typ != problem.TypeInsufficientStock {
		t.Fatalf("超过库存的 type 是 %s", typ)
	}
	if typ := problemType(t, bs.addToCart(t, b, bs.NorthStore, bs.ShirtSKU, 1),
		http.StatusUnprocessableEntity, "本店不卖"); typ != problem.TypeSKUNotSoldInStore {
		t.Fatalf("本店不卖的 type 是 %s", typ)
	}
	if typ := problemType(t, bs.addToCart(t, b, bs.NorthStore, offSKU, 1),
		http.StatusUnprocessableEntity, "已下架"); typ != problem.TypeInvalidRequest {
		t.Fatalf("已下架的 type 是 %s", typ)
	}
	// 别家店的 SKU：RLS 让它与「不存在」同形。
	other := newBuyerShop(t)
	if typ := problemType(t, bs.addToCart(t, b, bs.NorthStore, other.DressSKU, 1),
		http.StatusUnprocessableEntity, "别家店的 SKU"); typ != problem.TypeInvalidRequest {
		t.Fatalf("别家店 SKU 的 type 是 %s", typ)
	}
	wantStatus(t, bs.addToCart(t, b, 999999999, bs.DressSKU, 1), http.StatusUnprocessableEntity, "不存在的门店")
	for _, body := range []string{`{"sku_id":1,"quantity":0}`, `{"sku_id":1,"quantity":1000}`, `{"sku_id":0,"quantity":1}`} {
		wantStatus(t, bs.call(t, http.MethodPost, cartPath("/api/v1/cart/items", bs.NorthStore), body, b),
			http.StatusUnprocessableEntity, "非法请求体 "+body)
	}
	if n := cartItemCount(t, b.UserID); n != 0 {
		t.Fatalf("被拒的加购落了库：车里有 %d 行", n)
	}
	// 缺货并没有让它进不了别的店：广州门店有货，照样能加。
	bs.mustAdd(t, b, bs.SouthStore, bs.ShirtSKU, 1)
}

// 999 上限：累加后超过就 422 cart-quantity-exceeded，不静默截断。
func TestCartQuantityCapIsNotTruncated(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "上限")
	setStoreStock(t, bs.adminShop, bs.NorthStore, bs.DressSKU, 5000)
	bs.mustAdd(t, b, bs.NorthStore, bs.DressSKU, 998)
	w := bs.addToCart(t, b, bs.NorthStore, bs.DressSKU, 2)
	if typ := problemType(t, w, http.StatusUnprocessableEntity, "超过 999"); typ != problem.TypeCartQuantityExceeded {
		t.Fatalf("type 是 %s", typ)
	}
	if !strings.Contains(w.Body.String(), "998") || !strings.Contains(w.Body.String(), "999") {
		t.Fatalf("detail 应给出当前数量与上限：%s", w.Body.String())
	}
	if ln := lineOf(t, bs.getCart(t, b, bs.NorthStore), bs.DressSKU); ln.Quantity != 998 {
		t.Fatalf("超限的加购改动了数量：%d", ln.Quantity)
	}
	c := bs.mustAdd(t, b, bs.NorthStore, bs.DressSKU, 1)
	if ln := lineOf(t, c, bs.DressSKU); ln.Quantity != 999 {
		t.Fatalf("恰好 999 应当放行，实得 %d", ln.Quantity)
	}
}

// 调大数量判库存，调小永远放行。
func TestCartPatchQuantityOnlyChecksStockWhenIncreasing(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "改数量")
	setStoreStock(t, bs.adminShop, bs.NorthStore, bs.DressSKU, 5)
	c := bs.mustAdd(t, b, bs.NorthStore, bs.DressSKU, 5)
	item := lineOf(t, c, bs.DressSKU).Id
	path := cartPath(fmt.Sprintf("/api/v1/cart/items/%d", item), bs.NorthStore)

	setStoreStock(t, bs.adminShop, bs.NorthStore, bs.DressSKU, 2)
	decodeInto(t, bs.call(t, http.MethodPatch, path, `{"quantity":4}`, b), http.StatusOK, "调小", &c)
	if ln := lineOf(t, c, bs.DressSKU); ln.Quantity != 4 || ln.Status != api.CartItemStatusInsufficientStock {
		t.Fatalf("调小之后：%+v", ln)
	}
	if typ := problemType(t, bs.call(t, http.MethodPatch, path, `{"quantity":6}`, b),
		http.StatusConflict, "调大超过库存"); typ != problem.TypeInsufficientStock {
		t.Fatalf("type 是 %s", typ)
	}
	decodeInto(t, bs.call(t, http.MethodPatch, path, `{"quantity":2,"selected":false}`, b), http.StatusOK, "调到刚好", &c)
	if ln := lineOf(t, c, bs.DressSKU); ln.Quantity != 2 || ln.Selected || ln.Status != api.CartItemStatusAvailable {
		t.Fatalf("调到刚好之后：%+v", ln)
	}
	if c.SelectedTotalCents != 0 || c.TotalCents != 12000 {
		t.Fatalf("取消勾选之后已选合计应为 0、全车 12000：%d / %d", c.SelectedTotalCents, c.TotalCents)
	}
	for _, body := range []string{`{}`, `{"quantity":0}`, `{"quantity":1000}`} {
		wantStatus(t, bs.call(t, http.MethodPatch, path, body, b), http.StatusUnprocessableEntity, "非法 PATCH "+body)
	}
}

// 勾选、批量删除、单删、清空。
func TestCartSelectionAndDeletion(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "勾选")
	bs.mustAdd(t, b, bs.NorthStore, bs.DressSKU, 1)
	c := bs.mustAdd(t, b, bs.NorthStore, bs.ShirtSKU, 1)
	dress, shirt := lineOf(t, c, bs.DressSKU).Id, lineOf(t, c, bs.ShirtSKU).Id
	sel := cartPath("/api/v1/cart/selection", bs.NorthStore)

	decodeInto(t, bs.call(t, http.MethodPut, sel, `{"selected":false}`, b), http.StatusOK, "全不选", &c)
	if c.SelectedTotalCents != 0 || c.TotalCents != 11000 {
		t.Fatalf("全不选之后：%d / %d", c.SelectedTotalCents, c.TotalCents)
	}
	decodeInto(t, bs.call(t, http.MethodPut, sel, fmt.Sprintf(`{"selected":true,"item_ids":[%d,%d]}`, dress, dress), b),
		http.StatusOK, "只选连衣裙", &c)
	if c.SelectedTotalCents != 6000 {
		t.Fatalf("只选连衣裙之后已选合计 %d", c.SelectedTotalCents)
	}
	wantStatus(t, bs.call(t, http.MethodPut, sel, `{"item_ids":[1]}`, b), http.StatusUnprocessableEntity, "缺 selected")

	// 批量删除的三种非法形状。
	del := cartPath("/api/v1/cart/items/batch-delete", bs.NorthStore)
	for _, body := range []string{`{}`, `{"selected":false}`, fmt.Sprintf(`{"selected":true,"item_ids":[%d]}`, dress), `{"item_ids":[]}`} {
		wantStatus(t, bs.call(t, http.MethodPost, del, body, b), http.StatusUnprocessableEntity, "非法批量删除 "+body)
	}
	// 删除已勾选：只剩衬衫。
	decodeInto(t, bs.call(t, http.MethodPost, del, `{"selected":true}`, b), http.StatusOK, "删除已勾选", &c)
	if len(c.Items) != 1 || c.Items[0].Id != shirt {
		t.Fatalf("删除已勾选之后应只剩衬衫：%+v", c.Items)
	}
	decodeInto(t, bs.call(t, http.MethodPost, del, fmt.Sprintf(`{"item_ids":[%d]}`, shirt), b), http.StatusOK, "按 id 删", &c)
	if len(c.Items) != 0 {
		t.Fatalf("按 id 删之后车应为空：%+v", c.Items)
	}

	c = bs.mustAdd(t, b, bs.NorthStore, bs.DressSKU, 1)
	item := lineOf(t, c, bs.DressSKU).Id
	wantStatus(t, bs.call(t, http.MethodDelete, fmt.Sprintf("/api/v1/cart/items/%d", item), "", b), http.StatusNoContent, "单删")
	wantStatus(t, bs.call(t, http.MethodDelete, fmt.Sprintf("/api/v1/cart/items/%d", item), "", b), http.StatusNotFound, "重复单删")
	bs.mustAdd(t, b, bs.NorthStore, bs.DressSKU, 1)
	bs.mustAdd(t, b, bs.NorthStore, bs.ShirtSKU, 1)
	wantStatus(t, bs.call(t, http.MethodDelete, "/api/v1/cart", "", b), http.StatusNoContent, "清空")
	if n := cartItemCount(t, b.UserID); n != 0 {
		t.Fatalf("清空之后车里还有 %d 行", n)
	}
	// 从没加购过的人：读车是一辆空车，清空也是 204。
	fresh := bs.newBuyer(t, "新人")
	if got := bs.getCart(t, fresh, bs.NorthStore); len(got.Items) != 0 || got.Items == nil {
		t.Fatalf("新人的车应是空数组（不是 null）：%+v", got)
	}
	wantStatus(t, bs.call(t, http.MethodDelete, "/api/v1/cart", "", fresh), http.StatusNoContent, "清空空车")
}

// 越权：同一家店里别人车里的条目，改、删、勾选、批量删一律 404（不是 403），
// 带着别人条目的批量操作一行都不改（包括我自己那几行），别人的那一行原封不动。
func TestCartItemsOfOthersAreNotFound(t *testing.T) {
	bs := newBuyerShop(t)
	owner := bs.newBuyer(t, "车主")
	intruder := bs.newBuyer(t, "旁人")
	c := bs.mustAdd(t, owner, bs.NorthStore, bs.DressSKU, 2)
	victim := lineOf(t, c, bs.DressSKU).Id
	mine := lineOf(t, bs.mustAdd(t, intruder, bs.NorthStore, bs.ShirtSKU, 1), bs.ShirtSKU).Id

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPatch, cartPath(fmt.Sprintf("/api/v1/cart/items/%d", victim), bs.NorthStore), `{"quantity":1}`},
		{http.MethodPatch, cartPath(fmt.Sprintf("/api/v1/cart/items/%d", victim), bs.NorthStore), `{"selected":false}`},
		{http.MethodDelete, fmt.Sprintf("/api/v1/cart/items/%d", victim), ""},
		{http.MethodPut, cartPath("/api/v1/cart/selection", bs.NorthStore),
			fmt.Sprintf(`{"selected":false,"item_ids":[%d,%d]}`, mine, victim)},
		{http.MethodPost, cartPath("/api/v1/cart/items/batch-delete", bs.NorthStore),
			fmt.Sprintf(`{"item_ids":[%d,%d]}`, mine, victim)},
	} {
		w := bs.call(t, tc.method, tc.path, tc.body, intruder)
		if typ := problemType(t, w, http.StatusNotFound, tc.method+" "+tc.path+" "+tc.body); typ != problem.TypeNotFound {
			t.Fatalf("%s %s：type 是 %s", tc.method, tc.path, typ)
		}
	}
	// 车主那一行原封不动。
	if ln := lineOf(t, bs.getCart(t, owner, bs.NorthStore), bs.DressSKU); ln.Quantity != 2 || !ln.Selected {
		t.Fatalf("旁人的请求改动了车主的条目：%+v", ln)
	}
	// 旁人自己那一行也没被那两个批量请求改掉：要么全改，要么一行不改。
	if ln := lineOf(t, bs.getCart(t, intruder, bs.NorthStore), bs.ShirtSKU); !ln.Selected {
		t.Fatal("带着别人条目的批量勾选改掉了我自己的那一行 —— 应当整体回滚")
	}
	if n := cartItemCount(t, intruder.UserID); n != 1 {
		t.Fatalf("带着别人条目的批量删除删掉了我自己的那一行（剩 %d 行）", n)
	}
	// 我的车里看不到别人的条目。
	for _, it := range bs.getCart(t, intruder, bs.NorthStore).Items {
		if it.Id == victim {
			t.Fatal("别人车里的条目出现在了我的车里")
		}
	}

	// 跨租户：另一家店的买家拿着这个条目 id 去他自己的店里改。
	other := newBuyerShop(t)
	stranger := other.newBuyer(t, "别家店")
	other.mustAdd(t, stranger, other.NorthStore, other.DressSKU, 1) // 让他有一辆车
	wantStatus(t, other.call(t, http.MethodPatch, cartPath(fmt.Sprintf("/api/v1/cart/items/%d", victim), other.NorthStore),
		`{"quantity":1}`, stranger), http.StatusNotFound, "跨租户改条目")
	wantStatus(t, other.call(t, http.MethodDelete, fmt.Sprintf("/api/v1/cart/items/%d", victim), "", stranger),
		http.StatusNotFound, "跨租户删条目")
	if ln := lineOf(t, bs.getCart(t, owner, bs.NorthStore), bs.DressSKU); ln.Quantity != 2 {
		t.Fatalf("跨租户的请求改动了车主的条目：%+v", ln)
	}
}

// 加购的幂等：同一把钥匙打两次只加一次。
func TestCartAddIsIdempotent(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "幂等加购")
	key := freshIdemKey()
	path := cartPath("/api/v1/cart/items", bs.NorthStore)
	body := fmt.Sprintf(`{"sku_id":%d,"quantity":2}`, bs.DressSKU)

	wantStatus(t, bs.callWithKey(t, path, body, b, key), http.StatusOK, "首次加购")
	w := bs.callWithKey(t, path, body, b, key)
	wantStatus(t, w, http.StatusOK, "重放")
	if w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("同一把钥匙第二次应带 Idempotency-Replayed: true")
	}
	if ln := lineOf(t, bs.getCart(t, b, bs.NorthStore), bs.DressSKU); ln.Quantity != 2 {
		t.Fatalf("同一把钥匙加购两次，数量变成了 %d（期望 2）", ln.Quantity)
	}
	// 同一个请求体换一家店是另一个请求。
	if typ := problemType(t, bs.callWithKey(t, cartPath("/api/v1/cart/items", bs.SouthStore), body, b, key),
		http.StatusUnprocessableEntity, "同钥匙换门店"); typ != problem.TypeIdempotencyKeyReused {
		t.Fatalf("同钥匙换门店的 type 是 %s", typ)
	}
	if typ := problemType(t, bs.callWithKey(t, path, body, b, ""),
		http.StatusUnprocessableEntity, "不带钥匙"); typ != problem.TypeInvalidRequest {
		t.Fatalf("不带钥匙的 type 是 %s", typ)
	}
}
