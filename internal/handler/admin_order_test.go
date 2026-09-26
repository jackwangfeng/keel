package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/api"
)

// 后台订单与退款单的列表 / 详情（GET /admin/orders、GET /admin/refunds 及详情，迁移 00035）。
//
// 权限矩阵（permission_test.go）只能逐格断言「放行 / 拒绝」：列表那一行对四种角色都是
// 200，而「200 里装的是哪些单」它看不见。这里补上那一半 —— 同一家连锁、四家门店各一单，
// 四种角色各自读一遍，逐个比对单号集合。一个把范围过滤整个删掉的实现，
// 在矩阵里是全绿的，在这里会红。

// aoOrder 在 storeID 这家门店造一笔订单（直接插库，理由同 permPaidOrder），返回单号。
//
// 收货人手机号与下单买家的账号手机号分开给，好验「phone 两个都认」；
// createdAt 为零值时用 now()。status 20 带 paid_at，30 另带 shipped_at
// （00033 的 chk_fulfillment_timestamps）。
func aoOrder(t *testing.T, fx *permFixture, storeID int64, status int, receiverPhone, buyerPhone string,
	createdAt time.Time) string {
	t.Helper()
	permCleanupOrders(t, fx)
	no := "AO" + fx.next()
	if buyerPhone == "" {
		buyerPhone = fmt.Sprintf("135%08d", fx.seq.Add(1)%100_000_000)
	}
	uid := adminQueryInt64(t, `INSERT INTO users (merchant_id, phone, nickname)
	                           VALUES ($1, $2, '权限矩阵下单人') RETURNING id`, fx.sh.MerchantID, buyerPhone)
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	paid, shipped := "NULL", "NULL"
	if status >= 20 {
		paid = "now()"
	}
	if status == 30 {
		shipped = "now()"
	}
	adminExec(t, `
		INSERT INTO orders (merchant_id, order_no, user_id, status, goods_amount_cents, payable_cents,
		                    paid_cents, paid_at, shipped_at, receiver_snapshot, expire_at,
		                    store_id, region_id, store_snapshot, created_at)
		SELECT $1, $2, $3, $4::smallint, 1000, 1000, CASE WHEN $4::smallint >= 20 THEN 1000 ELSE 0 END,
		       `+paid+`, `+shipped+`,
		       jsonb_build_object('receiver_name', '张三', 'phone', $5::text, 'province', '北京',
		                          'city', '北京', 'district', '朝阳', 'detail', '某路 1 号'),
		       now() + interval '30 minutes', st.id, st.region_id,
		       jsonb_build_object('store_name', st.name, 'region_name', r.name), $7
		  FROM stores st JOIN regions r ON r.id = st.region_id WHERE st.id = $6`,
		fx.sh.MerchantID, no, uid, status, receiverPhone, storeID, createdAt)
	return no
}

type aoOrderPage struct {
	api.PageMeta
	Items []api.AdminOrderSummary `json:"items"`
}

type aoRefundPage struct {
	api.PageMeta
	Items []api.AdminRefund `json:"items"`
}

// aoOrders 以 token 的身份读一页订单（page_size=100），返回单号集合与 total。
func aoOrders(t *testing.T, fx *permFixture, token, query string) (map[string]bool, int) {
	t.Helper()
	var page aoOrderPage
	decodeInto(t, getAs(t, fx.sh.Host, "/api/v1/admin/orders?page_size=100"+query, token),
		http.StatusOK, "后台订单列表"+query, &page)
	out := map[string]bool{}
	for _, o := range page.Items {
		out[o.OrderNo] = true
	}
	if page.Total != len(page.Items) {
		t.Fatalf("订单列表%s：total=%d 与本页条数 %d 对不上（计数与取页的谓词分叉了？）",
			query, page.Total, len(page.Items))
	}
	return out, page.Total
}

