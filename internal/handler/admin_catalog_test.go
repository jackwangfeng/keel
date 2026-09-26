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
	"github.com/keel/keel/internal/tenant"
)

// 商家自助发布那 16 条写接口的行为测试（M4 Task 3）。
//
// 这一组的重点**不是「接口能跑通」**，是那几处「同一个信号、两种成因、
// 两个响应码」的分辨真的被 handler 保住了。repository 那一层已经把它们
// 在一条 SQL 语句里分开回传并做过变异验证；混掉它们的最后一个机会在
// handler 的那张映射表上，而混掉的代价是调用方对着一个永远不会成功的
// 请求无限重试。

// ---------------------------------------------------------------------------
// 鉴权：16 条，一条都不能漏
// ---------------------------------------------------------------------------

// adminCatalogRoute 是下面那条测试要逐条打一遍的 16 条。
//
// 它与 contract_test.go 的 routes 表不是同一份清单，这是刻意的：那张表核的是
// 「契约 ↔ 路由 ↔ handler 文件」，这里核的是**中间件挂没挂**。
// 两者共用一份的话，「挂没挂 staffAuth」这件事就没有独立的判据了。
type adminCatalogRoute struct {
	method, path, body string
}

func adminCatalogRoutes(sh adminShop) []adminCatalogRoute {
	return []adminCatalogRoute{
		{http.MethodGet, "/api/v1/admin/products", ""},
		{http.MethodPost, "/api/v1/admin/products", `{"category_id":1,"title":"x"}`},
		{http.MethodGet, "/api/v1/admin/products/1", ""},
		{http.MethodPatch, "/api/v1/admin/products/1", `{"title":"x"}`},
		{http.MethodDelete, "/api/v1/admin/products/1", ""},
		{http.MethodPost, "/api/v1/admin/products/1/publication", `{"action":"publish"}`},
		{http.MethodPut, "/api/v1/admin/products/1/images", `{"images":[]}`},
		{http.MethodPost, "/api/v1/admin/products/1/skus", `{"sku_code":"x","price_cents":1}`},
		{http.MethodPatch, "/api/v1/admin/skus/1", `{"price_cents":1}`},
		{http.MethodDelete, "/api/v1/admin/skus/1", ""},
		{http.MethodPut, "/api/v1/admin/skus/1/inventory", `{"expected_available_qty":0,"available_qty":1}`},
		{http.MethodGet, "/api/v1/admin/categories", ""},
		{http.MethodPost, "/api/v1/admin/categories", `{"name":"x"}`},
		{http.MethodPatch, "/api/v1/admin/categories/1", `{"name":"x"}`},
		{http.MethodDelete, "/api/v1/admin/categories/1", ""},
		// /admin/uploads 是 multipart 的，不走 reqAs —— 它单独在下面打一次。
	}
}

