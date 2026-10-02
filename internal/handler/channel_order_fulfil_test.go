package handler_test

// 第三期 Task 5：keel 发货回传平台、平台上的取消 / 退款 / 发货转成 keel 订单上的动作
// （打 Shopify 模拟平台 + 真实适配器 + 真库）。Review Focus 5、6 各有一条。

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/channel/shopify/shopifytest"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// paidChannelOrder：模拟店卖出 2 件、回调进来 → keel 订单 20。返回（订单 gid、渠道单 id、keel 单号）。
func (r *orderRig) paidChannelOrder(t *testing.T) (string, int64, string) {
	t.Helper()
	gid := r.twoPieces()
	r.orderWebhook(t, gid, "orders/create")
	co := r.channelOrderID(t, gid)
	no := adminQueryString(t, `SELECT order_no FROM orders WHERE channel_order_id = $1 AND status = 20`, co)
	return gid, co, no
}

// shipInKeel 走后台发货（AdminOrderService 接着这台渠道服务）。
func (r *orderRig) shipInKeel(t *testing.T, orderNo, carrier, tracking string) {
	t.Helper()
	mid := r.cs.MerchantID
	ctx := auth.NewStaffContext(r.ctx, auth.StaffIdentity{StaffID: r.cs.StaffID, MerchantID: &mid,
		Role: auth.StaffRoleAdmin, Status: auth.StaffStatusActive})
	svc := service.NewAdminOrderService(repository.New(testPool)).WithChannels(r.svc)
	if _, _, err := svc.Ship(ctx, orderNo, service.ShipRequest{CarrierCode: carrier, TrackingNo: tracking}, "ship-"+uniqueKey()); err != nil {
		t.Fatal(err)
	}
}