func aoRefunds(t *testing.T, fx *permFixture, token, query string) map[string]bool {
	t.Helper()
	var page aoRefundPage
	decodeInto(t, getAs(t, fx.sh.Host, "/api/v1/admin/refunds?page_size=100"+query, token),
		http.StatusOK, "后台退款单列表"+query, &page)
	if page.Total != len(page.Items) {
		t.Fatalf("退款单列表%s：total=%d 与本页条数 %d 对不上", query, page.Total, len(page.Items))
	}
	out := map[string]bool{}
	for _, r := range page.Items {
		out[r.RefundNo] = true
	}
	return out
}

func sameSet(got map[string]bool, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if !got[w] {
			return false
		}
	}
	return true
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// 列表只返回范围内的：管理员 / 操作员全店，大区管理员本大区（N1、N2），门店管理员自己的店（N1）。
// 带一个范围外的 store_id 拿到的是空页（交集），不是 403，也不是那家店的单。
func TestAdminOrderAndRefundListsAreScopedByRole(t *testing.T) {
	fx := newPermFixture(t)
	n1 := aoOrder(t, fx, fx.N1, 20, "13900000001", "", time.Time{})
	n2 := aoOrder(t, fx, fx.N2, 20, "13900000002", "", time.Time{})
	e1 := aoOrder(t, fx, fx.E1, 20, "13900000003", "", time.Time{})
	s0 := aoOrder(t, fx, fx.sh.StoreID, 20, "13900000004", "", time.Time{})
	rn1 := permRefund(t, fx, fx.N1, 10)
	rn2 := permRefund(t, fx, fx.N2, 10)
	re1 := permRefund(t, fx, fx.E1, 10)
	rs0 := permRefund(t, fx, fx.sh.StoreID, 10)
	// permRefund 每张都挂在一笔新订单上，那四笔也在列表里。
	orderOf := func(refundNo string) string {
		return adminQueryText(t, `SELECT o.order_no FROM refunds r JOIN orders o ON o.id = r.order_id
		                          WHERE r.refund_no = $1`, refundNo)
	}
	on1, on2, oe1, os0 := orderOf(rn1), orderOf(rn2), orderOf(re1), orderOf(rs0)

	cases := []struct {
		role    permRole
		orders  []string
		refunds []string
	}{
		{roleAdmin, []string{n1, n2, e1, s0, on1, on2, oe1, os0}, []string{rn1, rn2, re1, rs0}},
		{roleOperator, []string{n1, n2, e1, s0, on1, on2, oe1, os0}, []string{rn1, rn2, re1, rs0}},
		{roleRegion, []string{n1, n2, on1, on2}, []string{rn1, rn2}},
		{roleStore, []string{n1, on1}, []string{rn1}},
	}
	for _, c := range cases {
		got, _ := aoOrders(t, fx, fx.tokens[c.role], "")
		if !sameSet(got, c.orders...) {
			t.Errorf("[%s] 订单列表 = %v，期望 %v", c.role, keysOf(got), c.orders)
		}
		gotR := aoRefunds(t, fx, fx.tokens[c.role], "")
		if !sameSet(gotR, c.refunds...) {
			t.Errorf("[%s] 退款单列表 = %v，期望 %v", c.role, keysOf(gotR), c.refunds)
		}
	}

	// 交集：大区管理员点名华东一店、门店管理员点名华北二店 —— 空页。
	for _, c := range []struct {
		role  permRole
		store int64
	}{{roleRegion, fx.E1}, {roleStore, fx.N2}} {
		q := fmt.Sprintf("&store_id=%d", c.store)
		if got, total := aoOrders(t, fx, fx.tokens[c.role], q); len(got) != 0 || total != 0 {
			t.Errorf("[%s] 带范围外的 store_id 拿到了 %v（total=%d），期望空页", c.role, keysOf(got), total)
		}
		if got := aoRefunds(t, fx, fx.tokens[c.role], q); len(got) != 0 {
			t.Errorf("[%s] 退款单列表带范围外的 store_id 拿到了 %v，期望空页", c.role, keysOf(got))
		}
	}
	// 对照：范围内的 store_id 真的在筛（不是「带了 store_id 就一律空」）。
	if got, _ := aoOrders(t, fx, fx.tokens[roleRegion], fmt.Sprintf("&store_id=%d", fx.N2)); !sameSet(got, n2, on2) {
		t.Errorf("大区管理员带 store_id=N2 拿到 %v，期望 [%s %s]", keysOf(got), n2, on2)
	}
}

