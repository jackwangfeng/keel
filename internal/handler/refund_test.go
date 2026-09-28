package handler_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 售后（数据模型 §11，迁移 00034）的端到端测试。夹具是 couponShop：一家新开的店，
// 连衣裙 60 元、衬衫 50 元，华北门店各 50 件；支付走真的支付回调，退款审核通过后
// 由沙箱渠道（testEngine 的 Sandbox: true）在同一个事务里走真的退款入账路径。

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// twoLineOrder 在华北门店下一单：连衣裙 × 2（120 元）+ 衬衫 × 1（50 元），可带券。
func (cs couponShop) twoLineOrder(t *testing.T, b couponBuyer, couponID *int64) api.Order {
	t.Helper()
	body := fmt.Sprintf(`{"items":[{"sku_id":%d,"quantity":2},{"sku_id":%d,"quantity":1}],"address_id":%d,"store_id":%d`,
		cs.DressSKU, cs.ShirtSKU, b.Address, cs.NorthStore)
	if couponID != nil {
		body += fmt.Sprintf(`,"user_coupon_id":%d`, *couponID)
	}
	var o api.Order
	decodeInto(t, createOrder(t, cs.Host, body+"}", b.Token, "rf-"+uniqueKey()), http.StatusCreated, "下两行的单", &o)
	return o
}

// lines 读订单详情里的行，按 sku 索引。
func (cs couponShop) lines(t *testing.T, b couponBuyer, orderNo string) (api.OrderDetail, map[int64]api.OrderItem) {
	t.Helper()
	var d api.OrderDetail
	decodeInto(t, getAs(t, cs.Host, "/api/v1/orders/"+orderNo, b.Token), http.StatusOK, "订单详情", &d)
	out := map[int64]api.OrderItem{}
	for _, it := range *d.Items {
		out[it.SkuId] = it
	}
	return d, out
}

func refundBody(refundType int, lines ...[2]int64) string {
	parts := make([]string, 0, len(lines))
	for _, l := range lines {
		parts = append(parts, fmt.Sprintf(`{"order_item_id":%d,"quantity":%d}`, l[0], l[1]))
	}
	return fmt.Sprintf(`{"items":[%s],"refund_type":%d,"reason_code":1}`, strings.Join(parts, ","), refundType)
}

func applyRefund(t *testing.T, host, orderNo, token, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	return postWithKey(t, host, "/api/v1/orders/"+orderNo+"/refunds", body, token, key)
}

func (cs couponShop) mustApply(t *testing.T, b couponBuyer, orderNo, body string) api.Refund {
	t.Helper()
	var r api.Refund
	decodeInto(t, applyRefund(t, cs.Host, orderNo, b.Token, body, "rfa-"+uniqueKey()), http.StatusCreated, "申请退款", &r)
	return r
}

func (cs couponShop) audit(t *testing.T, refundNo, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postIdem(t, cs.Host, "/api/v1/admin/refunds/"+refundNo+"/audit", body, cs.Token)
}

func (cs couponShop) mustApprove(t *testing.T, refundNo string) api.Refund {
	t.Helper()
	var r api.Refund
	decodeInto(t, cs.audit(t, refundNo, `{"action":"approve"}`), http.StatusOK, "审核通过", &r)
	return r
}

type orderMoney struct {
	Status, RefundStatus int16
	Paid, Refunded       int64
}

func orderMoneyOf(t *testing.T, orderNo string) orderMoney {
	t.Helper()
	var m orderMoney
	if err := admin(t).QueryRow(context.Background(), `
		SELECT status, refund_status, paid_cents, refunded_cents FROM orders WHERE order_no = $1`,
		orderNo).Scan(&m.Status, &m.RefundStatus, &m.Paid, &m.Refunded); err != nil {
		t.Fatalf("读订单 %s 失败: %v", orderNo, err)
	}
	return m
}

func refundStatusOf(t *testing.T, refundNo string) int16 {
	t.Helper()
	return int16(adminQueryInt64(t, `SELECT status FROM refunds WHERE refund_no = $1`, refundNo))
}

func notifyRefund(t *testing.T, cs couponShop, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postJSON(t, cs.Host, "/api/v1/webhooks/refunds/wechat", body, "",
		map[string]string{service.SignatureHeader: sign(couponWebhookSecret(cs.adminShop), []byte(body))})
}

func refundPayload(refundNo, channelRefundID string, amount int64) string {
	return fmt.Sprintf(`{"refund_no":%q,"channel_refund_id":%q,"amount_cents":%d}`, refundNo, channelRefundID, amount)
}

// ---------------------------------------------------------------------------
// 部分退款：优惠按行分摊，退完一行钱退全
// ---------------------------------------------------------------------------

