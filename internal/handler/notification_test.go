package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 消息通知（数据模型 §16，00053）的端到端测试。
//
// 状态变化一律走真接口（下单、支付回调、发货、售后）或真任务（超时补偿、自动确认），
// 通知一律从对外接口读回来（/me/notifications、/admin/notifications）—— 测的是
// 「这次状态变化之后，那个人打开消息中心看到了什么」，而不是某张表里有没有一行。
// 只有核对「没有」与外发投递记录时才走管理员连接直读。

type notifList struct {
	api.PageMeta
	Items       []api.Notification `json:"items"`
	UnreadCount int                `json:"unread_count"`
}

func myNotifications(t *testing.T, host, token, query string) notifList {
	t.Helper()
	var out notifList
	decodeInto(t, getAuth(t, host, "/api/v1/me/notifications?page_size=100"+query, token),
		http.StatusOK, "我的消息", &out)
	return out
}

func staffNotifications(t *testing.T, host, token, query string) notifList {
	t.Helper()
	var out notifList
	decodeInto(t, getAuth(t, host, "/api/v1/admin/notifications?page_size=100"+query, token),
		http.StatusOK, "后台提醒", &out)
	return out
}

// findNotif 找种类为 kind、定位到 no（订单号或退款单号）的那一条；找不到返回 nil。
func findNotif(items []api.Notification, kind, no string) *api.Notification {
	for i, n := range items {
		if string(n.Kind) != kind {
			continue
		}
		if (n.Target.OrderNo != nil && *n.Target.OrderNo == no) ||
			(n.Target.RefundNo != nil && *n.Target.RefundNo == no) || no == "" {
			return &items[i]
		}
	}
	return nil
}

func countNotif(items []api.Notification, kind, no string) int {
	c := 0
	for _, n := range items {
		if string(n.Kind) == kind && ((n.Target.OrderNo != nil && *n.Target.OrderNo == no) ||
			(n.Target.RefundNo != nil && *n.Target.RefundNo == no)) {
			c++
		}
	}
	return c
}

func mustNotif(t *testing.T, items []api.Notification, kind, no string) api.Notification {
	t.Helper()
	n := findNotif(items, kind, no)
	if n == nil {
		var got []string
		for _, it := range items {
			got = append(got, string(it.Kind))
		}
		t.Fatalf("没有 %s（%s）这条通知；实际有：%v", kind, no, got)
	}
	return *n
}

// notificationsFor 走管理员连接数一张单（订单号或退款单号）身上的通知，不分收件人。
func notificationsFor(t *testing.T, no string) int64 {
	t.Helper()
	return adminQueryInt64(t, `SELECT count(*) FROM notifications WHERE order_no = $1 OR refund_no = $1`, no)
}

