package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// 订单后半程的履约维度（00033）：买家取消、后台发货、买家确认收货，以及订单状态机
// 的数据库兜底。全部在一家新开的店里跑（couponShop 的夹具），买家与后台都走真接口。

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// placeOrder 下一单并断言 201。
func (cs couponShop) placeOrder(t *testing.T, b couponBuyer, storeID, skuID int64, qty int, couponID *int64) api.Order {
	t.Helper()
	var o api.Order
	decodeInto(t, createOrder(t, cs.Host, cs.orderJSON(b, storeID, skuID, qty, couponID), b.Token, "ful-"+uniqueKey()),
		http.StatusCreated, "下单", &o)
	return o
}

// placePaid 下一单并经支付回调付掉它。
func (cs couponShop) placePaid(t *testing.T, b couponBuyer, storeID, skuID int64, qty int, couponID *int64) api.Order {
	t.Helper()
	o := cs.placeOrder(t, b, storeID, skuID, qty, couponID)
	cs.pay(t, o.OrderNo, o.PayableCents)
	return o
}

// orderAction 调 POST /orders/{no}/{action}（cancel / confirm），带指定的幂等键。
func orderAction(t *testing.T, host, orderNo, action, token, key string) *httptest.ResponseRecorder {
	t.Helper()
	return postWithKey(t, host, "/api/v1/orders/"+orderNo+"/"+action, "", token, key)
}

// ship 用后台管理员发货。
func (cs couponShop) ship(t *testing.T, orderNo, carrier, tracking string) *httptest.ResponseRecorder {
	t.Helper()
	return postIdem(t, cs.Host, "/api/v1/admin/orders/"+orderNo+"/shipments",
		fmt.Sprintf(`{"carrier_code":%q,"tracking_no":%q}`, carrier, tracking), cs.Token)
}

// wholeStoreCoupon 建一张全场可用、可领的满 100 减 20，并让 b 领一张。
func (cs couponShop) wholeStoreCoupon(t *testing.T, b couponBuyer) api.UserCoupon {
	t.Helper()
	tpl := cs.createTemplate(t, full100minus20(0, 5))
	wantStatus(t, cs.setScopes(t, tpl.Id, `[{"scope_type":1,"include":true}]`), http.StatusOK, "全场范围")
	wantStatus(t, cs.patchTemplate(t, tpl.Id, `{"claimable":true}`), http.StatusOK, "设为可领")
	return cs.mustClaim(t, b, tpl.Id)
}

type orderTimes struct {
	Status                        int16
	PaidAt, ShippedAt, FinishedAt bool
}

func orderTimesOf(t *testing.T, orderNo string) orderTimes {
	t.Helper()
	var o orderTimes
	if err := admin(t).QueryRow(context.Background(), `
		SELECT status, paid_at IS NOT NULL, shipped_at IS NOT NULL, finished_at IS NOT NULL
		  FROM orders WHERE order_no = $1`, orderNo).Scan(&o.Status, &o.PaidAt, &o.ShippedAt, &o.FinishedAt); err != nil {
		t.Fatalf("读订单 %s 失败: %v", orderNo, err)
	}
	return o
}

// ---------------------------------------------------------------------------
// 取消
// ---------------------------------------------------------------------------