// 门店被调到别的大区之后，它的单跟着门店走：看门店**此刻**的大区，不看订单上
// 冗余的 region_id（那是下单时的大区）。门店软删之后，原大区的人仍然看得见、打得开。
func TestAdminOrderScopeFollowsTheStoreNotTheOrderSnapshot(t *testing.T) {
	fx := newPermFixture(t)
	moved := fx.freshStore(t, fx.North)
	gone := fx.freshStore(t, fx.North)
	onMoved := aoOrder(t, fx, moved, 20, "13900000011", "", time.Time{})
	onGone := aoOrder(t, fx, gone, 20, "13900000012", "", time.Time{})

	wantStatus(t, reqAs(t, http.MethodPatch, fx.sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d", moved),
		fmt.Sprintf(`{"region_id":%d}`, fx.East), fx.sh.Token), http.StatusOK, "把门店调到华东")
	wantStatus(t, deleteAs(t, fx.sh.Host, fmt.Sprintf("/api/v1/admin/stores/%d", gone), fx.sh.Token),
		http.StatusNoContent, "软删门店")

	got, _ := aoOrders(t, fx, fx.tokens[roleRegion], "")
	if got[onMoved] {
		t.Errorf("门店已调到华东，华北的大区管理员仍然在列表里看到它的单 %s —— 范围按了订单上的 region_id", onMoved)
	}
	if !got[onGone] {
		t.Errorf("门店软删之后，华北的大区管理员在列表里看不到它的单 %s 了", onGone)
	}
	wantStatus(t, getAs(t, fx.sh.Host, "/api/v1/admin/orders/"+onGone, fx.tokens[roleRegion]),
		http.StatusOK, "大区管理员打开已软删门店的订单")
	if typ := problemType(t, getAs(t, fx.sh.Host, "/api/v1/admin/orders/"+onMoved, fx.tokens[roleRegion]),
		http.StatusForbidden, "大区管理员打开调走的门店的订单"); !strings.HasSuffix(typ, "/out-of-scope") {
		t.Errorf("调走的门店的订单：期望 out-of-scope，实际 %s", typ)
	}
}

