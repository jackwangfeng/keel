package handler_test

// 渠道层对外可售数的全链路（单体）：启用 binding → 库存服务记下「开了渠道」→ 改库存 → stock.changed →
// core 重算入队 → worker 推给假渠道 → channel_listings 记下推出去的值。

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/channeltest"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/rpc"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

type channelRig struct {
	svc   *service.ChannelService
	fake  *channeltest.Adapter
	local *inventory.Local
	n     *inventory.StockNotifier
	// beforeFinish 非空时，接单 SAGA 的收尾分支在做事之前先调它（测试用它制造「SAGA 在途时平台取消」）。
	beforeFinish *atomic.Pointer[func()]
}

func newChannelRig(t *testing.T) channelRig { t.Helper(); return newChannelRigWith(t) }

// newChannelRigWith 同 newChannelRig，另外登记 extra 里的适配器（Shopify 打模拟平台）。
func newChannelRigWith(t *testing.T, extra ...channel.Adapter) channelRig {
	t.Helper()
	return newChannelRigOpts(t, false, extra...)
}

// newChannelRigOpts：split 为真时库存走拆分形态（库存接口与 SAGA 分支挂在一个 httptest 内网服务上，
// core 用 HTTP 实现、接单 SAGA 的库存步骤指向它）；同一个测试库，协调器仍是嵌入式。
func newChannelRigOpts(t *testing.T, split bool, extra ...channel.Adapter) channelRig {
	t.Helper()
	store := repository.NewInventoryStore(testPool)
	gate := inventory.NewChannelGate(true)
	n := inventory.NewStockNotifier(store, "local://"+inventory.BranchStockChanged, "local://"+inventory.BranchStockMsgQuery).
		WithChannels(gate, "local://"+inventory.BranchChannelStockChanged)
	local := inventory.NewLocal(store).WithStockNotifier(n)
	reg := channel.NewRegistry()
	fake := channeltest.New()
	reg.Register(fake)
	for _, a := range extra {
		reg.Register(a)
	}
	var inv inventory.Service = local
	res := dtm.BranchResolver{}
	ex := map[string]dtm.BranchFuncEx{}
	if split {
		r, routes := rpc.NewRouter(rpc.ServerConfig{Secret: remoteInventorySecret})
		inventory.Mount(routes.Tenant, local)
		inventory.MountSaga(routes.Saga, local)
		dtm.MountBranches(routes.Saga, map[string]dtm.BranchFuncEx{
			inventory.BranchChannelMerchantSync: local.ChannelMerchantSyncBranch(gate)})
		srv := httptest.NewServer(r)
		t.Cleanup(srv.Close)
		c, err := rpc.NewClient(srv.URL, remoteInventorySecret, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		inv = inventory.NewRemote(c)
		if res, err = dtm.NewBranchResolver(srv.URL, remoteInventorySecret); err != nil {
			t.Fatal(err)
		}
	} else {
		ex = app.InventoryBranches(local)
		ex[inventory.BranchChannelMerchantSync] = local.ChannelMerchantSyncBranch(gate)
	}
	svc := service.NewChannelService(repository.New(testPool), inv, reg, res, dtm.BranchResolver{})
	ex[inventory.BranchStockMsgQuery] = dtm.Ex(n.QueryBranch())
	ex[inventory.BranchStockChanged] = func(string, string, string, string) int { return dtm.Success }
	ex[inventory.BranchChannelStockChanged] = svc.StockChangedBranch()
	ex[service.BranchChannelMerchantQuery] = dtm.Ex(svc.MerchantQueryBranch())
	beforeFinish := new(atomic.Pointer[func()])
	for name, fn := range svc.OrderBranches() {
		if name == service.BranchChannelOrderFinish {
			inner := fn
			fn = func(gid, branchID, op string) int {
				if h := beforeFinish.Load(); h != nil {
					(*h)()
				}
				return inner(gid, branchID, op)
			}
		}
		ex[name] = dtm.Ex(fn)
	}
	tc, err := dtm.StartEx("sqlite:"+filepath.Join(t.TempDir(), "dtm.db"), 0, nil, ex)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tc.Close)
	n.Attach(tc)
	svc.Attach(tc)
	return channelRig{svc: svc, fake: fake, local: local, n: n, beforeFinish: beforeFinish}
}

