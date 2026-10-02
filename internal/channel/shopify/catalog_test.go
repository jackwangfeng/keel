package shopify_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/shopify/shopifytest"
)

func TestPullCatalogAllPages(t *testing.T) {
	r := newRig(t)
	loc1, loc2 := r.sim.AddLocation("A"), r.sim.AddLocation("B")
	r.sim.AddProduct(shopifytest.Product{Title: "礼品卡", GiftCard: true, Variants: []shopifytest.Variant{{Price: "10.00"}}})
	shirt := r.sim.AddProduct(shopifytest.Product{Title: "T 恤", Description: "<p>棉</p>", Images: []string{"https://cdn.shopify.com/a.png", "https://cdn.shopify.com/b.png"},
		Variants: []shopifytest.Variant{
			{SKU: "TS-R-M", Price: "99.50", Options: map[string]string{"颜色": "红", "尺码": "M"}, Tracked: true, Levels: map[string]int32{loc1: 7, loc2: 1}},
			{Price: "99.50", Options: map[string]string{"颜色": "蓝", "尺码": "M"}, Tracked: false, Levels: map[string]int32{loc1: 0}},
		}})
	r.sim.AddProduct(shopifytest.Product{Title: "草稿", Status: "DRAFT", Variants: []shopifytest.Variant{{SKU: "D-1", Price: "5", Tracked: true, Levels: map[string]int32{loc1: 3}}}})

	var items []channel.CatalogItem
	cursor, pages := "", 0
	for {
		p, err := r.a.PullCatalog(context.Background(), r.b, cursor)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		items = append(items, p.Items...)
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if pages != 2 || len(items) != 2 {
		t.Fatalf("%d 页 %d 件，期望 2 页 2 件（礼品卡滤掉）", pages, len(items))
	}
	ts := items[0]
	if ts.ExternalID != shirt || ts.Title != "T 恤" || ts.Description != "<p>棉</p>" || ts.Status != channel.CatalogActive {
		t.Fatalf("商品 = %+v", ts)
	}
	if len(ts.ImageURLs) != 2 || ts.ImageURLs[0] != "https://cdn.shopify.com/a.png" {
		t.Fatalf("图片 = %v", ts.ImageURLs)
	}
	v := ts.Variants[0]
	if v.SKUCode != "TS-R-M" || v.PriceCents != 9950 || v.Options["颜色"] != "红" || v.Options["尺码"] != "M" {
		t.Fatalf("变体 = %+v", v)
	}
	if len(v.Levels) != 2 {
		t.Fatalf("水位 = %+v", v.Levels)
	}
	var ex struct {
		InventoryItemID string `json:"inventory_item_id"`
		ProductID       string `json:"product_id"`
		Tracked         bool   `json:"tracked"`
	}
	_ = json.Unmarshal(v.Extra, &ex)
	if ex.InventoryItemID == "" || ex.ProductID != shirt || !ex.Tracked {
		t.Fatalf("extra = %s", v.Extra)
	}
	_ = json.Unmarshal(ts.Variants[1].Extra, &ex)
	if ts.Variants[1].SKUCode != "" || ex.Tracked {
		t.Fatalf("第二个变体（无货号、不跟踪）= %+v %s", ts.Variants[1], ts.Variants[1].Extra)
	}
	if items[1].Status != channel.CatalogDraft || len(items[1].Variants[0].Options) != 0 {
		t.Fatalf("草稿商品 = %+v（单规格 Default Title 应为空 map）", items[1])
	}
}

func TestPullItemFoundAndDeleted(t *testing.T) {
	r := newRig(t)
	g := r.sim.AddProduct(shopifytest.Product{Title: "x", Variants: []shopifytest.Variant{{SKU: "X", Price: "1.00"}}})
	it, found, err := r.a.PullItem(context.Background(), r.b, g)
	if err != nil || !found || it.Title != "x" {
		t.Fatalf("PullItem = %+v %v %v", it, found, err)
	}
	r.sim.DeleteProduct(g)
	_, found, err = r.a.PullItem(context.Background(), r.b, g)
	if err != nil || found {
		t.Fatalf("删了之后 PullItem found=%v err=%v", found, err)
	}
}
