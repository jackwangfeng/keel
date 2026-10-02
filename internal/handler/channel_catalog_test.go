package handler_test

// 第二期：Shopify 商品进 keel（打模拟平台 + 真实适配器 + 真库）。Review Focus 1 / 2 在这里。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/shopify"
	"github.com/keel/keel/internal/channel/shopify/shopifytest"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

type shopifyRig struct {
	channelRig
	sim  *shopifytest.Server
	cs   couponShop
	ctx  context.Context
	loc  string
	b    repository.ChannelBinding
	evts int
}

const simSecret = "sim-client-secret-123456"

// newShopifyRig 建一个停用的 Shopify binding（映射北店 ↔ 模拟店的 location），config 由调用方给。
func newShopifyRig(t *testing.T, config map[string]any) *shopifyRig {
	t.Helper()
	cs := newCouponShop(t)
	shop := fmt.Sprintf("s%d.myshopify.com", time.Now().UnixNano())
	sim := shopifytest.New(t, shop, "sim-client-id", simSecret)
	rig := newChannelRigWith(t, shopify.New(shopify.Options{BaseURL: sim.BaseURL}))
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	t.Cleanup(func() { adminExec(t, `DELETE FROM channel_merchants WHERE merchant_id = $1`, cs.MerchantID) })
	r := &shopifyRig{channelRig: rig, sim: sim, cs: cs, ctx: ctx, loc: sim.AddLocation("Shop location")}
	cfgJSON, _ := json.Marshal(config)
	b, err := rig.svc.CreateBinding(ctx, service.ChannelBindingCreate{Channel: shopify.Kind, ExternalAccount: shop,
		Name: "Shopify", Roles: channel.RoleCatalogSource | channel.RoleOutlet, Config: cfgJSON})
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := json.Marshal(map[string]string{"client_id": "sim-client-id", "client_secret": simSecret})
	if err := rig.svc.SetSecrets(ctx, b.ID, sec); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.UpsertStoreLink(ctx, repository.ChannelStoreLink{BindingID: b.ID, StoreID: cs.NorthStore, ExternalStoreID: r.loc}); err != nil {
		t.Fatal(err)
	}
	r.b = b
	return r
}

func (r *shopifyRig) activate(t *testing.T) {
	t.Helper()
	on := repository.ChannelBindingActive
	if _, err := r.svc.UpdateBinding(r.ctx, r.b.ID, service.ChannelBindingUpdate{Status: &on}); err != nil {
		t.Fatal(err)
	}
	r.drain(t)
}

func (r *shopifyRig) deactivate(t *testing.T) {
	t.Helper()
	off := repository.ChannelBindingDisabled
	if _, err := r.svc.UpdateBinding(r.ctx, r.b.ID, service.ChannelBindingUpdate{Status: &off}); err != nil {
		t.Fatal(err)
	}
	r.drain(t)
}

