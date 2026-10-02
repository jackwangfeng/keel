package handler_test

// AI 调渠道库存分配 Task 6（spec §7.1）：推送带完整状态的渠道（Caps.OutOfOrderInbound = false）直接用回调载荷收单，
// 不调 FetchOrder；版本守卫照旧。乱序渠道带了载荷也照样回读。

import (
	"fmt"
	"testing"
	"time"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/channeltest"
)

// payloadChannel 是「推送带完整状态、要接单、不支持回读」的渠道（照演示外卖）。
func payloadChannel(c *channel.Caps) {
	c.OutOfOrderInbound, c.AcceptRequired, c.AcceptTimeout = false, true, 5*time.Minute
}

func (r *fakeOrderRig) dressOrder(id string, version int64, st channel.OrderStatus, qty int32) channel.ChannelOrder {
	return channel.ChannelOrder{ExternalOrderID: id, ExternalOrderName: "P-" + id, ExternalStoreID: "loc-1",
		Status: st, Version: version, PlacedAt: time.Now(),
		Lines:   []channel.OrderLine{{ExternalLineID: "l1", ExternalSKUID: "var-1", Title: "连衣裙", Qty: qty, PriceCents: 6000}},
		Amounts: channel.OrderAmounts{GoodsCents: 6000 * int64(qty), BuyerPaidCents: 6000 * int64(qty)}}
}

// putPayload 投一条正文带整张订单的回调（假渠道上不放单：要回读就会找不到）。
func (r *fakeOrderRig) putPayload(t *testing.T, o channel.ChannelOrder) {
	t.Helper()
	r.n++
	req, raw := channeltest.OrderPayloadWebhook("k", fmt.Sprintf("evt-%d-%d", r.b.ID, r.n), o)
	if _, err := r.svc.Inbound(r.ctx, r.b.ID, req, raw); err != nil {
		t.Fatal(err)
	}
	r.drain(t)
}

func TestChannelOrderPayloadPathSkipsFetch(t *testing.T) {
	r := newFakeOrderRig(t, payloadChannel, map[string]any{"auto_accept": true})
	r.fake.FetchErr = channel.ErrUnsupported

	r.putPayload(t, r.dressOrder("p-1", 1, channel.OrderNew, 1))
	co := r.channelOrderID(t, "p-1")
	if n := adminQueryInt64(t, `SELECT count(*) FROM orders WHERE channel_order_id = $1 AND status = 20`, co); n != 1 {
		t.Fatalf("载荷收单后已支付的 keel 订单 %d 张，期望 1", n)
	}
	if acts := r.fake.ActsOf(channel.ActAccept); len(acts) != 1 {
		t.Fatalf("接单动作 %d 次，期望 1", len(acts))
	}
	// 同版本重放（平台重推、换了事件 ID）：每一步幂等，不多建单、不多接单。
	r.putPayload(t, r.dressOrder("p-1", 1, channel.OrderNew, 1))
	if n := keelOrdersOf(t, co); n != 1 {
		t.Fatalf("同版本重放后 keel 订单 %d 张，期望 1", n)
	}
	if acts := r.fake.ActsOf(channel.ActAccept); len(acts) != 1 {
		t.Fatalf("同版本重放后接单动作 %d 次，期望 1", len(acts))
	}
	if n := r.fake.FetchCalls(); n != 0 {
		t.Fatalf("载荷路径调了 %d 次 FetchOrder", n)
	}
}

// Review Focus 2 在载荷路径上：先到 version 2 的「已取消」、再到 version 1 的「新单」→ 不建单、状态不回退。
func TestChannelOrderPayloadVersionGuard(t *testing.T) {
	r := newFakeOrderRig(t, payloadChannel, map[string]any{"auto_accept": true})
	r.fake.FetchErr = channel.ErrUnsupported
	r.putPayload(t, r.dressOrder("p-2", 2, channel.OrderCancelled, 1))
	r.putPayload(t, r.dressOrder("p-2", 1, channel.OrderNew, 1))
	co := r.channelOrderID(t, "p-2")
	if n := keelOrdersOf(t, co); n != 0 {
		t.Fatalf("迟到的旧版本建了 %d 张 keel 订单", n)
	}
	if got := adminQueryString(t, `SELECT status || ':' || version FROM channel_orders WHERE id = $1`, co); got != "6:2" {
		t.Fatalf("渠道单 状态:版本 = %q，期望 6:2", got)
	}
	if n := r.fake.FetchCalls(); n != 0 {
		t.Fatalf("载荷路径调了 %d 次 FetchOrder", n)
	}
}

// 乱序渠道（Shopify 那种）：载荷只当提示，照样回读权威状态。
func TestChannelOrderOutOfOrderIgnoresPayload(t *testing.T) {
	r := newFakeOrderRig(t, func(c *channel.Caps) { c.OutOfOrderInbound = true }, nil)
	r.fake.PutOrder(r.dressOrder("p-3", 2, channel.OrderCancelled, 1)) // 平台上的权威状态
	r.putPayload(t, r.dressOrder("p-3", 1, channel.OrderNew, 1))       // 回调里的旧状态
	co := r.channelOrderID(t, "p-3")
	if r.fake.FetchCalls() == 0 {
		t.Fatal("乱序渠道没有回读")
	}
	if got := adminQueryString(t, `SELECT status || ':' || version FROM channel_orders WHERE id = $1`, co); got != "6:2" {
		t.Fatalf("渠道单 状态:版本 = %q，期望回读到的 6:2", got)
	}
}

// 不支持回读的渠道，人工接单用渠道单上存着的最近一次载荷（last_payload）建单。
func TestChannelOrderPayloadManualAcceptUsesStored(t *testing.T) {
	r := newFakeOrderRig(t, payloadChannel, nil)
	r.fake.FetchErr = channel.ErrUnsupported
	o := r.dressOrder("p-4", 1, channel.OrderNew, 1)
	d := time.Now().Add(5 * time.Minute)
	o.AcceptDeadline = &d
	r.putPayload(t, o)
	co := r.channelOrderID(t, "p-4")
	if n := keelOrdersOf(t, co); n != 0 {
		t.Fatalf("没接单就建了 %d 张 keel 订单", n)
	}
	if err := r.svc.AcceptChannelOrder(r.ctx, co); err != nil {
		t.Fatal(err)
	}
	r.drain(t)
	if n := adminQueryInt64(t, `SELECT count(*) FROM orders WHERE channel_order_id = $1 AND status = 20`, co); n != 1 {
		t.Fatalf("人工接单后已支付的 keel 订单 %d 张，期望 1", n)
	}
}