// 买家取消一笔带券的待支付订单：关到 90、库存按门店放回、券回到未使用，
// 流水记成「买家取消释放」（6），与超时关单（3）分得开。
func TestBuyerCancelReleasesStockAndCoupon(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "cancel")
	coupon := cs.wholeStoreCoupon(t, b)

	before := availableAt(t, cs.NorthStore, cs.DressSKU)
	o := cs.placeOrder(t, b, cs.NorthStore, cs.DressSKU, 2, &coupon.Id)
	if got := availableAt(t, cs.NorthStore, cs.DressSKU); got != before-2 {
		t.Fatalf("下单之后水位 %d，期望 %d —— 前提不成立", got, before-2)
	}
	if st, _ := couponState(t, coupon.Id); st != 2 {
		t.Fatalf("下单之后券状态 %d，期望 2 锁定 —— 前提不成立", st)
	}

	key := "cancel-" + uniqueKey()
	var got api.Order
	decodeInto(t, orderAction(t, cs.Host, o.OrderNo, "cancel", b.Token, key), http.StatusOK, "取消", &got)
	if got.Status != 90 || got.OrderNo != o.OrderNo {
		t.Fatalf("取消之后响应里的订单是 %s / %d，期望 %s / 90", got.OrderNo, got.Status, o.OrderNo)
	}
	if st := orderStatusOf(t, o.OrderNo); st != 90 {
		t.Fatalf("取消之后库里状态 %d，期望 90", st)
	}
	if got := availableAt(t, cs.NorthStore, cs.DressSKU); got != before {
		t.Fatalf("取消之后水位 %d，期望回到 %d —— 库存没放回去", got, before)
	}
	if st, no := couponState(t, coupon.Id); st != 1 || no != nil {
		t.Fatalf("取消之后券状态 %d 订单 %v，期望 1 未使用、不挂订单", st, no)
	}
	logs := inventoryLogsOf(t, o.OrderNo)
	if len(logs) != 2 || logs[1].BizType != 6 || logs[1].ChangeQty != 2 {
		t.Fatalf("库存流水应是「下单扣减 -2」+「买家取消释放 +2（biz_type 6）」，实得 %+v", logs)
	}

	// 同一把钥匙重放：回首次的结果，带 Idempotency-Replayed，库存不再动。
	w := orderAction(t, cs.Host, o.OrderNo, "cancel", b.Token, key)
	wantStatus(t, w, http.StatusOK, "取消重放")
	if w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("同一把钥匙第二次取消没有带 Idempotency-Replayed: true")
	}
	if got := availableAt(t, cs.NorthStore, cs.DressSKU); got != before {
		t.Fatalf("重放之后水位 %d，期望仍是 %d —— 重放又回补了一次", got, before)
	}

	// 换一把钥匙再取消：409 order-status-not-cancelable，库存一件没多。
	p := problemOf(t, orderAction(t, cs.Host, o.OrderNo, "cancel", b.Token, "cancel-"+uniqueKey()), http.StatusConflict)
	if p.Type != problem.TypeOrderStatusNotCancelable {
		t.Fatalf("取消一笔已关闭的订单应 409 order-status-not-cancelable，实得 %+v", p)
	}
	if got := availableAt(t, cs.NorthStore, cs.DressSKU); got != before {
		t.Fatalf("第二次取消之后水位 %d，期望仍是 %d", got, before)
	}
}

// 已支付的订单不能取消；别人的订单 404；不带钥匙 422；钥匙配了别的订单 422。
func TestCancelIsRefusedWhereTheContractSaysSo(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "cancel-no")
	other := cs.newBuyer(t, "cancel-other")

	paid := cs.placePaid(t, b, cs.NorthStore, cs.ShirtSKU, 1, nil)
	p := problemOf(t, orderAction(t, cs.Host, paid.OrderNo, "cancel", b.Token, "c-"+uniqueKey()), http.StatusConflict)
	if p.Type != problem.TypeOrderStatusNotCancelable {
		t.Fatalf("取消已支付订单应 409 order-status-not-cancelable，实得 %+v", p)
	}
	if st := orderStatusOf(t, paid.OrderNo); st != 20 {
		t.Fatalf("被拒的取消把订单改成了 %d", st)
	}

	pending := cs.placeOrder(t, b, cs.NorthStore, cs.ShirtSKU, 1, nil)
	if p := problemOf(t, orderAction(t, cs.Host, pending.OrderNo, "cancel", other.Token, "c-"+uniqueKey()),
		http.StatusNotFound); p.Type != problem.TypeNotFound {
		t.Fatalf("取消别人的订单应 404，实得 %+v", p)
	}
	if st := orderStatusOf(t, pending.OrderNo); st != 10 {
		t.Fatalf("别人的取消把订单改成了 %d", st)
	}

	w := postJSON(t, cs.Host, "/api/v1/orders/"+pending.OrderNo+"/cancel", "", b.Token, nil)
	if p := problemOf(t, w, http.StatusUnprocessableEntity); p.Type != problem.TypeInvalidRequest {
		t.Fatalf("不带 Idempotency-Key 应 422 invalid-request，实得 %+v", p)
	}

	// 同一把钥匙先取消 A 成功，再拿去取消 B：422 idempotency-key-reused，B 不动。
	second := cs.placeOrder(t, b, cs.NorthStore, cs.ShirtSKU, 1, nil)
	key := "c-" + uniqueKey()
	wantStatus(t, orderAction(t, cs.Host, pending.OrderNo, "cancel", b.Token, key), http.StatusOK, "取消 A")
	if p := problemOf(t, orderAction(t, cs.Host, second.OrderNo, "cancel", b.Token, key),
		http.StatusUnprocessableEntity); p.Type != problem.TypeIdempotencyKeyReused {
		t.Fatalf("同一把钥匙拿去取消另一笔订单应 422 idempotency-key-reused，实得 %+v", p)
	}
	if st := orderStatusOf(t, second.OrderNo); st != 10 {
		t.Fatalf("复用钥匙的取消把第二笔订单改成了 %d", st)
	}
}

// ---------------------------------------------------------------------------
// 发货与确认收货
// ---------------------------------------------------------------------------

