package handler_test

// 挂零时段（00330，docs/superpowers/specs/2026-10-03-ai-channel-allocation-design.md §3.2）：推送成功的事务里记渠道上
// 挂 0 的起止，分「keel 有货但规则算 0」（held）与「keel 自己没货」。推送失败不写；渠道开关关着时不写。

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/channeltest"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// zeroSpans 把一个格子的挂零时段按开段先后写成 "held:open" 列表，如 "true:false,false:true"。
func zeroSpans(t *testing.T, binding, store, sku int64) string {
	t.Helper()
	return adminQueryString(t, `SELECT COALESCE(string_agg(held::text || ':' || (ended_at IS NULL)::text, ',' ORDER BY id), '')
		  FROM channel_listing_zero_spans WHERE binding_id = $1 AND store_id = $2 AND sku_id = $3`, binding, store, sku)
}

func waitZeroSpans(t *testing.T, ctx context.Context, rig channelRig, binding, store, sku int64, want, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := rig.svc.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		got := zeroSpans(t, binding, store, sku)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s：等了 20 秒挂零时段是 %q，期望 %q", what, got, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestChannelZeroSpans(t *testing.T) {
	cs := newCouponShop(t)
	rig := newChannelRig(t)
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	t.Cleanup(func() { adminExec(t, `DELETE FROM channel_merchants WHERE merchant_id = $1`, cs.MerchantID) })
	store, sku := cs.NorthStore, cs.DressSKU
	avail := func() int32 {
		return int32(adminQueryInt64(t, `SELECT available_qty FROM inventories WHERE store_id = $1 AND sku_id = $2`, store, sku))
	}

	b, err := rig.svc.CreateBinding(ctx, service.ChannelBindingCreate{Channel: channeltest.Kind, ExternalAccount: "zero-1",
		Name: "假渠道", Roles: channel.RoleOutlet})
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := json.Marshal(channeltest.Secrets{WebhookSecret: "k"})
	if err := rig.svc.SetSecrets(ctx, b.ID, sec); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.UpsertStoreLink(ctx, repository.ChannelStoreLink{BindingID: b.ID, StoreID: store, ExternalStoreID: "loc-1"}); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.LinkSKU(ctx, repository.ChannelItemLink{BindingID: b.ID, KeelID: sku, ExternalID: "var-1"}); err != nil {
		t.Fatal(err)
	}
	active := repository.ChannelBindingActive
	if _, err := rig.svc.UpdateBinding(ctx, b.ID, service.ChannelBindingUpdate{Status: &active}); err != nil {
		t.Fatal(err)
	}
	if avail() <= 0 {
		t.Fatalf("夹具不成立：北店连衣裙可售 %d", avail())
	}
	rig.waitPushed(t, ctx, store, sku, avail(), "启用之后")
	if got := zeroSpans(t, b.ID, store, sku); got != "" {
		t.Fatalf("推的是正数却记了挂零时段 %q", got)
	}
	setRatio := func(bp int32) {
		t.Helper()
		if _, err := rig.svc.UpsertStockRule(ctx, repository.ChannelStockRule{BindingID: b.ID, RatioBP: bp}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("规则算0_开一段held", func(t *testing.T) {
		setRatio(0)
		rig.waitPushed(t, ctx, store, sku, 0, "比例调到 0 之后")
		waitZeroSpans(t, ctx, rig, b.ID, store, sku, "true:true", "比例调到 0 之后")
	})

	t.Run("再推0_held不变_不新开", func(t *testing.T) {
		before := len(rig.fake.Pushes())
		// 上次推送记成失败 → 重算一定再推一次同样的 0。
		adminExec(t, `UPDATE channel_listings SET last_error = 'x' WHERE binding_id = $1 AND store_id = $2 AND sku_id = $3`, b.ID, store, sku)
		if err := rig.svc.RecomputeListings(ctx, store, []int64{sku}, 0); err != nil {
			t.Fatal(err)
		}
		if err := rig.svc.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if len(rig.fake.Pushes()) == before {
			t.Fatal("夹具不成立：没有再推一次")
		}
		if got := zeroSpans(t, b.ID, store, sku); got != "true:true" {
			t.Fatalf("再推 0 之后挂零时段 %q，期望仍是一段", got)
		}
	})

	t.Run("keel断货_关held段开非held段", func(t *testing.T) {
		adjust(t, rig.local, cs.MerchantID, store, sku, -avail())
		waitZeroSpans(t, ctx, rig, b.ID, store, sku, "true:false,false:true", "keel 断货之后")
	})

	t.Run("keel补货但规则仍算0_关旧开新", func(t *testing.T) {
		adjust(t, rig.local, cs.MerchantID, store, sku, 3)
		waitZeroSpans(t, ctx, rig, b.ID, store, sku, "true:false,false:false,true:true", "补货之后")
	})

	t.Run("推正数_关段", func(t *testing.T) {
		setRatio(10000)
		rig.waitPushed(t, ctx, store, sku, avail(), "比例调回之后")
		waitZeroSpans(t, ctx, rig, b.ID, store, sku, "true:false,false:false,true:false", "比例调回之后")
	})

	t.Run("推送失败_不写", func(t *testing.T) {
		rig.fake.FailNext(1)
		setRatio(0)
		deadline := time.Now().Add(20 * time.Second)
		for rig.fake.FailuresLeft() != 0 {
			if err := rig.svc.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			if time.Now().After(deadline) {
				t.Fatal("等了 20 秒编排的失败还没用掉")
			}
			time.Sleep(20 * time.Millisecond)
		}
		if got := zeroSpans(t, b.ID, store, sku); got != "true:false,false:false,true:false" {
			t.Fatalf("推送失败却写了挂零时段：%q", got)
		}
		rig.waitPushed(t, ctx, store, sku, 0, "失败重试之后")
		waitZeroSpans(t, ctx, rig, b.ID, store, sku, "true:false,false:false,true:false,true:true", "失败重试之后")
	})

	// 上线前就是 0（或段被清理掉了）：上次成功推的是 0、这次还是 0，但没有挂着的段 → 重算也要补推一次 0 开段。
	t.Run("一直是0但没有挂着的段_补开", func(t *testing.T) {
		adminExec(t, `DELETE FROM channel_listing_zero_spans WHERE binding_id = $1 AND store_id = $2 AND sku_id = $3 AND ended_at IS NULL`,
			b.ID, store, sku)
		if err := rig.svc.RecomputeListings(ctx, store, []int64{sku}, 0); err != nil {
			t.Fatal(err)
		}
		waitZeroSpans(t, ctx, rig, b.ID, store, sku, "true:false,false:false,true:false,true:true", "没有挂着的段、重算之后")
		before := len(rig.fake.Pushes())
		if err := rig.svc.RecomputeListings(ctx, store, []int64{sku}, 0); err != nil {
			t.Fatal(err)
		}
		if err := rig.svc.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if n := len(rig.fake.Pushes()); n != before {
			t.Fatalf("段已经开了，再重算不该再推（多推了 %d 次）", n-before)
		}
	})

	repo := repository.New(testPool)
	t.Run("连推0_0_5_0_两段_第一段已关", func(t *testing.T) {
		cell := cs.ShirtSKU
		t0 := time.Now().Add(-10 * time.Hour).Truncate(time.Second)
		for i, q := range []int32{0, 0, 5, 0} {
			if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
				return tx.RecordChannelListingZero(ctx, b.ID, store, cell, nil, q, true, t0.Add(time.Duration(i)*time.Hour))
			}); err != nil {
				t.Fatal(err)
			}
		}
		if got := zeroSpans(t, b.ID, store, cell); got != "true:false,true:true" {
			t.Fatalf("0、0、5、0 之后挂零时段 %q，期望两段、第一段已关", got)
		}
	})

	t.Run("按窗口汇总_open段截到to", func(t *testing.T) {
		cell := cs.ShirtSKU
		other := cs.SouthStore
		t0 := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
		// 南店衬衫：held [t0, t0+2h)，非 held [t0+2h, 还挂着)。
		for _, step := range []struct {
			q    int32
			held bool
			at   time.Duration
		}{{0, true, 0}, {0, false, 2 * time.Hour}} {
			if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
				return tx.RecordChannelListingZero(ctx, b.ID, other, cell, nil, step.q, step.held, t0.Add(step.at))
			}); err != nil {
				t.Fatal(err)
			}
		}
		var hs []repository.ChannelZeroHours
		if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
			var e error
			hs, e = tx.ChannelZeroHours(ctx, other, []int64{cell}, t0.Add(time.Hour), t0.Add(5*time.Hour))
			return e
		}); err != nil {
			t.Fatal(err)
		}
		if len(hs) != 1 || hs[0].BindingID != b.ID || hs[0].SKUID != cell ||
			math.Abs(hs[0].HeldHours-1) > 1e-6 || math.Abs(hs[0].EmptyHours-3) > 1e-6 {
			t.Fatalf("窗口汇总 = %+v，期望 held 1 小时、没货 3 小时", hs)
		}
		// 窗口在全部时段之前：没有行。
		if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
			var e error
			hs, e = tx.ChannelZeroHours(ctx, other, []int64{cell}, t0.Add(-5*time.Hour), t0)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		if len(hs) != 0 {
			t.Fatalf("窗口在时段之前却汇总出 %+v", hs)
		}
	})

	t.Run("清理_只删早于cutoff已结束的段", func(t *testing.T) {
		total := adminQueryInt64(t, `SELECT count(*) FROM channel_listing_zero_spans WHERE binding_id = $1`, b.ID)
		closedOld := adminQueryInt64(t, `SELECT count(*) FROM channel_listing_zero_spans WHERE binding_id = $1
			AND ended_at < now() - interval '1 hour'`, b.ID)
		if closedOld == 0 {
			t.Fatal("夹具不成立：没有一小时之前结束的段")
		}
		var n int64
		if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
			var e error
			n, e = tx.PurgeChannelZeroSpans(ctx, time.Now().Add(-time.Hour), 1000)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		left := adminQueryInt64(t, `SELECT count(*) FROM channel_listing_zero_spans WHERE binding_id = $1`, b.ID)
		if n != closedOld || left != total-closedOld {
			t.Fatalf("清理删了 %d 条、剩 %d 条；期望删 %d、剩 %d", n, left, closedOld, total-closedOld)
		}
	})
}