// waitPushed 反复跑 worker，直到假渠道上 (store, sku) 的数是 want。
func (r channelRig) waitPushed(t *testing.T, ctx context.Context, store, sku int64, want int32, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := r.svc.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if q, ok := r.fake.LastQty(store, sku); ok && q == want {
			return
		}
		if time.Now().After(deadline) {
			q, ok := r.fake.LastQty(store, sku)
			t.Fatalf("%s：等了 20 秒假渠道上是 %d（推过=%v），期望 %d", what, q, ok, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestChannelListingsFollowStock(t *testing.T) {
	cs := newCouponShop(t)
	rig := newChannelRig(t)
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	t.Cleanup(func() { adminExec(t, `DELETE FROM channel_merchants WHERE merchant_id = $1`, cs.MerchantID) })
	store, sku := cs.NorthStore, cs.DressSKU
	avail := func() int32 {
		return int32(adminQueryInt64(t, `SELECT available_qty FROM inventories WHERE store_id = $1 AND sku_id = $2`, store, sku))
	}

	b, err := rig.svc.CreateBinding(ctx, service.ChannelBindingCreate{Channel: channeltest.Kind, ExternalAccount: "shop-1",
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
	if err := rig.svc.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rig.fake.Pushes()) != 0 {
		t.Fatalf("binding 还没启用就推了 %d 批", len(rig.fake.Pushes()))
	}

	t.Run("启用_通知库存服务_整店推一遍", func(t *testing.T) {
		active := repository.ChannelBindingActive
		if _, err := rig.svc.UpdateBinding(ctx, b.ID, service.ChannelBindingUpdate{Status: &active}); err != nil {
			t.Fatal(err)
		}
		rig.waitPushed(t, ctx, store, sku, avail(), "启用之后")
		deadline := time.Now().Add(15 * time.Second)
		for adminQueryInt64(t, `SELECT count(*) FROM channel_merchants WHERE merchant_id = $1 AND enabled`, cs.MerchantID) != 1 {
			if time.Now().After(deadline) {
				t.Fatal("等了 15 秒库存服务仍未记下这家商家开了渠道")
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	t.Run("改库存_不跨0也推", func(t *testing.T) {
		adjust(t, rig.local, cs.MerchantID, store, sku, -2)
		rig.waitPushed(t, ctx, store, sku, avail(), "改库存之后")
	})

	t.Run("分配规则_一半减安全库存", func(t *testing.T) {
		if _, err := rig.svc.UpsertStockRule(ctx, repository.ChannelStockRule{BindingID: b.ID, RatioBP: 5000, SafetyQty: 1}); err != nil {
			t.Fatal(err)
		}
		rig.waitPushed(t, ctx, store, sku, channel.PublishedQty(avail(), channel.StockRule{RatioBP: 5000, SafetyQty: 1}), "改规则之后")
	})

	t.Run("同样的值不重推", func(t *testing.T) {
		before := len(rig.fake.Pushes())
		if err := rig.svc.RecomputeListings(ctx, store, []int64{sku}, 0); err != nil {
			t.Fatal(err)
		}
		if err := rig.svc.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if got := len(rig.fake.Pushes()); got != before {
			t.Fatalf("值没变却又推了 %d 批", got-before)
		}
	})

	t.Run("渠道上被人改过_CAS冲突后以keel覆盖", func(t *testing.T) {
		want := channel.PublishedQty(avail()-1, channel.StockRule{RatioBP: 5000, SafetyQty: 1})
		rig.fake.ConflictOnce(store, sku, 99)
		adjust(t, rig.local, cs.MerchantID, store, sku, -1)
		rig.waitPushed(t, ctx, store, sku, want, "冲突重推之后")
		var ls []repository.ChannelListing
		ls, err := rig.svc.ListListings(ctx, b.ID, &store, false, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(ls) != 1 || ls[0].PublishedQty != want || ls[0].LastError != nil {
			t.Fatalf("channel_listings = %+v，期望记下 %d、错误已清", ls, want)
		}
	})

	t.Run("推送失败_退避后重推", func(t *testing.T) {
		rule := channel.StockRule{RatioBP: 5000, SafetyQty: 1}
		before := channel.PublishedQty(avail(), rule)
		rig.fake.FailNext(1)
		adjust(t, rig.local, cs.MerchantID, store, sku, -4) // 按一半取整也一定会变
		want := channel.PublishedQty(avail(), rule)
		if want == before {
			t.Fatalf("夹具不成立：调 -4 之后对外可售数没变（%d）", want)
		}
		rig.waitPushed(t, ctx, store, sku, want, "失败重试之后")
		if left := rig.fake.FailuresLeft(); left != 0 {
			t.Fatalf("编排的失败还剩 %d 次没用掉 —— 这条测试没测到重试", left)
		}
	})

	t.Run("停用_库存服务不再发", func(t *testing.T) {
		off := repository.ChannelBindingDisabled
		if _, err := rig.svc.UpdateBinding(ctx, b.ID, service.ChannelBindingUpdate{Status: &off}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(15 * time.Second)
		for adminQueryInt64(t, `SELECT count(*) FROM channel_merchants WHERE merchant_id = $1 AND NOT enabled`, cs.MerchantID) != 1 {
			if time.Now().After(deadline) {
				t.Fatal("等了 15 秒库存服务仍未记下这家商家关了渠道")
			}
			time.Sleep(20 * time.Millisecond)
		}
		sent := rig.n.ChangedSent()
		adjust(t, rig.local, cs.MerchantID, store, sku, -1)
		if rig.n.ChangedSent() != sent {
			t.Fatal("关了渠道之后改库存还在发 stock.changed")
		}
	})
}