func unreadCount(t *testing.T, w interface {
	Result() *http.Response
}) int {
	t.Helper()
	var out api.NotificationUnreadCount
	if err := json.NewDecoder(w.Result().Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.UnreadCount
}

// ---------------------------------------------------------------------------
// 订单的一生
// ---------------------------------------------------------------------------

// 支付成功 → 买家「支付成功」+ 门店「新订单待发货」；发货 → 买家「已发货」（带物流）。
// 只看得到自己的；标已读、全部已读、未读数前后一致。
func TestNotificationsFollowTheOrderLifecycle(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "notify")
	other := cs.newBuyer(t, "notify-other")

	o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	mine := myNotifications(t, cs.Host, b.Token, "")
	paid := mustNotif(t, mine.Items, service.KindOrderPaid, o.OrderNo)
	if paid.Title != "支付成功" || !strings.Contains(paid.Body, o.OrderNo) || !strings.Contains(paid.Body, "¥60.00") {
		t.Errorf("支付成功那条渲染成了 %q / %q", paid.Title, paid.Body)
	}
	if paid.Target.Type != api.NotificationTargetTypeOrder || paid.Target.RefundNo != nil || paid.ReadAt != nil {
		t.Errorf("支付成功那条的跳转目标 / 已读状态不对：%+v read_at=%v", paid.Target, paid.ReadAt)
	}
	staff := staffNotifications(t, cs.Host, cs.Token, "")
	np := mustNotif(t, staff.Items, service.KindMerchantOrderPaid, o.OrderNo)
	if np.Target.StoreId == nil || *np.Target.StoreId != cs.NorthStore || np.Title != "新订单待发货" {
		t.Errorf("门店那条是 %q，store_id=%v，期望挂在华北门店 %d 上", np.Title, np.Target.StoreId, cs.NorthStore)
	}
	if findNotif(mine.Items, service.KindMerchantOrderPaid, o.OrderNo) != nil {
		t.Error("商家那条出现在了买家的消息中心里")
	}
	if findNotif(staff.Items, service.KindOrderPaid, o.OrderNo) != nil {
		t.Error("买家那条出现在了后台铃铛里")
	}

	// 别的买家一条都看不到，也标不了。
	if got := myNotifications(t, cs.Host, other.Token, ""); len(got.Items) != 0 || got.UnreadCount != 0 {
		t.Fatalf("另一个买家看到了 %d 条（未读 %d）—— 越权", len(got.Items), got.UnreadCount)
	}
	wantStatus(t, post(t, cs.Host, fmt.Sprintf("/api/v1/me/notifications/%d/read", paid.Id), "", other.Token),
		http.StatusNotFound, "标别人的通知已读")
	wantStatus(t, post(t, cs.Host, fmt.Sprintf("/api/v1/me/notifications/%d/read", np.Id), "", b.Token),
		http.StatusNotFound, "买家标一条商家通知已读")

	// 发货：正文带承运商与运单号。
	tracking := "SF" + uniqueKey()
	wantStatus(t, cs.ship(t, o.OrderNo, "sf", tracking), http.StatusCreated, "发货")
	mine = myNotifications(t, cs.Host, b.Token, "")
	shipped := mustNotif(t, mine.Items, service.KindOrderShipped, o.OrderNo)
	if !strings.Contains(shipped.Body, "顺丰") || !strings.Contains(shipped.Body, tracking) {
		t.Errorf("已发货那条没带物流：%q", shipped.Body)
	}
	if mine.UnreadCount != 2 || mine.Items[0].Id != shipped.Id {
		t.Errorf("未读 %d 条、第一条是 %s，期望 2 条未读、按时间倒序最新的是已发货", mine.UnreadCount, mine.Items[0].Kind)
	}

	// 标一条已读：未读数减一；再标一次照样 200、数不变（幂等）。
	w := post(t, cs.Host, fmt.Sprintf("/api/v1/me/notifications/%d/read", paid.Id), "", b.Token)
	wantStatus(t, w, http.StatusOK, "标已读")
	if n := unreadCount(t, w); n != 1 {
		t.Errorf("标一条已读之后未读是 %d，期望 1", n)
	}
	w = post(t, cs.Host, fmt.Sprintf("/api/v1/me/notifications/%d/read", paid.Id), "", b.Token)
	if w.Code != http.StatusOK || unreadCount(t, w) != 1 {
		t.Errorf("重复标已读：%d，期望 200 且未读仍是 1", w.Code)
	}
	if got := myNotifications(t, cs.Host, b.Token, "&unread_only=true"); len(got.Items) != 1 || got.Items[0].Id != shipped.Id || got.Total != 1 {
		t.Errorf("unread_only=true 拿到 %d 条（total %d），期望只剩已发货那一条", len(got.Items), got.Total)
	}
	w = post(t, cs.Host, "/api/v1/me/notifications/read-all", "", b.Token)
	if w.Code != http.StatusOK || unreadCount(t, w) != 0 {
		t.Errorf("全部已读：%d，期望 200 且未读 0", w.Code)
	}
	w = getAuth(t, cs.Host, "/api/v1/me/notifications/unread-count", b.Token)
	if w.Code != http.StatusOK || unreadCount(t, w) != 0 {
		t.Errorf("未读数接口：%d，期望 200 且 0", w.Code)
	}
	if got := myNotifications(t, cs.Host, b.Token, ""); got.Items[1].ReadAt == nil {
		t.Error("全部已读之后通知上还是没有 read_at")
	}

	// 没带令牌：401。
	wantStatus(t, getAuth(t, cs.Host, "/api/v1/me/notifications", ""), http.StatusUnauthorized, "不带令牌读消息")
}

// 刻意不发的几条：买家自己取消、买家自己确认收货。
func TestBuyerOwnActionsDoNotNotifyThemselves(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "notify-self")

	o := cs.placeOrder(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	wantStatus(t, orderAction(t, cs.Host, o.OrderNo, "cancel", b.Token, "c-"+uniqueKey()), http.StatusOK, "取消")
	if n := notificationsFor(t, o.OrderNo); n != 0 {
		t.Errorf("买家自己取消的订单身上有 %d 条通知，期望 0", n)
	}

	o2 := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	wantStatus(t, cs.ship(t, o2.OrderNo, "sf", "SF"+uniqueKey()), http.StatusCreated, "发货")
	wantStatus(t, orderAction(t, cs.Host, o2.OrderNo, "confirm", b.Token, "cf-"+uniqueKey()), http.StatusOK, "确认收货")
	if findNotif(myNotifications(t, cs.Host, b.Token, "").Items, service.KindOrderFinished, o2.OrderNo) != nil {
		t.Error("买家自己确认收货之后又收到了一条「订单已完成」")
	}
}

// ---------------------------------------------------------------------------
// 售后
// ---------------------------------------------------------------------------

