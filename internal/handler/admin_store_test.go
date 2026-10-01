package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// 门店 / 大区那 21 条后台接口的行为测试（00020）。
//
// 分两组：
//   - 鉴权：**每一条都挂了 staffAuth** 的唯一执行者；
//   - 产出标志：一家商家能建大区、建店、画围栏、分层定价、分层上下架，
//     而买家看到的价与可见性跟着他站的位置变。

type adminStoreRoute struct {
	method, path, body string
}

// adminStoreRoutes 是那 21 条。**路径里的 id 用 1**：这家店是现建的，
// id = 1 几乎必然不属于它，于是带 token 那一次会落在 404 上 ——
// 而 404 不是 401，正是阳性对照要的。
func adminStoreRoutes() []adminStoreRoute {
	return []adminStoreRoute{
		{http.MethodGet, "/api/v1/admin/regions", ""},
		{http.MethodPost, "/api/v1/admin/regions", `{"code":"r","name":"r"}`},
		{http.MethodPatch, "/api/v1/admin/regions/1", `{"name":"r"}`},
		{http.MethodDelete, "/api/v1/admin/regions/1", ""},
		{http.MethodGet, "/api/v1/admin/regions/1/products", ""},
		{http.MethodPut, "/api/v1/admin/regions/1/products/1/listing", `{"listed":false}`},
		{http.MethodPut, "/api/v1/admin/regions/1/skus/1/price", `{"price_cents":1}`},
		{http.MethodDelete, "/api/v1/admin/regions/1/skus/1/price", ""},
		{http.MethodGet, "/api/v1/admin/stores", ""},
		{http.MethodPost, "/api/v1/admin/stores", `{"region_id":1,"code":"s","name":"s"}`},
		{http.MethodGet, "/api/v1/admin/stores/1", ""},
		{http.MethodPatch, "/api/v1/admin/stores/1", `{"name":"s"}`},
		{http.MethodDelete, "/api/v1/admin/stores/1", ""},
		{http.MethodPut, "/api/v1/admin/stores/1/fence", `{"fence":null}`},
		{http.MethodPut, "/api/v1/admin/stores/1/default", ""},
		{http.MethodGet, "/api/v1/admin/stores/1/products", ""},
		{http.MethodPut, "/api/v1/admin/stores/1/products/1/listing", `{"listed":false}`},
		{http.MethodPut, "/api/v1/admin/stores/1/skus/1/price", `{"price_cents":1}`},
		{http.MethodDelete, "/api/v1/admin/stores/1/skus/1/price", ""},
		{http.MethodGet, "/api/v1/admin/stores/1/inventories", ""},
		{http.MethodPut, "/api/v1/admin/stores/1/skus/1/inventory",
			`{"expected_available_qty":0,"available_qty":1}`},
	}
}

// 21 条全部要后台会话。
//
// **这条测试是「每一行都挂了 staffAuth」唯一的执行者**，与
// TestAdminCatalogRoutesAllRequireStaffSession 一字不差的理由：
// app/run_test.go 那张路由表核的是路径，而把某一行的中间件删掉，
// 路由表一个字都不会变 —— 那时一个匿名请求能把这家店的售价改成 0，
// 并且拿到 200。
//
// 判据有两段，缺第二段这条测试就是空转：
//
//	① 不带 Authorization 头打过去，必须是 401；
//	② **阳性对照**：同一条路径带上真实会话 token 再打一次，必须**不是** 401。
//	   少了它，「handler 里第一行无条件写 401」这种写法会让①全绿。
func TestAdminStoreRoutesAllRequireStaffSession(t *testing.T) {
	sh := newAdminShop(t)

	for _, r := range adminStoreRoutes() {
		what := r.method + " " + r.path
		anon := reqAs(t, r.method, sh.Host, r.path, r.body, "")
		if anon.Code != http.StatusUnauthorized {
			t.Errorf("%s 不带后台会话返回 %d，期望 401 —— 这条路由是不是漏挂了 "+
				"auth.StaffBearer？漏挂之后租户中间件照样解得出 Host，"+
				"WithTenant 照常开事务，于是匿名请求能写这家店的数据。响应体：%s",
				what, anon.Code, anon.Body.String())
		}
		withToken := reqAs(t, r.method, sh.Host, r.path, r.body, sh.Token)
		if withToken.Code == http.StatusUnauthorized {
			t.Errorf("%s 带上一串真实会话 token 仍然是 401 —— 上面那条断言因此"+
				"证明不了任何事（它可能只是被一句无条件的 401 满足了）。响应体：%s",
				what, withToken.Body.String())
		}
	}
}

// ---------------------------------------------------------------------------
// 产出标志：门店决定了买家看到什么、按什么价看
// ---------------------------------------------------------------------------

