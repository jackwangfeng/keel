package handler_test

// 第一期最终审查发现的问题的复现（C1 推送期间的变化丢失、C2 并发启停、I1 缺 offer 按 0 元推、
// I2 重算与回调任务重试太少、I3 开关消息被回查抢先作废不补发、M2 CAS 冲突后新价格推不出去）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/channeltest"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// activeFakeBinding 建一个启用中的假渠道 binding，映射北店与连衣裙 SKU，并等第一次整店推送完成。
func activeFakeBinding(t *testing.T, rig channelRig, cs couponShop) repository.ChannelBinding {
	t.Helper()
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	t.Cleanup(func() { adminExec(t, `DELETE FROM channel_merchants WHERE merchant_id = $1`, cs.MerchantID) })
	b, err := rig.svc.CreateBinding(ctx, service.ChannelBindingCreate{Channel: channeltest.Kind,
		ExternalAccount: fmt.Sprintf("acct-%d", time.Now().UnixNano()), Name: "假渠道", Roles: channel.RoleOutlet})
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := json.Marshal(channeltest.Secrets{WebhookSecret: "k"})
	if err := rig.svc.SetSecrets(ctx, b.ID, sec); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.UpsertStoreLink(ctx, repository.ChannelStoreLink{BindingID: b.ID, StoreID: cs.NorthStore, ExternalStoreID: "loc"}); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.LinkSKU(ctx, repository.ChannelItemLink{BindingID: b.ID, KeelID: cs.DressSKU, ExternalID: "var"}); err != nil {
		t.Fatal(err)
	}
	active := repository.ChannelBindingActive
	if b, err = rig.svc.UpdateBinding(ctx, b.ID, service.ChannelBindingUpdate{Status: &active}); err != nil {
		t.Fatal(err)
	}
	rig.waitPushed(t, ctx, cs.NorthStore, cs.DressSKU, availOf(t, cs), "启用之后")
	// 等库存服务收到「这家店开了渠道」：在那之前改库存，闸门会把「没开」缓存 30 秒、不发 stock.changed。
	deadline := time.Now().Add(15 * time.Second)
	for adminQueryInt64(t, `SELECT count(*) FROM channel_merchants WHERE merchant_id = $1 AND enabled`, cs.MerchantID) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("等了 15 秒库存服务仍未记下这家商家开了渠道")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return b
}

func availOf(t *testing.T, cs couponShop) int32 {
	return int32(adminQueryInt64(t, `SELECT available_qty FROM inventories WHERE store_id = $1 AND sku_id = $2`, cs.NorthStore, cs.DressSKU))
}