// 退货退款：申请 → 门店待审核；通过 → 买家（提示填物流）；填 / 改寄回物流 → 门店；
// 确认收到退货 → （沙箱入账）买家退款到账。仅退款被驳回 → 买家收到带理由的那条。
func TestNotificationsFollowTheRefundLifecycle(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "notify-refund")

	o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	wantStatus(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusCreated, "发货")
	_, lines := cs.lines(t, b, o.OrderNo)
	r := cs.mustApply(t, b, o.OrderNo, refundBody(2, [2]int64{lines[cs.DressSKU].Id, 1}))

	req := mustNotif(t, staffNotifications(t, cs.Host, cs.Token, "").Items, service.KindMerchantRefundRequest, r.RefundNo)
	if req.Target.Type != api.NotificationTargetTypeRefund || req.Target.OrderNo == nil || *req.Target.OrderNo != o.OrderNo ||
		!strings.Contains(req.Body, "退货退款") {
		t.Errorf("待审核那条是 %+v / %q", req.Target, req.Body)
	}

	cs.mustApprove(t, r.RefundNo)
	ap := mustNotif(t, myNotifications(t, cs.Host, b.Token, "").Items, service.KindRefundApproved, r.RefundNo)
	if !strings.Contains(ap.Body, "寄回") {
		t.Errorf("退货退款审核通过那条没提示去填寄回物流：%q", ap.Body)
	}

	body := `{"carrier_code":"yto","tracking_no":"YT100"}`
	wantStatus(t, returnShipment(t, cs, r.RefundNo, b.Token, body, "rs-"+uniqueKey()), http.StatusOK, "填寄回物流")
	wantStatus(t, returnShipment(t, cs, r.RefundNo, b.Token, body, "rs-"+uniqueKey()), http.StatusOK, "同一个单号再填一次")
	staff := staffNotifications(t, cs.Host, cs.Token, "")
	if c := countNotif(staff.Items, service.KindMerchantReturnShipped, r.RefundNo); c != 1 {
		t.Errorf("同一个运单号填了两次，门店收到 %d 条，期望 1 条", c)
	}
	if rs := mustNotif(t, staff.Items, service.KindMerchantReturnShipped, r.RefundNo); !strings.Contains(rs.Body, "圆通") ||
		!strings.Contains(rs.Body, "YT100") {
		t.Errorf("寄回物流那条是 %q", rs.Body)
	}
	wantStatus(t, returnShipment(t, cs, r.RefundNo, b.Token, `{"carrier_code":"yto","tracking_no":"YT200"}`,
		"rs-"+uniqueKey()), http.StatusOK, "改运单号")
	if c := countNotif(staffNotifications(t, cs.Host, cs.Token, "").Items, service.KindMerchantReturnShipped, r.RefundNo); c != 2 {
		t.Errorf("改了运单号之后门店有 %d 条寄回提醒，期望 2 条", c)
	}

	wantStatus(t, postIdem(t, cs.Host, "/api/v1/admin/refunds/"+r.RefundNo+"/receipt", "", cs.Token),
		http.StatusOK, "确认收到退货")
	done := mustNotif(t, myNotifications(t, cs.Host, b.Token, "").Items, service.KindRefundSucceeded, r.RefundNo)
	if !strings.Contains(done.Body, "¥60.00") {
		t.Errorf("退款到账那条是 %q", done.Body)
	}

	// 仅退款，驳回：带理由。
	o2 := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	_, lines2 := cs.lines(t, b, o2.OrderNo)
	r2 := cs.mustApply(t, b, o2.OrderNo, refundBody(1, [2]int64{lines2[cs.DressSKU].Id, 1}))
	wantStatus(t, cs.audit(t, r2.RefundNo, `{"action":"reject","reject_reason":"商品已拆封 <影响二次销售>"}`),
		http.StatusOK, "驳回")
	rj := mustNotif(t, myNotifications(t, cs.Host, b.Token, "").Items, service.KindRefundRejected, r2.RefundNo)
	if !strings.Contains(rj.Body, "商品已拆封 <影响二次销售>") {
		t.Errorf("驳回那条没带原样的理由：%q", rj.Body)
	}
	if findNotif(myNotifications(t, cs.Host, b.Token, "").Items, service.KindRefundApproved, r2.RefundNo) != nil {
		t.Error("被驳回的售后单也收到了「审核通过」")
	}
}

// ---------------------------------------------------------------------------
// 定时任务：超时关单、自动确认收货（含到期前一天的提醒）
// ---------------------------------------------------------------------------

