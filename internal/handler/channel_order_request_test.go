package handler_test

// 第三期 Task 6：人工接单 / 拒单、平台申请流、接单与申请截止扫描（假适配器：AcceptRequired + RefundNeedsApproval）。
// 另有 Task 5 补的一条：接单 SAGA 在途时平台取消。

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/channeltest"
	"github.com/keel/keel/internal/service"
)

func needsAccept(c *channel.Caps) {
	c.AcceptRequired, c.AcceptTimeout, c.RefundNeedsApproval = true, 5*time.Minute, true
}

// putAwaiting 放一张要接单的连衣裙单（接单截止 = 现在 + in）并投回调。
func (r *fakeOrderRig) putAwaiting(t *testing.T, id string, qty int32, in time.Duration) int64 {
	t.Helper()
	dl := time.Now().Add(in)
	r.fake.PutOrder(channel.ChannelOrder{ExternalOrderID: id, ExternalOrderName: "F-" + id, ExternalStoreID: "loc-1",
		Status: channel.OrderNew, Version: 1, PlacedAt: time.Now(), AcceptDeadline: &dl,
		Lines:   []channel.OrderLine{{ExternalLineID: "l1", ExternalSKUID: "var-1", Title: "连衣裙", Qty: qty, PriceCents: 6000}},
		Amounts: channel.OrderAmounts{GoodsCents: 6000 * int64(qty), BuyerPaidCents: 6000 * int64(qty)}})
	r.n++
	req, raw := channeltest.OrderWebhook("k", fmt.Sprintf("evt-%d-%d", r.b.ID, r.n), id)
	if _, err := r.svc.Inbound(r.ctx, r.b.ID, req, raw); err != nil {
		t.Fatal(err)
	}
	r.drain(t)
	return r.channelOrderID(t, id)
}

// request 投一条平台申请回调（每次一个新的事件 ID）。
func (r *fakeOrderRig) request(t *testing.T, orderID string, q channel.OrderRequest) {
	t.Helper()
	r.n++
	req, raw := channeltest.RequestWebhook("k", fmt.Sprintf("req-%d-%d", r.b.ID, r.n), orderID, q)
	if _, err := r.svc.Inbound(r.ctx, r.b.ID, req, raw); err != nil {
		t.Fatal(err)
	}
	r.drain(t)
}

func (r *fakeOrderRig) dressStock(t *testing.T) int64 {
	t.Helper()
	return adminQueryInt64(t, `SELECT available_qty FROM inventories WHERE store_id = $1 AND sku_id = $2`, r.cs.NorthStore, r.cs.DressSKU)
}

func (r *fakeOrderRig) pendingNotes(t *testing.T, dedupe string) int64 {
	t.Helper()
	return adminQueryInt64(t, `SELECT count(*) FROM notifications WHERE merchant_id = $1 AND kind = 'merchant_channel_order_pending'
		AND target_type = 'channel_orders' AND store_id = $2 AND dedupe_key = $3`, r.cs.MerchantID, r.cs.NorthStore,
		"merchant_channel_order_pending:"+dedupe)
}

func requestID(t *testing.T, co int64, ext string) int64 {
	t.Helper()
	return adminQueryInt64(t, `SELECT id FROM channel_order_requests WHERE channel_order_id = $1 AND external_request_id = $2`, co, ext)
}

func requestStatus(t *testing.T, id int64) int64 {
	t.Helper()
	return adminQueryInt64(t, `SELECT status FROM channel_order_requests WHERE id = $1`, id)
}

