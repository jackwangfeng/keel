package handler_test

// 第二期：全渠道一个价（Caps.PricePerStore = false，Shopify）时只有价格源门店推价格；keel 里改价之后重算。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

func TestChannelPriceFromPriceStoreOnly(t *testing.T) {
	cs := newCouponShop(t)
	rig := newChannelRig(t)
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	b := activeFakeBinding(t, rig, cs)
	if err := rig.svc.UpsertStoreLink(ctx, repository.ChannelStoreLink{BindingID: b.ID, StoreID: cs.SouthStore, ExternalStoreID: "loc-s"}); err != nil {
		t.Fatal(err)
	}
	priceStore, other := cs.NorthStore, cs.SouthStore
	if other < priceStore {
		priceStore, other = other, priceStore
	}
	setPrice := func(store, cents int64) {
		t.Helper()
		adminExec(t, `INSERT INTO store_sku_prices (merchant_id, store_id, sku_id, price_cents) VALUES ($1, $2, $3, $4)
			ON CONFLICT (store_id, sku_id) DO UPDATE SET price_cents = EXCLUDED.price_cents`, cs.MerchantID, store, cs.DressSKU, cents)
		rig.svc.SKUsChanged(ctx, []int64{cs.DressSKU})
	}
	waitPrice := func(store, want int64, what string) {
		t.Helper()
		deadline := time.Now().Add(channelWaitWindow)
		for {
			if err := rig.svc.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			if p, _ := rig.fake.LastPrice(store, cs.DressSKU); p == want {
				return
			}
			if time.Now().After(deadline) {
				p, ok := rig.fake.LastPrice(store, cs.DressSKU)
				t.Fatalf("%s：门店 %d 渠道上的价格是 %d（推过=%v），期望 %d", what, store, p, ok, want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	pendingFor := func(store int64) int64 {
		return adminQueryInt64(t, `SELECT count(*) FROM jobs WHERE merchant_id = $1 AND queue = 'channel.listing.push'
			AND status IN (0, 1) AND (payload->>'store_id')::bigint = $2`, cs.MerchantID, store)
	}
	// 新门店映射之后先把它的那一格推完（可售数）。
	if err := rig.svc.RecomputeListings(ctx, other, []int64{cs.DressSKU}, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.Drain(ctx); err != nil {
		t.Fatal(err)
	}

	t.Run("价格源门店改价_推价格", func(t *testing.T) {
		setPrice(priceStore, 7777)
		waitPrice(priceStore, 7777, "价格源门店改价之后")
	})
	t.Run("另一家改价_不入推送任务", func(t *testing.T) {
		setPrice(other, 8888)
		if n := pendingFor(other); n != 0 {
			t.Fatalf("非价格源门店改价入了 %d 条推送任务", n)
		}
		if p, ok := rig.fake.LastPrice(other, cs.DressSKU); ok {
			t.Fatalf("非价格源门店推过价格 %d", p)
		}
	})
	t.Run("非价格源门店推数量_不把价格记成已推", func(t *testing.T) {
		// 只推了数量（PushPrice=false）：published_cents 不能记成 8888，否则下面换价格源之后会判成「价格没变」。
		adminExec(t, `UPDATE channel_listings SET published_qty = published_qty + 1 WHERE binding_id = $1 AND store_id = $2`, b.ID, other)
		if err := rig.svc.RecomputeListings(ctx, other, []int64{cs.DressSKU}, b.ID); err != nil {
			t.Fatal(err)
		}
		if err := rig.svc.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if c := adminQueryInt64(t, `SELECT published_cents FROM channel_listings WHERE binding_id = $1 AND store_id = $2 AND sku_id = $3`,
			b.ID, other, cs.DressSKU); c == 8888 {
			t.Fatal("非价格源门店推数量时把没推过的价格记成了已推")
		}
	})
	t.Run("config_改价格源门店_改用那家的价", func(t *testing.T) {
		cfg, _ := json.Marshal(map[string]int64{"price_store_id": other})
		if _, err := rig.svc.UpdateBinding(ctx, b.ID, service.ChannelBindingUpdate{Config: cfg}); err != nil {
			t.Fatal(err)
		}
		waitPrice(other, 8888, "改价格源门店之后")
	})
	t.Run("按门店定价的渠道_两家都推", func(t *testing.T) {
		rig.fake.CapsValue.PricePerStore = true
		setPrice(priceStore, 6666)
		waitPrice(priceStore, 6666, "按门店定价时")
	})
}

// 商品源 binding（如 Shopify）不看 keel 的上架状态：keel 里没上架的商品照样推真实可售数，不推 0。
// 非商品源 binding 照旧：下架 → 推 0。
func TestChannelCatalogSourceIgnoresKeelPublication(t *testing.T) {
	cs := newCouponShop(t)
	rig := newChannelRig(t)
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	b := activeFakeBinding(t, rig, cs)
	adminExec(t, `UPDATE products SET status = 0 WHERE id = $1`, cs.DressProduct)
	rig.svc.ProductChanged(ctx, cs.DressProduct)
	rig.waitPushed(t, ctx, cs.NorthStore, cs.DressSKU, 0, "非商品源 binding、商品下架之后")

	both := channel.RoleCatalogSource | channel.RoleOutlet
	rig.fake.CapsValue.Roles = both
	if _, err := rig.svc.UpdateBinding(ctx, b.ID, service.ChannelBindingUpdate{Roles: &both}); err != nil {
		t.Fatal(err)
	}
	rig.svc.ProductChanged(ctx, cs.DressProduct)
	rig.waitPushed(t, ctx, cs.NorthStore, cs.DressSKU, availOf(t, cs), "商品源 binding、商品没上架")
}
