package handler_test

// 渠道层的库存变化通知（inventory 包 channel_gate.go / stock_msg.go 的 stock.changed、channel_msg.go 的开关渠道）。
//
// 单体装配：一个嵌入式协调器，库存回查分支、开关渠道的接收分支、一个记录投递的假 core 接收分支都注册在它上面。
// 断言：
//
//   - 开关关闭：改库存不发 stock.changed、闸门一次库都不查（不变量「不配渠道零开销」）；
//   - 开关打开、商家没开渠道：不发（查一次，之后走缓存）；
//   - 商家开了：10 → 9 这种不跨 0 的变化也发一条，载荷是门店与 SKU；
//   - 商家关了：不再发；晚到的旧「开」消息翻不回来。

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

type stockChangedRecorder struct {
	mu   sync.Mutex
	msgs []inventory.StockMsgPayload
}

func (r *stockChangedRecorder) branch(gid, _, _, payload string) int {
	var p inventory.StockMsgPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return dtm.Failure
	}
	r.mu.Lock()
	r.msgs = append(r.msgs, p)
	r.mu.Unlock()
	return dtm.Success
}

func (r *stockChangedRecorder) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.msgs) }

func (r *stockChangedRecorder) waitFor(t *testing.T, n int) inventory.StockMsgPayload {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for r.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("等了 15 秒只收到 %d 条 stock.changed，期望 %d", r.count(), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.msgs[n-1]
}

type channelStockRig struct {
	local *inventory.Local
	n     *inventory.StockNotifier
	gate  *inventory.ChannelGate
	rec   *stockChangedRecorder
	tc    dtm.Coordinator
}

func newChannelStockRig(t *testing.T, channelsOn bool) channelStockRig {
	t.Helper()
	store := repository.NewInventoryStore(testPool)
	gate := inventory.NewChannelGate(channelsOn)
	n := inventory.NewStockNotifier(store, "local://"+inventory.BranchStockChanged, "local://"+inventory.BranchStockMsgQuery).
		WithChannels(gate, "local://"+inventory.BranchChannelStockChanged)
	local := inventory.NewLocal(store).WithStockNotifier(n)
	rec := &stockChangedRecorder{}
	ex := app.InventoryBranches(local)
	ex[inventory.BranchStockMsgQuery] = dtm.Ex(n.QueryBranch())
	ex[inventory.BranchStockChanged] = func(string, string, string, string) int { return dtm.Success } // 跨 0：这里不关心
	ex[inventory.BranchChannelStockChanged] = rec.branch
	ex[inventory.BranchChannelMerchantSync] = local.ChannelMerchantSyncBranch(gate)
	tc, err := dtm.StartEx("sqlite:"+filepath.Join(t.TempDir(), "dtm.db"), 0, nil, ex)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tc.Close)
	n.Attach(tc)
	return channelStockRig{local: local, n: n, gate: gate, rec: rec, tc: tc}
}

func (r channelStockRig) setMerchant(t *testing.T, merchantID int64, on bool, rev int64) {
	t.Helper()
	gid, err := inventory.ChannelMerchantMsgGID(merchantID)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(inventory.ChannelMerchantPayload{Enabled: on, Rev: rev})
	if got := r.local.ChannelMerchantSyncBranch(r.gate)(gid, "01", "action", string(payload)); got != dtm.Success {
		t.Fatalf("开关渠道分支返回 %d", got)
	}
}

func adjust(t *testing.T, local *inventory.Local, merchantID, store, sku int64, delta int32) {
	t.Helper()
	ctx := tenant.NewContext(t.Context(), merchantID)
	if _, err := local.Adjust(ctx, inventory.AdjustRequest{SKUID: sku, StoreID: store, Delta: delta,
		BizID: fmt.Sprintf("chtest-%d", time.Now().UnixNano())}); err != nil {
		t.Fatal(err)
	}
}

func TestChannelStockChangedOffByDefault(t *testing.T) {
	cs := newCouponShop(t)
	rig := newChannelStockRig(t, false)
	rig.setMerchant(t, cs.MerchantID, true, 1) // 库里记成开了，但进程开关关着
	adjust(t, rig.local, cs.MerchantID, cs.NorthStore, cs.DressSKU, -1)
	adjust(t, rig.local, cs.MerchantID, cs.NorthStore, cs.DressSKU, +1)
	time.Sleep(200 * time.Millisecond)
	if rig.n.ChangedSent() != 0 || rig.rec.count() != 0 {
		t.Fatalf("开关关闭却发了 stock.changed：登记 %d、收到 %d", rig.n.ChangedSent(), rig.rec.count())
	}
	if q := rig.gate.Queries(); q != 0 {
		t.Fatalf("开关关闭时闸门查了 %d 次库，期望 0", q)
	}
}

func TestChannelStockChangedFollowsMerchantFlag(t *testing.T) {
	cs := newCouponShop(t)
	rig := newChannelStockRig(t, true)

	adjust(t, rig.local, cs.MerchantID, cs.NorthStore, cs.DressSKU, -1)
	adjust(t, rig.local, cs.MerchantID, cs.NorthStore, cs.DressSKU, -1)
	if rig.n.ChangedSent() != 0 {
		t.Fatalf("商家没开渠道却发了 %d 条", rig.n.ChangedSent())
	}
	if q := rig.gate.Queries(); q != 1 {
		t.Fatalf("没开渠道的商家查了 %d 次库，期望 1（之后走缓存）", q)
	}

	rig.setMerchant(t, cs.MerchantID, true, 10)
	adjust(t, rig.local, cs.MerchantID, cs.NorthStore, cs.DressSKU, -1) // 不跨 0 的变化
	got := rig.rec.waitFor(t, 1)
	if got.StoreID != cs.NorthStore || len(got.SKUIDs) != 1 || got.SKUIDs[0] != cs.DressSKU {
		t.Fatalf("stock.changed 载荷 = %+v，期望门店 %d、SKU [%d]", got, cs.NorthStore, cs.DressSKU)
	}

	rig.setMerchant(t, cs.MerchantID, false, 20)
	rig.setMerchant(t, cs.MerchantID, true, 15) // 晚到的旧消息
	before := rig.n.ChangedSent()
	adjust(t, rig.local, cs.MerchantID, cs.NorthStore, cs.DressSKU, +1)
	time.Sleep(200 * time.Millisecond)
	if rig.n.ChangedSent() != before {
		t.Fatalf("关了渠道（rev 20）之后、旧的「开」（rev 15）晚到，又发了 stock.changed")
	}
}
