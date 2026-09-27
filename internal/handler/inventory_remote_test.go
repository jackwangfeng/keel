package handler_test

// 库存服务的跨进程形态（微服务拆分阶段 1a）。
//
// 单体与拆分走的是同一条业务代码路径（inventory 包的文件头），全量测试在单体形态上覆盖了
// 业务逻辑。这一组只验**传输层**：把公网路由接到一个指向 httptest 库存服务的 HTTP 实现上
// （core 的形状），同一个请求在两种形态下必须给出**逐字节相同**的响应，库里留下的也一样；
// 库存服务不在时，读页面按契约降级、写与加购回 503 且一点副作用都没有。
//
// 1a 用同一个测试库：库存服务的 httptest 进程与 core 连的是同一个库（两库形态的集成测试是
// 阶段 1b 的验收项，那时下单扣减也跨过去了）。对这组测试而言这不是捷径 —— 要验的是
// 「数据从 HTTP 那一头过来之后，core 合并出来的东西与进程内调用完全一样」。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/rpc"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

const remoteInventorySecret = "0123456789abcdef0123456789abcdef-inventory-test"

// startInventoryServer 起一个只挂库存接口的内网服务（验签 + 租户头，与 KEEL_ROLE=inventory
// 的 internalRouter 同一个装法），背后是进程内实现。
func startInventoryServer(t *testing.T) *httptest.Server {
	t.Helper()
	r, routes := rpc.NewRouter(rpc.ServerConfig{Secret: remoteInventorySecret})
	inventory.Mount(routes.Tenant, localInventory())
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// remoteEngine 装一套公网路由，库存走 HTTP 实现，指向 baseURL —— KEEL_ROLE=core 的形状。
// 除了库存之外的一切（池、协调器、签名器、推理替身）与包级的 testEngine 是同一批实例。
func remoteEngine(t *testing.T, baseURL string) *gin.Engine {
	t.Helper()
	c, err := rpc.NewClient(baseURL, remoteInventorySecret, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return app.Router(testPool,
		tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}), testSigner, testOrders,
		service.PaymentConfig{Sandbox: true}, conceptEmbedder{}, app.WithInventory(inventory.NewRemote(c)))
}

// serve 在指定的路由上打一个请求。
func serve(e *gin.Engine, method, host, path, body, bearer, idemKey string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Host = host
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if idemKey != "" {
		r.Header.Set("Idempotency-Key", idemKey)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, r)
	return w
}

// sameOnBoth 在单体路由与 core 路由上各打一次同一个只读请求，要求状态码与响应体逐字节相同。
func sameOnBoth(t *testing.T, remote *gin.Engine, what, host, path, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	l := serve(testEngine, http.MethodGet, host, path, "", bearer, "")
	r := serve(remote, http.MethodGet, host, path, "", bearer, "")
	if l.Code != http.StatusOK {
		t.Fatalf("%s：单体形态得到 %d（这条对照的前提不成立）：%s", what, l.Code, l.Body.String())
	}
	if r.Code != l.Code || r.Body.String() != l.Body.String() {
		t.Fatalf("%s：core 形态与单体形态不一致\n单体 %d：%s\ncore %d：%s",
			what, l.Code, l.Body.String(), r.Code, r.Body.String())
	}
	return l
}