// 筛选：状态、下单时间（半开区间）、单号、手机号（收货人或买家账号，精确匹配）。
func TestAdminOrderListFilters(t *testing.T) {
	fx := newPermFixture(t)
	day := time.Date(2026, 1, 15, 8, 0, 0, 0, time.UTC)
	paid := aoOrder(t, fx, fx.N1, 20, "13911110001", "13922220001", time.Time{})
	shipped := aoOrder(t, fx, fx.N1, 30, "13911110002", "", time.Time{})
	old := aoOrder(t, fx, fx.N1, 20, "13911110003", "", day)
	edge := aoOrder(t, fx, fx.N1, 20, "13911110004", "", day.Add(24*time.Hour-8*time.Hour)) // 次日零点
	tok := fx.sh.Token

	if got, _ := aoOrders(t, fx, tok, "&status=30"); !sameSet(got, shipped) {
		t.Errorf("status=30 拿到 %v，期望只有 %s", keysOf(got), shipped)
	}
	if got, _ := aoOrders(t, fx, tok, "&status=20"); got[shipped] || !got[paid] {
		t.Errorf("status=20 拿到 %v，期望含 %s、不含 %s", keysOf(got), paid, shipped)
	}

	q := "&created_from=" + url.QueryEscape("2026-01-15T00:00:00Z") + "&created_to=" + url.QueryEscape("2026-01-16T00:00:00Z")
	if got, _ := aoOrders(t, fx, tok, q); !sameSet(got, old) {
		t.Errorf("按 1 月 15 日查拿到 %v，期望只有 %s（%s 落在次日零点，区间不含上界）", keysOf(got), old, edge)
	}

	if got, _ := aoOrders(t, fx, tok, "&order_no="+paid); !sameSet(got, paid) {
		t.Errorf("order_no=%s 拿到 %v", paid, keysOf(got))
	}
	if got, _ := aoOrders(t, fx, tok, "&order_no="+paid[:len(paid)-1]); len(got) != 0 {
		t.Errorf("单号少一位也命中了 %v —— order_no 应当是精确匹配", keysOf(got))
	}
	if got, _ := aoOrders(t, fx, tok, "&phone=13911110002"); !sameSet(got, shipped) {
		t.Errorf("按收货人手机号拿到 %v，期望只有 %s", keysOf(got), shipped)
	}
	if got, _ := aoOrders(t, fx, tok, "&phone=13922220001"); !sameSet(got, paid) {
		t.Errorf("按买家账号手机号拿到 %v，期望只有 %s", keysOf(got), paid)
	}
	if got, _ := aoOrders(t, fx, tok, "&phone=1391111000"); len(got) != 0 {
		t.Errorf("手机号少一位也命中了 %v —— phone 应当是精确匹配", keysOf(got))
	}

	// 写错的时间不当成没传：422，而不是一页不带筛选的全量结果。
	for _, bad := range []string{
		"&created_from=2026-01-15",
		"&created_from=" + url.QueryEscape("2026-01-16T00:00:00Z") + "&created_to=" + url.QueryEscape("2026-01-15T00:00:00Z"),
	} {
		for _, path := range []string{"/api/v1/admin/orders?", "/api/v1/admin/refunds?"} {
			typ := problemType(t, getAs(t, fx.sh.Host, path+bad[1:], tok), http.StatusUnprocessableEntity, path+bad)
			if !strings.HasSuffix(typ, "/invalid-request") {
				t.Errorf("%s%s：期望 invalid-request，实际 %s", path, bad, typ)
			}
		}
	}
}

// 退款单列表按状态筛：待审核（10）与待确认收到退货（20）是后台每天要找的两堆。
func TestAdminRefundListFiltersByStatus(t *testing.T) {
	fx := newPermFixture(t)
	pending := permRefund(t, fx, fx.N1, 10)
	awaiting := permRefund(t, fx, fx.N1, 20)
	if got := aoRefunds(t, fx, fx.sh.Token, "&status=10"); !sameSet(got, pending) {
		t.Errorf("status=10 拿到 %v，期望只有 %s", keysOf(got), pending)
	}
	if got := aoRefunds(t, fx, fx.sh.Token, "&status=20"); !sameSet(got, awaiting) {
		t.Errorf("status=20 拿到 %v，期望只有 %s", keysOf(got), awaiting)
	}
	if got := aoRefunds(t, fx, fx.sh.Token, fmt.Sprintf("&store_id=%d", fx.N2)); len(got) != 0 {
		t.Errorf("store_id=N2 拿到 %v，期望空（两张都在 N1）", keysOf(got))
	}
}

