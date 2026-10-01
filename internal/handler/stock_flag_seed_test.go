package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/keel/keel/internal/service"
)

// 新门店建好就补齐有货排序标记（2026-09-30，宝安中心区店）：当时标记表缺行的商品按「有没有在售 SKU」排进
// 「有货」那一段（2026-10-01 起缺行按无货），新店一行都没有，于是它所有商品都排在前面、而库存一件都没设。跨 0 消息管不到从没设过
// 库存的 SKU，全量刷新又降到了每小时 —— 所以建店时当场按实际水位写一遍。
func TestNewStoreGetsStockFlagsImmediately(t *testing.T) {
	sh := newCouponShop(t)
	store := createStore(t, sh.adminShop, sh.NorthRegion, "seed", "新开的店", 116.41, 39.91)

	var rows, inStock int
	if err := admin(t).QueryRow(context.Background(),
		`SELECT count(*), count(*) FILTER (WHERE in_stock) FROM product_store_stock WHERE store_id = $1`, store).
		Scan(&rows, &inStock); err != nil {
		t.Fatal(err)
	}
	if rows < 2 {
		t.Fatalf("新门店应当场有每件在售商品的标记（夹具至少两件），实得 %d 行", rows)
	}
	if inStock != 0 {
		t.Fatalf("新门店一件库存都没设，标记应全是无货，实得 %d 件有货", inStock)
	}

	// 给新店设一件的库存：这件排到最前面，而不是和一堆没货的混在「有货」段里。
	w := putAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/inventory", store, sh.DressSKU),
		`{"available_qty":3}`, sh.Token)
	if w.Code != http.StatusOK {
		t.Fatalf("设库存 %d：%s", w.Code, w.Body.String())
	}
	var list struct {
		Items []struct {
			ID      int64 `json:"id"`
			InStock bool  `json:"in_stock"`
		} `json:"items"`
	}
	decodeInto(t, getNoAuth(t, sh.Host, fmt.Sprintf("/api/v1/products?store_id=%d&page_size=20", store)),
		http.StatusOK, "新店商品列表", &list)
	if len(list.Items) == 0 || list.Items[0].ID != sh.DressProduct || !list.Items[0].InStock {
		t.Fatalf("唯一有货的商品应排第一，实得 %+v", list.Items)
	}
	for _, it := range list.Items[1:] {
		if it.InStock {
			t.Fatalf("除了设过库存的那件，其余都该是无货：%+v", list.Items)
		}
	}
}

// 缺行按无货（2026-10-01，破坏性测试 P2）：标记表里缺行的商品排进无货段、in_stock_only 不返回它、
// total 按同一段数。之前缺行按「有没有在售 SKU」算（≈ 有货），新店种标记失败的那段时间里整店商品排进
// 有货段，in_stock_only 返回一屏 in_stock:false。
func TestMissingStockFlagRowCountsAsOutOfStock(t *testing.T) {
	cs := newCouponShop(t)
	type page struct {
		Items []struct {
			ID      int64 `json:"id"`
			InStock *bool `json:"in_stock"`
		} `json:"items"`
		Total int64 `json:"total"`
	}
	list := func(query string) page {
		t.Helper()
		var p page
		decodeInto(t, getNoAuth(t, cs.Host, fmt.Sprintf("/api/v1/products?store_id=%d&page_size=50%s", cs.NorthStore, query)),
			http.StatusOK, "商品列表"+query, &p)
		return p
	}
	pos := func(p page, id int64) int {
		for i, it := range p.Items {
			if it.ID == id {
				return i
			}
		}
		return -1
	}
	// 夹具：北京门店连衣裙、衬衫都有货，衬衫晚建排在前（TestProductListPutsInStockFirst）。
	if p := list(""); pos(p, cs.ShirtProduct) > pos(p, cs.DressProduct) {
		t.Fatalf("前提不成立：都有货时衬衫应在连衣裙前面：%+v", p.Items)
	}
	adminExec(t, `DELETE FROM product_store_stock WHERE store_id = $1 AND product_id = $2`, cs.NorthStore, cs.ShirtProduct)

	all := list("")
	if s, d := pos(all, cs.ShirtProduct), pos(all, cs.DressProduct); s < 0 || d < 0 || s < d {
		t.Fatalf("缺行的衬衫应排进无货段（连衣裙之后），实得衬衫 %d、连衣裙 %d", s, d)
	}
	only := list("&in_stock_only=true")
	if pos(only, cs.ShirtProduct) >= 0 {
		t.Fatalf("in_stock_only=true 返回了缺行的衬衫：%+v", only.Items)
	}
	if pos(only, cs.DressProduct) < 0 {
		t.Fatalf("in_stock_only=true 没有返回有货的连衣裙：%+v", only.Items)
	}
	flagged := adminQueryInt64(t, `SELECT count(*) FROM product_store_stock pss JOIN products p ON p.id = pss.product_id
		WHERE pss.store_id = $1 AND pss.in_stock AND p.status = 1 AND p.deleted_at IS NULL`, cs.NorthStore)
	if only.Total != flagged || int64(len(only.Items)) != flagged {
		t.Fatalf("in_stock_only 的 total=%d、本页 %d 行，期望都等于有货标记为真的在架商品数 %d",
			only.Total, len(only.Items), flagged)
	}
	if all.Total <= only.Total {
		t.Fatalf("不带 in_stock_only 的 total=%d 应当大于只看有货的 %d", all.Total, only.Total)
	}
}

// 库存服务不在时新建门店（2026-10-01，破坏性测试 P2）：当场种标记失败，进库存 outbox 的队列重试；
// 库存服务回来之后 worker 一跑就补齐，不等整点的全量刷新。
func TestStoreStockFlagSeedRetriedWhenInventoryReturns(t *testing.T) {
	cs := newCouponShop(t)
	e := newTwoDB(t)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, tbl := range []string{"inventory_logs", "activity_stocks", "inventories"} {
			e.invAdmin.Exec(ctx, `DELETE FROM `+tbl+` WHERE merchant_id = $1`, cs.MerchantID)
		}
	})
	e.moveStock(t, cs.MerchantID)
	e.use(t)

	e.down.Store(true)
	store := createStore(t, cs.adminShop, cs.NorthRegion, "seedretry", "库存服务不在时开的店", 116.42, 39.92)
	e.down.Store(false)
	rows := func() int64 {
		return adminQueryInt64(t, `SELECT count(*) FROM product_store_stock WHERE store_id = $1`, store)
	}
	if n := rows(); n != 0 {
		t.Fatalf("库存服务不在，新店不该种上标记，实得 %d 行", n)
	}
	key := fmt.Sprintf("stock_flags:store:%d", store)
	if n := adminQueryInt64(t, `SELECT count(*) FROM jobs WHERE queue = $1 AND job_key = $2 AND status = 0`,
		service.QueueInventoryRelease, key); n != 1 {
		t.Fatalf("种标记失败后应入队一条重试任务 %s，实得 %d 条", key, n)
	}
	rep, err := e.outbox.Drain(context.Background())
	if err != nil || rep.Done == 0 {
		t.Fatalf("库存服务回来之后重试任务应当跑成：%+v %v", rep, err)
	}
	if n := rows(); n < 2 {
		t.Fatalf("重试之后新店应有每件在售商品的标记（夹具至少两件），实得 %d 行", n)
	}
}