func TestTimeoutCloseAndAutoConfirmNotifyTheBuyer(t *testing.T) {
	cs := newCouponShop(t)
	adminExec(t, `UPDATE shop_settings SET auto_confirm_days = 3 WHERE merchant_id = $1`, cs.MerchantID)
	b := cs.newBuyer(t, "notify-jobs")
	ctx := context.Background()

	// 超时关单。
	pending := cs.placeOrder(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	expire(t, pending.OrderNo)
	if _, err := service.NewSweepService(repository.New(testPool), service.SweepConfig{}, nil).SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if orderStatusOf(t, pending.OrderNo) != 90 {
		t.Fatalf("过期的待支付订单没被关掉")
	}
	closed := mustNotif(t, myNotifications(t, cs.Host, b.Token, "").Items, service.KindOrderTimeoutClosed, pending.OrderNo)
	if closed.Title != "订单已关闭" {
		t.Errorf("超时关单那条标题是 %q", closed.Title)
	}

	shipped := func() string {
		o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
		wantStatus(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusCreated, "发货")
		return o.OrderNo
	}
	soon := shipped()       // 发货满 2 天（配置 3 天）：到期前一天，该提醒
	fresh := shipped()      // 发货 1 天：还早
	withRefund := shipped() // 满 2 天但有在途售后：自动确认对它是暂停的，不提醒
	shippedDaysAgo(t, soon, 2)
	shippedDaysAgo(t, fresh, 1)
	shippedDaysAgo(t, withRefund, 2)
	_, lines := cs.lines(t, b, withRefund)
	cs.mustApply(t, b, withRefund, refundBody(2, [2]int64{lines[cs.DressSKU].Id, 1}))

	confirmer := newConfirmer()
	for i := 0; i < 2; i++ { // 跑两轮：提醒只发一次
		if _, err := confirmer.ConfirmOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	mine := myNotifications(t, cs.Host, b.Token, "")
	if c := countNotif(mine.Items, service.KindOrderAutoConfirmSoon, soon); c != 1 {
		t.Fatalf("到期前一天的订单收到 %d 条「即将自动确认」，期望恰好 1 条（跑了两轮）", c)
	}
	if n := mustNotif(t, mine.Items, service.KindOrderAutoConfirmSoon, soon); !strings.Contains(n.Body, "3 天") {
		t.Errorf("提醒里没写店铺配置的天数：%q", n.Body)
	}
	for _, no := range []string{fresh, withRefund} {
		if findNotif(mine.Items, service.KindOrderAutoConfirmSoon, no) != nil {
			t.Errorf("订单 %s 不该收到「即将自动确认」", no)
		}
	}
	if orderStatusOf(t, soon) != 30 {
		t.Fatal("提醒那一轮就把订单确认了")
	}

	// 真到期：确认 + 「订单已完成」。
	shippedDaysAgo(t, soon, 3)
	if _, err := confirmer.ConfirmOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if orderStatusOf(t, soon) != 40 {
		t.Fatal("到期的订单没被自动确认")
	}
	fin := mustNotif(t, myNotifications(t, cs.Host, b.Token, "").Items, service.KindOrderFinished, soon)
	if !strings.Contains(fin.Body, "自动确认") {
		t.Errorf("自动确认那条是 %q", fin.Body)
	}
}

// ---------------------------------------------------------------------------
// 库存预警
// ---------------------------------------------------------------------------

// 只在跌破预警线的那一次发；扣到 0 标题是「已售罄」。
func TestLowStockAlertFiresOnceWhenCrossingTheWarningLine(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "notify-stock")
	setStock := func(storeID, skuID int64, qty, warning int) {
		w := putAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/inventory", storeID, skuID),
			fmt.Sprintf(`{"expected_available_qty":%d,"available_qty":%d,"warning_qty":%d}`,
				availableAt(t, storeID, skuID), qty, warning), cs.Token)
		wantStatus(t, w, http.StatusOK, "设库存与预警线")
	}
	alerts := func(skuID int64) []api.Notification {
		var out []api.Notification
		for _, n := range staffNotifications(t, cs.Host, cs.Token, "").Items {
			if string(n.Kind) == service.KindMerchantInventoryLow && n.Target.SkuId != nil && *n.Target.SkuId == skuID {
				out = append(out, n)
			}
		}
		return out
	}

	setStock(cs.SouthStore, cs.DressSKU, 3, 2)
	cs.placeOrder(t, b, cs.SouthStore, cs.DressSKU, 1, nil) // 3 → 2：跌到线上，算跨线
	got := alerts(cs.DressSKU)
	if len(got) != 1 {
		t.Fatalf("3 → 2（预警线 2）之后有 %d 条库存预警，期望 1", len(got))
	}
	a := got[0]
	if a.Title != "库存预警" || !strings.Contains(a.Body, "剩 2 件") || !strings.Contains(a.Body, "广州门店") ||
		a.Target.Type != api.NotificationTargetTypeInventory || a.Target.StoreId == nil || *a.Target.StoreId != cs.SouthStore {
		t.Errorf("库存预警是 %q / %q / %+v", a.Title, a.Body, a.Target)
	}
	cs.placeOrder(t, b, cs.SouthStore, cs.DressSKU, 1, nil) // 2 → 1：已经在线下，不再提醒
	if n := len(alerts(cs.DressSKU)); n != 1 {
		t.Errorf("已经在预警线下再卖一件，预警变成了 %d 条，期望仍是 1", n)
	}

	setStock(cs.SouthStore, cs.ShirtSKU, 1, 0)
	cs.placeOrder(t, b, cs.SouthStore, cs.ShirtSKU, 1, nil) // 1 → 0，预警线 0：售罄
	sold := alerts(cs.ShirtSKU)
	if len(sold) != 1 || sold[0].Title != "商品已售罄" {
		t.Errorf("扣到 0 之后是 %+v，期望一条「商品已售罄」", sold)
	}
	// 华北门店还有 50 件、预警线 0：卖一件不提醒。
	cs.placeOrder(t, b, cs.NorthStore, cs.ShirtSKU, 1, nil)
	if n := len(alerts(cs.ShirtSKU)); n != 1 {
		t.Errorf("远在预警线之上的门店卖了一件，衬衫的预警变成了 %d 条", n)
	}
}