// 人工接单 → 建单 + Act(接单)；再接一次是 409；拒单 → Act(拒单) 不建单；缺货 → 关单、自动 Act(拒单)。
func TestChannelOrderManualAcceptAndReject(t *testing.T) {
	r := newFakeOrderRig(t, needsAccept, nil)

	co := r.putAwaiting(t, "m-1", 2, 5*time.Minute)
	if n := keelOrdersOf(t, co); n != 0 {
		t.Fatalf("没接单就建了 %d 张 keel 订单", n)
	}
	if err := r.svc.AcceptChannelOrder(r.ctx, co); err != nil {
		t.Fatal(err)
	}
	r.drain(t)
	if n := adminQueryInt64(t, `SELECT count(*) FROM orders WHERE channel_order_id = $1 AND status = 20`, co); n != 1 {
		t.Fatalf("人工接单后已支付的 keel 订单 %d 张", n)
	}
	if acts := r.fake.ActsOf(channel.ActAccept); len(acts) != 1 || acts[0].Ref.ExternalOrderID != "m-1" {
		t.Fatalf("接单动作 %+v，期望对 m-1 一次", acts)
	}
	if got := r.dressStock(t); got != 48 {
		t.Fatalf("接单后北店库存 %d，期望 48", got)
	}
	if err := r.svc.AcceptChannelOrder(r.ctx, co); !errors.Is(err, service.ErrChannelOrderNotAcceptable) {
		t.Fatalf("再接一次：%v，期望 ErrChannelOrderNotAcceptable", err)
	}

	co2 := r.putAwaiting(t, "m-2", 1, 5*time.Minute)
	if err := r.svc.RejectChannelOrder(r.ctx, co2, "门店打烊"); err != nil {
		t.Fatal(err)
	}
	r.drain(t)
	if n := keelOrdersOf(t, co2); n != 0 {
		t.Fatalf("拒单建了 %d 张 keel 订单", n)
	}
	if acts := r.fake.ActsOf(channel.ActReject); len(acts) != 1 || acts[0].Action.Reason != "门店打烊" {
		t.Fatalf("拒单动作 %+v", acts)
	}
	if got := adminQueryInt64(t, `SELECT status FROM channel_orders WHERE id = $1`, co2); got != 7 {
		t.Fatalf("拒单后渠道单状态 %d，期望 7", got)
	}
	if err := r.svc.AcceptChannelOrder(r.ctx, co2); !errors.Is(err, service.ErrChannelOrderNotAcceptable) {
		t.Fatalf("拒过的单再接：%v", err)
	}
	if err := r.svc.RejectChannelOrder(r.ctx, co, "x"); !errors.Is(err, service.ErrChannelOrderNotAcceptable) {
		t.Fatalf("已接的单再拒：%v", err)
	}

	// 缺货（Review Focus 4 后半）：北店只剩 48 件。
	co3 := r.putAwaiting(t, "m-3", 100, 5*time.Minute)
	if err := r.svc.AcceptChannelOrder(r.ctx, co3); !errors.Is(err, service.ErrChannelOrderAcceptFailed) {
		t.Fatalf("缺货接单：%v，期望 ErrChannelOrderAcceptFailed", err)
	}
	r.drain(t)
	if n := adminQueryInt64(t, `SELECT count(*) FROM orders WHERE channel_order_id = $1 AND status <> 90`, co3); n != 0 {
		t.Fatalf("缺货还有 %d 张没关的 keel 订单", n)
	}
	if acts := r.fake.ActsOf(channel.ActReject); len(acts) != 2 || acts[1].Ref.ExternalOrderID != "m-3" {
		t.Fatalf("缺货没自动拒单：%+v", acts)
	}
	if got := r.dressStock(t); got != 48 {
		t.Fatalf("缺货之后北店库存 %d，期望 48（没扣过）", got)
	}
}

// 接单截止前 accept_remind_minutes 提醒一次（扫描多跑几轮也只一条）；离截止还远的不提醒。
func TestChannelOrderAcceptReminderOnce(t *testing.T) {
	r := newFakeOrderRig(t, needsAccept, map[string]any{"accept_remind_minutes": 5})
	soon := r.putAwaiting(t, "r-1", 1, 4*time.Minute)
	later := r.putAwaiting(t, "r-2", 1, 30*time.Minute)
	for i := 0; i < 3; i++ {
		r.svc.SweepChannelDeadlines(r.ctx)
	}
	if n := r.pendingNotes(t, fmt.Sprintf("accept_remind:%d", soon)); n != 1 {
		t.Fatalf("快到截止的单提醒了 %d 次，期望 1", n)
	}
	if n := r.pendingNotes(t, fmt.Sprintf("accept_remind:%d", later)); n != 0 {
		t.Fatalf("离截止还远的单提醒了 %d 次", n)
	}
}