// 详情：订单行（含优惠分摊）、支付、包裹、退款单与审核记录都在；
// 退款单详情带审核人、收货人与所属订单摘要（含运费，审核退运费要看它）。
func TestAdminOrderAndRefundDetailCarryTheWholeStory(t *testing.T) {
	fx := newPermFixture(t)
	adminExec(t, `UPDATE staff SET name = '审核员甲' WHERE id = $1`, fx.sh.StaffID)

	// —— 一张待审核的单：列表上挂「售后中」，驳回之后摘掉，详情里有审核记录。
	rejected := permRefund(t, fx, fx.N1, 10)
	orderNo := adminQueryText(t, `SELECT o.order_no FROM refunds r JOIN orders o ON o.id = r.order_id
	                              WHERE r.refund_no = $1`, rejected)
	if !aoSummary(t, fx, orderNo).HasOpenRefund {
		t.Errorf("订单 %s 有一张待审核的退款单，列表上 has_open_refund 却是 false", orderNo)
	}
	wantStatus(t, postIdem(t, fx.sh.Host, "/api/v1/admin/refunds/"+rejected+"/audit",
		`{"action":"reject","reject_reason":"凭证不清晰"}`, fx.sh.Token), http.StatusOK, "驳回")
	if aoSummary(t, fx, orderNo).HasOpenRefund {
		t.Errorf("退款单驳回之后，订单 %s 在列表上仍然 has_open_refund = true", orderNo)
	}

	var rd api.AdminRefundDetail
	decodeInto(t, getAs(t, fx.sh.Host, "/api/v1/admin/refunds/"+rejected, fx.tokens[roleStore]),
		http.StatusOK, "门店管理员读本店退款单详情", &rd)
	if rd.Status != 50 || rd.RejectReason == nil || *rd.RejectReason != "凭证不清晰" || rd.AuditedAt == nil {
		t.Errorf("驳回之后的退款单详情：status=%d reject_reason=%v audited_at=%v", rd.Status, rd.RejectReason, rd.AuditedAt)
	}
	if rd.AuditedBy == nil || rd.AuditedBy.Id != fx.sh.StaffID || rd.AuditedBy.Name == nil || *rd.AuditedBy.Name != "审核员甲" {
		t.Errorf("审核记录里的审核人 = %+v，期望 id=%d name=审核员甲", rd.AuditedBy, fx.sh.StaffID)
	}
	if rd.StoreId != fx.N1 || rd.Order.OrderNo != orderNo || rd.Order.FreightCents == nil {
		t.Errorf("退款单详情的门店 / 订单摘要不对：store_id=%d order=%s freight=%v", rd.StoreId, rd.Order.OrderNo, rd.Order.FreightCents)
	}
	if len(rd.Items) != 1 || rd.Items[0].AmountCents != 1000 {
		t.Errorf("退款明细 = %+v，期望一行、实退 1000 分（服务端算好的数）", rd.Items)
	}

	var od api.AdminOrderDetail
	decodeInto(t, getAs(t, fx.sh.Host, "/api/v1/admin/orders/"+orderNo, fx.sh.Token),
		http.StatusOK, "后台订单详情", &od)
	if len(od.Items) != 1 || od.Items[0].DiscountCents == nil || od.Items[0].RefundingQty == nil {
		t.Errorf("订单行 = %+v，期望一行且带 discount_cents 与 refunding_qty", od.Items)
	}
	if len(od.Payments) != 1 || len(od.Refunds) != 1 || od.Refunds[0].AuditedBy == nil {
		t.Errorf("订单详情：payments=%d refunds=%d（第一张的审核人 %+v）", len(od.Payments), len(od.Refunds),
			func() any {
				if len(od.Refunds) == 0 {
					return nil
				}
				return od.Refunds[0].AuditedBy
			}())
	}

	// —— 一张待买家退货的单：确认收到退货之后，详情里有收货人与收货时间。
	received := permRefund(t, fx, fx.N1, 20)
	wantStatus(t, postIdem(t, fx.sh.Host, "/api/v1/admin/refunds/"+received+"/receipt", "", fx.tokens[roleStore]),
		http.StatusOK, "门店管理员确认收到退货")
	decodeInto(t, getAs(t, fx.sh.Host, "/api/v1/admin/refunds/"+received, fx.sh.Token),
		http.StatusOK, "读确认收货之后的退款单", &rd)
	if rd.ReceivedAt == nil || rd.ReceivedBy == nil || rd.ReceivedBy.Id != fx.StoreMgrID {
		t.Errorf("确认收到退货之后：received_at=%v received_by=%+v，期望收货人是门店管理员 %d",
			rd.ReceivedAt, rd.ReceivedBy, fx.StoreMgrID)
	}

	// —— 发货：详情里出现包裹，快照字段原样回来。
	shipNo := aoOrder(t, fx, fx.N1, 20, "13933330001", "", time.Time{})
	wantStatus(t, postIdem(t, fx.sh.Host, "/api/v1/admin/orders/"+shipNo+"/shipments",
		`{"carrier_code":"sf","tracking_no":"SF`+fx.next()+`"}`, fx.tokens[roleRegion]), http.StatusCreated, "大区管理员发货")
	decodeInto(t, getAs(t, fx.sh.Host, "/api/v1/admin/orders/"+shipNo, fx.tokens[roleRegion]),
		http.StatusOK, "读发过货的订单", &od)
	if od.Status != 30 || len(od.Shipments) != 1 || od.Shipments[0].CarrierCode != "sf" {
		t.Errorf("发货之后：status=%d shipments=%+v", od.Status, od.Shipments)
	}
	if od.Receiver.Phone != "13933330001" || od.Receiver.ReceiverName != "张三" || od.Store.StoreName != "华北一店" {
		t.Errorf("快照没原样回来：receiver=%+v store=%+v", od.Receiver, od.Store)
	}
}