// ---------------------------------------------------------------------------
// 后台：范围与每员工已读
// ---------------------------------------------------------------------------

func TestAdminNotificationsAreScopedAndReadPerStaff(t *testing.T) {
	fx := newPermFixture(t)
	n1 := permNotification(t, fx, fx.N1)
	n2 := permNotification(t, fx, fx.N2)
	e1 := permNotification(t, fx, fx.E1)
	s0 := permNotification(t, fx, fx.sh.StoreID)

	ids := func(l notifList) map[int64]bool {
		out := map[int64]bool{}
		for _, n := range l.Items {
			out[n.Id] = true
		}
		return out
	}
	cases := []struct {
		role permRole
		want []int64
	}{
		{roleAdmin, []int64{n1, n2, e1, s0}},
		{roleOperator, []int64{n1, n2, e1, s0}},
		{roleRegion, []int64{n1, n2}},
		{roleStore, []int64{n1}},
	}
	for _, c := range cases {
		l := staffNotifications(t, fx.sh.Host, fx.tokens[c.role], "")
		got := ids(l)
		if len(got) != len(c.want) || l.UnreadCount != len(c.want) {
			t.Errorf("[%s] 看到 %v（未读 %d），期望 %v", c.role, got, l.UnreadCount, c.want)
		}
		for _, id := range c.want {
			if !got[id] {
				t.Errorf("[%s] 看不到范围内的提醒 %d", c.role, id)
			}
		}
	}

	// 范围外：404，不是 403（看不见的不承认它存在）。
	wantStatus(t, post(t, fx.sh.Host, fmt.Sprintf("/api/v1/admin/notifications/%d/read", e1), "", fx.tokens[roleRegion]),
		http.StatusNotFound, "大区管理员标别的大区的提醒")
	wantStatus(t, post(t, fx.sh.Host, fmt.Sprintf("/api/v1/admin/notifications/%d/read", n2), "", fx.tokens[roleStore]),
		http.StatusNotFound, "门店管理员标别的门店的提醒")

	// 每员工已读：管理员读了 n1，大区管理员那边 n1 仍是未读。
	w := post(t, fx.sh.Host, fmt.Sprintf("/api/v1/admin/notifications/%d/read", n1), "", fx.tokens[roleAdmin])
	wantStatus(t, w, http.StatusOK, "管理员标已读")
	if n := unreadCount(t, w); n != 3 {
		t.Errorf("管理员标一条之后未读 %d，期望 3", n)
	}
	for _, n := range staffNotifications(t, fx.sh.Host, fx.tokens[roleRegion], "").Items {
		if n.Id == n1 && n.ReadAt != nil {
			t.Error("管理员读了一条，大区管理员那边也变成了已读 —— 已读应当每员工各自一份")
		}
	}
	// 门店管理员全部已读：只动他自己范围里的那一条，别人不受影响。
	w = post(t, fx.sh.Host, "/api/v1/admin/notifications/read-all", "", fx.tokens[roleStore])
	if w.Code != http.StatusOK || unreadCount(t, w) != 0 {
		t.Errorf("门店管理员全部已读：%d", w.Code)
	}
	w = getAuth(t, fx.sh.Host, "/api/v1/admin/notifications/unread-count", fx.tokens[roleRegion])
	if w.Code != http.StatusOK || unreadCount(t, w) != 2 {
		t.Errorf("门店管理员全部已读之后，大区管理员的未读不是 2")
	}
	if l := staffNotifications(t, fx.sh.Host, fx.tokens[roleAdmin], "&unread_only=true"); len(l.Items) != 3 || l.Total != 3 {
		t.Errorf("管理员 unread_only 拿到 %d 条（total %d），期望 3", len(l.Items), l.Total)
	}
}

// ---------------------------------------------------------------------------
// 可靠性：通知与状态变化同生同灭
// ---------------------------------------------------------------------------