// README 那句「优惠按行分摊，所以部分退款能算对金额」的验收用例。
//
// 两件连衣裙（120 元）+ 一件衬衫（50 元），满 100 减 20。§7 的分摊：
// 连衣裙行 floor(2000 × 12000 / 17000) = 1411，衬衫行 floor(2000 × 5000 / 17000) = 588，
// 余 1 分给金额最大的连衣裙行 → 1412 / 588。实付 150 元。
//
//	第一次退 1 件连衣裙：floor((12000 - 1412) × 1 / 2) = 5294
//	第二次退剩下的连衣裙 + 衬衫：连衣裙 10588 - 5294 = 5294（最后一件吃掉余数），
//	                          衬衫 5000 - 588 = 4412
//
// 三笔之和 = 15000 = 实付，逐分相等。
func TestPartialRefundsAllocateTheCouponPerLine(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "partial")
	coupon := cs.wholeStoreCoupon(t, b)
	o := cs.twoLineOrder(t, b, &coupon.Id)
	if o.PayableCents != 15000 {
		t.Fatalf("应付 %d，期望 15000 —— 前提不成立", o.PayableCents)
	}
	cs.pay(t, o.OrderNo, o.PayableCents)
	wantStatus(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusCreated, "发货")
	stock := availableAt(t, cs.NorthStore, cs.DressSKU)

	_, lines := cs.lines(t, b, o.OrderNo)
	dress, shirt := lines[cs.DressSKU], lines[cs.ShirtSKU]
	if *dress.DiscountCents != 1412 || *shirt.DiscountCents != 588 {
		t.Fatalf("分摊是 %d / %d，期望 1412 / 588 —— §7 的余数规则前提不成立", *dress.DiscountCents, *shirt.DiscountCents)
	}

	// 第一张：退 1 件连衣裙。客户端不传金额，服务端按分摊倒算。
	r1 := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{dress.Id, 1}))
	if r1.Status != 10 || r1.AmountCents != 5294 || *r1.GoodsAmountCents != 5294 || *r1.FreightCents != 0 ||
		len(r1.Items) != 1 || r1.Items[0].AmountCents != 5294 || r1.Items[0].Quantity != 1 {
		t.Fatalf("第一张退款单不对：%+v", r1)
	}
	if r1.PaymentNo == nil || *r1.PaymentNo == "" || r1.Channel == nil || *r1.Channel != "wechat" {
		t.Fatalf("退款单应挂在原支付单上、渠道 wechat：payment_no=%v channel=%v", r1.PaymentNo, r1.Channel)
	}
	if m := orderMoneyOf(t, o.OrderNo); m.Status != 30 || m.RefundStatus != 1 || m.Refunded != 0 {
		t.Fatalf("申请之后订单是 %+v，期望 status 30 不动、refund_status 1、refunded 0", m)
	}
	// 在途件数出现在详情里：还可退 = 2 - 0 - 1 = 1。
	if _, ls := cs.lines(t, b, o.OrderNo); *ls[cs.DressSKU].RefundingQty != 1 {
		t.Fatalf("连衣裙行的 refunding_qty 是 %d，期望 1", *ls[cs.DressSKU].RefundingQty)
	}

	// 同一行在途时不能再申请（跨单重复必须业务层拦）。
	if p := problemOf(t, applyRefund(t, cs.Host, o.OrderNo, b.Token, refundBody(1, [2]int64{dress.Id, 1}), "x-"+uniqueKey()),
		http.StatusConflict); p.Type != problem.TypeRefundAlreadyInProgress {
		t.Fatalf("在途的行再申请应 409 refund-already-in-progress，实得 %+v", p)
	}
	// 件数超了。
	if p := problemOf(t, applyRefund(t, cs.Host, o.OrderNo, b.Token, refundBody(1, [2]int64{shirt.Id, 2}), "x-"+uniqueKey()),
		http.StatusConflict); p.Type != problem.TypeRefundQuantityExceeded {
		t.Fatalf("衬衫退 2 件应 409 refund-quantity-exceeded，实得 %+v", p)
	}
	// 不属于这一单的订单项。
	other := cs.placePaid(t, b, cs.NorthStore, cs.ShirtSKU, 1, nil)
	_, otherLines := cs.lines(t, b, other.OrderNo)
	if p := problemOf(t, applyRefund(t, cs.Host, o.OrderNo, b.Token,
		refundBody(1, [2]int64{otherLines[cs.ShirtSKU].Id, 1}), "x-"+uniqueKey()),
		http.StatusUnprocessableEntity); p.Type != problem.TypeOrderItemMismatch {
		t.Fatalf("别的订单的订单项应 422 order-item-mismatch，实得 %+v", p)
	}

	// 审核通过：沙箱渠道同一事务入账，响应即 40。
	done := cs.mustApprove(t, r1.RefundNo)
	if done.Status != 40 || done.ChannelRefundId == nil || !strings.HasPrefix(*done.ChannelRefundId, "KEEL-SANDBOX-R-") ||
		done.RefundedAt == nil || done.AuditedAt == nil {
		t.Fatalf("审核通过之后应是 40 已退款、带沙箱流水号与时间：%+v", done)
	}
	if m := orderMoneyOf(t, o.OrderNo); m.Status != 30 || m.RefundStatus != 2 || m.Refunded != 5294 {
		t.Fatalf("第一笔到账之后订单是 %+v，期望 status 30 不动、refund_status 2、refunded 5294", m)
	}
	// 已发货：不回补库存（保守规则）。
	if got := availableAt(t, cs.NorthStore, cs.DressSKU); got != stock {
		t.Fatalf("已发货订单的退款把库存从 %d 改成了 %d —— 不该自动回补", stock, got)
	}
	// 部分退款不退券。
	if st, _ := couponState(t, coupon.Id); st != 3 {
		t.Fatalf("部分退款之后券状态 %d，期望仍是 3 已使用", st)
	}

	// 第二张：剩下的连衣裙 + 衬衫。最后一件吃掉余数。
	r2 := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{dress.Id, 1}, [2]int64{shirt.Id, 1}))
	amounts := map[int64]int64{}
	for _, it := range r2.Items {
		amounts[it.OrderItemId] = int64(it.AmountCents)
	}
	if r2.AmountCents != 9706 || amounts[dress.Id] != 5294 || amounts[shirt.Id] != 4412 {
		t.Fatalf("第二张退款单是 %d（连衣裙 %d 衬衫 %d），期望 9706（5294 + 4412）",
			r2.AmountCents, amounts[dress.Id], amounts[shirt.Id])
	}
	cs.mustApprove(t, r2.RefundNo)

	// 退完了：退款总额 = 实付，逐分相等；资金维度 3；每行已退 = 净额。
	if m := orderMoneyOf(t, o.OrderNo); m.RefundStatus != 3 || m.Refunded != m.Paid || m.Paid != 15000 {
		t.Fatalf("全部退完之后订单是 %+v，期望 refund_status 3、refunded = paid = 15000", m)
	}
	var mismatched int64
	if err := admin(t).QueryRow(context.Background(), `
		SELECT count(*) FROM order_items oi JOIN orders o ON o.id = oi.order_id
		 WHERE o.order_no = $1
		   AND (oi.refunded_qty <> oi.quantity
		        OR oi.refunded_cents <> oi.amount_cents - oi.discount_cents
		        OR oi.refunded_cents <> (SELECT COALESCE(sum(ri.amount_cents), 0) FROM refund_items ri
		                                   JOIN refunds r ON r.id = ri.refund_id
		                                  WHERE ri.order_item_id = oi.id AND r.status = 40))`,
		o.OrderNo).Scan(&mismatched); err != nil {
		t.Fatal(err)
	}
	if mismatched != 0 {
		t.Fatalf("%d 行的已退件数 / 金额与净额或退款明细对不上 —— SUM(refund_items) = net 的恒等式破了", mismatched)
	}
	// 整单的货都退完了：券退回未使用（没过期）。
	if st, no := couponState(t, coupon.Id); st != 1 || no != nil {
		t.Fatalf("整单退完之后券状态 %d 订单 %v，期望 1 未使用", st, no)
	}
	// 再申请：一件都不剩了。
	if p := problemOf(t, applyRefund(t, cs.Host, o.OrderNo, b.Token, refundBody(1, [2]int64{shirt.Id, 1}), "x-"+uniqueKey()),
		http.StatusConflict); p.Type != problem.TypeRefundQuantityExceeded {
		t.Fatalf("退完之后再申请应 409 refund-quantity-exceeded，实得 %+v", p)
	}
	// 订单详情里的 refunds 按申请时间倒序。
	d, _ := cs.lines(t, b, o.OrderNo)
	if d.Refunds == nil || len(*d.Refunds) != 2 || (*d.Refunds)[0].RefundNo != r2.RefundNo {
		t.Fatalf("详情里的 refunds 应是 [r2, r1]：%+v", d.Refunds)
	}
}

