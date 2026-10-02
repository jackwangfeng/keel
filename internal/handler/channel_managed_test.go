package handler_test

// 商品源管理的字段（标题、详情、图片、SKU 规格）在后台标「由 … 管理」并锁住（渠道管理页 Task 2）。
// 商品用第二期的 newShopifyRig 从模拟 Shopify 拉进来；改动走测试服务器的后台接口（同一个库、同一家店）。

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/channel/shopify/shopifytest"
)

const typeManagedByChannel = "https://keel.dev/problems/managed-by-channel"

func TestChannelManagedProductFields(t *testing.T) {
	r := newShopifyRig(t, map[string]any{"default_category_id": 0})
	adminExec(t, `UPDATE channel_bindings SET config = jsonb_build_object('default_category_id', $1::bigint) WHERE id = $2`, r.cs.ChildCat, r.b.ID)
	pg := r.sim.AddProduct(shopifytest.Product{Title: "Shopify 帽子", Description: "<p>毛线</p>", Variants: []shopifytest.Variant{
		{SKU: fmt.Sprintf("SH-M-%d", r.b.ID), Price: "29.90", Options: map[string]string{"颜色": "黑"}, Tracked: true, Levels: map[string]int32{r.loc: 3}},
	}})
	r.activate(t)
	sku := r.keelSKU(t, r.sim.VariantIDs(pg)[0])
	pid := adminQueryInt64(t, `SELECT product_id FROM skus WHERE id = $1`, sku)
	host, tok := r.cs.Host, r.cs.Token
	prod := fmt.Sprintf("%s/admin/products/%d", v1, pid)
	skuPath := fmt.Sprintf("%s/admin/skus/%d", v1, sku)

	locked := func(t *testing.T, method, path, body, what string) {
		t.Helper()
		w := reqAs(t, method, host, path, body, tok)
		if got := problemType(t, w, http.StatusConflict, what); got != typeManagedByChannel {
			t.Fatalf("%s：type = %s，期望 managed-by-channel", what, got)
		}
		if !strings.Contains(strings.ToLower(w.Body.String()), "shopify") {
			t.Fatalf("%s：detail 没说是哪个渠道：%s", what, w.Body.String())
		}
	}
	ok := func(t *testing.T, method, path, body, what string) {
		t.Helper()
		if w := reqAs(t, method, host, path, body, tok); w.Code != http.StatusOK {
			t.Fatalf("%s：%d %s，期望 200", what, w.Code, w.Body.String())
		}
	}
	detail := func(t *testing.T, id int64) api.AdminProductDetail {
		t.Helper()
		var d api.AdminProductDetail
		decodeInto(t, reqAs(t, http.MethodGet, host, fmt.Sprintf("%s/admin/products/%d", v1, id), "", tok), http.StatusOK, "商品详情", &d)
		return d
	}
	listed := func(t *testing.T, id int64) api.AdminProduct {
		t.Helper()
		var page struct{ Items []api.AdminProduct }
		decodeInto(t, reqAs(t, http.MethodGet, host, v1+"/admin/products?page_size=100", "", tok), http.StatusOK, "商品列表", &page)
		for _, p := range page.Items {
			if p.Id == id {
				return p
			}
		}
		t.Fatalf("商品列表里没有 %d", id)
		return api.AdminProduct{}
	}

	t.Run("锁住的字段_409", func(t *testing.T) {
		locked(t, http.MethodPatch, prod, `{"title":"商家改的标题"}`, "PATCH title")
		locked(t, http.MethodPatch, prod, `{"description":"商家改的详情"}`, "PATCH description")
		locked(t, http.MethodPatch, prod, `{"title":"x","category_id":1}`, "PATCH title + category_id")
		locked(t, http.MethodPatch, skuPath, `{"spec_values":{"颜色":"白"}}`, "PATCH sku spec_values")
		locked(t, http.MethodPut, prod+"/images", `{"images":[{"upload_id":999999999}]}`, "PUT images（换图）")
		ok(t, http.MethodPut, prod+"/images", `{"images":[]}`, "PUT images 原样回传（Shopify 上这件没图）")
		if got := adminQueryString(t, `SELECT title FROM products WHERE id = $1`, pid); got != "Shopify 帽子" {
			t.Fatalf("被拒之后标题成了 %q", got)
		}
	})

	t.Run("不锁的字段_200", func(t *testing.T) {
		var p api.AdminProduct
		decodeInto(t, reqAs(t, http.MethodPatch, host, prod, fmt.Sprintf(`{"category_id":%d,"subtitle":"副标题照改"}`, r.cs.ParentCat), tok),
			http.StatusOK, "PATCH category_id", &p)
		if p.ManagedBy == nil || *p.ManagedBy != "shopify" {
			t.Fatalf("PATCH 回显 managed_by = %v，期望 shopify", p.ManagedBy)
		}
		ok(t, http.MethodPatch, skuPath, `{"price_cents":3500}`, "PATCH sku price_cents")
		ok(t, http.MethodPatch, skuPath, `{"weight_gram":200,"status":1}`, "PATCH sku weight/status")
		// 原样回传当前值（AI 员工 / MCP 发整份请求体）不算改：放行。
		ok(t, http.MethodPatch, prod, `{"title":"Shopify 帽子","description":"<p>毛线</p>","subtitle":"整份回传"}`, "PATCH 原样回传 title/description")
		ok(t, http.MethodPatch, skuPath, `{"spec_values":{"颜色":"黑"},"price_cents":3600}`, "PATCH sku 原样回传 spec_values")
	})

	t.Run("详情与列表标出_自建商品不标", func(t *testing.T) {
		if d := detail(t, pid); d.ManagedBy == nil || *d.ManagedBy != "shopify" {
			t.Fatalf("详情 managed_by = %v，期望 shopify", d.ManagedBy)
		}
		if p := listed(t, pid); p.ManagedBy == nil || *p.ManagedBy != "shopify" {
			t.Fatalf("列表 managed_by = %v，期望 shopify", p.ManagedBy)
		}
		if d := detail(t, r.cs.DressProduct); d.ManagedBy != nil {
			t.Fatalf("自建商品详情 managed_by = %q，期望 null", *d.ManagedBy)
		}
		if p := listed(t, r.cs.DressProduct); p.ManagedBy != nil {
			t.Fatalf("自建商品列表 managed_by = %q，期望 null", *p.ManagedBy)
		}
		ok(t, http.MethodPatch, fmt.Sprintf("%s/admin/products/%d", v1, r.cs.DressProduct), `{"title":"自建商品改标题"}`, "自建商品 PATCH title")
		ok(t, http.MethodPatch, fmt.Sprintf("%s/admin/skus/%d", v1, r.cs.DressSKU), `{"spec_values":{"尺码":"M"}}`, "自建 SKU PATCH spec_values")
	})

	t.Run("停用渠道即放开", func(t *testing.T) {
		r.deactivate(t)
		ok(t, http.MethodPatch, prod, `{"title":"停用之后商家改的"}`, "停用后 PATCH title")
		ok(t, http.MethodPut, prod+"/images", `{"images":[]}`, "停用后 PUT images")
		ok(t, http.MethodPatch, skuPath, `{"spec_values":{"颜色":"白"}}`, "停用后 PATCH spec_values")
		if d := detail(t, pid); d.ManagedBy != nil {
			t.Fatalf("停用后详情 managed_by = %q，期望 null", *d.ManagedBy)
		}
		if p := listed(t, pid); p.ManagedBy != nil {
			t.Fatalf("停用后列表 managed_by = %q，期望 null", *p.ManagedBy)
		}
	})
}