// failAtCommit 在 table 上挂一个**延迟**约束触发器：本事务里一旦写过（INSERT / UPDATE）
// order_no = orderNo 且（给了 status 时）status = status 的行，**提交时**抛错。
//
// 延迟到提交时是这条测试的要点：它让失败发生在业务写与通知写**都已经执行完**之后，
// 于是「两者是不是同一个事务」成了唯一决定结果的东西 —— 同一个事务就一起回滚；
// 若通知在另一个事务里（提前或事后写），总有一边会留下来。
func failAtCommit(t *testing.T, table, event, orderNo, status string) {
	t.Helper()
	name := "notify_probe_" + strings.ReplaceAll(uniqueKey(), "-", "")
	adminExec(t, `
		CREATE OR REPLACE FUNCTION notify_probe_fail() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		    IF NEW.order_no = TG_ARGV[0] AND (TG_ARGV[1] = '' OR NEW.status::text = TG_ARGV[1]) THEN
		        RAISE EXCEPTION 'notify rollback probe on %', TG_TABLE_NAME;
		    END IF;
		    RETURN NULL;
		END $$`)
	adminExec(t, fmt.Sprintf(`CREATE CONSTRAINT TRIGGER %s AFTER %s ON %s
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION notify_probe_fail(%s, %s)`,
		name, event, table, quoteLiteral(orderNo), quoteLiteral(status)))
	t.Cleanup(func() { adminExec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, name, table)) })
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// 通知写不进去（提交时失败）→ 状态也不许改：不存在「状态改了、通知丢了」。
func TestNotificationFailureRollsBackTheStateChange(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "notify-rollback-1")
	o := cs.placeOrder(t, b, cs.NorthStore, cs.DressSKU, 1, nil)

	failAtCommitNotifications(t, o.OrderNo)
	body := payload(o.OrderNo, "txn-"+o.OrderNo, o.PayableCents)
	w := notifyPaymentSigned(t, cs.Host, "wechat", body, sign(couponWebhookSecret(cs.adminShop), []byte(body)))
	if w.Code == http.StatusOK {
		t.Fatalf("通知在提交时写失败了，支付回调却回了 200 —— 订单状态与通知不在同一个事务里")
	}
	if s := orderStatusOf(t, o.OrderNo); s != 10 {
		t.Fatalf("通知没写进去，订单却被推到了 %d —— 状态改了、通知丢了", s)
	}
	if n := notificationsFor(t, o.OrderNo); n != 0 {
		t.Fatalf("回滚之后还留着 %d 条通知", n)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM payments p JOIN orders o ON o.id = p.order_id
	                             WHERE o.order_no = $1`, o.OrderNo); n != 0 {
		t.Fatalf("回滚之后还留着 %d 笔支付单", n)
	}
}

// failAtCommitNotifications 是 failAtCommit 在 notifications 上的形状（那张表没有 status 列）。
func failAtCommitNotifications(t *testing.T, orderNo string) {
	t.Helper()
	name := "notify_probe_n" + strings.ReplaceAll(uniqueKey(), "-", "")
	adminExec(t, `
		CREATE OR REPLACE FUNCTION notify_probe_fail_n() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		    IF NEW.order_no = TG_ARGV[0] THEN
		        RAISE EXCEPTION 'notify rollback probe on notifications';
		    END IF;
		    RETURN NULL;
		END $$`)
	adminExec(t, fmt.Sprintf(`CREATE CONSTRAINT TRIGGER %s AFTER INSERT ON notifications
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION notify_probe_fail_n(%s)`,
		name, quoteLiteral(orderNo)))
	t.Cleanup(func() { adminExec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON notifications`, name)) })
}

// 状态变化回滚了（提交时失败）→ 通知也不许留下：不存在「回滚了通知却发了」。
// 外发任务同理：它与通知同一个事务，这里一并核对队列里没有指向这一单的任务。
func TestStateRollbackLeavesNoNotificationBehind(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "notify-rollback-2")
	o := cs.placeOrder(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	jobsBefore := adminQueryInt64(t, `SELECT count(*) FROM jobs WHERE merchant_id = $1 AND queue = $2`,
		cs.MerchantID, repository.QueueNotificationDelivery)

	failAtCommit(t, "orders", "UPDATE", o.OrderNo, "20")
	body := payload(o.OrderNo, "txn-"+o.OrderNo, o.PayableCents)
	w := notifyPaymentSigned(t, cs.Host, "wechat", body, sign(couponWebhookSecret(cs.adminShop), []byte(body)))
	if w.Code == http.StatusOK {
		t.Fatalf("订单在提交时失败了，支付回调却回了 200")
	}
	if s := orderStatusOf(t, o.OrderNo); s != 10 {
		t.Fatalf("探针没生效：订单是 %d", s)
	}
	if n := notificationsFor(t, o.OrderNo); n != 0 {
		t.Fatalf("状态变化回滚了，却留下了 %d 条通知 —— 回滚了通知却发了", n)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM jobs WHERE merchant_id = $1 AND queue = $2`,
		cs.MerchantID, repository.QueueNotificationDelivery); n != jobsBefore {
		t.Fatalf("状态变化回滚了，外发队列却多了 %d 条任务", n-jobsBefore)
	}

	// 对照：摘掉探针之后同一笔回调入账成功，通知两条（买家 + 门店）都在 —— 证明上面的失败
	// 确实是探针造成的，而不是这笔回调本来就入不了账。
	dropProbes(t, "orders")
	w = notifyPaymentSigned(t, cs.Host, "wechat", body, sign(couponWebhookSecret(cs.adminShop), []byte(body)))
	wantStatus(t, w, http.StatusOK, "摘掉探针之后重推回调")
	if n := notificationsFor(t, o.OrderNo); n != 2 {
		t.Fatalf("入账成功之后这一单有 %d 条通知，期望 2（买家 + 门店）", n)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM jobs WHERE merchant_id = $1 AND queue = $2`,
		cs.MerchantID, repository.QueueNotificationDelivery); n != jobsBefore+2 {
		t.Fatalf("入账成功之后外发队列多了 %d 条，期望 2（每条通知一条）", n-jobsBefore)
	}
}

