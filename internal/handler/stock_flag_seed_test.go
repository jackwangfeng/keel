package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// 新门店建好就补齐有货排序标记（2026-09-30，宝安中心区店）：标记表缺行的商品按「有没有在售 SKU」排进
// 「有货」那一段，新店一行都没有，于是它所有商品都排在前面、而库存一件都没设。跨 0 消息管不到从没设过
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