// 16 条写接口全部要后台会话。
//
// **这条测试是「每一行都挂了 staffAuth」唯一的执行者。** app/run_test.go 那张
// 路由表核的是路径，而把某一行的中间件删掉，路由表一个字都不会变 ——
// 那时一个匿名请求能改这家店的商品价格，并且拿到 200。
//
// 判据有两段，缺第二段这条测试就是空转：
//
//	① 不带 Authorization 头打过去，必须是 401；
//	② **阳性对照**：同一条路径带上会话 token 再打一次，必须**不是** 401。
//	   少了它，把这 16 条路由全删掉（于是它们落进 NoRoute）也能让①全绿 ——
//	   不，NoRoute 回的是 404 不是 401，但「handler 里第一行就无条件写 401」
//	   这种写法会让①全绿而②全红，而②正是抓它的那一段。
func TestAdminCatalogRoutesAllRequireStaffSession(t *testing.T) {
	sh := newAdminShop(t)

	for _, r := range adminCatalogRoutes(sh) {
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

	// /admin/uploads 单独打：它是 multipart 的。
	anon := reqAs(t, http.MethodPost, sh.Host, "/api/v1/admin/uploads", "", "")
	if anon.Code != http.StatusUnauthorized {
		t.Errorf("POST /admin/uploads 不带后台会话返回 %d，期望 401。响应体：%s",
			anon.Code, anon.Body.String())
	}
	ok := uploadImage(t, sh, "image/png", []byte("fake-png-bytes"))
	if ok.Code == http.StatusUnauthorized {
		t.Errorf("POST /admin/uploads 带上真实会话仍然 401，上面那条断言因此是空转：%s",
			ok.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 产出标志：一个商家能自助把一件商品发布出去，买家能看见它
// ---------------------------------------------------------------------------

// publishFlow 是那条主路径，顺带把几处闸门与现算价格一起验掉。
//
// 它是这一轮「商家可自助发布」这个产出标志在测试里的样子。做成一条长测试
// 而不是十条短的，是因为它验的正是**这些步骤连成一条**：分开之后每一步都
// 能各自绿着，而「建完 SKU 就能上架、上架完买家就看得见」这件事没有靶子。
func TestMerchantCanPublishAProductAndBuyersSeeIt(t *testing.T) {
	sh := newAdminShop(t)

	// ① 建类目。path / level 由服务端算 —— 请求体里没有它们。
	var cat api.AdminCategory
	decodeInto(t, postIdem(t, sh.Host, "/api/v1/admin/categories",
		`{"name":"咖啡器具","sort_order":1}`, sh.Token),
		http.StatusCreated, "建类目", &cat)
	if cat.Level != 1 || cat.Path == "" {
		t.Fatalf("新建的根类目 level=%d path=%q，期望 level=1 且 path 非空 —— "+
			"path 是 idx_categories_path 的内容，空串会让前缀查询对这一支整个失效",
			cat.Level, cat.Path)
	}
	if cat.Path != fmt.Sprintf("/%d/", cat.Id) {
		t.Fatalf("根类目的 path 是 %q，期望 /%d/（物化路径含自己的 id）", cat.Path, cat.Id)
	}

	// ② 建商品：落地即草稿。
	var p api.AdminProduct
	decodeInto(t, postIdem(t, sh.Host, "/api/v1/admin/products",
		fmt.Sprintf(`{"category_id":%d,"title":"手冲咖啡壶 %s","subtitle":"600ml 玻璃"}`,
			cat.Id, sh.Suffix), sh.Token),
		http.StatusCreated, "建商品", &p)
	if p.Status != 0 {
		t.Fatalf("新建商品的 status 是 %d，期望 0 草稿 —— 创建与发布是两个动作", p.Status)
	}
	if p.PublishedAt != nil {
		t.Fatalf("新建商品的 published_at 是 %v，期望缺席", p.PublishedAt)
	}
	if p.MinPriceCents != 0 || p.MaxPriceCents != 0 || p.TotalStock != 0 {
		t.Fatalf("新建商品的价格区间/库存是 (%d, %d, %d)，期望全 0 —— 它一个 SKU 都还没有",
			p.MinPriceCents, p.MaxPriceCents, p.TotalStock)
	}

	// ③ **闸门**：一个 SKU 都没有时不能上架。
	// 这一条排在加 SKU 之前，因为加完就再也造不出这个状态了。
	got := problemType(t, postIdem(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/products/%d/publication", p.Id),
		`{"action":"publish"}`, sh.Token), http.StatusConflict, "没有 SKU 就上架")
	if got != problem.TypeProductHasNoSKU {
		t.Fatalf("没有 SKU 就上架，Problem type 是 %q，期望 %q", got, problem.TypeProductHasNoSKU)
	}
	// 阳性对照留在下面第 ⑦ 步：同一件商品加完 SKU 之后 publish 必须是 200。
	// 没有那一步的话，把 Publication 写成恒 409 也能让这里绿。

	// ④ 加两个**不同价**的 SKU。价格不同是这条测试的判据之一：
	// 两个同价的 SKU 下，把现算的 min() 写成 max() 不会红。
	var sku1, sku2 api.AdminSku
	decodeInto(t, postIdem(t, sh.Host, fmt.Sprintf("/api/v1/admin/products/%d/skus", p.Id),
		fmt.Sprintf(`{"sku_code":"HCP-600-%s","price_cents":12900,"cost_cents":7000,
		              "spec_values":{"容量":"600ml"},"available_qty":9,"warning_qty":2}`, sh.Suffix),
		sh.Token), http.StatusCreated, "建 SKU 1", &sku1)
	if sku1.AvailableQty != 9 {
		t.Fatalf("新建 SKU 的 available_qty 是 %d，期望 9", sku1.AvailableQty)
	}
	decodeInto(t, postIdem(t, sh.Host, fmt.Sprintf("/api/v1/admin/products/%d/skus", p.Id),
		fmt.Sprintf(`{"sku_code":"HCP-900-%s","price_cents":15900,"available_qty":4}`, sh.Suffix),
		sh.Token), http.StatusCreated, "建 SKU 2", &sku2)

	// **建 SKU 必须在同事务里建出 inventories 行。** 直接用下单 SAGA 那条
	// 语句验一次，而不是查「有没有那一行」：漏建时 SAGA 返回的是
	// 「库存不足」，那正是最误导人的症状（商品表现为永远缺货，
	// 而排查方向从第一步就是错的）。
	ctx := tenant.NewContext(context.Background(), sh.MerchantID)
	var left int32
	if err := repository.New(testPool).WithTenant(ctx, func(q repository.Tx) error {
		var e error
		left, e = q.DeductInventory(ctx, sku2.Id, 1)
		return e
	}); err != nil {
		t.Fatalf("新建 SKU 扣不动库存: %v —— 那一行 inventories 没建出来，"+
			"而 SAGA 会把它判成缺货，症状是「这件商品永远缺货」", err)
	}
	if left != 3 {
		t.Fatalf("扣 1 件之后水位是 %d，期望 3", left)
	}

	// ⑤ 价格区间是**现算**的：00019 删了那两列，没有任何同步器跑过。
	var detail api.AdminProductDetail
	decodeInto(t, getAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/products/%d", p.Id), sh.Token),
		http.StatusOK, "后台详情", &detail)
	if detail.MinPriceCents != 12900 || detail.MaxPriceCents != 15900 {
		t.Fatalf("后台详情的价格区间是 (%d, %d)，期望 (12900, 15900) —— "+
			"它应当等于这两个 SKU 的 min/max，而这两个数在库里的 products 行上"+
			"根本不存在（00019 删了那两列）",
			detail.MinPriceCents, detail.MaxPriceCents)
	}
	if detail.TotalStock != 12 {
		t.Fatalf("后台详情的 total_stock 是 %d，期望 12（9 + 4 - 1 刚扣掉的那件）—— "+
			"它是从 inventories 现算的当下水位，不是某一次写入的快照", detail.TotalStock)
	}
	if len(detail.Skus) != 2 {
		t.Fatalf("后台详情里有 %d 个 SKU，期望 2", len(detail.Skus))
	}

	// ⑥ 库存：比较并设置。expected 用当前真实值。
	var inv api.AdminInventory
	decodeInto(t, putAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/skus/%d/inventory", sku2.Id),
		`{"expected_available_qty":3,"available_qty":30,"warning_qty":5}`, sh.Token),
		http.StatusOK, "改库存", &inv)
	if inv.AvailableQty != 30 || inv.WarningQty != 5 {
		t.Fatalf("改完库存回的是 (%d, %d)，期望 (30, 5)", inv.AvailableQty, inv.WarningQty)
	}

	// ⑦ 传图元数据 + 整组替换。
	var up api.Upload
	decodeInto(t, uploadImage(t, sh, "image/png", []byte("\x89PNG\r\n\x1a\n fake but real bytes")),
		http.StatusCreated, "上传商品图", &up)
	if up.SizeBytes == 0 || up.Url == "" {
		t.Fatalf("上传回的是 size=%d url=%q", up.SizeBytes, up.Url)
	}
	var imgs []api.ProductImage
	decodeInto(t, putAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/products/%d/images", p.Id),
		fmt.Sprintf(`{"images":[{"upload_id":%d}]}`, up.Id), sh.Token),
		http.StatusOK, "设置商品图", &imgs)
	if len(imgs) != 1 || imgs[0].SortOrder != 0 || imgs[0].UploadId != up.Id {
		t.Fatalf("整组替换之后的图是 %+v，期望一张 sort_order = 0 的 upload %d", imgs, up.Id)
	}
	// referenced 必须在那次写入的**同一个事务**里置位（§13）：不置的话
	// 孤儿回收会在 24 小时后把这张刚用上的图删掉，而商品详情页上那张图已经 404。
	if ref := adminQueryInt64(t,
		`SELECT count(*) FROM uploads WHERE id = $1 AND referenced`, up.Id); ref != 1 {
		t.Fatalf("挂上商品之后 upload %d 的 referenced 还是 FALSE —— "+
			"它会在 24 小时后被孤儿回收删掉，而商品还指着它", up.Id)
	}

	// ⑧ 上架。**这一步是第 ③ 步那个 409 的阳性对照。**
	var published api.AdminProduct
	decodeInto(t, postIdem(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/products/%d/publication", p.Id),
		`{"action":"publish"}`, sh.Token), http.StatusOK, "上架", &published)
	if published.Status != 1 {
		t.Fatalf("上架之后 status 是 %d，期望 1", published.Status)
	}
	if published.PublishedAt == nil {
		t.Fatal("上架之后 published_at 还是缺席的 —— 前台按它倒序排")
	}
	first := *published.PublishedAt

	// 重复上架是幂等的（契约：返回 200 且什么都不改，不报 409），
	// 而且 **published_at 不许被覆盖** —— 它是「首次发布时间」，
	// 每次上架都覆盖的话，一次临时下架再上架就能把一件老商品顶到列表最前面。
	//
	// **这里要的是一把新钥匙**（postIdem 每次给一把新的）：被测的是状态机
	// 天生的幂等（再上架一次什么都不改），不是幂等键的存档回放。复用上一把
	// 的话回的是存档，于是这条断言验的就成了另一件事。
	var again api.AdminProduct
	decodeInto(t, postIdem(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/products/%d/publication", p.Id),
		`{"action":"publish"}`, sh.Token), http.StatusOK, "重复上架", &again)
	if again.PublishedAt == nil || !again.PublishedAt.Equal(first) {
		t.Fatalf("再上架一次之后 published_at 从 %v 变成了 %v —— 它只在首次上架时置位",
			first, again.PublishedAt)
	}

	// ⑨ 买家侧：前台列表看得见它，而且价格区间就是那两个 SKU 的 min/max。
	//
	// 这一段走的是**没有后台会话**的公开路由 —— 它才是「商家可自助发布」
	// 这个产出标志的另一半。
	var list struct {
		Total int                  `json:"total"`
		Items []api.ProductSummary `json:"items"`
	}
	decodeInto(t, do(t, sh.Host, "/api/v1/products?page_size=100"),
		http.StatusOK, "买家侧商品列表", &list)
	var seen *api.ProductSummary
	for i := range list.Items {
		if list.Items[i].Id == p.Id {
			seen = &list.Items[i]
		}
	}
	if seen == nil {
		t.Fatalf("买家在 /products 里看不到刚上架的商品 %d，列表里有 %d 件",
			p.Id, len(list.Items))
	}
	// max_price_cents 在契约里不是必填的，所以生成类型上它是指针。
	// 先判 nil 再解引用，而且**打印的是值不是指针** —— 一条打出
	// 「期望 15900，实际 0xc0001a2b30」的失败信息，读它的人得再跑一次才知道
	// 那个数是多少，而这条断言恰恰是价格现算那件事唯一的买家侧靶子。
	if seen.MaxPriceCents == nil {
		t.Fatalf("买家看到的 max_price_cents 缺席了，期望 15900")
	}
	if seen.MinPriceCents != 12900 || *seen.MaxPriceCents != 15900 {
		t.Fatalf("买家看到的价格区间是 (%d, %d)，期望 (12900, 15900) —— "+
			"这两个数在库里的 products 行上根本不存在（00019 删了那两列），"+
			"它们由 ListProducts 的 LEFT JOIN LATERAL 从 skus 现算",
			seen.MinPriceCents, *seen.MaxPriceCents)
	}

	// ⑩ 检索：跑一次**真实的**派生数据入库任务，再搜。
	//
	// 不手写 search_text 与向量：那两份数据由 service.IndexService 维护，
	// 自己写一份等于绕开被测的那条路。没有这一步商品搜不到，
	// 而那不是检索坏了 —— 是索引任务还没跑（app.Run 里它是一个后台 goroutine，
	// 测试进程里没有起它）。
	idx, err := service.NewIndexService(repository.New(testPool), conceptEmbedder{},
		service.IndexConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 两步：全量**入队**，再把队列抽干。M4 阶段 1 把这条路切成了生产者 +
	// 消费者（jobs 表，数据模型 §12），Backfill 现在只负责前半段。
	// 少了 Drain 这一步的症状正是下面那条断言：索引「跑完」了却搜不到 ——
	// 因为活还躺在队列里。
	if _, err := idx.Backfill(context.Background(), sh.MerchantID, false); err != nil {
		t.Fatalf("派生数据入队失败: %v", err)
	}
	if _, err := idx.Drain(context.Background()); err != nil {
		t.Fatalf("抽干理解队列失败: %v", err)
	}
	_, hits := doSearch(t, sh.Host, `{"query":"咖啡壶"}`)
	if !contains(titlesOf(hits), published.Title) {
		t.Fatalf("索引跑完之后仍然搜不到刚上架的 %q，搜到的是 %v",
			published.Title, titlesOf(hits))
	}
}

// ---------------------------------------------------------------------------
// 两种 rows_affected = 0：混掉它们会造出一个无限重试的循环
// ---------------------------------------------------------------------------

// CAS 不匹配是 409（带当前真实值），SKU 不在本租户是 404。
//
// **这是这一组里最要紧的一条。** 契约在那条端点上明写了混掉的代价：
// 把「不是你的 SKU」也报成 409，会让调用方以为重读一次再试就能成功，
// 而那个循环永远不会结束。
//
// 三段判据，缺任何一段这条测试就分辨不出它要分辨的东西：
//
//	① 阳性对照：expected 用真实值时必须成功（否则下面两条 409/404 可能
//	   只是「这条接口整个坏了」）；
//	② CAS 对不上 → 409 + inventory-precondition-failed + **current 是真值**；
//	③ 另一家店的 sku_id → 404。
func TestInventoryTellsCASMismatchApartFromAForeignSKU(t *testing.T) {
	a := newAdminShop(t)
	b := newAdminShop(t)

	skuA := seedOneSKU(t, a, "INV-A", 1000, 7)
	skuB := seedOneSKU(t, b, "INV-B", 1000, 7)

	// ① 阳性对照。
	var inv api.AdminInventory
	decodeInto(t, putAs(t, a.Host, fmt.Sprintf("/api/v1/admin/skus/%d/inventory", skuA),
		`{"expected_available_qty":7,"available_qty":11}`, a.Token),
		http.StatusOK, "CAS 用真实值", &inv)
	if inv.AvailableQty != 11 {
		t.Fatalf("CAS 成功之后水位是 %d，期望 11", inv.AvailableQty)
	}

	// ② CAS 对不上 → 409，而且要带回当前真实值。
	w := putAs(t, a.Host, fmt.Sprintf("/api/v1/admin/skus/%d/inventory", skuA),
		`{"expected_available_qty":7,"available_qty":99}`, a.Token)
	if got := problemType(t, w, http.StatusConflict, "CAS 对不上"); got != problem.TypeInventoryPrecondition {
		t.Fatalf("CAS 对不上的 Problem type 是 %q，期望 %q", got, problem.TypeInventoryPrecondition)
	}
	var conflict api.InventoryConflict
	decodeInto(t, w, http.StatusConflict, "CAS 冲突体", &conflict)
	if conflict.Current.AvailableQty != 11 {
		t.Fatalf("409 里的 current.available_qty 是 %d，期望 11（当前真实值）—— "+
			"契约把 current 定成必填就是为了省掉调用方「先重查再重试」的那一跳，"+
			"而那一跳本身又是一次会过期的读。少了它客户端只能盲目重试",
			conflict.Current.AvailableQty)
	}
	if conflict.Current.SkuId != skuA {
		t.Fatalf("409 里的 current.sku_id 是 %d，期望 %d", conflict.Current.SkuId, skuA)
	}

	// ③ 别家店的 sku_id → 404，**不是** 409。
	w = putAs(t, a.Host, fmt.Sprintf("/api/v1/admin/skus/%d/inventory", skuB),
		`{"expected_available_qty":7,"available_qty":11}`, a.Token)
	if w.Code == http.StatusConflict {
		t.Fatalf("拿别家店的 sku_id（%d）改库存返回了 409 —— 调用方会以为"+
			"重读一次再试就能成功，而那个循环永远不会结束。响应体：%s",
			skuB, w.Body.String())
	}
	if got := problemType(t, w, http.StatusNotFound, "别家店的 SKU"); got != problem.TypeNotFound {
		t.Fatalf("别家店的 sku_id 回的 Problem type 是 %q，期望 %q", got, problem.TypeNotFound)
	}
}

// ---------------------------------------------------------------------------
// 其余几条 409 闸门
// ---------------------------------------------------------------------------

// 在架商品不能删（409），先下架再删（204）。
//
// 后半段是前半段的阳性对照：没有它，把 Delete 写成恒 409 也能让前半段全绿。
func TestDeletingAPublishedProductIsRefusedUntilItIsUnpublished(t *testing.T) {
	sh := newAdminShop(t)
	prod, _ := seedPublishedProduct(t, sh, "DEL", 1000, 5)

	got := problemType(t, deleteAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/products/%d", prod), sh.Token),
		http.StatusConflict, "删在架商品")
	if got != problem.TypeProductStillPublished {
		t.Fatalf("删在架商品的 Problem type 是 %q，期望 %q", got, problem.TypeProductStillPublished)
	}

	wantStatus(t, postIdem(t, sh.Host, fmt.Sprintf("/api/v1/admin/products/%d/publication", prod),
		`{"action":"unpublish"}`, sh.Token), http.StatusOK, "下架")
	wantStatus(t, deleteAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/products/%d", prod), sh.Token),
		http.StatusNoContent, "下架之后再删")

	// 再删一次 → 404（已经删过了），**不是** 409。契约把「已软删」与
	// 「在架」分成了两个响应码，而它们的数据库信号都是 0 行。
	got = problemType(t, deleteAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/products/%d", prod), sh.Token),
		http.StatusNotFound, "重复删")
	if got != problem.TypeNotFound {
		t.Fatalf("重复删的 Problem type 是 %q，期望 %q", got, problem.TypeNotFound)
	}

	// 软删之后改它 → 409 product-deleted（又一对同信号不同码：
	// 「这件商品不存在」是 404，「它已软删」是 409）。
	got = problemType(t, patchAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/products/%d", prod),
		`{"title":"改个名"}`, sh.Token), http.StatusConflict, "改已软删的商品")
	if got != problem.TypeProductDeleted {
		t.Fatalf("改已软删商品的 Problem type 是 %q，期望 %q", got, problem.TypeProductDeleted)
	}
}

// 在架商品的最后一个 SKU 删不得（409）；加一个兄弟之后就删得动（204）。
func TestDeletingTheLastSKUOfAPublishedProductIsRefused(t *testing.T) {
	sh := newAdminShop(t)
	prod, sku := seedPublishedProduct(t, sh, "LAST", 1000, 5)

	got := problemType(t, deleteAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/skus/%d", sku), sh.Token),
		http.StatusConflict, "删在架商品的最后一个 SKU")
	if got != problem.TypeSKULastOfPublished {
		t.Fatalf("Problem type 是 %q，期望 %q", got, problem.TypeSKULastOfPublished)
	}

	// 阳性对照：加一个兄弟规格之后，同一次删除必须成功。
	var sibling api.AdminSku
	decodeInto(t, postIdem(t, sh.Host, fmt.Sprintf("/api/v1/admin/products/%d/skus", prod),
		fmt.Sprintf(`{"sku_code":"LAST-2-%s","price_cents":2000}`, sh.Suffix), sh.Token),
		http.StatusCreated, "加兄弟 SKU", &sibling)
	wantStatus(t, deleteAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/skus/%d", sku), sh.Token),
		http.StatusNoContent, "有兄弟之后删")

	// 软删掉的规格不占货号了（uk_skus_code 是部分唯一索引）。
	// 这一条顺带证明上面那次删真的落了 deleted_at，而不是只返回了 204。
	wantStatus(t, postIdem(t, sh.Host, fmt.Sprintf("/api/v1/admin/products/%d/skus", prod),
		fmt.Sprintf(`{"sku_code":"LAST-%s","price_cents":3000}`, sh.Suffix), sh.Token),
		http.StatusCreated, "复用被软删规格的货号")
}

// 货号在租户内唯一 → 409 sku-code-duplicated；**另一家店用同一个货号是正常的**。
func TestDuplicateSKUCodeIsA409WithinTheTenantOnly(t *testing.T) {
	a := newAdminShop(t)
	b := newAdminShop(t)
	prodA, _ := seedPublishedProduct(t, a, "DUP", 1000, 5)
	prodB, _ := seedPublishedProduct(t, b, "DUP2", 1000, 5)

	body := fmt.Sprintf(`{"sku_code":"SHARED-%s","price_cents":100}`, a.Suffix)
	wantStatus(t, postIdem(t, a.Host, fmt.Sprintf("/api/v1/admin/products/%d/skus", prodA), body, a.Token),
		http.StatusCreated, "A 店第一次用这个货号")
	// **换一把新钥匙**：这里要的是「同一个货号第二次」撞 uk_skus_code，
	// 不是「同一把幂等键第二次」撞存档。复用上一把的话回的会是 201 重放，
	// 而那条 409 就再也测不到了。
	got := problemType(t, postIdem(t, a.Host,
		fmt.Sprintf("/api/v1/admin/products/%d/skus", prodA), body, a.Token),
		http.StatusConflict, "A 店第二次用同一个货号")
	if got != problem.TypeSKUCodeDuplicated {
		t.Fatalf("Problem type 是 %q，期望 %q", got, problem.TypeSKUCodeDuplicated)
	}
	// 阳性对照：uk_skus_code 是 (merchant_id, sku_code)，不是全局唯一 ——
	// 两家店各有一个 A001 是正常的。少了这一条，把索引改成全局唯一
	// 也能让上面那个 409 照样出现。
	wantStatus(t, postIdem(t, b.Host, fmt.Sprintf("/api/v1/admin/products/%d/skus", prodB), body, b.Token),
		http.StatusCreated, "B 店用同一个货号")
}

// 类目的三条 409：有商品、有子分类、移动成环。
func TestCategoryGatesRefuseDeleteAndCycles(t *testing.T) {
	sh := newAdminShop(t)

	var root, child api.AdminCategory
	decodeInto(t, postIdem(t, sh.Host, "/api/v1/admin/categories", `{"name":"根"}`, sh.Token),
		http.StatusCreated, "建根类目", &root)
	decodeInto(t, postIdem(t, sh.Host, "/api/v1/admin/categories",
		fmt.Sprintf(`{"name":"子","parent_id":%d}`, root.Id), sh.Token),
		http.StatusCreated, "建子类目", &child)
	if child.Level != 2 || child.Path != fmt.Sprintf("/%d/%d/", root.Id, child.Id) {
		t.Fatalf("子类目 level=%d path=%q，期望 level=2 path=/%d/%d/",
			child.Level, child.Path, root.Id, child.Id)
	}

	// 有子分类 → 409。
	got := problemType(t, deleteAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/categories/%d", root.Id), sh.Token),
		http.StatusConflict, "删有子分类的类目")
	if got != problem.TypeCategoryHasChildren {
		t.Fatalf("Problem type 是 %q，期望 %q", got, problem.TypeCategoryHasChildren)
	}

	// 有商品 → 409（挂在子类目下）。
	wantStatus(t, postIdem(t, sh.Host, "/api/v1/admin/products",
		fmt.Sprintf(`{"category_id":%d,"title":"占位商品 %s"}`, child.Id, sh.Suffix), sh.Token),
		http.StatusCreated, "往子类目下建商品")
	got = problemType(t, deleteAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/categories/%d", child.Id), sh.Token),
		http.StatusConflict, "删有商品的类目")
	if got != problem.TypeCategoryHasProducts {
		t.Fatalf("Problem type 是 %q，期望 %q", got, problem.TypeCategoryHasProducts)
	}

	// 移动成环：把根挪到自己的后代下面 → 409。
	got = problemType(t, patchAs(t, sh.Host,
		fmt.Sprintf("/api/v1/admin/categories/%d", root.Id),
		fmt.Sprintf(`{"parent_id":%d}`, child.Id), sh.Token),
		http.StatusConflict, "把根挪到自己的后代下")
	if got != problem.TypeCategoryCycle {
		t.Fatalf("Problem type 是 %q，期望 %q", got, problem.TypeCategoryCycle)
	}

	// 阳性对照：一次**合法**的移动必须成功，而且整棵子树的 path 跟着走。
	// 少了它，把 MoveCategory 写成恒 409 也能让上面那条绿。
	var third api.AdminCategory
	decodeInto(t, postIdem(t, sh.Host, "/api/v1/admin/categories", `{"name":"另一棵根"}`, sh.Token),
		http.StatusCreated, "建第二棵根", &third)
	var moved api.AdminCategory
	decodeInto(t, patchAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/categories/%d", root.Id),
		fmt.Sprintf(`{"parent_id":%d}`, third.Id), sh.Token),
		http.StatusOK, "合法移动", &moved)
	if moved.Level != 2 {
		t.Fatalf("移动之后 level 是 %d，期望 2", moved.Level)
	}
	// 子树的 path 必须跟着重写 —— 不跟的话，挂在它下面的商品在新位置查不出来，
	// 在旧位置还查得出来，而没有任何东西会报错。
	newChildPath := adminQueryText(t, `SELECT path FROM categories WHERE id = $1`, child.Id)
	want := fmt.Sprintf("/%d/%d/%d/", third.Id, root.Id, child.Id)
	if newChildPath != want {
		t.Fatalf("移动父节点之后，子节点的 path 是 %q，期望 %q —— "+
			"子树的 path 没跟着走，path 是分类查询的主索引", newChildPath, want)
	}

	// 「不传 parent_id」与「传 null」是两件事：只改名不许把它挪回根。
	var renamed api.AdminCategory
	decodeInto(t, patchAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/categories/%d", root.Id),
		`{"name":"改个名"}`, sh.Token), http.StatusOK, "只改名", &renamed)
	if renamed.Level != 2 || renamed.ParentId == nil || *renamed.ParentId != third.Id {
		t.Fatalf("只改名之后 level=%d parent_id=%v，期望 level=2 parent_id=%d —— "+
			"「没传 parent_id」被当成了「传了 null」，于是一次改名把整棵子树挪到了根下",
			renamed.Level, renamed.ParentId, third.Id)
	}
}