// 申请：入库幂等、员工收通知；同意 → Act(agree_request)、keel 订单不动，之后平台取消事件到 → 整单退款；
// 拒绝 → Act(reject_request)；处置过再处置 409；平台撤销 → 5。
func TestChannelOrderRequestFlow(t *testing.T) {
	r := newFakeOrderRig(t, needsAccept, map[string]any{"auto_accept": true})
	r.put(t, "q-1", 1, channel.OrderNew, 2)
	co := r.channelOrderID(t, "q-1")
	no := adminQueryString(t, `SELECT order_no FROM orders WHERE channel_order_id = $1 AND status = 20`, co)

	dl := time.Now().Add(10 * time.Minute)
	cancel := channel.OrderRequest{ExternalRequestID: "rq-1", Kind: channel.RequestCancel, Reason: "顾客不想要了",
		AmountCents: 12000, Deadline: &dl}
	r.request(t, "q-1", cancel)
	r.request(t, "q-1", cancel) // 平台重推同一个申请
	if n := adminQueryInt64(t, `SELECT count(*) FROM channel_order_requests WHERE channel_order_id = $1`, co); n != 1 {
		t.Fatalf("同一个申请记了 %d 行", n)
	}
	rq := requestID(t, co, "rq-1")
	if got := requestStatus(t, rq); got != 1 {
		t.Fatalf("手动策略下申请状态 %d，期望 1 待处理", got)
	}
	if n := r.pendingNotes(t, fmt.Sprintf("request:%d", rq)); n != 1 {
		t.Fatalf("新申请的员工通知 %d 条", n)
	}

	if err := r.svc.DecideRequest(r.ctx, rq, true, r.cs.StaffID); err != nil {
		t.Fatal(err)
	}
	r.drain(t)
	if acts := r.fake.ActsOf(channel.ActAgreeRequest); len(acts) != 1 || acts[0].Action.ExternalRequestID != "rq-1" {
		t.Fatalf("同意申请的动作 %+v", acts)
	}
	if got := adminQueryString(t, `SELECT status || ':' || refunded_cents FROM orders WHERE order_no = $1`, no); got != "20:0" {
		t.Fatalf("同意之后、平台确认之前 keel 订单 = %q，期望 20:0（不变量 2）", got)
	}
	if got := adminQueryInt64(t, `SELECT coalesce(decided_by, 0) FROM channel_order_requests WHERE id = $1`, rq); got != r.cs.StaffID {
		t.Fatalf("decided_by = %d", got)
	}
	if err := r.svc.DecideRequest(r.ctx, rq, false, r.cs.StaffID); !errors.Is(err, service.ErrChannelRequestDecided) {
		t.Fatalf("处置过再处置：%v", err)
	}

	r.put(t, "q-1", 2, channel.OrderCancelled, 2) // 平台确认取消
	if got := adminQueryString(t, `SELECT status || ':' || (refunded_cents = paid_cents)::text FROM orders WHERE order_no = $1`, no); got != "60:true" {
		t.Fatalf("平台取消之后 keel 订单 = %q，期望 60:true", got)
	}

	// 另一张单：拒绝申请；再一个申请被平台撤销。
	r.put(t, "q-2", 1, channel.OrderNew, 1)
	co2 := r.channelOrderID(t, "q-2")
	r.request(t, "q-2", channel.OrderRequest{ExternalRequestID: "rq-2", Kind: channel.RequestPartialRefund, AmountCents: 3000})
	rq2 := requestID(t, co2, "rq-2")
	if err := r.svc.DecideRequest(r.ctx, rq2, false, r.cs.StaffID); err != nil {
		t.Fatal(err)
	}
	r.drain(t)
	if acts := r.fake.ActsOf(channel.ActRejectRequest); len(acts) != 1 || acts[0].Action.ExternalRequestID != "rq-2" {
		t.Fatalf("拒绝申请的动作 %+v", acts)
	}
	if got := requestStatus(t, rq2); got != 3 {
		t.Fatalf("拒绝后申请状态 %d", got)
	}
	r.request(t, "q-2", channel.OrderRequest{ExternalRequestID: "rq-3", Kind: channel.RequestCancel})
	r.request(t, "q-2", channel.OrderRequest{ExternalRequestID: "rq-3", Kind: channel.RequestCancel, Withdrawn: true})
	if got := requestStatus(t, requestID(t, co2, "rq-3")); got != 5 {
		t.Fatalf("平台撤销后申请状态 %d，期望 5", got)
	}
}