// dropProbes 摘掉 table 上本测试挂的全部探针触发器。
func dropProbes(t *testing.T, table string) {
	t.Helper()
	rows, err := admin(t).Query(context.Background(), `
		SELECT tgname FROM pg_trigger WHERE tgrelid = $1::regclass AND tgname LIKE 'notify_probe_%'`, table)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		adminExec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, n, table))
	}
}

// ---------------------------------------------------------------------------
// 外发投递：可插拔渠道、未配置即跳过、失败退避、重试只补没定论的那一路
// ---------------------------------------------------------------------------

// fakeChannel 是外发渠道的测试替身。fail 里的通知 id 第一次投递失败，之后成功；
// noRecipient 为 true 时一律回 ErrNoRecipient。
type fakeChannel struct {
	name        string
	noRecipient bool
	mu          sync.Mutex
	fail        map[int64]bool
	calls       map[int64]int
}

func (f *fakeChannel) Name() string     { return f.name }
func (f *fakeChannel) Configured() bool { return true }
func (f *fakeChannel) Send(_ context.Context, m service.NotificationMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[int64]int{}
	}
	f.calls[m.NotificationID]++
	if f.noRecipient {
		return service.ErrNoRecipient
	}
	if f.fail[m.NotificationID] {
		delete(f.fail, m.NotificationID)
		return fmt.Errorf("替身：%s 渠道这一次失败", f.name)
	}
	return nil
}

func (f *fakeChannel) callsFor(id int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[id]
}

type deliveryRow struct {
	Channel string
	Status  int16
	Attempt int32
}