// 一条长测试而不是十条短的，理由与 TestMerchantCanPublishAProductAndBuyersSeeIt
// 一字不差：它验的正是**这些步骤连成一条**。分开之后每一步都能各自绿着，
// 而「在城东的人看到城东店的价」这件事没有靶子。
//
// 用到的坐标：城东围栏是 [116.5, 39.9] 附近的一个方块，城西是 [116.1, 39.9]
// 附近的另一个。两块不相交 —— 相交那一种（取最近的一家）由
// TestOverlappingFencesPickTheNearest 单独验。
func TestStoreScopeDecidesVisibilityAndPrice(t *testing.T) {
	sh := newAdminShop(t)

	// ① 一件在售商品 + 一个 SKU，基准价 1000。
	catID := createCategory(t, sh, "门店测试类目")
	productID, skuID := createPublishedSKU(t, sh, catID, "手冲壶", 1000)

	// ② 城东大区 + 城东门店（带围栏）。
	//
	// 建店与画围栏刻意分成两步：契约把围栏留给专门的端点，因为它要过
	// ST_IsValid，而建店时强制会把第一步卡死在第二步上。
	eastRegion := createRegion(t, sh, "east", "城东大区")
	eastStore := createStore(t, sh, eastRegion, "east", "城东店", 116.50, 39.90)
	setFence(t, sh, eastStore, 116.45, 39.85, 116.55, 39.95)

	// ③ 三层定价：大区价 900 盖住基准价 1000，门店价 800 再盖住大区价。
	//
	// 逐层验，而不是只验最内层：只验门店价的话，一个「把 COALESCE 写反、
	// 基准价永远赢」的实现在门店价存在时照样绿。
	var p api.ScopedSkuPrice
	decodeInto(t, putAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/regions/%d/skus/%d/price", eastRegion, skuID),
		`{"price_cents":900}`, sh.Token), http.StatusOK, "设大区价", &p)
	if p.EffectivePriceCents != 900 || p.PriceSource != 2 {
		t.Fatalf("设了大区价之后 effective=%d source=%d，期望 900 / 2（大区价）—— "+
			"source 回答的是「这个价来自哪一层」，错了运营就筛不出被本地覆盖过的行",
			p.EffectivePriceCents, p.PriceSource)
	}
	if p.OverridePriceCents == nil || *p.OverridePriceCents != 900 {
		t.Fatalf("大区价的 override_price_cents 是 %v，期望 900 —— "+
			"它与 effective 刻意分开：后台要同时显示「这一层填了什么」与「最终是多少」",
			p.OverridePriceCents)
	}

	decodeInto(t, putAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/price", eastStore, skuID),
		`{"price_cents":800}`, sh.Token), http.StatusOK, "设门店价", &p)
	if p.EffectivePriceCents != 800 || p.PriceSource != 3 {
		t.Fatalf("设了门店价之后 effective=%d source=%d，期望 800 / 3（门店价）",
			p.EffectivePriceCents, p.PriceSource)
	}

	// ④ 买家站在城东：围栏命中，价按门店价算。
	//
	// **这一步是整条链路的靶子**：resolve 与 /products 是两条独立的读路径，
	// 它们必须解析到同一家店。不一致时买家看到的是「A 店服务你」＋「B 店的价」。
	res := resolveAt(t, sh.Host, 116.50, 39.90)
	if res.MatchType != "fence" {
		t.Fatalf("站在城东围栏里 match_type=%q，期望 fence", res.MatchType)
	}
	if len(res.Stores) != 1 || res.Stores[0].Id != eastStore {
		t.Fatalf("围栏命中 %d 家，期望恰好城东店（id=%d）", len(res.Stores), eastStore)
	}
	if res.Stores[0].DistanceM == nil {
		t.Fatal("围栏命中却没有 distance_m —— 契约把它定成「为 null 当且仅当算不出距离」，" +
			"而这次请求带了坐标、门店也有坐标")
	}
	assertProductPrice(t, sh.Host, eastStore, productID, 800)

	// ⑤ 门店维度下架：这家店不卖它了，别的店不受影响。
	//
	// 两张 overrides 表都是**排除表**，缺一行即在售 —— 所以下架是插一行。
	var l api.ScopedProductListing
	decodeInto(t, putAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/stores/%d/products/%d/listing", eastStore, productID),
		`{"listed":false}`, sh.Token), http.StatusOK, "门店下架", &l)
	if l.Listed || l.EffectiveListed {
		t.Fatalf("门店下架后 listed=%v effective_listed=%v，两个都该是 false",
			l.Listed, l.EffectiveListed)
	}

	// ⑥ 门店重新上架，再从**大区**下架 —— 门店这一层捞不回来。
	//
	// 这是产品那条答复里最容易被实现成「或」的一条：effective_listed 是两层的
	// **与**。写成或的话，后台会显示「已上架」而买家看不到，
	// 而那种不一致没有任何东西会报出来。
	decodeInto(t, putAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/stores/%d/products/%d/listing", eastStore, productID),
		`{"listed":true}`, sh.Token), http.StatusOK, "门店恢复上架", &l)
	if !l.EffectiveListed {
		t.Fatalf("门店恢复上架后 effective_listed 仍是 false：%+v", l)
	}
	decodeInto(t, putAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/regions/%d/products/%d/listing", eastRegion, productID),
		`{"listed":false}`, sh.Token), http.StatusOK, "大区下架", &l)
	decodeInto(t, putAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/stores/%d/products/%d/listing", eastStore, productID),
		`{"listed":true}`, sh.Token), http.StatusOK, "大区排掉之后门店试图捞回来", &l)
	if !l.Listed {
		t.Fatalf("门店这一层设成上架之后 listed=false，它描述的是**本层**有没有排除它：%+v", l)
	}
	if l.EffectiveListed {
		t.Fatal("大区把这件商品排掉了，门店这一层设成上架之后 effective_listed 仍是 true —— " +
			"两层是**与**不是**或**。写成或的后果是后台显示「已上架」而买家看不到，" +
			"而那种不一致没有任何东西会报出来")
	}

	// 收拾：把大区那条排除删掉，免得后面几步都看不见这件商品。
	decodeInto(t, putAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/regions/%d/products/%d/listing", eastRegion, productID),
		`{"listed":true}`, sh.Token), http.StatusOK, "大区恢复上架", &l)

	// ⑦ 撤销门店价，回到大区价；再撤销大区价，回到基准价。
	//
	// 逐层独立：撤销大区价**不动门店价**。这一步顺序刻意是「先撤门店」——
	// 反过来的话，「撤销大区价之后门店价还在不在」就看不出来了。
	wantStatus(t, deleteAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/price", eastStore, skuID), sh.Token),
		http.StatusNoContent, "撤销门店价")
	assertProductPrice(t, sh.Host, eastStore, productID, 900)

	// 再撤一次：本来就没有那一行时**同样返回 204，不是 404**。
	// 调用方的意图是「这家店不要自己的价」，那个意图在两种情况下都已经达成。
	wantStatus(t, deleteAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/price", eastStore, skuID), sh.Token),
		http.StatusNoContent, "重复撤销门店价")

	wantStatus(t, deleteAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/regions/%d/skus/%d/price", eastRegion, skuID), sh.Token),
		http.StatusNoContent, "撤销大区价")
	assertProductPrice(t, sh.Host, eastStore, productID, 1000)

	// ⑧ **204 与 404 必须分得开。** 上面那两次「重复撤销」证明的是
	// 「没有那一行也成功」，而这一次证明的是它没有被做成「什么都成功」——
	// 那条 DELETE 语句本身对「没有覆盖」与「门店根本不存在」返回的东西
	// 一模一样（都是 0 行），分辨只能发生在 service 的存在性检查里。
	// 少了这一段，把那两行检查删掉不会有任何东西红，而症状是
	// 「撤销了一家不存在的门店的价格，返回 204」。
	for _, c := range []struct {
		what, path string
	}{
		{"门店不存在", fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/price", eastStore+999_000, skuID)},
		{"SKU 不存在", fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/price", eastStore, skuID+999_000)},
		{"大区不存在", fmt.Sprintf("/api/v1/admin/regions/%d/skus/%d/price", eastRegion+999_000, skuID)},
	} {
		w := deleteAs(t, sh.Host, c.path, sh.Token)
		if w.Code != http.StatusNotFound {
			t.Errorf("撤销价格时%s，返回 %d，期望 404 —— "+
				"「本来就没有覆盖」才是 204，「这个 id 根本不存在」是 404。响应体：%s",
				c.what, w.Code, w.Body.String())
		}
	}
}

// 不在任何围栏内、以及根本没传坐标，走的是**同一条**回落路径。
//
// 产品对这两个问题给的是同一个答复，所以这里也用同一条测试：两种输入的响应
// 必须逐字段相同。分成两条测试的话，「它们其实走了两条代码路径」这件事
// 不会有任何东西发现 —— 而那正是它们会慢慢分叉的方式。
func TestNoFenceHitAndNoCoordinateTakeTheSamePath(t *testing.T) {
	sh := newAdminShop(t)

	// newAdminShop 已经播了一家默认门店（无围栏、is_default = true）。
	// 再加一家带围栏的非默认店，好让「围栏外」这件事有意义。
	region := createRegion(t, sh, "east", "城东大区")
	east := createStore(t, sh, region, "east", "城东店", 116.50, 39.90)
	setFence(t, sh, east, 116.45, 39.85, 116.55, 39.95)

	outside := resolveAt(t, sh.Host, 100.0, 20.0) // 离围栏几千公里
	none := resolveNoCoord(t, sh.Host)

	for _, c := range []struct {
		what string
		got  api.StoreResolveResult
	}{{"坐标落在围栏外", outside}, {"根本没传坐标", none}} {
		if c.got.MatchType != "fallback_default" {
			t.Errorf("%s：match_type=%q，期望 fallback_default", c.what, c.got.MatchType)
		}
		if len(c.got.Stores) != 1 || !c.got.Stores[0].IsDefault {
			t.Errorf("%s：回落结果不是恰好一家默认门店，而是 %+v", c.what, c.got.Stores)
		}
		if len(c.got.Stores) == 1 && c.got.Stores[0].DistanceM != nil {
			// 回落不是按距离选出来的。给它算一个距离会让客户端以为
			// 「这家店离你 4000 公里但仍然在服务范围内」，而真相是
			// 「你不在任何围栏里，我们给你派了兜底店」。
			t.Errorf("%s：回落那一家带了 distance_m=%v，契约要求它恒为 null",
				c.what, *c.got.Stores[0].DistanceM)
		}
	}
	if outside.MatchType != none.MatchType || len(outside.Stores) != len(none.Stores) {
		t.Fatalf("两种输入的响应不同：围栏外 %+v，没坐标 %+v —— "+
			"产品对这两个问题给的是同一个答复，服务端应当只有一条实现",
			outside, none)
	}
}

// 没配默认门店 → match_type = none，空数组，**HTTP 仍然 200**。
//
// 「不在服务范围」是一个正常的查询结果，不是错误。用 404 表达它会让客户端的
// 错误分支同时装着「网络失败」「鉴权失败」和「这个地方我们不送」，
// 而第三种要渲染的是一个完全不同的页面。
func TestNoDefaultStoreIsOutOfServiceAreaNotAnError(t *testing.T) {
	sh := newAdminShopWithoutDefaultStore(t)

	w := getAs(t, sh.Host, "/api/v1/stores/resolve", "")
	var res api.StoreResolveResult
	decodeInto(t, w, http.StatusOK, "没配默认店时解析门店", &res)

	if res.MatchType != "none" {
		t.Fatalf("没配默认门店时 match_type=%q，期望 none", res.MatchType)
	}
	if res.Stores == nil {
		t.Fatal("stores 是 null，契约要求它是**空数组** —— " +
			"客户端对 null 与 [] 的处置不同，前者常常是一次 crash")
	}
	if len(res.Stores) != 0 {
		t.Fatalf("没配默认门店却回了 %d 家：%+v", len(res.Stores), res.Stores)
	}
}

// 围栏重叠时按距离升序，第一家就是最近的那一家。
//
// 服务端不替客户端挑（挑哪一家涉及配送时效、是否自提、用户上次选过谁），
// 但**「当前门店」取最近的那一家** —— 价格与库存按它算。
// 少了排序的话，两家都命中时「当前门店」取决于 SQL 的行序，
// 而那会随统计信息变化，是一个复现不了的 bug。
func TestOverlappingFencesPickTheNearest(t *testing.T) {
	sh := newAdminShop(t)
	region := createRegion(t, sh, "overlap", "重叠大区")

	// 两块**相交**的围栏：都盖住 [116.50, 39.90]。
	far := createStore(t, sh, region, "far", "远店", 116.54, 39.90)
	setFence(t, sh, far, 116.40, 39.85, 116.60, 39.95)
	near := createStore(t, sh, region, "near", "近店", 116.501, 39.90)
	setFence(t, sh, near, 116.45, 39.86, 116.58, 39.94)

	res := resolveAt(t, sh.Host, 116.50, 39.90)
	if res.MatchType != "fence" {
		t.Fatalf("站在两块围栏的交集里 match_type=%q，期望 fence", res.MatchType)
	}
	if len(res.Stores) != 2 {
		t.Fatalf("两块围栏都盖住这个点，却命中 %d 家：%+v", len(res.Stores), res.Stores)
	}
	if res.Stores[0].Id != near {
		t.Fatalf("第一家是 %d，期望近店 %d —— 契约要求按 distance_m 升序，"+
			"而「当前门店」取的就是第一家", res.Stores[0].Id, near)
	}
	if res.Stores[0].DistanceM == nil || res.Stores[1].DistanceM == nil {
		t.Fatal("围栏命中的门店缺 distance_m")
	}
	if *res.Stores[0].DistanceM > *res.Stores[1].DistanceM {
		t.Fatalf("没有按距离升序：%v, %v", *res.Stores[0].DistanceM, *res.Stores[1].DistanceM)
	}
	if far == 0 {
		t.Fatal("远店没建出来")
	}
}

// 自交的多边形是 422，而且 detail 里要有 PostGIS 那句话。
//
// 那句话（Self-intersection at or near point ...）是运营唯一能拿来定位
// 自己画错在哪儿的东西。丢掉它，422 就只剩「你画的不对」。
func TestSelfIntersectingFenceIsRejectedWithTheReason(t *testing.T) {
	sh := newAdminShop(t)
	region := createRegion(t, sh, "bow", "领结大区")
	store := createStore(t, sh, region, "bow", "领结店", 116.5, 39.9)

	// 领结形：[0,0] → [1,1] → [1,0] → [0,1] → 闭合。四个顶点、环闭合，
	// 但它自己穿过自己 —— ST_IsValid 是唯一能看出这一点的东西。
	w := putAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d/fence", store),
		`{"fence":{"type":"Polygon","coordinates":[[[116.0,39.0],[116.1,39.1],[116.1,39.0],[116.0,39.1],[116.0,39.0]]]}}`,
		sh.Token)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("自交多边形返回 %d，期望 422。响应体：%s", w.Code, w.Body.String())
	}
	var prob api.Problem
	decodeInto(t, w, http.StatusUnprocessableEntity, "自交围栏", &prob)
	if prob.Type != "https://keel.dev/problems/invalid-fence" {
		t.Fatalf("problem type 是 %q，期望 .../invalid-fence", prob.Type)
	}
	// 契约：detail 转述 ST_IsValidReason。第一版这里断言的是 title，
	// 照着一个把原因写进 title 的实现写的，两边一起错、一直是绿的。
	if prob.Detail == nil || !strings.Contains(*prob.Detail, "Self-intersection") {
		got := "<nil>"
		if prob.Detail != nil {
			got = *prob.Detail
		}
		t.Fatalf("detail 是 %q，期望转述 ST_IsValidReason（含 Self-intersection）—— "+
			"那句话是运营唯一能拿来定位自己画错在哪儿的东西", got)
	}
	// title 是这一类问题固定的那句话，不随每次的原因变化（RFC 7807）。
	if strings.Contains(prob.Title, "Self-intersection") {
		t.Errorf("title 里出现了具体原因 %q —— 原因该在 detail 里", prob.Title)
	}
}

