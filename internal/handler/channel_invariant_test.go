package handler_test

// 不变量「不配渠道零开销」（docs/superpowers/specs/2026-10-02-channel-adapter-design.md §2、§8）：
//
//   - KEEL_CHANNELS 关：下单、取消（回补库存）、后台改库存走一遍，库存服务一条 stock.changed 都不发、
//     闸门一次库都不查，jobs 里没有任何 channel.* 任务，渠道表一行都没有；
//   - KEEL_CHANNELS 开、但这家店没接任何渠道：同样不发、不入队、不落行；闸门对这家店只查一次（之后走缓存）。
//
// 装配照 app.Run 的单体形态（真路由、嵌入式协调器、库存与 core 同进程），开关只通过 withChannels 的同一条规则生效：
// 关着时通知器上根本不接渠道这一支。

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

func runChannelInvariant(t *testing.T, channelsOn bool) {
	cs := newCouponShop(t)
	store := repository.NewInventoryStore(testPool)
	gate := inventory.NewChannelGate(channelsOn)
	n := inventory.NewStockNotifier(store, "local://"+inventory.BranchStockChanged, "local://"+inventory.BranchStockMsgQuery)
	if gate.Enabled() { // 与 app.withChannels 同一条规则
		n.WithChannels(gate, "local://"+inventory.BranchChannelStockChanged)
	}
	local := inventory.NewLocal(store).WithStockNotifier(n)
	orders := service.NewOrderService(repository.New(testPool), local, nil, nil)
	flags := service.NewStockFlagService(repository.New(testPool), local, 0, nil)
	ex := app.InventoryBranches(local)
	for name, fn := range app.StockMsgBranches(n, flags) {
		ex[name] = fn
	}
	var opts []app.RouterOption
	opts = append(opts, app.WithInventory(local))
	var chSvc *service.ChannelService
	if channelsOn {
		chSvc = service.NewChannelService(repository.New(testPool), local, channel.NewRegistry(), dtm.BranchResolver{}, dtm.BranchResolver{})
		for name, fn := range app.ChannelBranches(chSvc, local, gate) {
			ex[name] = fn
		}
		opts = append(opts, app.WithChannels(chSvc))
	}
	tc, err := dtm.StartEx("sqlite:"+filepath.Join(t.TempDir(), "dtm.db"), 0, app.Branches(orders), ex)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tc.Close)
	orders.AttachCoordinator(tc)
	n.Attach(tc)
	if chSvc != nil {
		chSvc.Attach(tc)
	}
	useEngine(t, app.Router(testPool, tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}), testSigner,
		orders, service.PaymentConfig{Sandbox: true}, conceptEmbedder{}, opts...))

	// 下单 → 取消（关单回补走 inventory.release 的 outbox，当场就地跑）→ 后台相对调整
	b := cs.newBuyer(t, "chinv")
	o := cs.placeOrder(t, b, cs.NorthStore, cs.DressSKU, 2, nil)
	wantStatus(t, postIdem(t, cs.Host, "/api/v1/orders/"+o.OrderNo+"/cancel", "", b.Token), http.StatusOK, "取消")
	adjust(t, local, cs.MerchantID, cs.NorthStore, cs.DressSKU, -1)
	adjust(t, local, cs.MerchantID, cs.NorthStore, cs.DressSKU, +1)
	if chSvc != nil {
		if err := chSvc.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	if got := n.ChangedSent(); got != 0 {
		t.Errorf("发了 %d 条 stock.changed，期望 0", got)
	}
	wantQueries := int64(0)
	if channelsOn {
		wantQueries = 1
	}
	if got := gate.Queries(); got != wantQueries {
		t.Errorf("闸门查了 %d 次 channel_merchants，期望 %d", got, wantQueries)
	}
	if got := adminQueryInt64(t, `SELECT count(*) FROM jobs WHERE merchant_id = $1 AND queue LIKE 'channel.%'`, cs.MerchantID); got != 0 {
		t.Errorf("jobs 里有 %d 条渠道任务，期望 0", got)
	}
	for _, tbl := range []string{"channel_bindings", "channel_store_links", "channel_item_links", "channel_price_rules",
		"channel_stock_rules", "channel_listings", "channel_inbound_events", "channel_merchants"} {
		if got := adminQueryInt64(t, `SELECT count(*) FROM `+tbl+` WHERE merchant_id = $1`, cs.MerchantID); got != 0 {
			t.Errorf("%s 有 %d 行，期望 0", tbl, got)
		}
	}
}

func TestChannelsOffAddNothingToOrderAndStockPaths(t *testing.T) { runChannelInvariant(t, false) }

func TestChannelsOnWithoutBindingAddNothing(t *testing.T) { runChannelInvariant(t, true) }