// 除不尽的那一行：三件衬衫 150 元、满 100 减 20，净额 13000 分，三等分除不尽。
// 先退 1 件 floor(13000 / 3) = 4333，再退剩下 2 件 = 13000 - 4333 = 8667 ——
// 最后一次把余数退干净，合计 = 实付。把「最后一件吃掉余数」那一支删掉，
// 第二笔会变成 floor(13000 × 2 / 3) = 8666，少退 1 分，这条会红。
func TestLastRefundTakesTheRemainder(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "remainder")
	coupon := cs.wholeStoreCoupon(t, b)
	o := cs.placePaid(t, b, cs.NorthStore, cs.ShirtSKU, 3, &coupon.Id)
	if o.PayableCents != 13000 {
		t.Fatalf("应付 %d，期望 13000 —— 前提不成立", o.PayableCents)
	}
	wantStatus(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusCreated, "发货")
	_, lines := cs.lines(t, b, o.OrderNo)
	shirt := lines[cs.ShirtSKU]

	r1 := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{shirt.Id, 1}))
	cs.mustApprove(t, r1.RefundNo)
	r2 := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{shirt.Id, 2}))
	cs.mustApprove(t, r2.RefundNo)
	if r1.AmountCents != 4333 || r2.AmountCents != 8667 {
		t.Fatalf("两笔是 %d + %d，期望 4333 + 8667", r1.AmountCents, r2.AmountCents)
	}
	if m := orderMoneyOf(t, o.OrderNo); m.Refunded != m.Paid || m.RefundStatus != 3 {
		t.Fatalf("退完之后订单是 %+v，期望 refunded = paid、refund_status 3 —— 余数没退干净", m)
	}
}

// ---------------------------------------------------------------------------
// 未发货整单退：20 → 50 → 60，运费全退，库存回补，券退回
// ---------------------------------------------------------------------------

func TestWholeOrderRefundBeforeShipping(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "whole")
	coupon := cs.wholeStoreCoupon(t, b)
	before := availableAt(t, cs.NorthStore, cs.DressSKU)
	o := cs.twoLineOrder(t, b, &coupon.Id)
	cs.pay(t, o.OrderNo, o.PayableCents)
	_, lines := cs.lines(t, b, o.OrderNo)
	dress, shirt := lines[cs.DressSKU], lines[cs.ShirtSKU]

	// 未发货时只能仅退款。
	if p := problemOf(t, applyRefund(t, cs.Host, o.OrderNo, b.Token,
		refundBody(2, [2]int64{dress.Id, 2}, [2]int64{shirt.Id, 1}), "x-"+uniqueKey()),
		http.StatusUnprocessableEntity); p.Type != problem.TypeInvalidRequest {
		t.Fatalf("未发货申请退货退款应 422，实得 %+v", p)
	}

	r := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{dress.Id, 2}, [2]int64{shirt.Id, 1}))
	if r.AmountCents != 15000 {
		t.Fatalf("整单退金额 %d，期望 15000（= 实付）", r.AmountCents)
	}
	if m := orderMoneyOf(t, o.OrderNo); m.Status != 50 || m.RefundStatus != 1 {
		t.Fatalf("整单退款申请之后订单是 %+v，期望 50 退款中 / refund_status 1", m)
	}
	// 整单退款在途：发货被拒（§5 规则二），再申请售后也被拒。
	if p := problemOf(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusConflict); p.Type != problem.TypeOrderHasPendingFullRefund {
		t.Fatalf("整单退款中发货应 409 order-has-pending-full-refund，实得 %+v", p)
	}
	if p := problemOf(t, applyRefund(t, cs.Host, o.OrderNo, b.Token, refundBody(1, [2]int64{shirt.Id, 1}), "x-"+uniqueKey()),
		http.StatusConflict); p.Type != problem.TypeOrderStatusNotRefundable {
		t.Fatalf("50 退款中再申请应 409 order-status-not-refundable，实得 %+v", p)
	}

	cs.mustApprove(t, r.RefundNo)
	if m := orderMoneyOf(t, o.OrderNo); m.Status != 60 || m.RefundStatus != 3 || m.Refunded != m.Paid {
		t.Fatalf("整单退款到账之后订单是 %+v，期望 60 已退款 / 3 / refunded = paid", m)
	}
	// 没发过货：库存回补到履约门店，流水 biz_type 4、biz_id 是退款单号。
	if got := availableAt(t, cs.NorthStore, cs.DressSKU); got != before {
		t.Fatalf("未发货整单退之后连衣裙水位 %d，期望回到 %d", got, before)
	}
	logs := inventoryLogsOf(t, r.RefundNo)
	if len(logs) != 2 || logs[0].BizType != 4 || logs[1].BizType != 4 {
		t.Fatalf("退款回补应留下两行 biz_type 4 的流水（biz_id = 退款单号），实得 %+v", logs)
	}
	if st, _ := couponState(t, coupon.Id); st != 1 {
		t.Fatalf("整单退款之后券状态 %d，期望 1 未使用", st)
	}
	// 60 已退款：不能再发货。
	if p := problemOf(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusConflict); p.Type != problem.TypeOrderStatusNotShippable {
		t.Fatalf("已退款订单发货应 409 order-status-not-shippable，实得 %+v", p)
	}
}