// 非默认门店不许清空围栏（409 store-fence-required）。
//
// 一家非默认店没有围栏就是一家永远接不到单的店 —— 后台会把它列出来、
// 能配库存、能上下架、能定价，而它一单也接不到。
func TestClearingFenceOnANonDefaultStoreIsRejected(t *testing.T) {
	sh := newAdminShop(t)
	region := createRegion(t, sh, "fenced", "有围栏大区")
	store := createStore(t, sh, region, "fenced", "有围栏店", 116.5, 39.9)
	setFence(t, sh, store, 116.45, 39.85, 116.55, 39.95)

	w := putAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d/fence", store),
		`{"fence":null}`, sh.Token)
	if w.Code != http.StatusConflict {
		t.Fatalf("给非默认门店清空围栏返回 %d，期望 409。响应体：%s", w.Code, w.Body.String())
	}
	var prob api.Problem
	decodeInto(t, w, http.StatusConflict, "清空非默认店的围栏", &prob)
	if prob.Type != "https://keel.dev/problems/store-fence-required" {
		t.Fatalf("problem type 是 %q，期望 .../store-fence-required", prob.Type)
	}
}

// 建店时带 is_default 而已经有一家默认店 → 409 default-store-conflict，
// 而切换默认店那条端点在同一个事务里先清旧再置新，所以它成功。
//
// 两件事放在一条测试里，因为它们是**同一条纪律的两面**：让 POST 顺手抢过
// 默认位，等于给「建一家店」附带一个谁也没预料到的副作用。
func TestDefaultStoreIsSwitchedOnlyByItsOwnEndpoint(t *testing.T) {
	sh := newAdminShop(t) // 已有一家默认门店
	region := createRegion(t, sh, "second", "第二大区")

	w := post(t, sh.Host, "/api/v1/admin/stores",
		fmt.Sprintf(`{"region_id":%d,"code":"grab","name":"抢默认位的店","is_default":true,"lng":116.4,"lat":39.9}`, region),
		sh.Token)
	if w.Code != http.StatusConflict {
		t.Fatalf("已经有默认店时再建一家 is_default=true 返回 %d，期望 409。响应体：%s",
			w.Code, w.Body.String())
	}
	var prob api.Problem
	decodeInto(t, w, http.StatusConflict, "抢默认位", &prob)
	if prob.Type != "https://keel.dev/problems/default-store-conflict" {
		t.Fatalf("problem type 是 %q，期望 .../default-store-conflict", prob.Type)
	}

	// 正经的切换：先清旧再置新，在同一个事务里。
	// 反过来（先置新）会撞 uk_stores_default 那条部分唯一索引。
	newStore := createStore(t, sh, region, "second", "第二家店", 116.2, 39.9)
	setFence(t, sh, newStore, 116.15, 39.85, 116.25, 39.95)
	var st api.AdminStore
	decodeInto(t, putAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/stores/%d/default", newStore), "", sh.Token),
		http.StatusOK, "切换默认店", &st)
	if !st.IsDefault {
		t.Fatal("切换之后这家店的 is_default 仍是 false")
	}

	// 旧的那家不再是默认 —— 「至多一家」由 uk_stores_default 保证，
	// 而这里要证明的是**清旧那一步真的跑了**：没跑的话上面那条 PUT
	// 会以 23505 失败，但如果实现改成「先删索引再插」之类的花招，
	// 两家都会是默认，而列表的 has_default 照样是 true。
	var list api.AdminStoreList
	decodeInto(t, getAs(t, sh.Host, "/api/v1/admin/stores", sh.Token),
		http.StatusOK, "列门店", &list)
	if !list.HasDefault {
		t.Fatal("切换之后 has_default 是 false")
	}
	defaults := 0
	for _, s := range list.Items {
		if s.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		t.Fatalf("有 %d 家默认门店，每个商家至多一家（uk_stores_default）", defaults)
	}
}