// 商品图那三条 422：重复的 upload_id、别家店的 upload、用途不对的 upload。
func TestProductImagesRejectDuplicateForeignAndWrongPurposeUploads(t *testing.T) {
	a := newAdminShop(t)
	b := newAdminShop(t)
	prodA, _ := seedPublishedProduct(t, a, "IMG", 1000, 5)

	var upA1, upA2, upB api.Upload
	decodeInto(t, uploadImage(t, a, "image/jpeg", []byte("a1")), http.StatusCreated, "A 传图 1", &upA1)
	decodeInto(t, uploadImage(t, a, "image/jpeg", []byte("a2")), http.StatusCreated, "A 传图 2", &upA2)
	decodeInto(t, uploadImage(t, b, "image/jpeg", []byte("b1")), http.StatusCreated, "B 传图", &upB)

	// 阳性对照先跑：两张自己的图必须挂得上去。
	var ok []api.ProductImage
	decodeInto(t, putAs(t, a.Host, fmt.Sprintf("/api/v1/admin/products/%d/images", prodA),
		fmt.Sprintf(`{"images":[{"upload_id":%d},{"upload_id":%d}]}`, upA1.Id, upA2.Id), a.Token),
		http.StatusOK, "挂两张自己的图", &ok)
	if len(ok) != 2 || ok[0].UploadId != upA1.Id || ok[0].SortOrder != 0 {
		t.Fatalf("挂图结果是 %+v，期望第 0 张是 upload %d", ok, upA1.Id)
	}

	// 重复。
	got := problemType(t, putAs(t, a.Host, fmt.Sprintf("/api/v1/admin/products/%d/images", prodA),
		fmt.Sprintf(`{"images":[{"upload_id":%d},{"upload_id":%d}]}`, upA1.Id, upA1.Id), a.Token),
		http.StatusUnprocessableEntity, "同一张图挂两次")
	if got != problem.TypeProductImageDuplicated {
		t.Fatalf("Problem type 是 %q，期望 %q", got, problem.TypeProductImageDuplicated)
	}

	// 别家店的 upload → 422 upload-not-found（RLS 让它根本查不到）。
	got = problemType(t, putAs(t, a.Host, fmt.Sprintf("/api/v1/admin/products/%d/images", prodA),
		fmt.Sprintf(`{"images":[{"upload_id":%d}]}`, upB.Id), a.Token),
		http.StatusUnprocessableEntity, "挂别家店的图")
	if got != problem.TypeUploadNotFound {
		t.Fatalf("Problem type 是 %q，期望 %q", got, problem.TypeUploadNotFound)
	}

	// 用途不对 → 422 upload-wrong-purpose。**数据库挡不住这一条**：
	// 复合外键挡的是跨租户，不是用途。所以造一条 purpose = 3 退款凭证。
	evidence := adminQueryInt64(t, `
		INSERT INTO uploads (merchant_id, staff_id, purpose, driver, storage_key,
		                     content_type, size_bytes, sha256)
		VALUES ($1, $2, 3, 1, 'evidence/'||$3, 'image/jpeg', 7, 'x') RETURNING id`,
		a.MerchantID, a.StaffID, a.Suffix)
	got = problemType(t, putAs(t, a.Host, fmt.Sprintf("/api/v1/admin/products/%d/images", prodA),
		fmt.Sprintf(`{"images":[{"upload_id":%d}]}`, evidence), a.Token),
		http.StatusUnprocessableEntity, "拿退款凭证当商品图")
	if got != problem.TypeUploadWrongPurpose {
		t.Fatalf("Problem type 是 %q，期望 %q", got, problem.TypeUploadWrongPurpose)
	}

	// 上面三次失败都不许改动已经挂好的那两张图（整组替换是一个事务）。
	var detail api.AdminProductDetail
	decodeInto(t, getAs(t, a.Host, fmt.Sprintf("/api/v1/admin/products/%d", prodA), a.Token),
		http.StatusOK, "失败之后再读详情", &detail)
	if still := detail.Images; len(still) != 2 {
		t.Fatalf("三次失败的整组替换之后还剩 %d 张图，期望 2 —— "+
			"ClearProductImages 已经执行过而事务没回滚？", len(still))
	}
}