// aoSummary 以管理员身份按单号从列表里取那一单的摘要。
func aoSummary(t *testing.T, fx *permFixture, orderNo string) api.AdminOrderSummary {
	t.Helper()
	var page aoOrderPage
	decodeInto(t, getAs(t, fx.sh.Host, "/api/v1/admin/orders?order_no="+orderNo, fx.sh.Token),
		http.StatusOK, "按单号查列表", &page)
	if len(page.Items) != 1 {
		t.Fatalf("按单号 %s 查到 %d 条", orderNo, len(page.Items))
	}
	return page.Items[0]
}

// 后台摘要的 Order 部分是从 apiOrder 逐字段搬过去的（契约 allOf 被生成器摊平了），
// 漏搬一个字段的症状是 JSON 里整个不出现。这里对一笔发过货的单逐键核对 ——
// 它该有的每一个 Order 字段都得在，外加运费（后台照实给出，买家侧缺席）。
func TestAdminOrderSummaryCarriesEveryOrderField(t *testing.T) {
	fx := newPermFixture(t)
	no := aoOrder(t, fx, fx.N1, 30, "13944440001", "", time.Time{})
	w := getAs(t, fx.sh.Host, "/api/v1/admin/orders?order_no="+no, fx.sh.Token)
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	decodeInto(t, w, http.StatusOK, "列表原样 JSON", &raw)
	if len(raw.Items) != 1 {
		t.Fatalf("按单号查到 %d 条", len(raw.Items))
	}
	wd := getAs(t, fx.sh.Host, "/api/v1/admin/orders/"+no, fx.sh.Token)
	var detail map[string]json.RawMessage
	decodeInto(t, wd, http.StatusOK, "详情原样 JSON", &detail)
	for _, k := range []string{"order_no", "store_id", "region_id", "status", "refund_status",
		"goods_amount_cents", "freight_cents", "discount_cents", "payable_cents", "paid_cents",
		"refunded_cents", "expire_at", "created_at", "paid_at", "shipped_at",
		"receiver", "store", "has_open_refund"} {
		if _, ok := raw.Items[0][k]; !ok {
			t.Errorf("后台订单列表的摘要里没有 %q", k)
		}
		if _, ok := detail[k]; !ok {
			t.Errorf("后台订单详情里没有 %q", k)
		}
	}
	for _, k := range []string{"items", "payments", "shipments", "refunds"} {
		if string(detail[k]) == "" || string(detail[k]) == "null" {
			t.Errorf("后台订单详情的 %q 是 %s，期望一个数组（空也要是 []）", k, detail[k])
		}
	}
}