// 名下还有门店的大区删不掉（409 region-has-stores），**不做级联**。
//
// 级联软删一个大区会连带让它下面所有门店接不到单，而调用方在点下删除时
// 看到的只是一个大区名。
func TestRegionWithStoresCannotBeDeleted(t *testing.T) {
	sh := newAdminShop(t)
	region := createRegion(t, sh, "busy", "有店的大区")
	store := createStore(t, sh, region, "busy", "有店", 116.5, 39.9)
	setFence(t, sh, store, 116.45, 39.85, 116.55, 39.95)

	w := deleteAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/regions/%d", region), sh.Token)
	if w.Code != http.StatusConflict {
		t.Fatalf("删一个名下还有门店的大区返回 %d，期望 409。响应体：%s", w.Code, w.Body.String())
	}
	var prob api.Problem
	decodeInto(t, w, http.StatusConflict, "删有店的大区", &prob)
	if prob.Type != "https://keel.dev/problems/region-has-stores" {
		t.Fatalf("problem type 是 %q，期望 .../region-has-stores", prob.Type)
	}

	// 把门店删掉之后就删得动了 —— 阳性对照：少了它，一个「DELETE 永远 409」
	// 的实现也能让上面那段全绿。
	wantStatus(t, deleteAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d", store), sh.Token),
		http.StatusNoContent, "软删门店")
	wantStatus(t, deleteAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/regions/%d", region), sh.Token),
		http.StatusNoContent, "门店删掉之后再删大区")
}