// 跨租户：拿别家店的 product_id 打过来一律 404，不是 403。
//
// 403 会让自增 id 空间变成一个跨租户的存在性探针（契约 /admin/ 段头的约定 3）。
func TestAdminCatalogIsTenantScoped(t *testing.T) {
	a := newAdminShop(t)
	b := newAdminShop(t)
	prodB, skuB := seedPublishedProduct(t, b, "X", 1000, 5)

	cases := []struct {
		what   string
		method string
		path   string
		body   string
	}{
		{"读别家的商品", http.MethodGet, fmt.Sprintf("/api/v1/admin/products/%d", prodB), ""},
		{"改别家的商品", http.MethodPatch, fmt.Sprintf("/api/v1/admin/products/%d", prodB), `{"title":"抢过来"}`},
		{"删别家的商品", http.MethodDelete, fmt.Sprintf("/api/v1/admin/products/%d", prodB), ""},
		{"下架别家的商品", http.MethodPost, fmt.Sprintf("/api/v1/admin/products/%d/publication", prodB), `{"action":"unpublish"}`},
		{"给别家的商品挂图", http.MethodPut, fmt.Sprintf("/api/v1/admin/products/%d/images", prodB), `{"images":[]}`},
		{"给别家的商品加 SKU", http.MethodPost, fmt.Sprintf("/api/v1/admin/products/%d/skus", prodB), `{"sku_code":"Z","price_cents":1}`},
		{"改别家的 SKU", http.MethodPatch, fmt.Sprintf("/api/v1/admin/skus/%d", skuB), `{"price_cents":1}`},
		{"删别家的 SKU", http.MethodDelete, fmt.Sprintf("/api/v1/admin/skus/%d", skuB), ""},
	}
	for _, c := range cases {
		w := reqAs(t, c.method, a.Host, c.path, c.body, a.Token)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s 返回 %d，期望 404。响应体：%s", c.what, w.Code, w.Body.String())
		}
	}

	// 阳性对照：同样这几条，B 店自己来做必须**不是** 404。
	// 少了它，把 /admin/products/:id 那几条写成恒 404 也能让上面全绿。
	w := getAs(t, b.Host, fmt.Sprintf("/api/v1/admin/products/%d", prodB), b.Token)
	if w.Code != http.StatusOK {
		t.Fatalf("B 店读自己的商品返回 %d —— 上面那批 404 因此证明不了跨租户：%s",
			w.Code, w.Body.String())
	}

	// B 店的商品也不许出现在 A 店的后台列表里。
	var list struct {
		Total int                `json:"total"`
		Items []api.AdminProduct `json:"items"`
	}
	decodeInto(t, getAs(t, a.Host, "/api/v1/admin/products?page_size=100", a.Token),
		http.StatusOK, "A 店的后台列表", &list)
	for _, it := range list.Items {
		if it.Id == prodB {
			t.Fatalf("A 店的后台列表里出现了 B 店的商品 %d", prodB)
		}
	}
}

