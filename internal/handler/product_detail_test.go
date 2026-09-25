package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/keel/keel/internal/api"
)

// 商品详情（GET /products/{product_id}）的行为测试。
//
// 核对事实一律走管理员连接直接读 products / skus / inventories，不靠再打一次
// HTTP：后者只能证明同一段代码前后自洽（这个仓库反复踩到的正是这个）。

// productDetail 打一次详情并解成契约类型。
func productDetail(t *testing.T, host string, id int64) (int, api.ProductDetail, []byte) {
	t.Helper()
	w := do(t, host, "/api/v1/products/"+strconv.FormatInt(id, 10))
	var d api.ProductDetail
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
			t.Fatalf("详情响应不是 ProductDetail: %v\n%s", err, w.Body.String())
		}
	}
	return w.Code, d, w.Body.Bytes()
}

// productIDOf 按商家 code + 标题取商品 id（绕过 RLS）。
//
// 不写死 id：种子里的 id 是自增的，跟加载顺序走（惯例同 skuIDOf）。
func productIDOf(t *testing.T, merchantCode, title string) int64 {
	t.Helper()
	var id int64
	err := admin(t).QueryRow(context.Background(), `
		SELECT p.id FROM products p
		  JOIN merchants m ON m.id = p.merchant_id
		 WHERE m.code = $1 AND p.title = $2`, merchantCode, title).Scan(&id)
	if err != nil {
		t.Fatalf("取 %s 的商品 %q 失败: %v", merchantCode, title, err)
	}
	return id
}