// C1：推送进行中（任务 status=1）发生的变化，入队会被同 key 的在途任务挡掉；推完必须再核对一次。
func TestChannelPushDoesNotLoseChangesDuringPush(t *testing.T) {
	cs := newCouponShop(t)
	rig := newChannelRig(t)
	activeFakeBinding(t, rig, cs)
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	fired := false
	rig.fake.OnPush = func([]channel.Listing) {
		if fired {
			return
		}
		fired = true
		// 推送进行中库存又变了：直接改库（不经通知器），再同步跑一次 stock.changed 会做的重算 —— 它的入队撞上在途任务。
		adminExec(t, `UPDATE inventories SET available_qty = available_qty - 3 WHERE store_id = $1 AND sku_id = $2`, cs.NorthStore, cs.DressSKU)
		if err := rig.svc.RecomputeListings(ctx, cs.NorthStore, []int64{cs.DressSKU}, 0); err != nil {
			t.Error(err)
		}
	}
	adjust(t, rig.local, cs.MerchantID, cs.NorthStore, cs.DressSKU, -1)
	// 期望值每轮现取：推送进行中那次变化发生在 waitPushed 开始之后。
	deadline := time.Now().Add(channelWaitWindow)
	for {
		if err := rig.svc.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		want := availOf(t, cs)
		if q, ok := rig.fake.LastQty(cs.NorthStore, cs.DressSKU); fired && ok && q == want {
			return
		}
		if time.Now().After(deadline) {
			q, _ := rig.fake.LastQty(cs.NorthStore, cs.DressSKU)
			t.Fatalf("推送期间又变了一次之后，渠道上停在 %d，库存是 %d（钩子触发=%v）", q, want, fired)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// C2：启停 binding 按商家串行化 —— 别的事务拿着这家店的渠道锁时，启用要等它提交。
func TestChannelMerchantSyncIsSerializedPerMerchant(t *testing.T) {
	cs := newCouponShop(t)
	rig := newChannelRig(t)
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	t.Cleanup(func() { adminExec(t, `DELETE FROM channel_merchants WHERE merchant_id = $1`, cs.MerchantID) })
	b, err := rig.svc.CreateBinding(ctx, service.ChannelBindingCreate{Channel: channeltest.Kind, ExternalAccount: "lock", Name: "x", Roles: channel.RoleOutlet})
	if err != nil {
		t.Fatal(err)
	}
	conn := adminSession(t)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2::int)`, service.ChannelMerchantLockKey, cs.MerchantID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		active := repository.ChannelBindingActive
		_, err := rig.svc.UpdateBinding(ctx, b.ID, service.ChannelBindingUpdate{Status: &active})
		done <- err
	}()
	select {
	case err := <-done:
		_ = tx.Rollback(ctx)
		t.Fatalf("别的事务拿着这家店的渠道锁，启用却没有等（err=%v）—— 并发启停的计数与版本不按提交顺序", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("锁放开之后启用 10 秒还没完成")
	}
}

// I1：拿不到 offer 的 SKU（不存在 / 不属于本店）不推，更不能按 0 元推。
func TestChannelSkipsSKUWithoutOffer(t *testing.T) {
	cs := newCouponShop(t)
	rig := newChannelRig(t)
	b := activeFakeBinding(t, rig, cs)
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	const ghost = 999999999
	adminExec(t, `INSERT INTO channel_item_links (merchant_id, binding_id, kind, keel_id, external_id) VALUES ($1, $2, 2, $3, 'ghost')`,
		cs.MerchantID, b.ID, ghost)
	if err := rig.svc.RecomputeListings(ctx, cs.NorthStore, []int64{ghost}, 0); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := rig.fake.LastQty(cs.NorthStore, ghost); ok {
		p, _ := rig.fake.LastPrice(cs.NorthStore, ghost)
		t.Fatalf("不存在的 SKU 被推了出去（价格 %d）", p)
	}
}

// I2：整店重算与回调处理的任务也要长重试（与推送任务同一个上限），库存服务抖一分钟不该进死信。
func TestChannelRecomputeAndInboundJobsRetryLong(t *testing.T) {
	cs := newCouponShop(t)
	rig := newChannelRig(t)
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	b, err := rig.svc.CreateBinding(ctx, service.ChannelBindingCreate{Channel: channeltest.Kind, ExternalAccount: "retry", Name: "x", Roles: channel.RoleOutlet})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.UpsertStoreLink(ctx, repository.ChannelStoreLink{BindingID: b.ID, StoreID: cs.NorthStore, ExternalStoreID: "loc"}); err != nil {
		t.Fatal(err)
	}
	if got := adminQueryInt64(t, `SELECT COALESCE(min(max_attempts), 0) FROM jobs WHERE merchant_id = $1 AND queue = $2`,
		cs.MerchantID, service.QueueChannelListingRecompute); got < 20 {
		t.Fatalf("整店重算任务 max_attempts = %d，期望 ≥ 20", got)
	}
}

// I3：开关渠道消息被回查抢先作废时，提交之后要换新 gid 补发（否则库存服务永远不知道这家店开了渠道）。
func TestChannelMerchantMsgResentWhenLostToQuery(t *testing.T) {
	cs := newCouponShop(t)
	rig := newChannelRig(t)
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	t.Cleanup(func() { adminExec(t, `DELETE FROM channel_merchants WHERE merchant_id = $1`, cs.MerchantID) })
	lost := fmt.Sprintf("chm-%d-lost%d", cs.MerchantID, time.Now().UnixNano())
	calls := 0
	rig.svc.SetMerchantMsgGIDForTest(func(int64) (string, error) {
		calls++
		if calls == 1 {
			return lost, nil
		}
		return fmt.Sprintf("chm-%d-again%d", cs.MerchantID, time.Now().UnixNano()), nil
	})
	// 回查先到：插下 rollback 标记，这条 gid 作废。
	rig.svc.MerchantQueryBranch()(lost, "00", "msg")
	if _, err := rig.svc.CreateBinding(ctx, service.ChannelBindingCreate{Channel: channeltest.Kind, ExternalAccount: "lost",
		Name: "x", Roles: channel.RoleOutlet, Status: repository.ChannelBindingActive}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for adminQueryInt64(t, `SELECT count(*) FROM channel_merchants WHERE merchant_id = $1 AND enabled`, cs.MerchantID) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("开关消息被回查作废之后没有补发（gid 生成了 %d 次）", calls)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// M2：CAS 冲突时渠道上的数恰好等于 keel 要推的数，新价格也必须推上去（冲突那一次价格没生效）。
func TestChannelPriceIsPushedAfterConflictWithEqualQty(t *testing.T) {
	cs := newCouponShop(t)
	rig := newChannelRig(t)
	b := activeFakeBinding(t, rig, cs)
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	qty, _ := rig.fake.LastQty(cs.NorthStore, cs.DressSKU)
	rig.fake.ConflictOnce(cs.NorthStore, cs.DressSKU, qty) // 渠道上的数被人改成了……同一个数
	if _, err := rig.svc.UpsertPriceRule(ctx, repository.ChannelPriceRule{BindingID: b.ID, MarkupBP: 1000}); err != nil {
		t.Fatal(err)
	}
	base := adminQueryInt64(t, `SELECT price_cents FROM sku_prices_by_store WHERE store_id = $1 AND sku_id = $2`, cs.NorthStore, cs.DressSKU)
	want := channel.PublishedPrice(base, channel.PriceRule{MarkupBP: 1000})
	deadline := time.Now().Add(channelWaitWindow)
	for {
		if err := rig.svc.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if p, _ := rig.fake.LastPrice(cs.NorthStore, cs.DressSKU); p == want {
			return
		}
		if time.Now().After(deadline) {
			p, _ := rig.fake.LastPrice(cs.NorthStore, cs.DressSKU)
			t.Fatalf("冲突之后渠道上的价格是 %d，期望 %d", p, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

var _ = http.StatusOK