// ---------------------------------------------------------------------------
// 幂等（M4 收尾）
// ---------------------------------------------------------------------------

// 那 5 条 POST 真的幂等了：同一把 Idempotency-Key 第二次拿到的是**首次那一件**，
// 库里不多一行；同一把钥匙配不同的请求体回 422。
//
// ===========================================================================
// 这条测试本轮翻了个面
// ===========================================================================
//
// 它原先叫 TestAdminWritesAreNotYetIdempotent，断言的是「同一把钥匙会建出两件
// 商品」—— 那是 contract_test.go 里那 5 笔 NotYetImplementedHeader 挂账的反向
// 执行者，也是那笔账的暴露面说明书。挡着实现的是一次 schema 决定
// （00023：idempotency_keys 的主键里那个 user_id 在后台这条路上要放 staff_id，
// 而两张表的 id 来自同一种自增序列）。决定做完了，账销了，靶子就该跟着翻过来。
//
// 三条断言缺一不可，而第二条最容易被漏掉：
//
//	① 第二次回的是同一件（id 相同）；
//	② **库里真的只有一行** —— 只比 id 的话，一个「建两件、回第一件」的实现
//	   也是绿的，而那正是幂等要防的那件事本身；
//	③ Idempotency-Replayed: true —— 没有它，客户端把重放记成一次新建，
//	   连点两下会被记成两次转化（契约明写它影响埋点与提示文案）。
func TestAdminWritesAreIdempotent(t *testing.T) {
	sh := newAdminShop(t)
	var cat api.AdminCategory
	decodeInto(t, postIdem(t, sh.Host, "/api/v1/admin/categories", `{"name":"幂等靶子"}`, sh.Token),
		http.StatusCreated, "建类目", &cat)

	key := "11111111-2222-3333-4444-555555555555"
	title := "重发不该变两件 " + sh.Suffix
	body := fmt.Sprintf(`{"category_id":%d,"title":%q}`, cat.Id, title)

	var first, second api.AdminProduct
	w1 := postWithKey(t, sh.Host, "/api/v1/admin/products", body, sh.Token, key)
	decodeInto(t, w1, http.StatusCreated, "第一次建", &first)
	if got := w1.Header().Get("Idempotency-Replayed"); got != "" {
		t.Errorf("首次调用带上了 Idempotency-Replayed: %q —— 那是重放专用的头，"+
			"首次带上它会让客户端把一次真的新建记成重放", got)
	}

	w2 := postWithKey(t, sh.Host, "/api/v1/admin/products", body, sh.Token, key)
	decodeInto(t, w2, http.StatusCreated, "带同一把 Idempotency-Key 再建一次", &second)

	// ① 同一件。
	if first.Id != second.Id {
		t.Fatalf("同一把 Idempotency-Key 打两次建出了两件商品（id=%d 与 %d）—— "+
			"幂等没生效", first.Id, second.Id)
	}
	// ③ 重放头。
	if got := w2.Header().Get("Idempotency-Replayed"); got != "true" {
		t.Errorf("重放没有带 Idempotency-Replayed: true（实得 %q）—— "+
			"客户端分不清「我真的建了」与「这是上次那件」", got)
	}
	// ② 库里只有一行。**这一条才是真正的判据。**
	if n := adminQueryInt64(t,
		`SELECT count(*) FROM products WHERE merchant_id = $1 AND title = $2`,
		sh.MerchantID, title); n != 1 {
		t.Fatalf("库里有 %d 行标题是 %q 的商品，期望 1 —— "+
			"接口回的是同一个 id，但业务真的跑了两遍", n, title)
	}

	// 同一把钥匙配**不同**的请求体：422，而且绝不能被当成重放静默吞掉
	// （数据模型 §12 原话：那会让用户以为第二个请求生效了）。
	other := fmt.Sprintf(`{"category_id":%d,"title":"换了个标题 %s"}`, cat.Id, sh.Suffix)
	got := problemType(t, postWithKey(t, sh.Host, "/api/v1/admin/products", other, sh.Token, key),
		http.StatusUnprocessableEntity, "同一把钥匙配另一个请求体")
	if got != problem.TypeIdempotencyKeyReused {
		t.Errorf("Problem type 是 %q，期望 %q", got, problem.TypeIdempotencyKeyReused)
	}
	if n := adminQueryInt64(t,
		`SELECT count(*) FROM products WHERE merchant_id = $1 AND title LIKE $2`,
		sh.MerchantID, "换了个标题%"); n != 0 {
		t.Fatalf("回了 422，库里却建出了 %d 行 —— 拒绝发生在写之后", n)
	}
}