// 整单退款被驳回：退款单 50 已拒绝（带理由），订单从 50 回到 20，资金维度回到 0，可以发货。
func TestRejectedWholeRefundReturnsTheOrderToPaid(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "reject")
	o := cs.placePaid(t, b, cs.NorthStore, cs.ShirtSKU, 1, nil)
	_, lines := cs.lines(t, b, o.OrderNo)
	r := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{lines[cs.ShirtSKU].Id, 1}))
	if st := orderStatusOf(t, o.OrderNo); st != 50 {
		t.Fatalf("整单退款申请之后订单是 %d，期望 50", st)
	}

	if p := problemOf(t, cs.audit(t, r.RefundNo, `{"action":"reject"}`), http.StatusUnprocessableEntity); p.Type != problem.TypeInvalidRequest {
		t.Fatalf("驳回不带理由应 422，实得 %+v", p)
	}
	if p := problemOf(t, cs.audit(t, r.RefundNo, `{"action":"approve","freight_cents":100}`), http.StatusUnprocessableEntity); p.Type != problem.TypeInvalidRequest {
		t.Fatalf("仅退款改运费应 422，实得 %+v", p)
	}
	var rej api.Refund
	decodeInto(t, cs.audit(t, r.RefundNo, `{"action":"reject","reject_reason":"商品已拆封"}`), http.StatusOK, "驳回", &rej)
	if rej.Status != 50 || rej.RejectReason == nil || *rej.RejectReason != "商品已拆封" || rej.AuditedAt == nil {
		t.Fatalf("驳回之后退款单是 %+v", rej)
	}
	if m := orderMoneyOf(t, o.OrderNo); m.Status != 20 || m.RefundStatus != 0 || m.Refunded != 0 {
		t.Fatalf("驳回之后订单是 %+v，期望回到 20 / 0 / 0", m)
	}
	if p := problemOf(t, cs.audit(t, r.RefundNo, `{"action":"approve"}`), http.StatusConflict); p.Type != problem.TypeRefundStatusNotAuditable {
		t.Fatalf("已驳回的单再审应 409 refund-status-not-auditable，实得 %+v", p)
	}
	wantStatus(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusCreated, "驳回之后发货")
}

// 未发货订单按行分别退（前一张还在处理时申请把剩下的退完）：拒，引导撤回后整单退（整单退连运费一起退、订单进 50）。
// 兜底：每一件都已退完 / 在退的单不能发货。
// 2026-09-28 破坏性测试：两张各自都不是整单退，运费谁也不退，订单停在 20，货款全退、库存回补之后货照样发出。
func TestSplitRefundThatWouldEmptyAnUnshippedOrderIsRefused(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "split-refund")
	o := cs.twoLineOrder(t, b, nil)
	cs.pay(t, o.OrderNo, o.PayableCents)
	_, lines := cs.lines(t, b, o.OrderNo)
	first := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{lines[cs.DressSKU].Id, 2}))

	w := applyRefund(t, cs.Host, o.OrderNo, b.Token, refundBody(1, [2]int64{lines[cs.ShirtSKU].Id, 1}), "r-"+uniqueKey())
	if p := problemOf(t, w, http.StatusConflict); p.Type != problem.TypeRefundAlreadyInProgress {
		t.Fatalf("前一张在处理、这张加上它正好退完未发货的单：应 409 refund-already-in-progress，实得 %+v", p)
	}
	// 发货兜底：把两行都记成已退完（模拟历史上按行退完的单），发货应拒。
	adminExec(t, `UPDATE order_items SET refunded_qty = quantity WHERE order_id = (SELECT id FROM orders WHERE order_no = $1)`, o.OrderNo)
	if p := problemOf(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusConflict); p.Type != problem.TypeOrderStatusNotShippable {
		t.Fatalf("每一件都已退完的单不该能发货：%+v", p)
	}
	adminExec(t, `UPDATE order_items SET refunded_qty = 0 WHERE order_id = (SELECT id FROM orders WHERE order_no = $1)`, o.OrderNo)

	// 撤回前一张，整单申请：连运费一起退，订单进 50。
	wantStatus(t, postWithKey(t, cs.Host, "/api/v1/refunds/"+first.RefundNo+"/cancel", "", b.Token, "c-"+uniqueKey()), http.StatusOK, "撤回")
	whole := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{lines[cs.DressSKU].Id, 2}, [2]int64{lines[cs.ShirtSKU].Id, 1}))
	if whole.AmountCents != o.PayableCents {
		t.Fatalf("整单退应退实付 %d（含运费）：%+v", o.PayableCents, whole)
	}
	if st := orderStatusOf(t, o.OrderNo); st != 50 {
		t.Fatalf("整单退后订单应进 50，实得 %d", st)
	}
}