func deliveriesOf(t *testing.T, notificationID int64) []deliveryRow {
	t.Helper()
	rows, err := admin(t).Query(context.Background(), `
		SELECT channel, status, attempt FROM notification_deliveries
		 WHERE notification_id = $1 ORDER BY id`, notificationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []deliveryRow
	for rows.Next() {
		var d deliveryRow
		if err := rows.Scan(&d.Channel, &d.Status, &d.Attempt); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func buyerNotificationID(t *testing.T, kind, orderNo string) int64 {
	t.Helper()
	return adminQueryInt64(t, `SELECT id FROM notifications WHERE kind = $1 AND order_no = $2`, kind, orderNo)
}

// 默认渠道：三个都未配置 → 各记一行「跳过」，任务标成功，不报错、不重试。
func TestDeliveryWithDefaultChannelsRecordsSkippedAndFinishes(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "notify-deliver")
	o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	id := buyerNotificationID(t, service.KindOrderPaid, o.OrderNo)

	w := service.NewNotificationDeliveryService(repository.New(testPool), nil, service.NotificationDeliveryConfig{}, nil)
	if _, err := w.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := deliveriesOf(t, id)
	if len(got) != 3 {
		t.Fatalf("默认渠道投递之后有 %d 行投递记录，期望 3（微信订阅消息 / 短信 / 邮件各一行）：%+v", len(got), got)
	}
	for _, d := range got {
		if d.Status != repository.NotificationDeliverySkipped || d.Attempt != 1 {
			t.Errorf("默认渠道的投递记录是 %+v，期望 status 2（未配置跳过）、attempt 1", d)
		}
	}
	if s := adminQueryInt64(t, `SELECT status FROM jobs WHERE queue = $1 AND job_key = $2`,
		repository.QueueNotificationDelivery, fmt.Sprintf("notification:%d", id)); s != 2 {
		t.Errorf("默认渠道投递之后任务状态是 %d，期望 2 已成功（跳过不是失败，不该重试）", s)
	}
}

// 替身渠道：发出 / 没有收件人（跳过）/ 失败退避；重试只补失败的那一路，已发出的不重发。
func TestDeliveryRetriesOnlyTheChannelThatFailed(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "notify-deliver-fake")
	o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	id := buyerNotificationID(t, service.KindOrderPaid, o.OrderNo)

	wechat := &fakeChannel{name: service.ChannelWechatSubscribe, noRecipient: true}
	sms := &fakeChannel{name: service.ChannelSMS, fail: map[int64]bool{id: true}}
	email := &fakeChannel{name: service.ChannelEmail}
	w := service.NewNotificationDeliveryService(repository.New(testPool),
		[]service.NotificationChannel{wechat, sms, email}, service.NotificationDeliveryConfig{}, nil)
	ctx := context.Background()
	if _, err := w.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	jobKey := fmt.Sprintf("notification:%d", id)
	if s := adminQueryInt64(t, `SELECT status FROM jobs WHERE queue = $1 AND job_key = $2`,
		repository.QueueNotificationDelivery, jobKey); s != 0 {
		t.Fatalf("短信失败之后任务状态是 %d，期望 0（退避后重来）", s)
	}
	// 不等退避：把 run_after 拨到现在。
	adminExec(t, `UPDATE jobs SET run_after = now() WHERE queue = $1 AND job_key = $2`,
		repository.QueueNotificationDelivery, jobKey)
	if _, err := w.Drain(ctx); err != nil {
		t.Fatal(err)
	}

	want := []deliveryRow{
		{service.ChannelWechatSubscribe, repository.NotificationDeliverySkipped, 1},
		{service.ChannelSMS, repository.NotificationDeliveryFailed, 1},
		{service.ChannelEmail, repository.NotificationDeliverySent, 1},
		{service.ChannelSMS, repository.NotificationDeliverySent, 2},
	}
	got := deliveriesOf(t, id)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("投递记录是 %+v，期望 %+v", got, want)
	}
	if email.callsFor(id) != 1 || wechat.callsFor(id) != 1 || sms.callsFor(id) != 2 {
		t.Errorf("各渠道被调的次数：微信 %d、短信 %d、邮件 %d —— 期望 1 / 2 / 1（重试只补失败的那一路）",
			wechat.callsFor(id), sms.callsFor(id), email.callsFor(id))
	}
	if s := adminQueryInt64(t, `SELECT status FROM jobs WHERE queue = $1 AND job_key = $2`,
		repository.QueueNotificationDelivery, jobKey); s != 2 {
		t.Errorf("重试成功之后任务状态是 %d，期望 2", s)
	}
}

// ---------------------------------------------------------------------------
// 保留期
// ---------------------------------------------------------------------------

func TestNotificationsOlderThanRetentionArePurged(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "notify-retention")
	insert := func(age string, key string) int64 {
		return adminQueryInt64(t, `
			INSERT INTO notifications (merchant_id, audience, user_id, kind, title, body, target_type,
			                           order_no, dedupe_key, created_at)
			VALUES ($1, 1, $2, 'order_paid', '支付成功', '保留期', 'order', 'RET', $3, now() - $4::interval)
			RETURNING id`, cs.MerchantID, b.UserID, key+uniqueKey(), age)
	}
	old := insert("91 days", "ret-old-")
	young := insert("89 days", "ret-young-")
	adminExec(t, `INSERT INTO notification_deliveries (merchant_id, notification_id, channel, status, attempt)
	              VALUES ($1, $2, 'sms', 2, 1)`, cs.MerchantID, old)

	w := service.NewNotificationDeliveryService(repository.New(testPool), nil, service.NotificationDeliveryConfig{}, nil)
	if _, err := w.PurgeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM notifications WHERE id = $1`, old); n != 0 {
		t.Error("91 天前的通知没被清理")
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM notification_deliveries WHERE notification_id = $1`, old); n != 0 {
		t.Error("清理了通知，它的投递记录还在（级联没生效）")
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM notifications WHERE id = $1`, young); n != 1 {
		t.Error("89 天前的通知被清理了 —— 保留期是 90 天")
	}
}

// ---------------------------------------------------------------------------
// 契约
// ---------------------------------------------------------------------------

// 服务端的模板表与契约的 NotificationKind 枚举逐一对应：多一种，客户端的 switch 认不出；
// 少一种，契约里写着一种永远不会出现的通知。
func TestNotificationKindsMatchTheContract(t *testing.T) {
	kinds := service.NotificationKinds()
	for _, k := range kinds {
		if !api.NotificationKind(k).Valid() {
			t.Errorf("服务端会发 %q，但契约的 NotificationKind 里没有它", k)
		}
	}
	doc := loadContract(t)
	var enum []any
	if s, ok := doc.Components.Schemas["NotificationKind"]; ok {
		enum, _ = s["enum"].([]any)
	}
	if len(enum) != len(kinds) {
		t.Errorf("契约 NotificationKind 有 %d 种，服务端模板有 %d 种", len(enum), len(kinds))
	}
}