// 契约把 Idempotency-Key 定成**必填**，所以不带它是 422，不是「照常执行」。
//
// 它值得单独一条：把 service 那句 idemKey == "" 的检查删掉，上面那条测试
// 一个字都不会变（它每次都带着钥匙），而这条接口会安静地退回到不幂等。
func TestAdminWritesRequireAnIdempotencyKey(t *testing.T) {
	sh := newAdminShop(t)
	var cat api.AdminCategory
	decodeInto(t, postIdem(t, sh.Host, "/api/v1/admin/categories", `{"name":"缺钥匙靶子"}`, sh.Token),
		http.StatusCreated, "建类目", &cat)

	title := "没带钥匙 " + sh.Suffix
	// post 这个工具刻意不带 Idempotency-Key（见 postWithKey 上的注释）。
	w := post(t, sh.Host, "/api/v1/admin/products",
		fmt.Sprintf(`{"category_id":%d,"title":%q}`, cat.Id, title), sh.Token)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("不带 Idempotency-Key 回了 %d，期望 422（契约把它定成必填）：%s",
			w.Code, w.Body.String())
	}
	if n := adminQueryInt64(t,
		`SELECT count(*) FROM products WHERE merchant_id = $1 AND title = $2`,
		sh.MerchantID, title); n != 0 {
		t.Fatalf("回了 422，库里却建出了 %d 行", n)
	}
}