// 别人的订单申请不了售后：404 order-not-found，与「订单不存在」同一个响应，也不落任何退款单。
// 退款金额由服务端倒算、钱退回原支付渠道，但一张挂在别人名下的退款单本身就是越权。
func TestRefundOnAnotherBuyersOrderIs404(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "rother-owner")
	other := cs.newBuyer(t, "rother-intruder")
	o := cs.twoLineOrder(t, b, nil)
	cs.pay(t, o.OrderNo, o.PayableCents)
	_, lines := cs.lines(t, b, o.OrderNo)

	w := applyRefund(t, cs.Host, o.OrderNo, other.Token, refundBody(1, [2]int64{lines[cs.ShirtSKU].Id, 1}), "r-"+uniqueKey())
	if p := problemOf(t, w, http.StatusNotFound); p.Type != problem.TypeOrderNotFound {
		t.Fatalf("给别人的订单申请售后应 404 order-not-found，实得 %+v", p)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM refunds r JOIN orders o ON o.id = r.order_id WHERE o.order_no = $1`, o.OrderNo); n != 0 {
		t.Fatalf("别人的申请在这笔订单下落了 %d 张退款单", n)
	}
	// 本人照常能申请：上面的 404 不是订单本身的问题。
	cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{lines[cs.ShirtSKU].Id, 1}))
}

// ---------------------------------------------------------------------------
// 撤回
// ---------------------------------------------------------------------------

func TestBuyerCancelsARefund(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "rcancel")
	other := cs.newBuyer(t, "rcancel-other")
	o := cs.twoLineOrder(t, b, nil)
	cs.pay(t, o.OrderNo, o.PayableCents)
	_, lines := cs.lines(t, b, o.OrderNo)
	r := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{lines[cs.ShirtSKU].Id, 1}))

	// 别人撤不了、看不到。
	if p := problemOf(t, postWithKey(t, cs.Host, "/api/v1/refunds/"+r.RefundNo+"/cancel", "", other.Token, "c-"+uniqueKey()),
		http.StatusNotFound); p.Type != problem.TypeNotFound {
		t.Fatalf("撤回别人的退款单应 404，实得 %+v", p)
	}
	wantStatus(t, getAs(t, cs.Host, "/api/v1/refunds/"+r.RefundNo, other.Token), http.StatusNotFound, "看别人的退款单")

	key := "c-" + uniqueKey()
	var got api.Refund
	decodeInto(t, postWithKey(t, cs.Host, "/api/v1/refunds/"+r.RefundNo+"/cancel", "", b.Token, key), http.StatusOK, "撤回", &got)
	if got.Status != 60 {
		t.Fatalf("撤回之后退款单是 %d，期望 60", got.Status)
	}
	if m := orderMoneyOf(t, o.OrderNo); m.Status != 20 || m.RefundStatus != 0 {
		t.Fatalf("撤回部分退款之后订单是 %+v，期望 20 / 0", m)
	}
	w := postWithKey(t, cs.Host, "/api/v1/refunds/"+r.RefundNo+"/cancel", "", b.Token, key)
	wantStatus(t, w, http.StatusOK, "撤回重放")
	if w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("撤回重放没有带 Idempotency-Replayed")
	}
	if p := problemOf(t, postWithKey(t, cs.Host, "/api/v1/refunds/"+r.RefundNo+"/cancel", "", b.Token, "c-"+uniqueKey()),
		http.StatusConflict); p.Type != problem.TypeRefundStatusNotCancelable {
		t.Fatalf("已取消的单再撤回应 409 refund-status-not-cancelable，实得 %+v", p)
	}

	// 撤回之后同一行可以重新申请；已退款（40）的单撤不回来。
	r2 := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{lines[cs.ShirtSKU].Id, 1}))
	cs.mustApprove(t, r2.RefundNo)
	if p := problemOf(t, postWithKey(t, cs.Host, "/api/v1/refunds/"+r2.RefundNo+"/cancel", "", b.Token, "c-"+uniqueKey()),
		http.StatusConflict); p.Type != problem.TypeRefundStatusNotCancelable {
		t.Fatalf("已退款的单撤回应 409，实得 %+v", p)
	}

	// 我的退款单：两张，按状态筛得出来，total 跟着筛。
	var page struct {
		api.PageMeta
		Items []api.Refund `json:"items"`
	}
	decodeInto(t, getAs(t, cs.Host, "/api/v1/refunds", b.Token), http.StatusOK, "我的退款单", &page)
	if page.Total != 2 || len(page.Items) != 2 || page.Items[0].RefundNo != r2.RefundNo {
		t.Fatalf("我的退款单应是 [r2, r1]、total 2：%+v", page)
	}
	decodeInto(t, getAs(t, cs.Host, "/api/v1/refunds?status=60", b.Token), http.StatusOK, "按状态筛", &page)
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].RefundNo != r.RefundNo {
		t.Fatalf("status=60 应只有 r1：%+v", page)
	}
	decodeInto(t, getAs(t, cs.Host, "/api/v1/refunds", other.Token), http.StatusOK, "别人的退款单", &page)
	if page.Total != 0 {
		t.Fatalf("另一个买家看到了 %d 张退款单", page.Total)
	}
	var byOrder []api.Refund
	decodeInto(t, getAs(t, cs.Host, "/api/v1/orders/"+o.OrderNo+"/refunds", b.Token), http.StatusOK, "订单的退款单", &byOrder)
	if len(byOrder) != 2 {
		t.Fatalf("订单的退款单应有 2 张，实得 %d", len(byOrder))
	}
	wantStatus(t, getAs(t, cs.Host, "/api/v1/orders/"+o.OrderNo+"/refunds", other.Token), http.StatusNotFound, "别人订单的退款单")
}

// ---------------------------------------------------------------------------
// 退货退款：审核 → 待买家退货 → 商家确认收货 → 入账
// ---------------------------------------------------------------------------

func TestReturnAndRefund(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "return")
	o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 2, nil)
	wantStatus(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusCreated, "发货")
	wantStatus(t, orderAction(t, cs.Host, o.OrderNo, "confirm", b.Token, "cf-"+uniqueKey()), http.StatusOK, "确认收货")
	stock := availableAt(t, cs.NorthStore, cs.DressSKU)
	_, lines := cs.lines(t, b, o.OrderNo)

	// 已完成的订单也能申请售后（§5：停在 40，资金维度走）。
	r := cs.mustApply(t, b, o.OrderNo, refundBody(2, [2]int64{lines[cs.DressSKU].Id, 1}))
	if p := problemOf(t, postIdem(t, cs.Host, "/api/v1/admin/refunds/"+r.RefundNo+"/receipt", "", cs.Token),
		http.StatusConflict); p.Type != problem.TypeRefundStatusNotReceivable {
		t.Fatalf("待审核的单确认收货应 409 refund-status-not-receivable，实得 %+v", p)
	}
	// 运费裁定超过实收运费（这家店没配运费模板，实收运费是 0；有运费的情形见 freight_test.go）。
	if p := problemOf(t, cs.audit(t, r.RefundNo, `{"action":"approve","freight_cents":1}`),
		http.StatusUnprocessableEntity); p.Type != problem.TypeRefundFreightExceeded {
		t.Fatalf("退运费超过实收应 422 refund-freight-exceeded，实得 %+v", p)
	}
	approved := cs.mustApprove(t, r.RefundNo)
	if approved.Status != 20 {
		t.Fatalf("退货退款审核通过应到 20 待买家退货，实得 %d", approved.Status)
	}
	var done api.Refund
	decodeInto(t, postIdem(t, cs.Host, "/api/v1/admin/refunds/"+r.RefundNo+"/receipt", "", cs.Token),
		http.StatusOK, "确认收到退货", &done)
	if done.Status != 40 || done.AmountCents != 6000 {
		t.Fatalf("确认收货之后应入账到 40、退 6000：%+v", done)
	}
	if m := orderMoneyOf(t, o.OrderNo); m.Status != 40 || m.RefundStatus != 2 || m.Refunded != 6000 {
		t.Fatalf("退货退款到账之后订单是 %+v，期望停在 40 / 2 / 6000", m)
	}
	if got := availableAt(t, cs.NorthStore, cs.DressSKU); got != stock {
		t.Fatalf("退回来的货被自动加回了可售（%d → %d）—— 验货入库应是手工调整", stock, got)
	}
	if p := problemOf(t, postIdem(t, cs.Host, "/api/v1/admin/refunds/"+r.RefundNo+"/receipt", "", cs.Token),
		http.StatusConflict); p.Type != problem.TypeRefundStatusNotReceivable {
		t.Fatalf("已退款的单再确认收货应 409，实得 %+v", p)
	}
}

// ---------------------------------------------------------------------------
// 退款回调：与支付回调同构（签名、金额校验、渠道流水号幂等）
// ---------------------------------------------------------------------------

// 退款单停在 30（模拟「沙箱关着、等真实渠道回调」），然后由渠道回调入账。
func TestRefundWebhookIsIsomorphicToThePaymentWebhook(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "webhook")
	o := cs.placePaid(t, b, cs.NorthStore, cs.ShirtSKU, 2, nil)
	wantStatus(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusCreated, "发货")
	_, lines := cs.lines(t, b, o.OrderNo)
	shirt := lines[cs.ShirtSKU]
	r := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{shirt.Id, 1}))
	// 10 → 30 是状态机里的合法边（审核通过、仅退款）；直接推，绕开沙箱渠道。
	adminExec(t, `UPDATE refunds SET status = 30, audited_at = now() WHERE refund_no = $1`, r.RefundNo)

	txn := "WX-RF-" + uniqueKey()
	good := refundPayload(r.RefundNo, txn, r.AmountCents)

	// 签名不对：401，什么都不动。
	w := postJSON(t, cs.Host, "/api/v1/webhooks/refunds/wechat", good, "",
		map[string]string{service.SignatureHeader: sign("wrong-secret", []byte(good))})
	if w.Code != http.StatusUnauthorized || w.Body.Len() != 0 {
		t.Fatalf("签名不对应 401 且空响应体，实得 %d %q", w.Code, w.Body.String())
	}
	// 金额不符：200（渠道重推没用），状态不动，原始报文留下。
	wantStatus(t, notifyRefund(t, cs, refundPayload(r.RefundNo, txn, r.AmountCents+1)), http.StatusOK, "金额不符的回调")
	if st := refundStatusOf(t, r.RefundNo); st != 30 {
		t.Fatalf("金额不符之后退款单是 %d，期望仍是 30", st)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM refunds WHERE refund_no = $1 AND notify_payload IS NOT NULL`, r.RefundNo); n != 1 {
		t.Fatal("金额不符的回调没有留下原始报文")
	}
	// 查不到的退款单：200。
	wantStatus(t, notifyRefund(t, cs, refundPayload("NOPE"+uniqueKey(), txn, 1)), http.StatusOK, "查不到的退款单")

	// 正常入账。
	wantStatus(t, notifyRefund(t, cs, good), http.StatusOK, "退款回调")
	if st := refundStatusOf(t, r.RefundNo); st != 40 {
		t.Fatalf("回调之后退款单是 %d，期望 40", st)
	}
	after := orderMoneyOf(t, o.OrderNo)
	if after.Refunded != r.AmountCents || after.RefundStatus != 2 {
		t.Fatalf("回调之后订单是 %+v", after)
	}

	// 重复推送：200，一分钱都不多记。
	wantStatus(t, notifyRefund(t, cs, good), http.StatusOK, "重复推送")
	if again := orderMoneyOf(t, o.OrderNo); again.Refunded != after.Refunded {
		t.Fatalf("重复推送把已退金额从 %d 改成了 %d —— 幂等失效", after.Refunded, again.Refunded)
	}

	// 另一张退款单拿同一个渠道流水号来：撞 uk_refunds_channel_txn，200，第二张不动。
	r2 := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{shirt.Id, 1}))
	adminExec(t, `UPDATE refunds SET status = 30, audited_at = now() WHERE refund_no = $1`, r2.RefundNo)
	wantStatus(t, notifyRefund(t, cs, refundPayload(r2.RefundNo, txn, r2.AmountCents)), http.StatusOK, "流水号撞车")
	if st := refundStatusOf(t, r2.RefundNo); st != 30 {
		t.Fatalf("流水号撞车的回调把第二张退款单推到了 %d", st)
	}
	if m := orderMoneyOf(t, o.OrderNo); m.Refunded != after.Refunded {
		t.Fatalf("流水号撞车的回调记了钱：%d → %d", after.Refunded, m.Refunded)
	}
	// 用自己的流水号就能入账。
	wantStatus(t, notifyRefund(t, cs, refundPayload(r2.RefundNo, txn+"-2", r2.AmountCents)), http.StatusOK, "第二张回调")
	if m := orderMoneyOf(t, o.OrderNo); m.RefundStatus != 3 || m.Refunded != m.Paid {
		t.Fatalf("两张都到账之后订单是 %+v，期望 3 / refunded = paid", m)
	}

	// 渠道名不在契约里：401（与支付回调一样不泄露）。
	w = postJSON(t, cs.Host, "/api/v1/webhooks/refunds/balance", good, "",
		map[string]string{service.SignatureHeader: sign(couponWebhookSecret(cs.adminShop), []byte(good))})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("balance 渠道的退款回调应 401，实得 %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// 幂等
// ---------------------------------------------------------------------------

func TestRefundWritesAreIdempotent(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "ridem")
	o := cs.twoLineOrder(t, b, nil)
	cs.pay(t, o.OrderNo, o.PayableCents)
	_, lines := cs.lines(t, b, o.OrderNo)
	body := refundBody(1, [2]int64{lines[cs.ShirtSKU].Id, 1})

	key := "ri-" + uniqueKey()
	var first, second api.Refund
	decodeInto(t, applyRefund(t, cs.Host, o.OrderNo, b.Token, body, key), http.StatusCreated, "申请", &first)
	w := applyRefund(t, cs.Host, o.OrderNo, b.Token, body, key)
	decodeInto(t, w, http.StatusCreated, "申请重放", &second)
	if w.Header().Get("Idempotency-Replayed") != "true" || second.RefundNo != first.RefundNo {
		t.Fatalf("同一把钥匙第二次申请应重放首次那张（%s），实得 %s replayed=%q",
			first.RefundNo, second.RefundNo, w.Header().Get("Idempotency-Replayed"))
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM refunds r JOIN orders o ON o.id = r.order_id WHERE o.order_no = $1`, o.OrderNo); n != 1 {
		t.Fatalf("重放之后库里有 %d 张退款单，期望 1", n)
	}
	if p := problemOf(t, applyRefund(t, cs.Host, o.OrderNo, b.Token, refundBody(1, [2]int64{lines[cs.DressSKU].Id, 1}), key),
		http.StatusUnprocessableEntity); p.Type != problem.TypeIdempotencyKeyReused {
		t.Fatalf("同一把钥匙配不同请求体应 422 idempotency-key-reused，实得 %+v", p)
	}
	if p := problemOf(t, postJSON(t, cs.Host, "/api/v1/orders/"+o.OrderNo+"/refunds", body, b.Token, nil),
		http.StatusUnprocessableEntity); p.Type != problem.TypeInvalidRequest {
		t.Fatalf("不带钥匙应 422，实得 %+v", p)
	}

	// 审核重放：不会入账两次。
	akey := "au-" + uniqueKey()
	path := "/api/v1/admin/refunds/" + first.RefundNo + "/audit"
	wantStatus(t, postWithKey(t, cs.Host, path, `{"action":"approve"}`, cs.Token, akey), http.StatusOK, "审核")
	m1 := orderMoneyOf(t, o.OrderNo)
	w = postWithKey(t, cs.Host, path, `{"action":"approve"}`, cs.Token, akey)
	wantStatus(t, w, http.StatusOK, "审核重放")
	if w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("审核重放没有带 Idempotency-Replayed")
	}
	if m2 := orderMoneyOf(t, o.OrderNo); m2.Refunded != m1.Refunded {
		t.Fatalf("审核重放又入账了一次：%d → %d", m1.Refunded, m2.Refunded)
	}
}

// 几个并发的申请抢同一行的最后一件：订单行锁让它们串行，恰好一个成功 ——
// 在途超退是 chk_item_refund 拦不住的那一种（§11），只能靠这把锁加在途复算。
//
// 用两行的订单、只退衬衫那一行：这是**部分**退款，订单不会进 50。要是用单行订单，
// 第一张申请就把订单推进 50，后来者全被「50 不能再申请」挡掉 —— 那样这条测试
// 在在途复算整个被删掉时照样绿（实测过），证明不了它要证明的东西。
func TestConcurrentRefundsCannotOverRefund(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "rrace")
	o := cs.twoLineOrder(t, b, nil)
	cs.pay(t, o.OrderNo, o.PayableCents)
	_, lines := cs.lines(t, b, o.OrderNo)
	body := refundBody(1, [2]int64{lines[cs.ShirtSKU].Id, 1})

	const n = 6
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = applyRefund(t, cs.Host, o.OrderNo, b.Token, body, fmt.Sprintf("race-%d-%s", i, uniqueKey())).Code
		}(i)
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Fatalf("并发申请出现了意外的状态码 %d（全部：%v）", c, codes)
		}
	}
	if created != 1 {
		t.Fatalf("%d 个并发申请里成功了 %d 个（%v），期望恰好 1 个", n, created, codes)
	}
	if st := orderStatusOf(t, o.OrderNo); st != 20 {
		t.Fatalf("部分退款不该改订单状态，实得 %d —— 这条测试的前提（不走 50）不成立", st)
	}
}

// ---------------------------------------------------------------------------
// 退款状态机的数据库兜底
// ---------------------------------------------------------------------------

func TestRefundStatusMachineIsEnforcedByTheDatabase(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "rguard")
	o := cs.placePaid(t, b, cs.NorthStore, cs.ShirtSKU, 1, nil)
	wantStatus(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusCreated, "发货")
	_, lines := cs.lines(t, b, o.OrderNo)
	r := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{lines[cs.ShirtSKU].Id, 1}))

	for _, c := range []struct{ name, sql, want string }{
		{"10 → 40（跳过审核与渠道）",
			`UPDATE refunds SET status = 40, channel_refund_id = 'x', refunded_at = now(), audited_at = now() WHERE refund_no = $1 RETURNING id`,
			"refund_status_transition"},
		{"10 → 30 但没写审核时间",
			`UPDATE refunds SET status = 30 WHERE refund_no = $1 RETURNING id`, "chk_refund_state"},
		{"10 → 50 但没写驳回理由",
			`UPDATE refunds SET status = 50, audited_at = now() WHERE refund_no = $1 RETURNING id`, "chk_refund_state"},
	} {
		var id int64
		err := withTenantConn(t, cs.MerchantID, c.sql, r.RefundNo).Scan(&id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != c.want {
			t.Errorf("%s：期望 23514 %s，实得 %v", c.name, c.want, err)
		}
	}
	if st := refundStatusOf(t, r.RefundNo); st != 10 {
		t.Fatalf("非法跳转之后退款单是 %d，期望仍是 10", st)
	}
}

// ---------------------------------------------------------------------------
// 退货寄回物流（00037）：只有退货退款停在 20 时能填，填完状态不变，20 期间能改，
// 后台详情看得见，离开 20 之后不能再改。
// ---------------------------------------------------------------------------

func returnShipment(t *testing.T, cs couponShop, refundNo, token, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	return postWithKey(t, cs.Host, "/api/v1/refunds/"+refundNo+"/return-shipment", body, token, key)
}

func TestBuyerFillsReturnShipmentWhileAwaitingReturn(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "rship")
	other := cs.newBuyer(t, "rship-other")
	o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 2, nil)
	wantStatus(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusCreated, "发货")
	_, lines := cs.lines(t, b, o.OrderNo)
	r := cs.mustApply(t, b, o.OrderNo, refundBody(2, [2]int64{lines[cs.DressSKU].Id, 1}))
	body := `{"carrier_code":"yto","tracking_no":"YT0001"}`

	// 还在 10 待审核：商家还没同意退货，没有地方寄。
	if p := problemOf(t, returnShipment(t, cs, r.RefundNo, b.Token, body, "rs-"+uniqueKey()),
		http.StatusConflict); p.Type != problem.TypeRefundStatusNotReturnable {
		t.Fatalf("待审核的单填寄回物流应 409 refund-status-not-returnable，实得 %+v", p)
	}
	if approved := cs.mustApprove(t, r.RefundNo); approved.Status != 20 {
		t.Fatalf("退货退款审核通过应到 20，实得 %d", approved.Status)
	}

	// 别人的单：404，与「不存在」同一个响应。
	if p := problemOf(t, returnShipment(t, cs, r.RefundNo, other.Token, body, "rs-"+uniqueKey()),
		http.StatusNotFound); p.Type != problem.TypeNotFound {
		t.Fatalf("给别人的退款单填物流应 404，实得 %+v", p)
	}
	// 空单号：422。
	if p := problemOf(t, returnShipment(t, cs, r.RefundNo, b.Token, `{"carrier_code":"yto","tracking_no":"  "}`,
		"rs-"+uniqueKey()), http.StatusUnprocessableEntity); p.Type != problem.TypeInvalidRequest {
		t.Fatalf("空运单号应 422 invalid-request，实得 %+v", p)
	}

	key := "rs-" + uniqueKey()
	var got api.Refund
	decodeInto(t, returnShipment(t, cs, r.RefundNo, b.Token, body, key), http.StatusOK, "填寄回物流", &got)
	if got.Status != 20 {
		t.Fatalf("填完寄回物流状态变成了 %d —— 应仍是 20，等商家收货", got.Status)
	}
	if got.ReturnShipment == nil || got.ReturnShipment.CarrierCode != "yto" ||
		got.ReturnShipment.TrackingNo != "YT0001" || got.ReturnShipment.SubmittedAt.IsZero() {
		t.Fatalf("响应里的 return_shipment 是 %+v，期望 yto / YT0001 且有 submitted_at", got.ReturnShipment)
	}
	if st := refundStatusOf(t, r.RefundNo); st != 20 {
		t.Fatalf("库里的退款单状态是 %d，期望仍是 20", st)
	}
	w := returnShipment(t, cs, r.RefundNo, b.Token, body, key)
	wantStatus(t, w, http.StatusOK, "填寄回物流重放")
	if w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("同一把钥匙第二次填寄回物流没有带 Idempotency-Replayed")
	}

	// 20 期间可以改（填错单号是常事）。
	decodeInto(t, returnShipment(t, cs, r.RefundNo, b.Token, `{"carrier_code":"sf","tracking_no":"SF0002"}`,
		"rs-"+uniqueKey()), http.StatusOK, "改寄回物流", &got)
	if got.ReturnShipment == nil || got.ReturnShipment.CarrierCode != "sf" || got.ReturnShipment.TrackingNo != "SF0002" {
		t.Fatalf("改过之后的 return_shipment 是 %+v，期望 sf / SF0002", got.ReturnShipment)
	}

	// 后台详情看得见寄回物流（商家据此查件）。
	var detail api.AdminRefundDetail
	decodeInto(t, getAs(t, cs.Host, "/api/v1/admin/refunds/"+r.RefundNo, cs.Token), http.StatusOK, "后台退款详情", &detail)
	if detail.ReturnShipment == nil || detail.ReturnShipment.TrackingNo != "SF0002" {
		t.Fatalf("后台退款详情里的 return_shipment 是 %+v，期望 SF0002", detail.ReturnShipment)
	}

	// 商家确认收货之后（沙箱入账到 40）不能再改，已填的那份留着。
	wantStatus(t, postIdem(t, cs.Host, "/api/v1/admin/refunds/"+r.RefundNo+"/receipt", "", cs.Token),
		http.StatusOK, "确认收到退货")
	if p := problemOf(t, returnShipment(t, cs, r.RefundNo, b.Token, body, "rs-"+uniqueKey()),
		http.StatusConflict); p.Type != problem.TypeRefundStatusNotReturnable {
		t.Fatalf("已收货的单再填寄回物流应 409，实得 %+v", p)
	}
	decodeInto(t, getAs(t, cs.Host, "/api/v1/refunds/"+r.RefundNo, b.Token), http.StatusOK, "退款详情", &got)
	if got.ReturnShipment == nil || got.ReturnShipment.TrackingNo != "SF0002" {
		t.Fatalf("退款完成之后寄回物流不见了：%+v", got.ReturnShipment)
	}
}

// 仅退款的单挂不上寄回物流：服务层 409，库里 chk_refund_return_shipment 兜底。
func TestMoneyOnlyRefundHasNoReturnShipment(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "rship-money")
	o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	_, lines := cs.lines(t, b, o.OrderNo)
	r := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{lines[cs.DressSKU].Id, 1}))
	if r.ReturnShipment != nil {
		t.Fatalf("仅退款的单带着 return_shipment：%+v", r.ReturnShipment)
	}
	if p := problemOf(t, returnShipment(t, cs, r.RefundNo, b.Token, `{"carrier_code":"sf","tracking_no":"X1"}`,
		"rs-"+uniqueKey()), http.StatusConflict); p.Type != problem.TypeRefundStatusNotReturnable {
		t.Fatalf("仅退款的单填寄回物流应 409，实得 %+v", p)
	}
	_, err := admin(t).Exec(context.Background(), `
		UPDATE refunds SET return_carrier_code = 'sf', return_tracking_no = 'X1', return_submitted_at = now()
		 WHERE refund_no = $1`, r.RefundNo)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "chk_refund_return_shipment" {
		t.Fatalf("给仅退款的单直接写寄回物流应撞 chk_refund_return_shipment，实得 %v", err)
	}
}