// 读：商品详情的 SKU 水位、购物车行水位、后台商品 / SKU / 门店库存页、库存预警，
// 两种形态逐字节相同。写：按门店的 CAS 与相对调整经 HTTP 走完，冲突带当前值、重放不加两遍。
func TestRemoteInventoryBehavesLikeInProcess(t *testing.T) {
	cs := newBuyerShop(t)
	buyer := cs.newBuyer(t, "remote")
	remote := remoteEngine(t, startInventoryServer(t).URL)

	// —— 写（经 HTTP）：按门店的 CAS。先造一个低于预警线的状态，下面的预警报表要用。
	cas := fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/inventory", cs.NorthStore, cs.ShirtSKU)
	w := serve(remote, http.MethodPut, cs.Host, cas, `{"expected_available_qty":49,"available_qty":3}`, cs.Token, "")
	if got := problemType(t, w, http.StatusConflict, "core 形态 CAS 前提不成立"); got != problem.TypeInventoryPrecondition {
		t.Fatalf("CAS 前提不成立的 type 是 %q，期望 %q", got, problem.TypeInventoryPrecondition)
	}
	var conflict api.InventoryConflict
	decodeInto(t, w, http.StatusConflict, "CAS 冲突体", &conflict)
	if conflict.Current.AvailableQty != 50 || conflict.Current.StoreId != cs.NorthStore {
		t.Fatalf("经 HTTP 回来的 current 是 %+v，期望北京门店 50", conflict.Current)
	}
	var inv api.AdminInventory
	decodeInto(t, serve(remote, http.MethodPut, cs.Host, cas,
		`{"expected_available_qty":50,"available_qty":3,"warning_qty":5}`, cs.Token, ""),
		http.StatusOK, "core 形态 CAS", &inv)
	if inv.AvailableQty != 3 || inv.WarningQty != 5 {
		t.Fatalf("CAS 之后是 %+v，期望 3 / 5", inv)
	}
	if q, _ := storeQtyInDB(t, cs.NorthStore, cs.ShirtSKU); q != 3 {
		t.Fatalf("CAS 之后库里是 %d，期望 3", q)
	}

	// —— 写：相对调整，同一把钥匙重放不加两遍、流水只有一行。
	adj := adjustPath(cs.NorthStore, cs.DressSKU)
	key := freshIdemKey()
	decodeInto(t, serve(remote, http.MethodPost, cs.Host, adj, `{"delta":-10,"reason":"盘亏"}`, cs.Token, key),
		http.StatusOK, "core 形态相对调整", &inv)
	if inv.AvailableQty != 40 {
		t.Fatalf("调整之后是 %d，期望 40", inv.AvailableQty)
	}
	again := serve(remote, http.MethodPost, cs.Host, adj, `{"delta":-10,"reason":"盘亏"}`, cs.Token, key)
	wantStatus(t, again, http.StatusOK, "core 形态相对调整的重放")
	if again.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("同一把钥匙的第二次没有标成重放")
	}
	if q, _ := storeQtyInDB(t, cs.NorthStore, cs.DressSKU); q != 40 {
		t.Fatalf("重放之后库里是 %d，期望还是 40 —— 加了两遍", q)
	}
	if n := adjLogCount(t, cs.NorthStore, cs.DressSKU); n != 1 {
		t.Fatalf("相对调整的流水有 %d 行，期望 1 行", n)
	}
	// 扣过头：409 inventory-insufficient，current 经 HTTP 带回来。
	w = serve(remote, http.MethodPost, cs.Host, adj, `{"delta":-41}`, cs.Token, freshIdemKey())
	if got := problemType(t, w, http.StatusConflict, "core 形态扣过头"); got != problem.TypeInventoryInsufficient {
		t.Fatalf("扣过头的 type 是 %q，期望 %q", got, problem.TypeInventoryInsufficient)
	}
	decodeInto(t, w, http.StatusConflict, "扣过头的冲突体", &conflict)
	if conflict.Current.AvailableQty != 40 {
		t.Fatalf("扣过头的 current 是 %d，期望 40", conflict.Current.AvailableQty)
	}

	// —— 读：两种形态逐字节相同。
	sameOnBoth(t, remote, "商品详情（SKU 水位）", cs.Host,
		fmt.Sprintf("/api/v1/products/%d?store_id=%d", cs.DressProduct, cs.NorthStore), "")
	var detail api.ProductDetail
	decodeInto(t, sameOnBoth(t, remote, "商品详情（另一家店）", cs.Host,
		fmt.Sprintf("/api/v1/products/%d?store_id=%d", cs.ShirtProduct, cs.NorthStore), ""),
		http.StatusOK, "详情", &detail)
	if len(detail.Skus) != 1 || detail.Skus[0].AvailableQty != 3 {
		t.Fatalf("详情里的水位是 %+v，期望 3（刚经 HTTP 设的值）", detail.Skus)
	}

	// 购物车：经 core 形态加购（加购的库存判断走 HTTP），两种形态读出来的车逐字节相同。
	add := func(sku int64, qty int) *httptest.ResponseRecorder {
		return serve(remote, http.MethodPost, cs.Host, cartPath("/api/v1/cart/items", cs.NorthStore),
			fmt.Sprintf(`{"sku_id":%d,"quantity":%d}`, sku, qty), buyer.Token, freshIdemKey())
	}
	wantStatus(t, add(cs.ShirtSKU, 2), http.StatusOK, "core 形态加购衬衫")
	wantStatus(t, add(cs.DressSKU, 1), http.StatusOK, "core 形态加购连衣裙")
	w = add(cs.ShirtSKU, 2) // 车里 2 + 2 > 3
	if got := problemType(t, w, http.StatusConflict, "core 形态加购超过库存"); got != problem.TypeInsufficientStock {
		t.Fatalf("加购超过库存的 type 是 %q，期望 %q", got, problem.TypeInsufficientStock)
	}
	var cart api.Cart
	decodeInto(t, sameOnBoth(t, remote, "购物车", cs.Host, cartPath("/api/v1/cart", cs.NorthStore), buyer.Token),
		http.StatusOK, "购物车", &cart)
	if len(cart.Items) != 2 {
		t.Fatalf("车里有 %d 行，期望 2", len(cart.Items))
	}

	// 后台：商品详情（total_stock 与每个 SKU 的跨门店合计）、门店库存清单（含 low_stock_only）、库存预警。
	sameOnBoth(t, remote, "后台商品详情", cs.Host, fmt.Sprintf("/api/v1/admin/products/%d", cs.ShirtProduct), cs.Token)
	sameOnBoth(t, remote, "后台商品列表", cs.Host, "/api/v1/admin/products?page_size=50", cs.Token)
	sameOnBoth(t, remote, "门店库存清单", cs.Host,
		fmt.Sprintf("/api/v1/admin/stores/%d/inventories", cs.NorthStore), cs.Token)
	var low struct {
		Total int                  `json:"total"`
		Items []api.AdminInventory `json:"items"`
	}
	decodeInto(t, sameOnBoth(t, remote, "门店库存清单（只看低库存）", cs.Host,
		fmt.Sprintf("/api/v1/admin/stores/%d/inventories?low_stock_only=true", cs.NorthStore), cs.Token),
		http.StatusOK, "低库存清单", &low)
	if low.Total != 1 || len(low.Items) != 1 || low.Items[0].SkuId != cs.ShirtSKU {
		t.Fatalf("北京门店的低库存清单是 %+v，期望只有衬衫那一个（3 ≤ 5）", low)
	}
	var alerts api.ReportInventoryAlerts
	decodeInto(t, sameOnBoth(t, remote, "库存预警", cs.Host, "/api/v1/admin/reports/inventory-alerts", cs.Token),
		http.StatusOK, "库存预警", &alerts)
	if alerts.Total != 1 || len(alerts.Items) != 1 || alerts.Items[0].SkuId != cs.ShirtSKU ||
		alerts.Items[0].StoreName == "" || alerts.Items[0].ProductTitle == "" {
		t.Fatalf("库存预警是 %+v，期望一行（北京门店的衬衫），而且名字由 core 补上了", alerts)
	}
}