// 404：没有这一单、这一单还是创建中的草稿（status 0）、以及别家店的单。
func TestAdminOrderDetailNotFound(t *testing.T) {
	fx := newPermFixture(t)
	// status 0：下单 SAGA 还没走完的草稿。直接插成 0 —— 从 10 改回 0 会被状态机触发器拒掉。
	draft := aoOrder(t, fx, fx.N1, 0, "13955550001", "", time.Time{})
	for _, no := range []string{"NO-SUCH-ORDER", draft} {
		problemType(t, getAs(t, fx.sh.Host, "/api/v1/admin/orders/"+no, fx.sh.Token), http.StatusNotFound, "订单 "+no)
	}
	if got, _ := aoOrders(t, fx, fx.sh.Token, ""); got[draft] {
		t.Errorf("创建中的草稿单 %s 出现在了后台列表里", draft)
	}
	problemType(t, getAs(t, fx.sh.Host, "/api/v1/admin/refunds/NO-SUCH-REFUND", fx.sh.Token), http.StatusNotFound, "退款单")

	other := newPermFixture(t)
	theirs := aoOrder(t, other, other.N1, 20, "13955550002", "", time.Time{})
	problemType(t, getAs(t, fx.sh.Host, "/api/v1/admin/orders/"+theirs, fx.sh.Token), http.StatusNotFound, "别家店的订单")
	if got, _ := aoOrders(t, fx, fx.sh.Token, "&order_no="+theirs); len(got) != 0 {
		t.Errorf("按别家店的单号查到了 %v", keysOf(got))
	}
}

// 平台级员工带 X-Keel-Merchant 切进一家店：读到的是那家店的单；在那里审的退款单，
// 审核记录里只有 id（平台级员工不属于这家店，租户作用域里读不到他的名字）。
func TestPlatformStaffSeesTheSwitchedShopsOrders(t *testing.T) {
	a, b := newPermFixture(t), newPermFixture(t)
	mine := aoOrder(t, a, a.N1, 20, "13966660001", "", time.Time{})
	theirs := aoOrder(t, b, b.N1, 20, "13966660002", "", time.Time{})
	refundNo := permRefund(t, b, b.N1, 10)

	email := fmt.Sprintf("ao-platform-%d@keel.test", time.Now().UnixNano())
	pid := mkStaff(t, "", email, 1, 1)
	// 给他一个名字：没有名字的话「详情里没有名字」这条断言分不出是 RLS 挡住了还是本来就空。
	adminExec(t, `UPDATE staff SET name = '平台甲' WHERE id = $1`, pid)
	t.Cleanup(func() { adminExec(t, `DELETE FROM staff WHERE id = $1`, pid) })
	// 这条清理排在 pid 那条之后注册 → 先执行：审过的退款单指着这位平台员工（audited_by），
	// 而退款单本身要等 permRefund 注册的那条（更早注册、更晚执行）才删。
	t.Cleanup(func() { adminExec(t, `UPDATE refunds SET audited_by = NULL WHERE merchant_id = $1`, b.sh.MerchantID) })
	token := staffSession(t, hostA, pid).Token

	var page aoOrderPage
	decodeInto(t, switchReq(t, nil, http.MethodGet, a.sh.Host, "/api/v1/admin/orders?page_size=100", "", token, shopCode(b.sh)),
		http.StatusOK, "平台员工切到 B 店读订单", &page)
	seen := map[string]bool{}
	for _, o := range page.Items {
		seen[o.OrderNo] = true
	}
	if !seen[theirs] || seen[mine] {
		t.Fatalf("切到 B 店读到的订单 = %v，期望含 %s、不含 A 店的 %s", keysOf(seen), theirs, mine)
	}

	wantStatus(t, switchReq(t, nil, http.MethodPost, a.sh.Host, "/api/v1/admin/refunds/"+refundNo+"/audit",
		`{"action":"reject","reject_reason":"平台代审"}`, token, shopCode(b.sh)), http.StatusOK, "平台员工切到 B 店驳回")
	var rd api.AdminRefundDetail
	decodeInto(t, getAs(t, b.sh.Host, "/api/v1/admin/refunds/"+refundNo, b.sh.Token), http.StatusOK, "B 店管理员读退款单", &rd)
	if rd.AuditedBy == nil || rd.AuditedBy.Id != pid || rd.AuditedBy.Name != nil {
		t.Errorf("平台员工代审之后审核人 = %+v，期望 id=%d 且没有名字", rd.AuditedBy, pid)
	}
}