func TestShipThenConfirm(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "ship")
	o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	stock := availableAt(t, cs.NorthStore, cs.DressSKU)

	// 已支付之前不能确认收货。
	if p := problemOf(t, orderAction(t, cs.Host, o.OrderNo, "confirm", b.Token, "cf-"+uniqueKey()),
		http.StatusConflict); p.Type != problem.TypeOrderStatusNotConfirmable {
		t.Fatalf("已支付未发货时确认收货应 409 order-status-not-confirmable，实得 %+v", p)
	}

	tracking := "SF" + uniqueKey()
	var sh api.Shipment
	decodeInto(t, cs.ship(t, o.OrderNo, "sf", tracking), http.StatusCreated, "发货", &sh)
	if sh.CarrierCode != "sf" || sh.TrackingNo != tracking || sh.Status != 1 || sh.ShippedAt.IsZero() {
		t.Fatalf("发货响应不对：%+v", sh)
	}
	if tm := orderTimesOf(t, o.OrderNo); tm.Status != 30 || !tm.ShippedAt || tm.FinishedAt {
		t.Fatalf("发货之后订单是 %+v，期望 30、有 shipped_at、没有 finished_at", tm)
	}
	// 规则一：发货不动库存。
	if got := availableAt(t, cs.NorthStore, cs.DressSKU); got != stock {
		t.Fatalf("发货之后水位 %d，期望仍是 %d —— 发货重复扣了库存", got, stock)
	}

	// 一期整单发货：第二次发货 409 not-shippable。
	if p := problemOf(t, cs.ship(t, o.OrderNo, "sf", "SF2"+uniqueKey()), http.StatusConflict); p.Type != problem.TypeOrderStatusNotShippable {
		t.Fatalf("第二次发货应 409 order-status-not-shippable，实得 %+v", p)
	}
	// 运单号录重：另一笔已支付的订单拿同一个单号，409 tracking-no-duplicated，订单不动。
	o2 := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	if p := problemOf(t, cs.ship(t, o2.OrderNo, "sf", tracking), http.StatusConflict); p.Type != problem.TypeTrackingNoDuplicated {
		t.Fatalf("重复的运单号应 409 tracking-no-duplicated，实得 %+v", p)
	}
	if st := orderStatusOf(t, o2.OrderNo); st != 20 {
		t.Fatalf("运单号撞车的发货把订单改成了 %d —— 状态推进没有随包裹一起回滚", st)
	}
	// 同一承运商之外的同号不是重复（唯一键是 承运商 + 单号）。
	wantStatus(t, cs.ship(t, o2.OrderNo, "jd", tracking), http.StatusCreated, "换承运商发货")

	// 待支付的订单不能发货。
	pending := cs.placeOrder(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	if p := problemOf(t, cs.ship(t, pending.OrderNo, "sf", "SF3"+uniqueKey()), http.StatusConflict); p.Type != problem.TypeOrderStatusNotShippable {
		t.Fatalf("待支付订单发货应 409 order-status-not-shippable，实得 %+v", p)
	}
	// 不存在的单号 404；空运单号 422。
	if p := problemOf(t, cs.ship(t, "NOPE"+uniqueKey(), "sf", "x"), http.StatusNotFound); p.Type != problem.TypeNotFound {
		t.Fatalf("发货一个不存在的订单应 404，实得 %+v", p)
	}
	if p := problemOf(t, cs.ship(t, pending.OrderNo, "sf", "  "), http.StatusUnprocessableEntity); p.Type != problem.TypeInvalidRequest {
		t.Fatalf("空运单号应 422 invalid-request，实得 %+v", p)
	}

	// 别人确认不了：404，订单停在 30。
	other := cs.newBuyer(t, "ship-other")
	if p := problemOf(t, orderAction(t, cs.Host, o.OrderNo, "confirm", other.Token, "cf-"+uniqueKey()),
		http.StatusNotFound); p.Type != problem.TypeNotFound {
		t.Fatalf("确认别人的订单应 404，实得 %+v", p)
	}
	if st := orderStatusOf(t, o.OrderNo); st != 30 {
		t.Fatalf("别人的确认收货把订单改成了 %d", st)
	}

	// 确认收货：30 → 40，记下完成时间；重放带头；再确认一次 409。
	key := "cf-" + uniqueKey()
	var done api.Order
	decodeInto(t, orderAction(t, cs.Host, o.OrderNo, "confirm", b.Token, key), http.StatusOK, "确认收货", &done)
	if done.Status != 40 || done.FinishedAt == nil || done.ShippedAt == nil {
		t.Fatalf("确认收货之后响应是 %d finished_at=%v shipped_at=%v", done.Status, done.FinishedAt, done.ShippedAt)
	}
	if tm := orderTimesOf(t, o.OrderNo); tm.Status != 40 || !tm.FinishedAt {
		t.Fatalf("确认收货之后库里是 %+v", tm)
	}
	w := orderAction(t, cs.Host, o.OrderNo, "confirm", b.Token, key)
	wantStatus(t, w, http.StatusOK, "确认收货重放")
	if w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("确认收货重放没有带 Idempotency-Replayed")
	}
	if p := problemOf(t, orderAction(t, cs.Host, o.OrderNo, "confirm", b.Token, "cf-"+uniqueKey()),
		http.StatusConflict); p.Type != problem.TypeOrderStatusNotConfirmable {
		t.Fatalf("已完成的订单再确认应 409，实得 %+v", p)
	}
}