// 自动策略 auto_agree_unshipped：未发货的取消申请自动同意（入队 Act），部分退款仍等人；申请过了截止 → 4 + 员工通知。
func TestChannelOrderRequestAutoPolicyAndTimeout(t *testing.T) {
	r := newFakeOrderRig(t, needsAccept, map[string]any{"auto_accept": true, "request_policy": "auto_agree_unshipped"})
	r.put(t, "p-1", 1, channel.OrderNew, 1)
	co := r.channelOrderID(t, "p-1")
	no := adminQueryString(t, `SELECT order_no FROM orders WHERE channel_order_id = $1`, co)

	r.request(t, "p-1", channel.OrderRequest{ExternalRequestID: "a-1", Kind: channel.RequestCancel})
	if got := requestStatus(t, requestID(t, co, "a-1")); got != 2 {
		t.Fatalf("自动策略下取消申请状态 %d，期望 2", got)
	}
	if acts := r.fake.ActsOf(channel.ActAgreeRequest); len(acts) != 1 {
		t.Fatalf("自动同意的动作 %d 次", len(acts))
	}
	if got := adminQueryInt64(t, `SELECT status FROM orders WHERE order_no = $1`, no); got != 20 {
		t.Fatalf("自动同意后 keel 订单 %d，期望还是 20（等平台确认）", got)
	}

	past := time.Now().Add(-time.Minute)
	r.request(t, "p-1", channel.OrderRequest{ExternalRequestID: "a-2", Kind: channel.RequestPartialRefund, AmountCents: 1000, Deadline: &past})
	rq := requestID(t, co, "a-2")
	if got := requestStatus(t, rq); got != 1 {
		t.Fatalf("部分退款申请 %d，期望 1 等人", got)
	}
	r.svc.SweepChannelDeadlines(r.ctx)
	r.svc.SweepChannelDeadlines(r.ctx)
	if got := requestStatus(t, rq); got != 4 {
		t.Fatalf("过了截止的申请 %d，期望 4", got)
	}
	if n := r.pendingNotes(t, fmt.Sprintf("request_timeout:%d", rq)); n != 1 {
		t.Fatalf("申请超时的员工通知 %d 条", n)
	}
	if n := len(r.fake.ActsOf(channel.ActAgreeRequest)); n != 1 {
		t.Fatalf("超时不该替平台做动作，agree_request 共 %d 次", n)
	}
}

// Task 5 补：接单 SAGA 在途（建单与扣库存已做、收尾还没做）时平台取消 → keel 订单关到 90、库存放回、没有退款单。
func TestPlatformCancelWhileAcceptSagaInFlight(t *testing.T) {
	r := newFakeOrderRig(t, nil, nil)
	before := r.dressStock(t)
	fired := false
	hook := func() {
		if fired {
			return
		}
		fired = true
		// 平台上取消，且取消回调已经处理完：草稿还在途时 applyPlatformFacts 只把快照记进渠道单（状态 6、版本 2）。
		// 收单 worker 此刻正卡在等 SAGA（同一租户的下一条任务出不了队），所以直接写成那条回调处理完的样子。
		o, _ := r.fake.Order("f-1")
		o.Status, o.Version = channel.OrderCancelled, 2
		r.fake.PutOrder(o)
		adminExec(t, `UPDATE channel_orders SET status = 6, version = 2 WHERE binding_id = $1 AND external_order_id = 'f-1'`, r.b.ID)
	}
	r.beforeFinish.Store(&hook)
	t.Cleanup(func() { r.beforeFinish.Store(nil) })

	r.put(t, "f-1", 1, channel.OrderNew, 2)
	r.beforeFinish.Store(nil)
	if !fired {
		t.Fatal("收尾分支没跑到")
	}
	co := r.channelOrderID(t, "f-1")
	if got := adminQueryString(t, `SELECT string_agg(status::text, ',') FROM orders WHERE channel_order_id = $1`, co); got != "90" {
		t.Fatalf("keel 订单状态 %q，期望只有一张 90", got)
	}
	if got := r.dressStock(t); got != before {
		t.Fatalf("库存 %d，期望放回到 %d", got, before)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM refunds r JOIN orders o ON o.id = r.order_id WHERE o.channel_order_id = $1`, co); n != 0 {
		t.Fatalf("在途取消建了 %d 张退款单", n)
	}
	if got := adminQueryInt64(t, `SELECT status FROM channel_orders WHERE id = $1`, co); got != 6 {
		t.Fatalf("渠道单状态 %d，期望 6", got)
	}
}
