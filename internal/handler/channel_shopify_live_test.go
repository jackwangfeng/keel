package handler_test

// 对真实开发店的全链路联调（KEEL_SHOPIFY_LIVE=1 才跑，凭据读 ~/.config/keel/shopify-dev，不打印）：
// 建 binding（开发店的 Shop location ↔ 北店）→ 启用 → 拉商品 → keel 库存等于 Shopify 上的数 →
// keel 改库存 +1 → Shopify 上跟着 +1 → keel 再 −1 → Shopify 还原。结束时开发店的数与开始时一致。

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/shopify"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

func liveShopifyCreds(t *testing.T) map[string]string {
	t.Helper()
	if os.Getenv("KEEL_SHOPIFY_LIVE") != "1" {
		t.Skip("KEEL_SHOPIFY_LIVE=1 才对真实开发店跑")
	}
	home, _ := os.UserHomeDir()
	f, err := os.Open(filepath.Join(home, ".config/keel/shopify-dev"))
	if err != nil {
		t.Fatalf("读不到开发店凭据：%v", err)
	}
	defer f.Close()
	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok {
			kv[k] = strings.Trim(v, `"'`)
		}
	}
	return kv
}

func TestShopifyLiveEndToEnd(t *testing.T) {
	kv := liveShopifyCreds(t)
	cs := newCouponShop(t)
	adapter := shopify.New(shopify.Options{})
	rig := newChannelRigWith(t, adapter)
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	t.Cleanup(func() { adminExec(t, `DELETE FROM channel_merchants WHERE merchant_id = $1`, cs.MerchantID) })

	sec, _ := json.Marshal(map[string]string{"client_id": kv["SHOPIFY_CLIENT_ID"], "client_secret": kv["SHOPIFY_CLIENT_SECRET"]})
	ab := channel.Binding{ID: -1, ExternalAccount: kv["SHOPIFY_SHOP"], Secrets: sec}
	// 找 Shop location：拉一页商品，取第一个跟踪库存的变体所在的 location。
	page, err := adapter.PullCatalog(ctx, ab, "")
	if err != nil {
		t.Fatal(err)
	}
	var loc string
	for _, it := range page.Items {
		for _, v := range it.Variants {
			if len(v.Levels) > 0 && strings.Contains(string(v.Extra), `"tracked":true`) {
				loc = v.Levels[0].ExternalStoreID
				break
			}
		}
		if loc != "" {
			break
		}
	}
	if loc == "" {
		t.Fatal("开发店第一页没有跟踪库存的变体")
	}
	cfg, _ := json.Marshal(map[string]int64{"default_category_id": cs.ChildCat})
	b, err := rig.svc.CreateBinding(ctx, service.ChannelBindingCreate{Channel: shopify.Kind, ExternalAccount: kv["SHOPIFY_SHOP"],
		Name: "开发店", Roles: channel.RoleCatalogSource | channel.RoleOutlet, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.SetSecrets(ctx, b.ID, sec); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.UpsertStoreLink(ctx, repository.ChannelStoreLink{BindingID: b.ID, StoreID: cs.NorthStore, ExternalStoreID: loc}); err != nil {
		t.Fatal(err)
	}
	on := repository.ChannelBindingActive
	if _, err := rig.svc.UpdateBinding(ctx, b.ID, service.ChannelBindingUpdate{Status: &on}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := rig.svc.Drain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM jobs WHERE merchant_id = $1 AND queue LIKE 'channel.%' AND status IN (0, 3)`, cs.MerchantID); n != 0 {
		msg := adminQueryString(t, `SELECT coalesce(string_agg(queue || ': ' || coalesce(last_error, ''), ' | '), '') FROM jobs
			WHERE merchant_id = $1 AND queue LIKE 'channel.%' AND status IN (0, 3)`, cs.MerchantID)
		t.Fatalf("拉完商品还有 %d 条渠道任务没做完：%s", n, msg)
	}
	links := adminQueryInt64(t, `SELECT count(*) FROM channel_item_links WHERE binding_id = $1 AND kind = 2`, b.ID)
	t.Logf("拉进 %d 件商品、%d 个规格", adminQueryInt64(t, `SELECT count(*) FROM channel_item_links WHERE binding_id = $1 AND kind = 1`, b.ID), links)

	// 挑一个跟踪库存、在这个 location 有水位的规格。
	var sku int64
	var extra, variant string
	conn, err := pgx.Connect(context.Background(), db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if err := conn.QueryRow(context.Background(), `SELECT keel_id, extra::text, external_id FROM channel_item_links
		WHERE binding_id = $1 AND kind = 2 AND extra->>'tracked' = 'true'
		  AND EXISTS (SELECT 1 FROM inventories i WHERE i.sku_id = keel_id AND i.store_id = $2)
		ORDER BY keel_id LIMIT 1`, b.ID, cs.NorthStore).Scan(&sku, &extra, &variant); err != nil {
		t.Fatalf("挑规格：%v", err)
	}
	var ex struct {
		InventoryItemID string `json:"inventory_item_id"`
	}
	_ = json.Unmarshal([]byte(extra), &ex)
	shopQty := func() int32 {
		q, ok, err := adapter.Available(ctx, ab, ex.InventoryItemID, loc)
		if err != nil || !ok {
			t.Fatalf("回读 Shopify：%v %v", ok, err)
		}
		return q
	}
	start := shopQty()
	keelStart := adminQueryInt64(t, `SELECT available_qty FROM inventories WHERE store_id = $1 AND sku_id = $2`, cs.NorthStore, sku)
	if int64(start) != keelStart {
		t.Fatalf("拉完商品 keel 库存 %d、Shopify 上 %d，应当相等", keelStart, start)
	}
	waitShop := func(want int32, what string) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for {
			if err := rig.svc.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			if q := shopQty(); q == want {
				return
			} else if time.Now().After(deadline) {
				t.Fatalf("%s：Shopify 上是 %d，期望 %d", what, q, want)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	defer func() {
		// 无论成败都把开发店的数还原。
		if q := shopQty(); q != start {
			adjust(t, rig.local, cs.MerchantID, cs.NorthStore, sku, int32(start-q))
			waitShop(start, "还原")
		}
	}()
	adjust(t, rig.local, cs.MerchantID, cs.NorthStore, sku, +1)
	waitShop(start+1, "keel +1 之后")
	adjust(t, rig.local, cs.MerchantID, cs.NorthStore, sku, -1)
	waitShop(start, "keel −1 之后")
	t.Logf("变体 %s：Shopify 上 %d → %d → %d，已还原", variant, start, start+1, start)
}