// restock 跑完库存 outbox（退款回补）。
func (r *orderRig) restock(t *testing.T) {
	t.Helper()
	if _, err := service.NewInventoryOutboxService(repository.New(testPool), r.local, service.InventoryOutboxConfig{}, nil).
		Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// noListingConflict：平台自己放回库存之后，keel 回补后的推送是一次正常的 CAS（基线跟着加了），没有记差异。
func (r *orderRig) noListingConflict(t *testing.T, when string) {
	t.Helper()
	r.drain(t)
	if got := adminQueryString(t, `SELECT coalesce(last_error, '') FROM channel_listings
	                                WHERE binding_id = $1 AND store_id = $2 AND sku_id = $3`, r.b.ID, r.cs.NorthStore, r.sku); got != "" {
		t.Fatalf("%s：推送记了差异 %q", when, got)
	}
	if q, _ := r.sim.Available(r.item, r.loc); int64(q) != r.stock(t, r.sku) {
		t.Fatalf("%s：模拟店 available %d，keel %d", when, q, r.stock(t, r.sku))
	}
}

func actionJobs(t *testing.T, co int64) int64 {
	t.Helper()
	return adminQueryInt64(t, `SELECT count(*) FROM jobs WHERE queue = $1 AND job_key LIKE $2`,
		service.QueueChannelOrderAction, "act:"+itoa(co)+":%")
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// keel 发货 → 模拟店上一条 fulfillment，带承运商与单号。Review Focus 6：第一次 fulfillmentCreate 平台做了、
// 回 503 → 重试（先读 FO，已经没有可发的）→ 模拟店上仍只有 1 条。之后平台的 fulfillments/create 回调不再建 keel 发货。
func TestKeelShipmentPushedToShopifyOnce(t *testing.T) {
	r := newOrderRig(t, false, 7)
	gid, co, no := r.paidChannelOrder(t)
	r.sim.FailNext("FulfillmentCreate", true)
	r.shipInKeel(t, no, "sf", "SF1234567")
	if n := actionJobs(t, co); n != 1 {
		t.Fatalf("发货后回传任务 %d 条，期望 1", n)
	}
	r.drain(t)
	if got := len(r.sim.Fulfillments(gid)); got != 1 {
		t.Fatalf("503 之后模拟店上的 fulfillment %d 条（还没重试就该已经落了 1 条）", got)
	}
	// 退避中的任务拨到现在，重跑。
	adminExec(t, `UPDATE jobs SET run_after = now() WHERE queue = $1 AND job_key = $2`, service.QueueChannelOrderAction,
		"act:"+itoa(co)+":ship")
	r.drain(t)
	fs := r.sim.Fulfillments(gid)
	if len(fs) != 1 || fs[0].Number != "SF1234567" || fs[0].Company != "SF Express" {
		t.Fatalf("模拟店上的 fulfillment = %+v，期望 1 条 SF Express / SF1234567", fs)
	}
	if st := adminQueryInt64(t, `SELECT status FROM jobs WHERE queue = $1 AND job_key = $2`, service.QueueChannelOrderAction,
		"act:"+itoa(co)+":ship"); st != 2 {
		t.Fatalf("回传任务状态 %d，期望 2 已成功", st)
	}
	// 平台发回 fulfillments/create：keel 已经 30，不再建发货、不回声。
	r.orderWebhook(t, gid, "fulfillments/create")
	if n := adminQueryInt64(t, `SELECT count(*) FROM shipments s JOIN orders o ON o.id = s.order_id WHERE o.order_no = $1`, no); n != 1 {
		t.Fatalf("keel 上的发货 %d 条，期望 1", n)
	}
	if n := actionJobs(t, co); n != 1 {
		t.Fatalf("回传任务 %d 条，期望仍是 1", n)
	}
	if got := adminQueryString(t, `SELECT status || ':' || coalesce(exception, '') FROM channel_orders WHERE id = $1`, co); got != "4:" {
		t.Fatalf("渠道单 状态:异常 = %q，期望 4:", got)
	}
}

// 回传一直失败到死信 → 渠道单标异常「发货没回传上」，门店收到通知。
func TestShipmentPushDeadLetterMarksException(t *testing.T) {
	r := newOrderRig(t, false, 7)
	_, co, no := r.paidChannelOrder(t)
	r.shipInKeel(t, no, "sf", "SF7654321")
	adminExec(t, `UPDATE jobs SET max_attempts = 1 WHERE queue = $1 AND job_key = $2`, service.QueueChannelOrderAction,
		"act:"+itoa(co)+":ship")
	r.sim.FailNext("Order", false)
	r.drain(t)
	if st := adminQueryInt64(t, `SELECT status FROM jobs WHERE queue = $1 AND job_key = $2`, service.QueueChannelOrderAction,
		"act:"+itoa(co)+":ship"); st != 3 {
		t.Fatalf("回传任务状态 %d，期望 3 死信", st)
	}
	if got := adminQueryString(t, `SELECT coalesce(exception, '') FROM channel_orders WHERE id = $1`, co); !strings.Contains(got, "发货没回传上") {
		t.Fatalf("渠道单异常 %q，期望「发货没回传上」", got)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM notifications WHERE order_no = $1 AND kind = 'merchant_channel_order_exception'`, no); n != 1 {
		t.Fatalf("门店通知 %d 条，期望 1", n)
	}
	if orderStatusOf(t, no) != 30 {
		t.Fatal("回传失败不该动 keel 订单")
	}
}

// Review Focus 5（一）：平台在 keel 发货前取消 → 20 → 50 → 60、refunded = paid、库存回补一次、不经支付渠道、
// 不入队回传；重放（含平台上的新版本）不重复退款。
func TestPlatformCancelBeforeShipRefundsWholeOrder(t *testing.T) {
	r := newOrderRig(t, false, 7)
	gid, co, no := r.paidChannelOrder(t)
	if got := r.stock(t, r.sku); got != 5 {
		t.Fatalf("接单后 keel 库存 %d", got)
	}
	r.sim.Cancel(gid, true)
	r.orderWebhook(t, gid, "orders/cancelled")
	r.restock(t)

	check := func(when string) {
		t.Helper()
		var status, refundStatus int16
		var paid, refunded int64
		if err := admin(t).QueryRow(context.Background(), `SELECT status, refund_status, paid_cents, refunded_cents
		   FROM orders WHERE order_no = $1`, no).Scan(&status, &refundStatus, &paid, &refunded); err != nil {
			t.Fatal(err)
		}
		if status != 60 || refundStatus != 3 || refunded != paid || paid != 3980 {
			t.Fatalf("%s：keel 订单 status=%d refund_status=%d paid=%d refunded=%d", when, status, refundStatus, paid, refunded)
		}
		if got := adminQueryString(t, `SELECT count(*) || ':' || min(r.status) || ':' || min(r.channel) || ':' || sum(r.amount_cents) ||
		        ':' || bool_and(r.payment_id IS NULL AND r.user_id IS NULL)
		   FROM refunds r JOIN orders o ON o.id = r.order_id WHERE o.order_no = $1`, no); got != "1:40:10:3980:true" {
			t.Fatalf("%s：退款单 数:状态:渠道:金额:无买家无支付 = %q", when, got)
		}
		if got := adminQueryInt64(t, `SELECT coalesce(sum(refunded_qty), 0) FROM order_items oi JOIN orders o ON o.id = oi.order_id
		   WHERE o.order_no = $1`, no); got != 2 {
			t.Fatalf("%s：订单行已退件数 %d，期望 2", when, got)
		}
		if got := r.stock(t, r.sku); got != 7 {
			t.Fatalf("%s：keel 库存 %d，期望回补到 7", when, got)
		}
		if n := actionJobs(t, co); n != 0 {
			t.Fatalf("%s：入队了 %d 条渠道动作", when, n)
		}
		if st := adminQueryInt64(t, `SELECT status FROM channel_orders WHERE id = $1`, co); st != 6 {
			t.Fatalf("%s：渠道单状态 %d，期望 6 已取消", when, st)
		}
	}
	check("取消后")
	r.noListingConflict(t, "取消后")
	if n := adminQueryInt64(t, `SELECT count(*) FROM payments p JOIN orders o ON o.id = p.order_id WHERE o.order_no = $1`, no); n != 0 {
		t.Fatalf("渠道单有 %d 条 payments 行", n)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM notifications WHERE order_no = $1 AND audience = 1`, no); n != 0 {
		t.Fatalf("发了 %d 条买家通知", n)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM notifications WHERE order_no = $1 AND kind = 'merchant_channel_order_exception'`, no); n != 1 {
		t.Fatalf("门店通知 %d 条，期望 1", n)
	}

	// 重放同一事件；再来一个平台上的新版本（updatedAt 前进）—— 都不重复退款、不重复回补。
	r.orderWebhook(t, gid, "orders/cancelled")
	r.sim.SetFinancial(gid, "REFUNDED")
	r.orderWebhook(t, gid, "orders/updated")
	r.restock(t)
	check("重放后")
}

// Review Focus 5（二）：keel 已发货后平台才取消 → keel 订单不动，渠道单标异常，门店收到通知。
func TestPlatformCancelAfterShipMarksException(t *testing.T) {
	r := newOrderRig(t, false, 7)
	gid, co, no := r.paidChannelOrder(t)
	r.shipInKeel(t, no, "sf", "SF5550001")
	r.drain(t)
	r.sim.Cancel(gid, false)
	r.orderWebhook(t, gid, "orders/cancelled")
	if st := orderStatusOf(t, no); st != 30 {
		t.Fatalf("keel 订单 %d，期望仍是 30", st)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM refunds r JOIN orders o ON o.id = r.order_id WHERE o.order_no = $1`, no); n != 0 {
		t.Fatalf("发货后取消记了 %d 张退款单", n)
	}
	if got := adminQueryString(t, `SELECT status || ':' || coalesce(exception, '') FROM channel_orders WHERE id = $1`, co); got != "6:平台在 keel 发货后取消了订单" {
		t.Fatalf("渠道单 状态:异常 = %q", got)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM notifications WHERE order_no = $1 AND kind = 'merchant_channel_order_exception'`, no); n != 1 {
		t.Fatalf("门店通知 %d 条，期望 1", n)
	}
	if got := r.stock(t, r.sku); got != 5 {
		t.Fatalf("keel 库存 %d，期望不动 5", got)
	}
}

// 平台上的部分退款（放回库存）：记一张成功退款单、行退款数、refund_status 2；未发货 → 回补；按平台退款 ID 幂等。
func TestPlatformPartialRefundRestocks(t *testing.T) {
	r := newOrderRig(t, false, 7)
	gid, _, no := r.paidChannelOrder(t)
	lines := r.sim.LineItemIDs(gid)
	r.sim.Refund(gid, []shopifytest.RefundLine{{LineItem: lines[0], Qty: 1}}, "", true)
	r.orderWebhook(t, gid, "refunds/create")
	r.restock(t)
	check := func(when string) {
		t.Helper()
		if got := adminQueryString(t, `SELECT status || ':' || refund_status || ':' || refunded_cents FROM orders WHERE order_no = $1`, no); got != "20:2:1990" {
			t.Fatalf("%s：keel 订单 状态:退款状态:已退 = %q，期望 20:2:1990", when, got)
		}
		if got := adminQueryString(t, `SELECT count(*) || ':' || sum(ri.quantity) || ':' || sum(ri.amount_cents)
		   FROM refunds r JOIN refund_items ri ON ri.refund_id = r.id JOIN orders o ON o.id = r.order_id
		  WHERE o.order_no = $1 AND r.status = 40`, no); got != "1:1:1990" {
			t.Fatalf("%s：退款明细 数:件:金额 = %q", when, got)
		}
		if got := r.stock(t, r.sku); got != 6 {
			t.Fatalf("%s：keel 库存 %d，期望 5 + 1 = 6", when, got)
		}
	}
	check("退款后")
	r.noListingConflict(t, "退款后")
	r.orderWebhook(t, gid, "refunds/create")
	r.sim.SetFinancial(gid, "PARTIALLY_REFUNDED")
	r.orderWebhook(t, gid, "orders/updated")
	r.restock(t)
	check("重放后")
}

// 在 Shopify 后台发货 → keel 订单 30（承运商、单号是平台给的），不入队回传；之后超过自动确认天数 → 40。
func TestShopifySideFulfillmentShipsKeelOrderWithoutEcho(t *testing.T) {
	r := newOrderRig(t, false, 7)
	gid, co, no := r.paidChannelOrder(t)
	if r.sim.FulfillInShopify(gid, "UPS", "1Z999AA10123456784") == "" {
		t.Fatal("模拟店没发出货")
	}
	r.orderWebhook(t, gid, "fulfillments/create")
	if st := orderStatusOf(t, no); st != 30 {
		t.Fatalf("keel 订单 %d，期望 30", st)
	}
	if got := adminQueryString(t, `SELECT s.carrier_code || '/' || s.tracking_no || '/' || (s.created_by IS NULL)
	   FROM shipments s JOIN orders o ON o.id = s.order_id WHERE o.order_no = $1`, no); got != "UPS/1Z999AA10123456784/true" {
		t.Fatalf("keel 发货 = %q", got)
	}
	if n := actionJobs(t, co); n != 0 {
		t.Fatalf("平台发的货回传了 %d 次（回声）", n)
	}
	if got := len(r.sim.Fulfillments(gid)); got != 1 {
		t.Fatalf("模拟店上 fulfillment %d 条", got)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM notifications WHERE order_no = $1 AND audience = 1`, no); n != 0 {
		t.Fatalf("发了 %d 条买家通知", n)
	}

	shippedDaysAgo(t, no, 8) // 默认 auto_confirm_days = 7
	if _, err := newConfirmer().ConfirmOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := orderStatusOf(t, no); st != 40 {
		t.Fatalf("渠道单发货满 8 天后 keel 订单 %d，期望 40", st)
	}
}