// 单店捷径那两条（PUT / POST /admin/skus/{id}/inventory…）经 HTTP：门店由 core 解析，
// 缺行 404（AllowInsert = false），写成功与单体形态一致。
func TestRemoteInventorySoleStoreShortcut(t *testing.T) {
	sh := newAdminShop(t)
	sku := seedOneSKU(t, sh, "RMTSOLE", 1000, 8)
	remote := remoteEngine(t, startInventoryServer(t).URL)

	var inv api.AdminInventory
	decodeInto(t, serve(remote, http.MethodPut, sh.Host, fmt.Sprintf("/api/v1/admin/skus/%d/inventory", sku),
		`{"expected_available_qty":8,"available_qty":20}`, sh.Token, ""), http.StatusOK, "单店捷径 CAS", &inv)
	if inv.AvailableQty != 20 || inv.StoreId != sh.StoreID {
		t.Fatalf("单店捷径 CAS 之后是 %+v，期望门店 %d、20", inv, sh.StoreID)
	}
	decodeInto(t, serve(remote, http.MethodPost, sh.Host, fmt.Sprintf("/api/v1/admin/skus/%d/inventory/adjustments", sku),
		`{"delta":5}`, sh.Token, freshIdemKey()), http.StatusOK, "单店捷径相对调整", &inv)
	if inv.AvailableQty != 25 {
		t.Fatalf("单店捷径调整之后是 %d，期望 25", inv.AvailableQty)
	}
	// 缺行的 SKU（建 SKU 时没有默认门店之外的店，这里直接删掉那一行来造）→ 404。
	adminExec(t, `DELETE FROM inventories WHERE sku_id = $1`, sku)
	w := serve(remote, http.MethodPut, sh.Host, fmt.Sprintf("/api/v1/admin/skus/%d/inventory", sku),
		`{"expected_available_qty":0,"available_qty":1}`, sh.Token, "")
	wantStatus(t, w, http.StatusNotFound, "单店捷径缺行")
}

