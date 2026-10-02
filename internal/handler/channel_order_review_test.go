package handler_test

// 第三期审查修复（渠道订单）：同版本重放、SAGA 提交失败后的恢复、后台重试救卡住的草稿、同一秒里的两次变化等。

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/shopify/shopifytest"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/service"
)

// failingSubmit 让 SubmitSaga 失败 n 次（n < 0 一直失败），其余照转真协调器。
type failingSubmit struct {
	dtm.Coordinator
	n atomic.Int32
}

func failSubmits(c dtm.Coordinator, n int32) *failingSubmit {
	f := &failingSubmit{Coordinator: c}
	f.n.Store(n)
	return f
}

func (f *failingSubmit) SubmitSaga(gid, steps string) error {
	if f.n.Load() != 0 {
		f.n.Add(-1)
		return errSubmitRefused
	}
	return f.Coordinator.SubmitSaga(gid, steps)
}

func orderCount(t *testing.T, co int64, status int) int64 {
	t.Helper()
	return adminQueryInt64(t, `SELECT count(*) FROM orders WHERE channel_order_id = $1 AND status = $2`, co, status)
}

// 审查 1：SAGA 提交失败 → 回调任务退避重试，回读到的还是同一个版本 —— 照样往下走，看到草稿还在 0 就再提交，成单。
func TestChannelOrderSagaSubmitFailureResumesOnSameVersion(t *testing.T) {
	r := newFakeOrderRig(t, nil, nil)
	r.svc.Attach(failSubmits(r.tc, 1))
	r.put(t, "s-1", 1, channel.OrderNew, 1)
	co := r.channelOrderID(t, "s-1")
	if n := orderCount(t, co, 0); n != 1 {
		t.Fatalf("提交失败之后草稿 %d 张，期望 1 张停在 0", n)
	}
	r.svc.Attach(r.tc)
	adminExec(t, `UPDATE jobs SET run_after = now() WHERE merchant_id = $1 AND queue = $2 AND status = 0`,
		r.cs.MerchantID, service.QueueChannelInbound)
	r.drain(t)
	if n := orderCount(t, co, 20); n != 1 || keelOrdersOf(t, co) != 1 {
		t.Fatalf("回调重试之后已支付 %d 张、共 %d 张，期望同一张草稿推到 20", n, keelOrdersOf(t, co))
	}
}

// 审查 1：草稿卡在 0（回调任务也重试完了）→ 后台「重试」能救；被孤儿清扫关到 90 而渠道单没有异常 → 「重试」重新建单。
func TestChannelOrderRetryRescuesStuckDraft(t *testing.T) {
	r := newFakeOrderRig(t, nil, nil)
	r.svc.Attach(failSubmits(r.tc, -1))
	r.put(t, "s-2", 1, channel.OrderNew, 1)
	co := r.channelOrderID(t, "s-2")
	if n := orderCount(t, co, 0); n != 1 {
		t.Fatalf("草稿 %d 张，期望 1 张停在 0", n)
	}
	r.svc.Attach(r.tc)
	if err := r.svc.RetryChannelOrder(r.ctx, co); err != nil {
		t.Fatalf("草稿卡在 0 时重试：%v", err)
	}
	r.drain(t)
	if n := orderCount(t, co, 20); n != 1 || keelOrdersOf(t, co) != 1 {
		t.Fatalf("重试之后已支付 %d 张、共 %d 张，期望同一张草稿推到 20", n, keelOrdersOf(t, co))
	}
	if err := r.svc.RetryChannelOrder(r.ctx, co); !errors.Is(err, service.ErrChannelOrderNotRetryable) {
		t.Fatalf("成单之后再重试：%v，期望 ErrChannelOrderNotRetryable", err)
	}

	// 孤儿清扫：草稿过了 expire_at 被关到 90，渠道单没有异常、还指着那张草稿。
	r.svc.Attach(failSubmits(r.tc, -1))
	r.put(t, "s-3", 1, channel.OrderNew, 1)
	co3 := r.channelOrderID(t, "s-3")
	adminExec(t, `UPDATE orders SET status = 90 WHERE channel_order_id = $1 AND status = 0`, co3)
	r.svc.Attach(r.tc)
	if err := r.svc.RetryChannelOrder(r.ctx, co3); err != nil {
		t.Fatalf("草稿被清扫关掉之后重试：%v", err)
	}
	r.drain(t)
	if n := orderCount(t, co3, 20); n != 1 || keelOrdersOf(t, co3) != 2 {
		t.Fatalf("重试之后已支付 %d 张、共 %d 张，期望关掉的 1 张 + 新建成单的 1 张", n, keelOrdersOf(t, co3))
	}
}