// skuStockOf 绕过 RLS 读一件商品全部在售 SKU 的 (sku_code → 水位)。
//
// LEFT JOIN 与详情那条查询一致：种子里 SKU-NOSTOCKROW 没有库存行，
// 而它必须以「在售、可售 0 件」的形式出现在两边，否则这条对照本身就是错的。
func skuStockOf(t *testing.T, productID int64) map[string]int32 {
	t.Helper()
	rows, err := admin(t).Query(context.Background(), `
		SELECT s.sku_code, COALESCE(i.available_qty, 0)
		  FROM skus s LEFT JOIN inventories i ON i.sku_id = s.id
		 WHERE s.product_id = $1 AND s.status = 1`, productID)
	if err != nil {
		t.Fatalf("读商品 %d 的 SKU 水位失败: %v", productID, err)
	}
	defer rows.Close()
	out := map[string]int32{}
	for rows.Next() {
		var code string
		var qty int32
		if err := rows.Scan(&code, &qty); err != nil {
			t.Fatal(err)
		}
		out[code] = qty
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// 详情带全部在售 SKU，而且每个 SKU 的水位与库里逐个对得上。
//
// 断言「逐个对得上」而不是「skus 非空」：非空那条对着一个把 available_qty 恒返
// 0（或者恒返 999）的实现照样绿，而客户端拿这个数做数量步进器的上限。
func TestProductDetailCarriesEverySellableSKUWithItsRealStock(t *testing.T) {
	id := productIDOf(t, "shop-a", "shop-a 的商品 1")
	want := skuStockOf(t, id)

	// 阳性对照：种子里这件商品挂着两个在售 SKU（SKU-<id> 与 SKU-NOSTOCKROW），
	// 其中一个有货、一个没有库存行。少于两个说明种子变了，
	// 下面那些断言会退化成「一个 SKU 对得上」。
	if len(want) < 2 {
		t.Fatalf("库里这件商品只有 %d 个在售 SKU（%v）—— 这条测试的区分力来自"+
			"「有货的」与「没有库存行的」两种 SKU 同时在场", len(want), want)
	}

	code, d, raw := productDetail(t, hostA, id)
	if code != http.StatusOK {
		t.Fatalf("详情失败：%d %s", code, raw)
	}
	// description 是详情比列表多出来的东西之一，而且它有真实数据来源
	// （种子里播了）。断言它非空，这一条把「详情只是把列表那行原样重发一遍」
	// 这种实现挡在外面。
	if d.Description == nil || *d.Description == "" {
		t.Fatal("详情里没有 description —— products.description 在种子里是有值的，" +
			"缺席说明这条路径根本没读那一列")
	}
	if len(d.Skus) != len(want) {
		t.Fatalf("详情给了 %d 个 SKU，库里有 %d 个（%v）", len(d.Skus), len(want), want)
	}
	for _, s := range d.Skus {
		q, ok := want[s.SkuCode]
		if !ok {
			t.Fatalf("详情里出现了库里没有的 SKU %q", s.SkuCode)
		}
		if int32(s.AvailableQty) != q {
			t.Fatalf("SKU %s 的 available_qty 是 %d，库里是 %d —— "+
				"这个数是客户端数量步进器的上限，错了用户会选一个买不到的数量",
				s.SkuCode, s.AvailableQty, q)
		}
		if s.PriceCents <= 0 {
			t.Fatalf("SKU %s 的 price_cents 是 %d —— 阳性对照失败", s.SkuCode, s.PriceCents)
		}
	}

	// in_stock 是算出来的，不是 products.total_stock 那一列。这件商品里
	// 至少有一个 SKU 有货，所以它必须是 true，而且必须**出现**在响应里。
	if d.InStock == nil {
		t.Fatal("响应里没有 in_stock —— 契约里它是可选字段，" +
			"但详情既然已经把每个 SKU 的水位都查出来了，留空就只是懒")
	}
	if !*d.InStock {
		t.Fatalf("in_stock 是 false，而库里的水位是 %v", want)
	}
	t.Logf("详情返回 %d 个 SKU，水位 %v，in_stock=%v", len(d.Skus), want, *d.InStock)
}

// 全部 SKU 都是 0 时 in_stock 必须是 false。
//
// 没有这一条，上一条测试对着一个「in_stock 恒为 true」的实现是绿的 ——
// 而那个实现在生产上的样子是：每一件售罄的商品都显示「有货」，
// 用户点进去加购，在下单那一步才被 409 打回来。
//
// 靶子是现造的（把 shop-b 某件商品的水位清零，测完还原）：种子里没有一件
// 全部 SKU 都为 0 的商品，而为它在种子里固定播一件，会让下单测试
// anySKUWithStock 那个「挑一个有货的」助手多一件永远挑不中的商品。
func TestProductDetailSaysOutOfStockWhenEverySKUIsZero(t *testing.T) {
	id := productIDOf(t, "shop-b", "shop-b 的商品 1")
	ctx := context.Background()
	conn := admin(t)

	var saved []struct {
		SKUID int64
		Qty   int32
	}
	rows, err := conn.Query(ctx, `
		SELECT i.sku_id, i.available_qty FROM inventories i
		  JOIN skus s ON s.id = i.sku_id WHERE s.product_id = $1`, id)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var one struct {
			SKUID int64
			Qty   int32
		}
		if err := rows.Scan(&one.SKUID, &one.Qty); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		saved = append(saved, one)
	}
	rows.Close()
	if len(saved) == 0 {
		t.Fatalf("商品 %d 一行库存都没有 —— 这条测试没有靶子", id)
	}

	// 先确认清零之前它确实是 true。没有这个前后对比，「清零之后是 false」
	// 与「它恒为 false」分不开。
	if _, before, _ := productDetail(t, hostB, id); before.InStock == nil || !*before.InStock {
		t.Fatalf("清零之前 in_stock 就不是 true（%v）—— 这条测试没有区分力", before.InStock)
	}

	t.Cleanup(func() {
		for _, one := range saved {
			if _, err := conn.Exec(context.Background(),
				`UPDATE inventories SET available_qty = $2 WHERE sku_id = $1`,
				one.SKUID, one.Qty); err != nil {
				t.Errorf("还原 sku %d 的水位失败: %v —— 后面的测试会看到一个被掏空的商品",
					one.SKUID, err)
			}
		}
	})
	for _, one := range saved {
		if _, err := conn.Exec(ctx,
			`UPDATE inventories SET available_qty = 0 WHERE sku_id = $1`, one.SKUID); err != nil {
			t.Fatal(err)
		}
	}

	code, d, raw := productDetail(t, hostB, id)
	if code != http.StatusOK {
		t.Fatalf("详情失败：%d %s", code, raw)
	}
	if d.InStock == nil {
		t.Fatal("全部 SKU 水位都是 0，而响应里连 in_stock 都没有")
	}
	if *d.InStock {
		// 打值而不是打指针：变异验证时这条信息是唯一的线索，
		// 而一个 0x6d0ca010580 说明不了任何事（实测：第一次跑这条变异，
		// 它红了，但红出来的那行读不出 in_stock 到底是什么）。
		t.Fatalf("全部 SKU 水位都是 0，in_stock 却是 %v", *d.InStock)
	}
	for _, s := range d.Skus {
		if s.AvailableQty != 0 {
			t.Fatalf("SKU %s 的 available_qty 是 %d，应该是 0", s.SkuCode, s.AvailableQty)
		}
	}
}

// 草稿商品与软删商品在详情上也看不见。
//
// 列表那条（TestDraftAndDeletedProductsAreInvisible）守的是同一条规矩的另一半。
// 两条都要：只守列表的话，详情就是一条「列表里看不见、知道 id 就点得进去」的
// 后门，而商品 id 是自增的 —— 从 1 数到 100 就能把全店的草稿商品翻一遍。
func TestDraftAndDeletedProductsAreInvisibleOnDetailToo(t *testing.T) {
	for _, title := range []string{"shop-a 的草稿商品", "shop-a 的已删商品"} {
		id := productIDOf(t, "shop-a", title)
		code, _, raw := productDetail(t, hostA, id)
		if code != http.StatusNotFound {
			t.Fatalf("%s（id=%d）的详情回了 %d，期望 404：%s", title, id, code, raw)
		}
	}

	// 阳性对照：同样的路径形状对一件在架商品必须是 200。
	// 没有它，「两个 404」也可能只是因为这条路由根本没挂上。
	ok := productIDOf(t, "shop-a", "shop-a 的商品 1")
	if code, _, raw := productDetail(t, hostA, ok); code != http.StatusOK {
		t.Fatalf("阳性对照失败：在架商品 %d 的详情回了 %d：%s", ok, code, raw)
	}
}

// 拿别家店的 product_id 打过来是 404，不是 200。
//
// 这条与列表那条跨租户测试不同：列表压根不需要客户端提供 id，而详情是
// **客户端指定一个自增 id**。RLS 是这里唯一挡着的东西（查询里没有
// WHERE merchant_id，那是 check_query_tenancy.py 的硬规矩），
// 所以这条测试就是 RLS 在详情这条路径上的直接验证。
func TestProductDetailIsNotReachableAcrossTenants(t *testing.T) {
	id := productIDOf(t, "shop-b", "shop-b 的商品 1")

	// 阳性对照先跑：这个 id 在它自己家必须是 200。
	// 反过来的话，「在 A 店拿到 404」可能只是因为这个 id 根本不存在。
	if code, _, raw := productDetail(t, hostB, id); code != http.StatusOK {
		t.Fatalf("阳性对照失败：shop-b 的商品 %d 在 shop-b 上回了 %d：%s", id, code, raw)
	}
	code, _, raw := productDetail(t, hostA, id)
	if code != http.StatusNotFound {
		t.Fatalf("shop-b 的商品 %d 在 shop-a 上回了 %d —— 跨租户读到了别家的商品：%s",
			id, code, raw)
	}
}

// 详情不声称自己有图片。
//
// image_url 与 images 在契约里都声明了，而 products 表上没有任何图片列。
// 回空串 / 空数组会让客户端渲染一个「加载失败」的占位图或者一个「无图」的轮播，
// 而那与「这件商品确实没有配图」是两件事。
//
// 这条同时是 contract_test.go 那份挂账的反向守卫：真的实现了商品图，
// 它会红，逼人回去把那两行挂账删掉。
func TestProductDetailDoesNotClaimImagesItDoesNotHave(t *testing.T) {
	id := productIDOf(t, "shop-a", "shop-a 的商品 1")
	code, _, raw := productDetail(t, hostA, id)
	if code != http.StatusOK {
		t.Fatalf("详情失败：%d %s", code, raw)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	r := routeOf(t, http.MethodGet, "/products/{product_id}")
	for _, field := range []string{"image_url", "images"} {
		if _, present := m[field]; present {
			t.Fatalf("响应里出现了 %s（%s）—— 商品图真的实现了？"+
				"那就把 contract_test.go 里 /products/{product_id} 的 "+
				"NotYetImplementedResponse 对应那一行删掉", field, m[field])
		}
		if _, listed := r.NotYetImplementedResponse[field]; !listed {
			t.Fatalf("NotYetImplementedResponse 里没有 %s —— 挂账清单烂了", field)
		}
	}

	// 阳性对照：别的字段必须在。整个响应是空对象的话，上面那两句是废话。
	for _, field := range []string{"id", "title", "skus", "in_stock", "description"} {
		if _, ok := m[field]; !ok {
			t.Fatalf("响应里连 %s 都没有 —— 这条断言没有区分力：%s", field, raw)
		}
	}
}