// 检索的 in_stock 与 in_stock_only：core 形态下由「召回 → 批量问库存 → 在 Go 里算 / 过滤」得出，
// 两种形态给出同一批结果、同一个 in_stock。
func TestRemoteInventorySearchInStockMatchesInProcess(t *testing.T) {
	fx := newSearchFixture(t)
	remote := remoteEngine(t, startInventoryServer(t).URL)
	for _, body := range []string{
		`{"query":"连衣裙"}`,
		`{"query":"连衣裙","filters":{"in_stock_only":true}}`,
	} {
		_, l := searchOn(t, testEngine, fx.HostA, body)
		_, r := searchOn(t, remote, fx.HostA, body)
		if len(l.Items) == 0 {
			t.Fatalf("%s：单体形态一条都没搜到，这条对照的前提不成立", body)
		}
		if len(l.Items) != len(r.Items) {
			t.Fatalf("%s：单体 %d 条、core %d 条", body, len(l.Items), len(r.Items))
		}
		for i := range l.Items {
			if l.Items[i]["id"] != r.Items[i]["id"] || l.Items[i]["in_stock"] != r.Items[i]["in_stock"] {
				t.Fatalf("%s：第 %d 条不一致：单体 %v / %v，core %v / %v", body, i,
					l.Items[i]["id"], l.Items[i]["in_stock"], r.Items[i]["id"], r.Items[i]["in_stock"])
			}
			if _, ok := r.Items[i]["in_stock"]; !ok {
				t.Fatalf("%s：库存服务好着，core 形态的结果里却没有 in_stock", body)
			}
		}
	}
}