// 门店数不是 1 时，那条不带门店的库存路径显式报 409 store-ambiguous，
// **不猜一家**。
//
// 库存是唯一真相，猜错一家店的后果是把另一家店的水位覆盖掉，
// 而且没有任何东西会响。
func TestInventoryWithoutStoreIsAmbiguousWhenThereAreTwoStores(t *testing.T) {
	sh := newAdminShop(t)
	catID := createCategory(t, sh, "库存测试类目")
	_, skuID := createPublishedSKU(t, sh, catID, "库存测试品", 1000)

	// 只有一家店时它可用 —— 阳性对照放在前面，因为后面会把它破坏掉。
	var inv api.AdminInventory
	// expected 是 50：createPublishedSKU 建 SKU 时带了 available_qty=50，
	// 而 CreateSKU 在同事务里就把那一行写进了默认门店。写 0 的话这里会拿到
	// 一个 409，而那个 409 是对的 —— CAS 的全部意义就是不让人按一个过期的
	// 读数去覆盖。
	decodeInto(t, putAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/skus/%d/inventory", skuID),
		`{"expected_available_qty":50,"available_qty":7}`, sh.Token),
		http.StatusOK, "单店商家改库存", &inv)
	if inv.StoreId != sh.StoreID {
		t.Fatalf("响应里的 store_id 是 %d，期望那家唯一的门店 %d —— "+
			"契约把它定成必返，正是为了让调用方知道自己刚改的是哪一家",
			inv.StoreId, sh.StoreID)
	}
	if inv.AvailableQty != 7 {
		t.Fatalf("改完水位是 %d，期望 7", inv.AvailableQty)
	}

	// 开第二家店，同一条路径就没有唯一答案了。
	region := createRegion(t, sh, "amb", "第二大区")
	second := createStore(t, sh, region, "amb", "第二家店", 116.2, 39.9)
	setFence(t, sh, second, 116.15, 39.85, 116.25, 39.95)

	w := putAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/skus/%d/inventory", skuID),
		`{"expected_available_qty":7,"available_qty":9}`, sh.Token)
	if w.Code != http.StatusConflict {
		t.Fatalf("两家门店时那条不带门店的库存路径返回 %d，期望 409。响应体：%s",
			w.Code, w.Body.String())
	}
	var prob api.Problem
	decodeInto(t, w, http.StatusConflict, "两家店时改库存", &prob)
	if prob.Type != "https://keel.dev/problems/store-ambiguous" {
		t.Fatalf("problem type 是 %q，期望 .../store-ambiguous —— "+
			"它与 inventory-precondition-failed 共用 409，"+
			"客户端只能靠 type 分辨「刷新重试」与「换一条路径」", prob.Type)
	}

	// 带门店那条仍然可用，而且**只改那一家**。
	decodeInto(t, putAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/inventory", second, skuID),
		`{"expected_available_qty":0,"available_qty":3}`, sh.Token),
		http.StatusOK, "按门店改库存", &inv)
	if inv.StoreId != second || inv.AvailableQty != 3 {
		t.Fatalf("按门店改完得到 store=%d qty=%d，期望 %d / 3", inv.StoreId, inv.AvailableQty, second)
	}
	// 第一家没被动过。CAS 的 WHERE 里漏掉 store_id 的话，上面那一次会把
	// **所有**门店的这一行都改成 3 —— 而那条 UPDATE 照样返回成功。
	var page struct {
		Items []api.AdminInventory `json:"items"`
	}
	decodeInto(t, getAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/stores/%d/inventories", sh.StoreID), sh.Token),
		http.StatusOK, "列第一家店的库存", &page)
	found := false
	for _, in := range page.Items {
		if in.SkuId != skuID {
			continue
		}
		found = true
		if in.AvailableQty != 7 {
			t.Fatalf("改第二家店的库存把第一家也改了：第一家现在是 %d，期望还是 7 —— "+
				"CAS 的 WHERE 里是不是漏了 store_id？", in.AvailableQty)
		}
	}
	if !found {
		t.Fatalf("第一家店的库存清单里找不到 sku %d —— 缺行要显示成 0，不能漏掉", skuID)
	}
}