func (r *shopifyRig) drain(t *testing.T) {
	t.Helper()
	for i := 0; i < 5; i++ { // 推送之后的核对会再入队一轮
		if err := r.svc.Drain(r.ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// webhook 投一条签好名的回调并跑完。
func (r *shopifyRig) webhook(t *testing.T, topic string, body any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	r.evts++
	req := r.sim.WebhookRequest("/x", topic, fmt.Sprintf("evt-%d-%d", r.b.ID, r.evts), raw)
	if _, err := r.svc.Inbound(r.ctx, r.b.ID, req, raw); err != nil {
		t.Fatal(err)
	}
	r.drain(t)
}

func (r *shopifyRig) keelSKU(t *testing.T, variantGID string) int64 {
	t.Helper()
	return adminQueryInt64(t, `SELECT keel_id FROM channel_item_links WHERE binding_id = $1 AND kind = 2 AND external_id = $2`, r.b.ID, variantGID)
}

func (r *shopifyRig) stock(t *testing.T, sku int64) int64 {
	t.Helper()
	return adminQueryInt64(t, `SELECT coalesce((SELECT available_qty FROM inventories WHERE store_id = $1 AND sku_id = $2), -1)`, r.cs.NorthStore, sku)
}

func (r *shopifyRig) products(t *testing.T) int64 {
	return adminQueryInt64(t, `SELECT count(*) FROM products WHERE merchant_id = $1 AND deleted_at IS NULL`, r.cs.MerchantID)
}

// Review Focus 1：首接不清零；回调更新、加删变体、删商品。
func TestShopifyCatalogFirstPullKeepsShopifyStock(t *testing.T) {
	r := newShopifyRig(t, map[string]any{"default_category_id": 0})
	adminExec(t, `UPDATE channel_bindings SET config = jsonb_build_object('default_category_id', $1::bigint) WHERE id = $2`, r.cs.ChildCat, r.b.ID)
	pg := r.sim.AddProduct(shopifytest.Product{Title: "Shopify T 恤", Description: "<p>棉</p>", Variants: []shopifytest.Variant{
		{SKU: fmt.Sprintf("SH-A-%d", r.b.ID), Price: "19.90", Options: map[string]string{"颜色": "红"}, Tracked: true, Levels: map[string]int32{r.loc: 7}},
		{Price: "19.90", Options: map[string]string{"颜色": "蓝"}, Tracked: true, Levels: map[string]int32{r.loc: 2}},
	}})
	before := r.products(t)
	r.activate(t)
	vs := r.sim.VariantIDs(pg)
	a, bSKU := r.keelSKU(t, vs[0]), r.keelSKU(t, vs[1])
	if got := r.products(t); got != before+1 {
		t.Fatalf("keel 商品数 %d → %d，期望多 1 件", before, got)
	}
	if r.stock(t, a) != 7 || r.stock(t, bSKU) != 2 {
		t.Fatalf("keel 初始库存 = %d / %d，期望 7 / 2（取 Shopify 的数）", r.stock(t, a), r.stock(t, bSKU))
	}
	if q, _ := r.sim.Available(r.sim.InventoryItem(vs[0]), r.loc); q != 7 {
		t.Fatalf("Shopify 上被改成了 %d（首接把现货推掉了）", q)
	}
	if n := r.sim.Calls("SetQty"); n != 0 {
		t.Fatalf("首接推了 %d 次库存，期望 0（基线就是 Shopify 的数）", n)
	}
	code := adminQueryString(t, `SELECT sku_code FROM skus WHERE id = $1`, bSKU)
	if !strings.HasPrefix(code, "shopify-") {
		t.Fatalf("没有货号的变体在 keel 的货号是 %q，期望 shopify-<id>", code)
	}
	if st := adminQueryInt64(t, `SELECT p.status FROM products p JOIN skus s ON s.product_id = p.id WHERE s.id = $1`, a); st != 0 {
		t.Fatalf("新商品在 keel 的状态 %d，期望草稿 0（上架是 keel 的决定）", st)
	}

	t.Run("keel_改库存_推到_Shopify", func(t *testing.T) {
		adjust(t, r.local, r.cs.MerchantID, r.cs.NorthStore, a, -3)
		deadline := time.Now().Add(20 * time.Second)
		for {
			r.drain(t)
			if q, _ := r.sim.Available(r.sim.InventoryItem(vs[0]), r.loc); q == 4 {
				break
			}
			if time.Now().After(deadline) {
				q, _ := r.sim.Available(r.sim.InventoryItem(vs[0]), r.loc)
				t.Fatalf("keel 减 3 之后 Shopify 上是 %d，期望 4", q)
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	t.Run("回调_改标题_加变体", func(t *testing.T) {
		r.sim.SetTitle(pg, "Shopify T 恤（新）")
		vc := r.sim.AddVariant(pg, shopifytest.Variant{SKU: fmt.Sprintf("SH-C-%d", r.b.ID), Price: "21.00",
			Options: map[string]string{"颜色": "绿"}, Tracked: true, Levels: map[string]int32{r.loc: 4}})
		r.webhook(t, "products/update", map[string]any{"admin_graphql_api_id": pg})
		if got := adminQueryString(t, `SELECT p.title FROM products p JOIN skus s ON s.product_id = p.id WHERE s.id = $1`, a); got != "Shopify T 恤（新）" {
			t.Fatalf("标题没跟上：%q", got)
		}
		c := r.keelSKU(t, vc)
		if r.stock(t, c) != 4 {
			t.Fatalf("新变体的 keel 库存 = %d，期望 4", r.stock(t, c))
		}
	})

	t.Run("回调_删变体_SKU停售", func(t *testing.T) {
		r.sim.RemoveVariant(vs[1])
		r.webhook(t, "products/update", map[string]any{"admin_graphql_api_id": pg})
		if st := adminQueryInt64(t, `SELECT status FROM skus WHERE id = $1`, bSKU); st != 0 {
			t.Fatalf("删掉的变体在 keel 的 SKU 状态 %d，期望停售 0", st)
		}
		if n := adminQueryInt64(t, `SELECT count(*) FROM channel_item_links WHERE binding_id = $1 AND kind = 2 AND keel_id = $2`, r.b.ID, bSKU); n != 0 {
			t.Fatal("删掉的变体映射还在")
		}
	})

	t.Run("停用再启用_幂等", func(t *testing.T) {
		n := r.products(t)
		stockA := r.stock(t, a)
		r.deactivate(t)
		r.activate(t)
		if r.products(t) != n || r.stock(t, a) != stockA {
			t.Fatalf("重拉之后商品数 %d→%d、库存 %d→%d", n, r.products(t), stockA, r.stock(t, a))
		}
	})

	t.Run("回调_删商品_下架删映射", func(t *testing.T) {
		adminExec(t, `UPDATE products SET status = 1 WHERE id = (SELECT product_id FROM skus WHERE id = $1)`, a)
		r.sim.DeleteProduct(pg)
		r.webhook(t, "products/delete", map[string]any{"id": strings.TrimPrefix(pg, "gid://shopify/Product/")})
		if st := adminQueryInt64(t, `SELECT p.status FROM products p JOIN skus s ON s.product_id = p.id WHERE s.id = $1`, a); st == 1 {
			t.Fatal("商品源上删了，keel 里还在架上")
		}
		if n := adminQueryInt64(t, `SELECT count(*) FROM channel_item_links WHERE binding_id = $1`, r.b.ID); n != 0 {
			t.Fatalf("映射还剩 %d 条", n)
		}
	})
}

// Review Focus 2：keel 里已有同货号的 SKU → 认领，不新建商品、不覆盖 keel 库存，并把 keel 的数推出去。
func TestShopifyCatalogAdoptsExistingSKU(t *testing.T) {
	r := newShopifyRig(t, map[string]any{})
	adminExec(t, `UPDATE channel_bindings SET config = jsonb_build_object('default_category_id', $1::bigint) WHERE id = $2`, r.cs.ChildCat, r.b.ID)
	code := adminQueryString(t, `SELECT sku_code FROM skus WHERE id = $1`, r.cs.DressSKU)
	keelStock := r.stock(t, r.cs.DressSKU)
	if keelStock == 9 || keelStock < 0 {
		t.Fatalf("夹具不成立：连衣裙在北店的库存是 %d", keelStock)
	}
	pg := r.sim.AddProduct(shopifytest.Product{Title: "连衣裙（Shopify）", Variants: []shopifytest.Variant{
		{SKU: code, Price: "60.00", Tracked: true, Levels: map[string]int32{r.loc: 9}}}})
	before := r.products(t)
	r.activate(t)
	if r.products(t) != before {
		t.Fatalf("认领时新建了商品（%d → %d）", before, r.products(t))
	}
	v := r.sim.VariantIDs(pg)[0]
	if got := r.keelSKU(t, v); got != r.cs.DressSKU {
		t.Fatalf("变体映射到了 %d，期望认领 %d", got, r.cs.DressSKU)
	}
	if r.stock(t, r.cs.DressSKU) != keelStock {
		t.Fatalf("keel 库存被 Shopify 的数覆盖了：%d", r.stock(t, r.cs.DressSKU))
	}
	if q, _ := r.sim.Available(r.sim.InventoryItem(v), r.loc); int64(q) != keelStock {
		t.Fatalf("Shopify 上是 %d，期望被 keel 的 %d 覆盖", q, keelStock)
	}
}

func TestShopifyCatalogNeedsDefaultCategory(t *testing.T) {
	r := newShopifyRig(t, map[string]any{})
	r.sim.AddProduct(shopifytest.Product{Title: "x", Variants: []shopifytest.Variant{{SKU: fmt.Sprintf("NC-%d", time.Now().UnixNano()), Price: "1.00"}}})
	before := r.products(t)
	r.activate(t)
	if r.products(t) != before {
		t.Fatal("没配类目却建了商品")
	}
	msg := adminQueryString(t, `SELECT coalesce(max(last_error), '') FROM jobs WHERE merchant_id = $1 AND queue = 'channel.catalog.pull'`, r.cs.MerchantID)
	if !strings.Contains(msg, "default_category_id") {
		t.Fatalf("拉商品任务的错误 = %q，期望提到 default_category_id", msg)
	}
}

func TestShopifyWebhooksInstalledOnFirstPull(t *testing.T) {
	r := newShopifyRig(t, map[string]any{"webhook_base_url": "https://demo.test/"})
	adminExec(t, `UPDATE channel_bindings SET config = config || jsonb_build_object('default_category_id', $1::bigint) WHERE id = $2`, r.cs.ChildCat, r.b.ID)
	r.activate(t)
	want := fmt.Sprintf("https://demo.test/api/v1/webhooks/channels/%d", r.b.ID)
	ws := r.sim.Webhooks()
	if len(ws) != 5 {
		t.Fatalf("装了 %d 条订阅：%+v", len(ws), ws)
	}
	for _, w := range ws {
		if w.URI != want {
			t.Fatalf("订阅地址 %q，期望 %q", w.URI, want)
		}
	}
}

func adminQueryString(t *testing.T, sql string, args ...any) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var out string
	if err := conn.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// 1×1 的 PNG。
var onePixelPNG = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
	0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0xf8, 0xcf, 0xc0, 0xf0, 0x1f, 0x00, 0x05, 0x00, 0x01, 0xff, 0x89, 0x99,
	0x3d, 0x1d, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82}

func TestShopifyCatalogImages(t *testing.T) {
	r := newShopifyRig(t, map[string]any{})
	adminExec(t, `UPDATE channel_bindings SET config = jsonb_build_object('default_category_id', $1::bigint) WHERE id = $2`, r.cs.ChildCat, r.b.ID)
	r.svc.WithImages(service.NewLocalDiskStore(t.TempDir()), nil, func(u *url.URL) bool { return u.Hostname() == "127.0.0.1" })
	img1, img2 := r.sim.ImageURL("a.png", onePixelPNG), r.sim.ImageURL("b.png", onePixelPNG)
	pg := r.sim.AddProduct(shopifytest.Product{Title: "带图", Images: []string{img1, img2}, Variants: []shopifytest.Variant{
		{SKU: fmt.Sprintf("IMG-%d", r.b.ID), Price: "1.00", Tracked: true, Levels: map[string]int32{r.loc: 1}}}})
	r.activate(t)
	sku := r.keelSKU(t, r.sim.VariantIDs(pg)[0])
	pid := adminQueryInt64(t, `SELECT product_id FROM skus WHERE id = $1`, sku)
	imgs := func() int64 {
		return adminQueryInt64(t, `SELECT count(*) FROM product_images i JOIN uploads u ON u.id = i.upload_id
			WHERE i.product_id = $1 AND u.purpose = 1 AND u.referenced AND u.channel_binding_id = $2`, pid, r.b.ID)
	}
	if n := imgs(); n != 2 {
		t.Fatalf("商品图 %d 张，期望 2（purpose 1、已引用、上传者是 binding）", n)
	}
	hits := r.sim.ImageHits()

	t.Run("图没变_不重下", func(t *testing.T) {
		r.webhook(t, "products/update", map[string]any{"admin_graphql_api_id": pg})
		if r.sim.ImageHits() != hits {
			t.Fatalf("图没变又下载了 %d 次", r.sim.ImageHits()-hits)
		}
	})
	t.Run("换图_替换", func(t *testing.T) {
		r.sim.SetImages(pg, []string{img2})
		r.webhook(t, "products/update", map[string]any{"admin_graphql_api_id": pg})
		if n := imgs(); n != 1 {
			t.Fatalf("换成 1 张之后商品图 %d 张", n)
		}
	})
	t.Run("图下载失败_商品照样同步_图不动", func(t *testing.T) {
		r.sim.SetImages(pg, []string{r.sim.URL + "/images/missing.png"})
		r.sim.SetTitle(pg, "带图（改）")
		r.webhook(t, "products/update", map[string]any{"admin_graphql_api_id": pg})
		if got := adminQueryString(t, `SELECT title FROM products WHERE id = $1`, pid); got != "带图（改）" {
			t.Fatalf("图失败挡住了商品同步：标题 %q", got)
		}
		if n := imgs(); n != 1 {
			t.Fatalf("图失败之后商品图 %d 张，期望保持 1", n)
		}
	})
	t.Run("不放行的主机_不下载", func(t *testing.T) {
		before := r.sim.ImageHits()
		r.sim.SetImages(pg, []string{"https://evil.example/x.png"})
		r.webhook(t, "products/update", map[string]any{"admin_graphql_api_id": pg})
		if r.sim.ImageHits() != before || imgs() != 1 {
			t.Fatal("不放行的地址被下载或替换了图")
		}
	})
}