// 上传那一条的幂等有它自己的一道坎：字节必须先落盘，幂等键才抢得了
// （request_hash 要认「这次传的是不是同一个文件」）。于是重放那一路会多出一个
// 刚落盘的文件，而它**不会**被孤儿回收看见 —— 回收扫的是 uploads 表，
// 而这个文件没有对应的行。
//
// 这条测试盯的就是那句善后：删掉 service 里那句 store.Remove，
// 上面 id 相同的断言照样绿，而每重试一次磁盘上多一份 10 MB。
func TestUploadIdempotencyDoesNotLeakAnOrphanFile(t *testing.T) {
	sh := newAdminShop(t)
	content := []byte("同一张图重传不该多出一个文件 " + sh.Suffix)
	key := "aaaaaaaa-bbbb-cccc-dddd-" + sh.Suffix + "0000"

	var first, second api.Upload
	decodeInto(t, uploadImageWithKey(t, sh, "image/webp", content, key),
		http.StatusCreated, "第一次传", &first)
	before := countFilesUnder(t, testUploadRoot)

	w2 := uploadImageWithKey(t, sh, "image/webp", content, key)
	decodeInto(t, w2, http.StatusCreated, "带同一把钥匙再传一次", &second)

	if first.Id != second.Id {
		t.Fatalf("同一把钥匙传两次登记了两条（id=%d 与 %d）", first.Id, second.Id)
	}
	if got := w2.Header().Get("Idempotency-Replayed"); got != "true" {
		t.Errorf("重放没有带 Idempotency-Replayed: true（实得 %q）", got)
	}
	if after := countFilesUnder(t, testUploadRoot); after != before {
		t.Fatalf("重放之后上传根目录下从 %d 个文件变成了 %d 个 —— "+
			"多出来的那个既不在 uploads 表里、也就永远不会被孤儿回收看见", before, after)
	}
	// 第一次那个文件还在（重放删的必须是**这次**写的那个，不是上次那个）。
	assertUploadedFileMatches(t,
		adminQueryText(t, `SELECT storage_key FROM uploads WHERE id = $1`, first.Id), content)
}

// ---------------------------------------------------------------------------
// 上传的两道闸门
// ---------------------------------------------------------------------------

// content_type 不在允许列表内 → 415；文件是真的落在磁盘上的。
func TestUploadChecksMediaTypeAndActuallyStoresTheBytes(t *testing.T) {
	sh := newAdminShop(t)

	got := problemType(t, uploadImage(t, sh, "application/pdf", []byte("%PDF-1.4")),
		http.StatusUnsupportedMediaType, "传一个 PDF")
	if got != problem.TypeUploadUnsupportedMedia {
		t.Fatalf("Problem type 是 %q，期望 %q", got, problem.TypeUploadUnsupportedMedia)
	}

	content := []byte("RIFF....WEBPVP8 这是一段可验证长度的内容")
	var up api.Upload
	decodeInto(t, uploadImage(t, sh, "image/webp", content), http.StatusCreated, "传一张 webp", &up)
	if up.SizeBytes != int64(len(content)) {
		t.Fatalf("登记的 size_bytes 是 %d，实际内容是 %d 字节", up.SizeBytes, len(content))
	}
	if up.ContentType != "image/webp" {
		t.Fatalf("登记的 content_type 是 %q，期望 image/webp", up.ContentType)
	}

	// **字节真的在磁盘上。** 这一条是「没有假装文件存下来了」的判据：
	// 只登记元数据的实现会让 storage_key 指向一个不存在的文件，
	// 而那条记录与一条正常记录长得一模一样。
	key := adminQueryText(t, `SELECT storage_key FROM uploads WHERE id = $1`, up.Id)
	if key == "" {
		t.Fatal("uploads 行上没有 storage_key")
	}
	assertUploadedFileMatches(t, key, content)
}

// ---------------------------------------------------------------------------
// 夹具小工具
// ---------------------------------------------------------------------------

// seedPublishedProduct 走**真实接口**造一件在架商品 + 一个 SKU，返回两个 id。
//
// 走接口而不是直接插库：这些测试断言的是接口之间的关系（建完能上架、
// 上架之后删不掉），而直接插库造出来的状态可能是接口根本产生不了的。
func seedPublishedProduct(t *testing.T, sh adminShop, tag string, cents, qty int) (int64, int64) {
	t.Helper()
	var cat api.AdminCategory
	decodeInto(t, postIdem(t, sh.Host, "/api/v1/admin/categories",
		fmt.Sprintf(`{"name":"%s 类目"}`, tag), sh.Token),
		http.StatusCreated, tag+" 建类目", &cat)

	var p api.AdminProduct
	decodeInto(t, postIdem(t, sh.Host, "/api/v1/admin/products",
		fmt.Sprintf(`{"category_id":%d,"title":"%s 商品 %s"}`, cat.Id, tag, sh.Suffix), sh.Token),
		http.StatusCreated, tag+" 建商品", &p)

	var sku api.AdminSku
	decodeInto(t, postIdem(t, sh.Host, fmt.Sprintf("/api/v1/admin/products/%d/skus", p.Id),
		fmt.Sprintf(`{"sku_code":"%s-%s","price_cents":%d,"available_qty":%d}`,
			tag, sh.Suffix, cents, qty), sh.Token),
		http.StatusCreated, tag+" 建 SKU", &sku)

	wantStatus(t, postIdem(t, sh.Host, fmt.Sprintf("/api/v1/admin/products/%d/publication", p.Id),
		`{"action":"publish"}`, sh.Token), http.StatusOK, tag+" 上架")
	return p.Id, sku.Id
}