// 相对调整在「库存服务已经生效、回包丢了」之后重试：同一把 Idempotency-Key（也就是同一个
// biz_id）不会再加一遍，而且这一次把存档补上。这是拆分之后那三段协议里最要紧的一个断点
// （service/inventory_admin.go 的 adjustInventory）。
func TestAdjustRetriedAfterLostResponseAppliesOnce(t *testing.T) {
	sh := newAdminShop(t)
	sku := seedOneSKU(t, sh, "RMTLOST", 1000, 10)
	remote := remoteEngine(t, startInventoryServer(t).URL)

	key := freshIdemKey()
	// 模拟「第一次调用已经在库存服务那一侧生效、core 没收到回包」：直接用同一个 biz_id 调一次。
	ctx := tenant.NewContext(context.Background(), sh.MerchantID)
	if _, err := localInventory().Adjust(ctx, inventory.AdjustRequest{
		SKUID: sku, StoreID: sh.StoreID, Delta: 7, BizID: fmt.Sprintf("adj:%d:%s", sh.StaffID, key),
	}); err != nil {
		t.Fatal(err)
	}
	var inv api.AdminInventory
	decodeInto(t, serve(remote, http.MethodPost, sh.Host, adjustPath(sh.StoreID, sku), `{"delta":7}`, sh.Token, key),
		http.StatusOK, "结果未知之后的重试", &inv)
	if inv.AvailableQty != 17 {
		t.Fatalf("重试回的水位是 %d，期望 17", inv.AvailableQty)
	}
	if q, _ := storeQtyInDB(t, sh.StoreID, sku); q != 17 {
		t.Fatalf("重试之后库里是 %d，期望 17 —— 同一次调整生效了两遍", q)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM idempotency_keys WHERE merchant_id = $1 AND idem_key = $2 AND status = 1`,
		sh.MerchantID, key); n != 1 {
		t.Fatalf("重试之后幂等存档有 %d 行，期望 1 行（这一次把存档补上）", n)
	}
	// 同一把钥匙换一个数量：库存服务发现 biz_id 上一次是别的调整 → 422，什么都不改。
	w := serve(remote, http.MethodPost, sh.Host, adjustPath(sh.StoreID, sku), `{"delta":8}`, sh.Token, key)
	if got := problemType(t, w, http.StatusUnprocessableEntity, "同一把钥匙换了数量"); got != problem.TypeIdempotencyKeyReused {
		t.Fatalf("同一把钥匙换了数量的 type 是 %q，期望 %q", got, problem.TypeIdempotencyKeyReused)
	}
	if q, _ := storeQtyInDB(t, sh.StoreID, sku); q != 17 {
		t.Fatalf("被拒的请求改了库存：%d", q)
	}
}

// 库存服务不在：浏览页照常（列表、检索 —— in_stock 缺席），取决于水位的页面与所有写回 503
// inventory-unavailable，而且写一点副作用都没有（库存、流水、幂等存档都不动）。
func TestInventoryDownDegradesBrowseAndRefusesStockCriticalCalls(t *testing.T) {
	cs := newBuyerShop(t)
	buyer := cs.newBuyer(t, "down")
	fx := newSearchFixture(t)
	dead := startInventoryServer(t)
	dead.Close() // 地址是真的，只是没人听了 —— 与库存容器被杀掉同一个症状
	down := remoteEngine(t, dead.URL)

	// 浏览：商品列表不读库存（契约里的列表本来就没有 in_stock），照常 200。
	wantStatus(t, serve(down, http.MethodGet, cs.Host, fmt.Sprintf("/api/v1/products?store_id=%d", cs.NorthStore), "", "", ""),
		http.StatusOK, "库存服务不在时的商品列表")
	// 检索照常返回，in_stock 缺席（不编 true / false）。
	w, res := searchOn(t, down, fx.HostA, `{"query":"连衣裙","filters":{"in_stock_only":true}}`)
	wantStatus(t, w, http.StatusOK, "库存服务不在时的检索")
	if len(res.Items) == 0 {
		t.Fatal("库存服务不在时检索一条都没有 —— 应当照常返回（只是不知道有没有货）")
	}
	for _, it := range res.Items {
		if _, ok := it["in_stock"]; ok {
			t.Fatalf("库存服务不在时检索结果里有 in_stock=%v —— 那是编出来的", it["in_stock"])
		}
	}

	unavailable := func(w *httptest.ResponseRecorder, what string) {
		t.Helper()
		if got := problemType(t, w, http.StatusServiceUnavailable, what); got != problem.TypeInventoryUnavailable {
			t.Fatalf("%s 的 type 是 %q，期望 %q", what, got, problem.TypeInventoryUnavailable)
		}
		if w.Header().Get("Retry-After") == "" {
			t.Errorf("%s 没有 Retry-After", what)
		}
	}
	// 详情：available_qty 是必填，编不出来 → 503。
	unavailable(serve(down, http.MethodGet, cs.Host,
		fmt.Sprintf("/api/v1/products/%d?store_id=%d", cs.DressProduct, cs.NorthStore), "", "", ""), "商品详情")
	// 加购与读车 → 503，车里什么都没进。车里先放一行（经单体路由）：一辆空车不需要问库存，
	// 库存服务不在时照样读得出来 —— 那是对的，但它验不了「有行的车」这一支。
	wantStatus(t, serve(testEngine, http.MethodPost, cs.Host, cartPath("/api/v1/cart/items", cs.NorthStore),
		fmt.Sprintf(`{"sku_id":%d,"quantity":1}`, cs.ShirtSKU), buyer.Token, freshIdemKey()), http.StatusOK, "单体形态加购")
	unavailable(serve(down, http.MethodPost, cs.Host, cartPath("/api/v1/cart/items", cs.NorthStore),
		fmt.Sprintf(`{"sku_id":%d,"quantity":1}`, cs.DressSKU), buyer.Token, freshIdemKey()), "加购")
	if n := adminQueryInt64(t, `SELECT count(*) FROM cart_items WHERE merchant_id = $1`, cs.MerchantID); n != 1 {
		t.Fatalf("加购回了 503，车里却有 %d 行（期望只有先前那 1 行）", n)
	}
	unavailable(serve(down, http.MethodGet, cs.Host, cartPath("/api/v1/cart", cs.NorthStore), "", buyer.Token, ""), "读车")

	// 后台相对调整 → 503，库存、流水、幂等存档都没动。
	key := freshIdemKey()
	before, _ := storeQtyInDB(t, cs.NorthStore, cs.DressSKU)
	unavailable(serve(down, http.MethodPost, cs.Host, adjustPath(cs.NorthStore, cs.DressSKU), `{"delta":5}`, cs.Token, key),
		"后台相对调整")
	if q, _ := storeQtyInDB(t, cs.NorthStore, cs.DressSKU); q != before {
		t.Fatalf("503 之后库存从 %d 变成了 %d", before, q)
	}
	if n := adjLogCount(t, cs.NorthStore, cs.DressSKU); n != 0 {
		t.Fatalf("503 之后留下了 %d 行相对调整的流水", n)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM idempotency_keys WHERE merchant_id = $1 AND idem_key = $2`,
		cs.MerchantID, key); n != 0 {
		t.Fatalf("503 之后那把钥匙被占住了（%d 行）—— 客户端原样重试会撞上存档", n)
	}
	// 后台 CAS、后台商品详情、库存预警 → 503。
	unavailable(serve(down, http.MethodPut, cs.Host,
		fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/inventory", cs.NorthStore, cs.DressSKU),
		fmt.Sprintf(`{"expected_available_qty":%d,"available_qty":1}`, before), cs.Token, ""), "后台 CAS")
	if q, _ := storeQtyInDB(t, cs.NorthStore, cs.DressSKU); q != before {
		t.Fatalf("CAS 回了 503，库存却从 %d 变成了 %d", before, q)
	}
	unavailable(serve(down, http.MethodGet, cs.Host, fmt.Sprintf("/api/v1/admin/products/%d", cs.DressProduct), "", cs.Token, ""),
		"后台商品详情")
	unavailable(serve(down, http.MethodGet, cs.Host, "/api/v1/admin/reports/inventory-alerts", "", cs.Token, ""), "库存预警")

	// 恢复之后同一把钥匙原样重试：生效一次。
	up := remoteEngine(t, startInventoryServer(t).URL)
	var inv api.AdminInventory
	decodeInto(t, serve(up, http.MethodPost, cs.Host, adjustPath(cs.NorthStore, cs.DressSKU), `{"delta":5}`, cs.Token, key),
		http.StatusOK, "恢复之后重试", &inv)
	if inv.AvailableQty != int(before)+5 {
		t.Fatalf("恢复之后重试得到 %d，期望 %d", inv.AvailableQty, before+5)
	}
}

// adjLogCount 数一个门店 SKU 的相对调整流水（biz_id 以 adj: 打头；夹具设库存走的是 CAS，
// 那几行是 set: 打头的，不算在内）。
func adjLogCount(t *testing.T, storeID, skuID int64) int {
	t.Helper()
	n := 0
	for _, l := range manualLogsOf(t, storeID, skuID) {
		if strings.HasPrefix(l.BizID, "adj:") {
			n++
		}
	}
	return n
}