// 审查 2：Shopify 的 updatedAt 只到秒，同一秒里的两次变化是同一个版本。付款晚于下单（同一秒）照样接单；
// 同一秒里再取消照样整单退款；同版本的回调反复重放不重复建单、扣库存、发通知、退款、回补。
func TestChannelOrderSameSecondChangesAndReplays(t *testing.T) {
	r := newOrderRig(t, false, 7)
	r.sim.FreezeClock()
	gid := r.sim.AddOrder(shopifytest.OrderSpec{Location: r.loc, Financial: "AUTHORIZED",
		Lines: []shopifytest.OrderLineSpec{{Variant: r.variant, Qty: 2}}})
	r.orderWebhook(t, gid, "orders/create")
	co := r.channelOrderID(t, gid)
	if n := keelOrdersOf(t, co); n != 0 {
		t.Fatalf("没付款就建了 %d 张 keel 订单", n)
	}
	r.sim.SetFinancial(gid, "PAID")
	r.orderWebhook(t, gid, "orders/paid")
	if n := orderCount(t, co, 20); n != 1 {
		t.Fatalf("同一秒里付了款：已支付的 keel 订单 %d 张，期望 1", n)
	}
	no := adminQueryString(t, `SELECT order_no FROM orders WHERE channel_order_id = $1`, co)

	paidState := func(when string) {
		t.Helper()
		if n := keelOrdersOf(t, co); n != 1 {
			t.Fatalf("%s：keel 订单 %d 张", when, n)
		}
		if n := adminQueryInt64(t, `SELECT count(*) FROM inventory_logs WHERE merchant_id = $1 AND sku_id = $2 AND biz_type = 1`,
			r.cs.MerchantID, r.sku); n != 1 {
			t.Fatalf("%s：扣了 %d 次库存", when, n)
		}
		if n := adminQueryInt64(t, `SELECT count(*) FROM notifications WHERE order_no = $1 AND kind = 'merchant_order_paid'`, no); n != 1 {
			t.Fatalf("%s：「新订单待发货」%d 条", when, n)
		}
		if n := actionJobs(t, co); n != 0 {
			t.Fatalf("%s：入队了 %d 条渠道动作", when, n)
		}
	}
	paidState("成单后")
	for _, topic := range []string{"orders/updated", "orders/paid", "orders/updated"} {
		r.orderWebhook(t, gid, topic)
	}
	paidState("同版本重放后")
	if got := r.stock(t, r.sku); got != 5 {
		t.Fatalf("keel 库存 %d，期望 5", got)
	}

	r.sim.Cancel(gid, true) // 还是同一秒
	r.orderWebhook(t, gid, "orders/cancelled")
	r.restock(t)
	cancelled := func(when string) {
		t.Helper()
		if got := adminQueryString(t, `SELECT status || ':' || refunded_cents || '/' || paid_cents FROM orders WHERE order_no = $1`, no); got != "60:3980/3980" {
			t.Fatalf("%s：keel 订单 状态:已退/实付 = %q", when, got)
		}
		if n := adminQueryInt64(t, `SELECT count(*) FROM refunds r JOIN orders o ON o.id = r.order_id WHERE o.order_no = $1`, no); n != 1 {
			t.Fatalf("%s：退款单 %d 张", when, n)
		}
		if got := r.stock(t, r.sku); got != 7 {
			t.Fatalf("%s：keel 库存 %d，期望回补到 7", when, got)
		}
		if n := adminQueryInt64(t, `SELECT count(*) FROM notifications WHERE order_no = $1 AND kind = 'merchant_channel_order_exception'`, no); n != 1 {
			t.Fatalf("%s：门店通知 %d 条，期望 1", when, n)
		}
	}
	cancelled("同一秒取消后")
	for _, topic := range []string{"orders/cancelled", "orders/updated"} {
		r.orderWebhook(t, gid, topic)
	}
	r.restock(t)
	cancelled("取消重放后")
	paidState("取消重放后（扣库存、成单通知）")
}

// 审查 4：Shopify 上的 FO 被暂停（ON_HOLD）→ 回传一直可重试、用尽进死信 → 渠道单标异常「发货没回传上」写明 FO 状态。
func TestShipmentPushHeldFulfillmentOrderDeadLetters(t *testing.T) {
	r := newOrderRig(t, false, 7)
	gid, co, no := r.paidChannelOrder(t)
	r.sim.SetFulfillmentOrderStatus(r.sim.FulfillmentOrders(gid)[0].ID, "ON_HOLD")
	r.shipInKeel(t, no, "sf", "SF0000001")
	adminExec(t, `UPDATE jobs SET max_attempts = 1 WHERE queue = $1 AND job_key = $2`, service.QueueChannelOrderAction,
		"act:"+itoa(co)+":ship")
	r.drain(t)
	if n := len(r.sim.Fulfillments(gid)); n != 0 {
		t.Fatalf("FO 暂停着却建了 %d 条 fulfillment", n)
	}
	if got := adminQueryString(t, `SELECT coalesce(exception, '') FROM channel_orders WHERE id = $1`, co); !strings.Contains(got, "发货没回传上") ||
		!strings.Contains(got, "ON_HOLD") {
		t.Fatalf("渠道单异常 %q，期望「发货没回传上」且写明 ON_HOLD", got)
	}
}