// seedOneSKU 造一件**草稿**商品加一个 SKU，只返回 SKU id。
// 草稿而不是在架：库存那组测试不该受「在架商品的最后一个 SKU」那条闸门影响。
func seedOneSKU(t *testing.T, sh adminShop, tag string, cents, qty int) int64 {
	t.Helper()
	var cat api.AdminCategory
	decodeInto(t, postIdem(t, sh.Host, "/api/v1/admin/categories",
		fmt.Sprintf(`{"name":"%s 类目"}`, tag), sh.Token),
		http.StatusCreated, tag+" 建类目", &cat)
	var p api.AdminProduct
	decodeInto(t, postIdem(t, sh.Host, "/api/v1/admin/products",
		fmt.Sprintf(`{"category_id":%d,"title":"%s 商品 %s"}`, cat.Id, tag, sh.Suffix), sh.Token),
		http.StatusCreated, tag+" 建商品", &p)
	var sku api.AdminSku
	decodeInto(t, postIdem(t, sh.Host, fmt.Sprintf("/api/v1/admin/products/%d/skus", p.Id),
		fmt.Sprintf(`{"sku_code":"%s-%s","price_cents":%d,"available_qty":%d}`,
			tag, sh.Suffix, cents, qty), sh.Token),
		http.StatusCreated, tag+" 建 SKU", &sku)
	return sku.Id
}

// ---------------------------------------------------------------------------
// 合规检查：拒绝的响应里要带得走「哪个字段、第几个字」
// ---------------------------------------------------------------------------

// 上架一件标题违规的商品：422 + compliance-rejected + errors[] 里有位置。
//
// 这一条走的是**整条真实链路**（HTTP → handler → service → 真检查器 →
// 事务回滚），它补的是 service/compliance_test.go 那一组补不上的一段：
// 那一组用假仓储证明了「命中就回滚、拒绝带着位置」，但证明不了
// handler 真的把 understanding.Violation 翻成了契约的 FieldError ——
// 而那一步翻错的症状是商家收到一个 422，里面 errors 是空的，
// 「拒绝」两个字又变回了不够用的那个样子。
//
// 同时它是「商品没有被上架」这件事在库里的唯一靶子：service 那一组断言的是
// 「事务没提交」（假仓储上的一个布尔），这里断言的是**真的库里 status 还是 0**。
func TestPublishingABannedTitleIsRejectedWithPositions(t *testing.T) {
	sh := newAdminShop(t)

	var cat api.AdminCategory
	decodeInto(t, post(t, sh.Host, "/api/v1/admin/categories",
		`{"name":"违禁词类目"}`, sh.Token), http.StatusCreated, "建类目", &cat)

	// 标题里塞一处违规：「最佳」。前缀「本店」是为了让 offset 不是 0 ——
	// offset 恒为 0 的实现（比如漏传那个字段）在 offset = 0 的用例上是绿的。
	var p api.AdminProduct
	decodeInto(t, post(t, sh.Host, "/api/v1/admin/products",
		fmt.Sprintf(`{"category_id":%d,"title":"本店最佳咖啡壶 %s","subtitle":"600ml 玻璃"}`,
			cat.Id, sh.Suffix), sh.Token),
		http.StatusCreated, "建违规草稿", &p)
	// **建草稿本身是 201**：草稿不查（compliance.go 文件头第一节）。
	// 这半句同时是下面那个 422 的阳性对照 —— 一个「哪里都查」的实现
	// 在上面这一步就会红。

	decodeInto(t, post(t, sh.Host, fmt.Sprintf("/api/v1/admin/products/%d/skus", p.Id),
		fmt.Sprintf(`{"sku_code":"BAN-%s","price_cents":9900,"available_qty":3}`, sh.Suffix),
		sh.Token), http.StatusCreated, "建 SKU", &api.AdminSku{})

	w := post(t, sh.Host, fmt.Sprintf("/api/v1/admin/products/%d/publication", p.Id),
		`{"action":"publish"}`, sh.Token)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("上架一件标题含「最佳」的商品回了 %d，期望 422 —— "+
			"设计 §2：命中违禁词时直接拒绝发布。响应体：%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type 是 %q，期望 application/problem+json —— "+
			"按契约生成的客户端会在它最需要读懂的那类响应上走错分支", ct)
	}

	var pb api.Problem
	decodeInto(t, w, http.StatusUnprocessableEntity, "合规拒绝", &pb)
	if pb.Type != problem.TypeComplianceRejected {
		t.Fatalf("Problem type 是 %q，期望 %q —— 客户端按它分辨"+
			"「改文案」与「退避重试」，而这两件事一个是死路一个是活路",
			pb.Type, problem.TypeComplianceRejected)
	}
	if pb.Errors == nil || len(*pb.Errors) != 1 {
		t.Fatalf("errors[] 是 %v，期望 1 条 —— 「拒绝」两个字不够，"+
			"商家得知道是哪个字段的第几个字（设计 §2：返回具体位置）", pb.Errors)
	}
	fe := (*pb.Errors)[0]
	if fe.Field == nil || *fe.Field != "title" {
		t.Errorf("errors[0].field 是 %v，期望 \"title\"", fe.Field)
	}
	if fe.Offset == nil || fe.Length == nil {
		t.Fatalf("errors[0] 缺 offset / length（%v / %v）—— "+
			"没有位置的话客户端只能把整个标题标红，而那与「拒绝」两个字一样没用",
			fe.Offset, fe.Length)
	}
	// 「本店最佳咖啡壶 …」：「最佳」在下标 2，长 2（Unicode 码点）。
	if *fe.Offset != 2 || *fe.Length != 2 {
		t.Errorf("errors[0] 的位置是 offset=%d length=%d，期望 2 / 2 —— "+
			"位置错了比没有位置更坏：客户端会高亮到一段没有问题的文字上",
			*fe.Offset, *fe.Length)
	}

	// **商品真的没有被上架。** 这一句是这条测试的另一半：一个「回了 422
	// 但事务照样提交」的实现，上面每一条断言都是绿的。
	if status := adminQueryInt64(t,
		`SELECT status FROM products WHERE id = $1`, p.Id); status != 0 {
		t.Fatalf("合规拒绝之后库里 products.status = %d，期望 0（还是草稿）—— "+
			"响应是 422，但商品已经上架了，而买家现在就看得到那个违禁标题", status)
	}
}