// 门店必须有坐标；有围栏时门店必须在围栏内（2026-09-27）。
//
// 四条路径各一刀：建店缺坐标 → 422；配一个不盖住门店的围栏 → 422 store-outside-fence
// 且围栏没被写进去；把坐标挪出已有围栏 → 422 且坐标没变；没有坐标的老门店配围栏 →
// 422 store-location-required。外加阳性对照：点正好落在围栏边上算在内。
func TestStoreMustHaveLocationInsideItsFence(t *testing.T) {
	sh := newAdminShop(t)
	region := createRegion(t, sh, "geo", "定位大区")

	w := post(t, sh.Host, "/api/v1/admin/stores",
		fmt.Sprintf(`{"region_id":%d,"code":"noloc-%s","name":"没坐标的店"}`, region, sh.Suffix), sh.Token)
	if p := problemOf(t, w, http.StatusUnprocessableEntity); p.Title == "" {
		t.Fatalf("建店缺坐标应 422：%+v", p)
	}

	store := createStore(t, sh, region, "geo", "定位店", 116.40, 39.90)
	fencePath := fmt.Sprintf("/api/v1/admin/stores/%d/fence", store)
	box := func(lng0, lat0, lng1, lat1 float64) string {
		return fmt.Sprintf(`{"fence":{"type":"Polygon","coordinates":[[[%v,%v],[%v,%v],[%v,%v],[%v,%v],[%v,%v]]]}}`,
			lng0, lat0, lng1, lat0, lng1, lat1, lng0, lat1, lng0, lat0)
	}

	// 围栏不盖住门店：拒，而且围栏没写进去（事务回滚）。
	p := problemOf(t, putAs(t, sh.Host, fencePath, box(117.0, 39.0, 117.5, 39.5), sh.Token), http.StatusUnprocessableEntity)
	if p.Type != problem.TypeStoreOutsideFence {
		t.Fatalf("门店在围栏外应 store-outside-fence，实得 %s", p.Type)
	}
	var got api.AdminStore
	decodeInto(t, getAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d", store), sh.Token), http.StatusOK, "读门店", &got)
	if got.Fence != nil {
		t.Fatal("被拒的围栏被写进去了 —— 判定在写之后，但事务没有回滚")
	}

	// 点正好在围栏边上（经度 116.40 是左边界）：算在内。
	setFence(t, sh, store, 116.40, 39.80, 116.60, 40.00)

	// 把坐标挪出围栏：拒，坐标不变。
	w = patchAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d", store), `{"lng":121.47,"lat":31.23}`, sh.Token)
	if p := problemOf(t, w, http.StatusUnprocessableEntity); p.Type != problem.TypeStoreOutsideFence {
		t.Fatalf("坐标挪出围栏应 store-outside-fence，实得 %s", p.Type)
	}
	decodeInto(t, getAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d", store), sh.Token), http.StatusOK, "读门店", &got)
	if got.Lng == nil || *got.Lng < 116.39 || *got.Lng > 116.41 {
		t.Fatalf("被拒的改坐标生效了：lng=%v", got.Lng)
	}
	// 挪到东西向的南边线中点上：算在内（平面判定；按 geography 的大圆弧，这个点在外约 2 米，2026-10-01）。
	wantStatus(t, patchAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d", store), `{"lng":116.5,"lat":39.8}`, sh.Token),
		http.StatusOK, "门店坐标在南边线上")
	// 围栏内挪动：放行。
	wantStatus(t, patchAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d", store), `{"lng":116.5,"lat":39.9}`, sh.Token),
		http.StatusOK, "围栏内改坐标")
	// 不碰坐标的改动不查围栏。
	wantStatus(t, patchAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d", store), `{"phone":"010-1"}`, sh.Token),
		http.StatusOK, "改电话")

	// 没有坐标的老门店（夹具里的默认门店就是：迁移回填 / 早期种子那一类）配围栏：先要选点。
	p = problemOf(t, putAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d/fence", sh.StoreID),
		box(70, 15, 140, 55), sh.Token), http.StatusUnprocessableEntity)
	if p.Type != problem.TypeStoreLocationRequired {
		t.Fatalf("没坐标的店配围栏应 store-location-required，实得 %s", p.Type)
	}
}

// 坐标精度（2026-09-30）：契约里的经纬度原来是裸 number，生成 float32，后台保存门店坐标与围栏时
// JSON 先解析进 float32 再存库，116.30 存成 116.30000305 —— 压在手画边线上的点会被判到围栏外。
// 契约改成 format: double 之后，写进去什么、读出来就是什么，顶点本身也判在围栏内。
func TestStoreCoordinatesAndFenceKeepFullPrecision(t *testing.T) {
	sh := newAdminShop(t)
	region := createRegion(t, sh, "prec", "精度大区")
	const lng, lat = 114.0579831, 22.5430967
	store := createStore(t, sh, region, "prec", "精度店", lng, lat)
	body := `{"fence":{"type":"Polygon","coordinates":[[[114.0512345,22.5401234],[114.0654321,22.5401234],` +
		`[114.0654321,22.5498765],[114.0512345,22.5498765],[114.0512345,22.5401234]]]}}`
	if w := putAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d/fence", store), body, sh.Token); w.Code != http.StatusOK {
		t.Fatalf("存围栏 %d：%s", w.Code, w.Body.String())
	}
	var got api.AdminStore
	decodeInto(t, getAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d", store), sh.Token), http.StatusOK, "门店详情", &got)
	if got.Lat == nil || got.Lng == nil || *got.Lat != lat || *got.Lng != lng {
		t.Fatalf("门店坐标读回来是 %v,%v，期望原样 %v,%v", got.Lat, got.Lng, lat, lng)
	}
	if got.Fence == nil || got.Fence.Coordinates[0][0][0] != 114.0512345 || got.Fence.Coordinates[0][2][1] != 22.5498765 {
		t.Fatalf("围栏顶点读回来掉了精度：%v", got.Fence)
	}
	// 顶点（边界上）按下单同一条判据算在围栏内。
	var served bool
	if err := admin(t).QueryRow(context.Background(), `SELECT ST_Intersects(fence::geometry, ST_SetSRID(ST_MakePoint(114.0654321, 22.5498765), 4326))
		FROM stores WHERE id = $1`, store).Scan(&served); err != nil {
		t.Fatal(err)
	}
	if !served {
		t.Fatal("手画的顶点被判到了围栏外：坐标在写入路径上掉了精度")
	}
}

// 围栏读回逐位相等（2026-10-01，破坏性测试 P2）：ST_AsGeoJSON 默认只出 9 位小数，后台「改一个顶点
// 再整体 PUT」会让所有顶点每存一次漂一次。随机取满精度的双精度坐标（含 0.30000000000000004、
// 1e-8 附近这些最长的十进制串），写进去、读出来，一位都不许差。
func TestFenceRoundTripsBitExact(t *testing.T) {
	sh := newAdminShop(t)
	region := createRegion(t, sh, "bits", "精度大区")
	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewPCG(uint64(seed), 0))
	t.Logf("随机种子 %d", seed)
	cases := []struct {
		name                   string
		lng0, lat0, lng1, lat1 float64
	}{
		{"北京一带随机", 116 + rng.Float64()*0.1, 39 + rng.Float64()*0.1, 116.2 + rng.Float64()*0.1, 39.2 + rng.Float64()*0.1},
		{"西南半球随机", -70 - rng.Float64(), -33 - rng.Float64(), -68 - rng.Float64(), -31 - rng.Float64()},
		{"最长的十进制串", 1.2345678901234567e-8, 1.0000000000000002e-8, 0.30000000000000004, 1.2345678901234567e-6 + 0.2},
		{"贴着 0 随机", rng.Float64() * 1e-7, rng.Float64() * 1e-9, 0.1 + rng.Float64()*0.01, 0.1 + rng.Float64()*1e-6},
	}
	for i, c := range cases {
		store := createStore(t, sh, region, fmt.Sprintf("bits%d", i), c.name, (c.lng0+c.lng1)/2, (c.lat0+c.lat1)/2)
		want := [][]float64{{c.lng0, c.lat0}, {c.lng1, c.lat0}, {c.lng1, c.lat1}, {c.lng0, c.lat1}, {c.lng0, c.lat0}}
		ring, _ := json.Marshal(want)
		body := fmt.Sprintf(`{"fence":{"type":"Polygon","coordinates":[%s]}}`, ring)
		var put, got api.AdminStore
		decodeInto(t, putAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d/fence", store), body, sh.Token),
			http.StatusOK, "存围栏", &put)
		decodeInto(t, getAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d", store), sh.Token),
			http.StatusOK, "读门店", &got)
		for what, s := range map[string]api.AdminStore{"PUT 的回显": put, "GET 详情": got} {
			if s.Fence == nil || len(s.Fence.Coordinates) != 1 || len(s.Fence.Coordinates[0]) != len(want) {
				t.Fatalf("%s · %s：围栏形状不对：%+v", c.name, what, s.Fence)
			}
			for j, pt := range s.Fence.Coordinates[0] {
				for k := range 2 {
					if math.Float64bits(pt[k]) != math.Float64bits(want[j][k]) {
						t.Errorf("%s · %s：顶点 %d 第 %d 维读回 %v，写入的是 %v", c.name, what, j, k,
							strconv.FormatFloat(pt[k], 'g', -1, 64), strconv.FormatFloat(want[j][k], 'g', -1, 64))
					}
				}
			}
		}
	}
}