// 发货规则二：订单停在 50 退款中（有一张未完结的整单退款申请）时拒绝发货，
// 用的是专门的 type，不是笼统的 not-shippable。
func TestShipIsRefusedWhileAFullRefundIsPending(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "ship-refund")
	o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	// 20 → 50 是状态机里的合法边（整单退款申请），这里直接推，
	// 退款申请那条接口自己的测试在 refund_test.go。
	adminExec(t, `UPDATE orders SET status = 50 WHERE order_no = $1`, o.OrderNo)

	if p := problemOf(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusConflict); p.Type != problem.TypeOrderHasPendingFullRefund {
		t.Fatalf("整单退款中发货应 409 order-has-pending-full-refund，实得 %+v", p)
	}
}

// ---------------------------------------------------------------------------
// 状态机的数据库兜底（00033）
// ---------------------------------------------------------------------------

// order_status_transitions 从 00033 起真的被执行：一条不带起点状态的 UPDATE
// 想把待支付订单直接推到已完成，数据库以 23514 order_status_transition 拒绝。
// 走 keel_app 连接（与应用同一个身份），不经过任何一层应用代码。
func TestOrderStatusMachineIsEnforcedByTheDatabase(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "guard")
	o := cs.placeOrder(t, b, cs.NorthStore, cs.ShirtSKU, 1, nil)

	illegal := []struct {
		name, sql string
		want      string
	}{
		{"10 → 40（跳过支付与发货）", `UPDATE orders SET status = 40, finished_at = now() WHERE order_no = $1 RETURNING id`, "order_status_transition"},
		{"10 → 30（没付钱就发货）", `UPDATE orders SET status = 30, shipped_at = now() WHERE order_no = $1 RETURNING id`, "order_status_transition"},
		{"10 → 60", `UPDATE orders SET status = 60 WHERE order_no = $1 RETURNING id`, "order_status_transition"},
	}
	for _, c := range illegal {
		var id int64
		err := withTenantConn(t, cs.MerchantID, c.sql, o.OrderNo).Scan(&id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != c.want {
			t.Errorf("%s：期望 23514 %s，实得 %v", c.name, c.want, err)
		}
	}
	if st := orderStatusOf(t, o.OrderNo); st != 10 {
		t.Fatalf("非法跳转之后订单是 %d，期望仍是 10", st)
	}

	// 合法边照常通过（阳性对照：触发器不是一律拒绝）。
	cs.pay(t, o.OrderNo, o.PayableCents)
	if st := orderStatusOf(t, o.OrderNo); st != 20 {
		t.Fatalf("支付之后是 %d，期望 20 —— 触发器挡住了合法边", st)
	}

	// 状态与时间戳钉在一起：20 → 30 是合法边，但不写 shipped_at 过不去。
	var id int64
	err := withTenantConn(t, cs.MerchantID,
		`UPDATE orders SET status = 30 WHERE order_no = $1 RETURNING id`, o.OrderNo).Scan(&id)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "chk_fulfillment_timestamps" {
		t.Fatalf("发货不写 shipped_at 应被 chk_fulfillment_timestamps 拒绝，实得 %v", err)
	}
}

// 订单详情里的状态随履约推进（买家看得到发货时间与完成时间）。
func TestOrderDetailFollowsFulfillment(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "detail")
	o := cs.placePaid(t, b, cs.NorthStore, cs.ShirtSKU, 1, nil)
	wantStatus(t, cs.ship(t, o.OrderNo, "yto", "YT"+uniqueKey()), http.StatusCreated, "发货")

	var d api.OrderDetail
	decodeInto(t, getAs(t, cs.Host, "/api/v1/orders/"+o.OrderNo, b.Token), http.StatusOK, "详情", &d)
	if d.Status != 30 || d.ShippedAt == nil {
		t.Fatalf("发货之后详情是 %d shipped_at=%v", d.Status, d.ShippedAt)
	}
	raw, _ := json.Marshal(d)
	if strings.Contains(string(raw), `"finished_at"`) {
		t.Fatalf("还没确认收货，详情里不该有 finished_at：%s", raw)
	}
}