// 渠道开关关着：库存怎么变都不写挂零时段（不变量「不配渠道零开销」）。
func TestChannelZeroSpansOffByDefault(t *testing.T) {
	cs := newCouponShop(t)
	rig := newChannelStockRig(t, false)
	rig.setMerchant(t, cs.MerchantID, true, 1)
	adjust(t, rig.local, cs.MerchantID, cs.NorthStore, cs.DressSKU,
		-int32(adminQueryInt64(t, `SELECT available_qty FROM inventories WHERE store_id = $1 AND sku_id = $2`, cs.NorthStore, cs.DressSKU)))
	time.Sleep(200 * time.Millisecond)
	if n := adminQueryInt64(t, `SELECT count(*) FROM channel_listing_zero_spans WHERE merchant_id = $1`, cs.MerchantID); n != 0 {
		t.Fatalf("渠道开关关着却写了 %d 段挂零时段", n)
	}
}

// 审查修复 3：格子不再算（SKU 映射删了、门店映射删了、binding 停用）时，还挂着的段在同一事务里关掉，
// 不然以后的复盘 / 分配建议会把它一直算成挂零。
func TestChannelZeroSpansClosedWhenCellGoesAway(t *testing.T) {
	cs := newCouponShop(t)
	rig := newChannelRig(t)
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	t.Cleanup(func() { adminExec(t, `DELETE FROM channel_merchants WHERE merchant_id = $1`, cs.MerchantID) })
	b, err := rig.svc.CreateBinding(ctx, service.ChannelBindingCreate{Channel: channeltest.Kind, ExternalAccount: "zero-gone",
		Name: "假渠道", Roles: channel.RoleOutlet})
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := json.Marshal(channeltest.Secrets{WebhookSecret: "k"})
	if err := rig.svc.SetSecrets(ctx, b.ID, sec); err != nil {
		t.Fatal(err)
	}
	for i, st := range []int64{cs.NorthStore, cs.SouthStore} {
		if err := rig.svc.UpsertStoreLink(ctx, repository.ChannelStoreLink{BindingID: b.ID, StoreID: st,
			ExternalStoreID: fmt.Sprintf("loc-%d", i+1)}); err != nil {
			t.Fatal(err)
		}
	}
	for i, sku := range []int64{cs.DressSKU, cs.ShirtSKU} {
		if err := rig.svc.LinkSKU(ctx, repository.ChannelItemLink{BindingID: b.ID, KeelID: sku, ExternalID: fmt.Sprintf("var-%d", i+1)}); err != nil {
			t.Fatal(err)
		}
	}
	active := repository.ChannelBindingActive
	if _, err := rig.svc.UpdateBinding(ctx, b.ID, service.ChannelBindingUpdate{Status: &active}); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	repo := repository.New(testPool)
	at := time.Now().Add(-10 * time.Hour)
	cells := [][2]int64{{cs.NorthStore, cs.DressSKU}, {cs.NorthStore, cs.ShirtSKU}, {cs.SouthStore, cs.DressSKU}}
	for _, c := range cells {
		if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
			return tx.RecordChannelListingZero(ctx, b.ID, c[0], c[1], nil, 0, true, at)
		}); err != nil {
			t.Fatal(err)
		}
	}
	open := func() string {
		return adminQueryString(t, `SELECT COALESCE(string_agg(store_id || '/' || sku_id, ',' ORDER BY store_id, sku_id), '')
			  FROM channel_listing_zero_spans WHERE binding_id = $1 AND ended_at IS NULL`, b.ID)
	}
	want := func(cs ...[2]int64) string {
		var parts []string
		for _, c := range cs {
			parts = append(parts, fmt.Sprintf("%d/%d", c[0], c[1]))
		}
		return strings.Join(parts, ",")
	}
	nd, sd := cells[0], cells[2]
	if cs.SouthStore < cs.NorthStore {
		t.Skip("夹具假设北店 id 小于南店")
	}

	// SKU 映射删了：它在每家门店的段都关。
	if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		return tx.DeleteChannelItemLink(ctx, b.ID, repository.ChannelItemSKU, cs.ShirtSKU)
	}); err != nil {
		t.Fatal(err)
	}
	if got := open(); got != want(nd, sd) {
		t.Fatalf("删 SKU 映射之后还挂着的段 %q，期望 %q", got, want(nd, sd))
	}
	// 门店映射删了。
	if err := rig.svc.DeleteStoreLink(ctx, b.ID, cs.SouthStore); err != nil {
		t.Fatal(err)
	}
	if got := open(); got != want(nd) {
		t.Fatalf("删门店映射之后还挂着的段 %q，期望 %q", got, want(nd))
	}
	// binding 停用。
	off := repository.ChannelBindingDisabled
	if _, err := rig.svc.UpdateBinding(ctx, b.ID, service.ChannelBindingUpdate{Status: &off}); err != nil {
		t.Fatal(err)
	}
	if got := open(); got != "" {
		t.Fatalf("停用之后还挂着的段 %q，期望全关", got)
	}
	// 关在「现在」：截到那一刻的小时数约 10，以后不再涨。
	var hs []repository.ChannelZeroHours
	if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		hs, e = tx.ChannelZeroHours(ctx, cs.NorthStore, []int64{cs.DressSKU}, at.Add(-time.Hour), time.Now().Add(48*time.Hour))
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(hs) != 1 || math.Abs(hs[0].HeldHours-10) > 0.1 {
		t.Fatalf("停用后挂零小时数 %+v，期望约 10（关在停用那一刻）", hs)
	}
}
