package handler_test

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/api"
)

// 经营报表（GET /admin/reports/*，契约 Report tag）的口径，逐项用构造的数据核对。
//
// ===========================================================================
// 为什么直接插库，而不是走下单 → 支付 → 退款的真实接口
// ===========================================================================
//
// 口径的每一条都落在「某个时间戳在窗口边界的哪一侧」「某个状态算不算」上：
// 00:00:00 整点付款、23:59:59 付款、跨天下单跨天付款、UTC 与店铺时区不在同一天……
// 真实接口的时间戳都是 now()，造不出这些边界。所以订单、支付、退款按口径需要的
// 形状直接写进库（与 permission_test.go 的 permPaidOrder 同一个做法），
// 读一律走真实的 HTTP 接口与真实的会话。
//
// 有几行是**约束允许、业务上不会出现**的（草稿 / 待支付 / 已关闭的单上带着 paid_at）：
// 它们存在的理由是让「状态口径」这一条可以单独被验 —— 只靠 paid_at IS NULL 把它们挡在外面
// 的话，把 status IN (...) 那一句整个删掉，全部测试照样绿。
//
// ===========================================================================
// 数据（店铺时区 Asia/Shanghai；窗口 custom 2025-03-10 ～ 2025-03-11）
// ===========================================================================
//
//	单    门店  状态       实付   支付时间（上海）        算不算
//	O1    N1    20 待发货  1000   03-10 00:00:00          算（窗口起点，含）
//	O2    N1    40 已完成  2000   03-11 23:59:59          算（窗口最后一秒）
//	O3    N1    30 已发货   500   03-12 00:00:00          不算（窗口终点，不含）
//	O4    N1    20 待发货   700   03-09 23:59:59          不算（落在上一周期）
//	O5    N1    90 已关闭   999   03-10 12:00             不算（已关闭）
//	O6    N1     0 草稿     888   03-10 12:00             不算（草稿）
//	O7    N1    10 待支付     0   —                       不算（没付钱）
//	O8    N1    60 已退款  1500   03-10 10:00             算（整单退掉的单仍然付过钱）
//	O9    E1    20 待发货  3000   03-10 09:00             算
//	O10   N2    20 待发货   400   03-10 00:05（03-09 23:50 下单）  算（按支付时间，不按下单时间）
//	O11   N1    20 待发货   600   03-12 00:30（UTC 还是 03-11）    不算（按店铺时区切天）
//
//	退款  单   金额   状态       到账时间（上海）    算不算
//	R1    O2    300   40 已退款  03-11 10:00         算（部分退款）
//	R2    O8   1500   40 已退款  03-10 11:00         算（整单退）
//	R3    O4    200   40 已退款  03-10 15:00         算（上期付款、本期到账 —— 按到账时间）
//	R4    O1    100   30 退款中  —                   不算（还没到账）
//	R5    O9    500   40 已退款  03-12 00:00:00      不算（窗口终点）
//	R6    O9    250   40 已退款  03-11 20:00         算
//
// 全店本期：支付 1000+2000+1500+3000+400 = 7900，5 单，买家 4 人（U1 U2 U4 U5）；
// 退款 300+1500+200+250 = 2250，4 笔；净销售额 5650；客单价 7900÷4 = 1975。
// 上一周期（03-08 ～ 03-09）：O4 700，1 单 1 人；没有退款。

const (
	rptStart = "2025-03-10"
	rptEnd   = "2025-03-11"
	rptWin   = "period=custom&start_date=" + rptStart + "&end_date=" + rptEnd
)

var shanghai = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic(err)
	}
	return loc
}()

// sh 是上海时间 2025-03-dd hh:mm:ss。
func sh(d, h, m, s int) time.Time { return time.Date(2025, 3, d, h, m, s, 0, shanghai) }

type reportFixture struct {
	*permFixture
	users      map[string]int64
	orders     map[string]int64
	p2         int64 // 第二件商品，挂在 childCat 下
	sku2       int64
	rootCat    int64
	childCat   int64
	userSeq    int
	paymentOf  map[int64]int64
	refundSeq  int
}

func newReportFixture(t *testing.T) *reportFixture {
	t.Helper()
	fx := &reportFixture{permFixture: newPermFixture(t), users: map[string]int64{},
		orders: map[string]int64{}, paymentOf: map[int64]int64{}}
	mid := fx.sh.MerchantID
	// 清理排在夹具自己的清理之前（t.Cleanup 后进先出）：订单、退款、店铺设置都挂着
	// 指向 merchants 的外键，不删掉的话夹具最后那句 DELETE FROM merchants 会失败。
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM refund_items WHERE merchant_id = $1`,
			`DELETE FROM refunds WHERE merchant_id = $1`,
			`DELETE FROM payments WHERE merchant_id = $1`,
			`DELETE FROM order_items WHERE merchant_id = $1`,
			`DELETE FROM orders WHERE merchant_id = $1`,
			`DELETE FROM users WHERE merchant_id = $1 AND nickname = '报表下单人'`,
			`DELETE FROM shop_settings WHERE merchant_id = $1`,
		} {
			adminExec(t, q, mid)
		}
	})

	// 第二件商品挂在一个子类目下（类目排行「含子孙」那一条要它）。
	fx.rootCat = createCategory(t, fx.sh, "报表根类目")
	var child api.AdminCategory
	decodeInto(t, postIdem(t, fx.sh.Host, "/api/v1/admin/categories",
		fmt.Sprintf(`{"name":"报表子类目 %s","parent_id":%d,"sort_order":1}`, fx.sh.Suffix, fx.rootCat), fx.sh.Token),
		http.StatusCreated, "建子类目", &child)
	fx.childCat = child.Id
	fx.p2, fx.sku2 = createPublishedSKU(t, fx.sh, fx.childCat, "报表商品二", 2200)
	return fx
}

func (fx *reportFixture) user(t *testing.T, name string) int64 {
	t.Helper()
	if id, ok := fx.users[name]; ok {
		return id
	}
	fx.userSeq++
	id := adminQueryInt64(t, `INSERT INTO users (merchant_id, phone, nickname)
	                          VALUES ($1, $2, '报表下单人') RETURNING id`,
		fx.sh.MerchantID, fmt.Sprintf("137%08d", fx.seq.Add(1)%100_000_000))
	fx.users[name] = id
	return id
}

// rptOrder 是一笔要插的订单。PaidAt 为零值即没付钱（paid_at IS NULL）。
type rptOrder struct {
	Name          string
	Store         int64
	User          string
	Status        int
	Paid          int64
	PaidAt        time.Time
	CreatedAt     time.Time
	RefundStatus  int
	RefundedCents int64
}

func (fx *reportFixture) order(t *testing.T, o rptOrder) int64 {
	t.Helper()
	payable := o.Paid
	if payable == 0 {
		payable = 1000
	}
	created := o.CreatedAt
	if created.IsZero() {
		created = o.PaidAt.Add(-time.Minute)
		if o.PaidAt.IsZero() {
			created = sh(10, 12, 0, 0)
		}
	}
	var paidAt, shippedAt, finishedAt any
	if !o.PaidAt.IsZero() {
		paidAt = o.PaidAt
		if o.Status == 30 || o.Status == 40 {
			shippedAt = o.PaidAt.Add(time.Hour)
		}
		if o.Status == 40 {
			finishedAt = o.PaidAt.Add(2 * time.Hour)
		}
	}
	id := adminQueryInt64(t, `
		INSERT INTO orders (merchant_id, order_no, user_id, status, refund_status,
		                    goods_amount_cents, payable_cents, paid_cents, refunded_cents,
		                    paid_at, shipped_at, finished_at, created_at,
		                    receiver_snapshot, expire_at, store_id, region_id, store_snapshot)
		SELECT $1, $2, $3, $4, $5, $6, $6, $7, $8, $9, $10, $11, $12,
		       '{}'::jsonb, $12::timestamptz + interval '30 minutes', st.id, st.region_id, '{}'::jsonb
		  FROM stores st WHERE st.id = $13
		RETURNING id`,
		fx.sh.MerchantID, "RPT"+o.Name+"-"+fx.next(), fx.user(t, o.User), o.Status, o.RefundStatus,
		payable, o.Paid, o.RefundedCents, paidAt, shippedAt, finishedAt, created, o.Store)
	fx.orders[o.Name] = id
	return id
}

// item 给订单加一行。refundedQty / refundedCents 是这一行截至此刻已退的（退款到账时回写的那两列）。
func (fx *reportFixture) item(t *testing.T, order string, productID, skuID int64,
	qty int, amount, discount int64, refundedQty int, refundedCents int64) {
	t.Helper()
	adminExec(t, `
		INSERT INTO order_items (merchant_id, order_id, sku_id, product_id, title_snapshot, spec_snapshot,
		                         price_cents, quantity, amount_cents, discount_cents, refunded_qty, refunded_cents)
		VALUES ($1, $2, $3, $4, '下单时的标题', '{}'::jsonb, $5, $6, $7, $8, $9, $10)`,
		fx.sh.MerchantID, fx.orders[order], skuID, productID, amount/int64(qty), qty, amount, discount,
		refundedQty, refundedCents)
}

// refund 给订单挂一张退款单。status 40 时 at 是到账时间；30 时 at 被忽略（还没到账）。
func (fx *reportFixture) refund(t *testing.T, order string, amount int64, status int, at time.Time) {
	t.Helper()
	oid := fx.orders[order]
	pid, ok := fx.paymentOf[oid]
	if !ok {
		pid = adminQueryInt64(t, `
			INSERT INTO payments (merchant_id, payment_no, order_id, channel, amount_cents, status,
			                      channel_txn_id, paid_at)
			SELECT $1, $2, o.id, 1, o.paid_cents, 1, $2, o.paid_at FROM orders o WHERE o.id = $3
			RETURNING id`, fx.sh.MerchantID, "RPTPAY"+fx.next(), oid)
		fx.paymentOf[oid] = pid
	}
	fx.refundSeq++
	no := fmt.Sprintf("RPTRF%s-%d", fx.next(), fx.refundSeq)
	var audited, crid, refundedAt any
	created := sh(10, 8, 0, 0)
	switch status {
	case 40:
		audited, crid, refundedAt = at.Add(-time.Hour), no, at
		created = at.Add(-2 * time.Hour)
	case 30:
		audited = created.Add(time.Minute)
	}
	adminExec(t, `
		INSERT INTO refunds (merchant_id, refund_no, order_id, payment_id, user_id, refund_type, reason_code,
		                     goods_amount_cents, amount_cents, status, channel, audited_at,
		                     channel_refund_id, refunded_at, created_at)
		SELECT $1, $2, o.id, $3, o.user_id, 1, 1, $4, $4, $5, 1, $6, $7, $8, $9
		  FROM orders o WHERE o.id = $10`,
		fx.sh.MerchantID, no, pid, amount, status, audited, crid, refundedAt, created, oid)
}

// seedStandard 按文件头那张表造数据。
func (fx *reportFixture) seedStandard(t *testing.T) {
	t.Helper()
	N1, N2, E1 := fx.N1, fx.N2, fx.E1
	fx.order(t, rptOrder{Name: "O1", Store: N1, User: "U1", Status: 20, Paid: 1000, PaidAt: sh(10, 0, 0, 0), RefundStatus: 1})
	fx.order(t, rptOrder{Name: "O2", Store: N1, User: "U2", Status: 40, Paid: 2000, PaidAt: sh(11, 23, 59, 59), RefundStatus: 2, RefundedCents: 300})
	fx.order(t, rptOrder{Name: "O3", Store: N1, User: "U1", Status: 30, Paid: 500, PaidAt: sh(12, 0, 0, 0)})
	fx.order(t, rptOrder{Name: "O4", Store: N1, User: "U3", Status: 20, Paid: 700, PaidAt: sh(9, 23, 59, 59), RefundStatus: 2, RefundedCents: 200})
	fx.order(t, rptOrder{Name: "O5", Store: N1, User: "U1", Status: 90, Paid: 999, PaidAt: sh(10, 12, 0, 0)})
	fx.order(t, rptOrder{Name: "O6", Store: N1, User: "U1", Status: 0, Paid: 888, PaidAt: sh(10, 12, 0, 0)})
	fx.order(t, rptOrder{Name: "O7", Store: N1, User: "U1", Status: 10})
	fx.order(t, rptOrder{Name: "O8", Store: N1, User: "U2", Status: 60, Paid: 1500, PaidAt: sh(10, 10, 0, 0), RefundStatus: 3, RefundedCents: 1500})
	fx.order(t, rptOrder{Name: "O9", Store: E1, User: "U4", Status: 20, Paid: 3000, PaidAt: sh(10, 9, 0, 0), RefundStatus: 2, RefundedCents: 750})
	fx.order(t, rptOrder{Name: "O10", Store: N2, User: "U5", Status: 20, Paid: 400, PaidAt: sh(10, 0, 5, 0), CreatedAt: sh(9, 23, 50, 0)})
	fx.order(t, rptOrder{Name: "O11", Store: N1, User: "U1", Status: 20, Paid: 600, PaidAt: time.Date(2025, 3, 11, 16, 30, 0, 0, time.UTC)})

	fx.refund(t, "O2", 300, 40, sh(11, 10, 0, 0))
	fx.refund(t, "O8", 1500, 40, sh(10, 11, 0, 0))
	fx.refund(t, "O4", 200, 40, sh(10, 15, 0, 0))
	fx.refund(t, "O1", 100, 30, time.Time{})
	fx.refund(t, "O9", 500, 40, sh(12, 0, 0, 0))
	fx.refund(t, "O9", 250, 40, sh(11, 20, 0, 0))

	P1, S1, P2, S2 := fx.ProductID, fx.SKUID, fx.p2, fx.sku2
	fx.item(t, "O1", P1, S1, 2, 1000, 0, 0, 0)
	fx.item(t, "O2", P2, S2, 1, 2200, 200, 0, 300)
	fx.item(t, "O8", P1, S1, 3, 1500, 0, 3, 1500)
	fx.item(t, "O9", P2, S2, 1, 3000, 0, 0, 0)
	fx.item(t, "O10", P1, S1, 1, 400, 0, 0, 0)
	fx.item(t, "O5", P2, S2, 10, 999, 0, 0, 0) // 已关闭：不进排行
	fx.item(t, "O3", P1, S1, 5, 500, 0, 0, 0)  // 窗口外：不进排行
}

func (fx *reportFixture) get(t *testing.T, role permRole, path string, v any) {
	t.Helper()
	decodeInto(t, getAs(t, fx.sh.Host, "/api/v1/admin/reports/"+path, fx.tokens[role]),
		http.StatusOK, "GET "+path+"（"+role.String()+"）", v)
}

func wantMetrics(t *testing.T, what string, got api.ReportMetrics, paid, refund, orders, buyers, refunds int64) {
	t.Helper()
	if got.PaidAmountCents != paid || got.RefundAmountCents != refund || got.OrderCount != orders ||
		got.BuyerCount != buyers || got.RefundCount != refunds {
		t.Errorf("%s：支付 %d 退款 %d 订单 %d 买家 %d 退款笔数 %d；想要 %d / %d / %d / %d / %d",
			what, got.PaidAmountCents, got.RefundAmountCents, got.OrderCount, got.BuyerCount, got.RefundCount,
			paid, refund, orders, buyers, refunds)
	}
	if got.NetSalesCents != paid-refund {
		t.Errorf("%s：净销售额 %d，想要 %d − %d", what, got.NetSalesCents, paid, refund)
	}
}

// ---------------------------------------------------------------------------
// 概览：口径、上一周期、时区边界
// ---------------------------------------------------------------------------

func TestReportOverviewMetricDefinitions(t *testing.T) {
	fx := newReportFixture(t)
	fx.seedStandard(t)

	var o api.ReportOverview
	fx.get(t, roleAdmin, "overview?"+rptWin, &o)
	wantMetrics(t, "全店本期", o.Current, 7900, 2250, 5, 4, 4)
	if o.Current.AvgOrderValueCents != 1975 {
		t.Errorf("客单价 = %d，想要 7900 ÷ 4 = 1975", o.Current.AvgOrderValueCents)
	}
	if o.Current.RefundRate == nil || math.Abs(*o.Current.RefundRate-2250.0/7900.0) > 1e-9 {
		t.Errorf("退款率 = %v，想要 2250 ÷ 7900", o.Current.RefundRate)
	}
	wantMetrics(t, "全店上一周期（03-08 ～ 03-09）", o.Previous, 700, 0, 1, 1, 0)

	// 窗口回显：半开区间、UTC、店铺时区里的起止日期。
	if o.Window.Timezone != "Asia/Shanghai" {
		t.Errorf("没有 shop_settings 的店应当按 Asia/Shanghai，回显的是 %q", o.Window.Timezone)
	}
	if !o.Window.Current.StartAt.Equal(sh(10, 0, 0, 0)) || !o.Window.Current.EndAt.Equal(sh(12, 0, 0, 0)) {
		t.Errorf("本期窗口 = [%s, %s)，想要上海 03-10 0 点到 03-12 0 点", o.Window.Current.StartAt, o.Window.Current.EndAt)
	}
	if o.Window.Current.EndDate.String() != rptEnd || o.Window.Previous.StartDate.String() != "2025-03-08" {
		t.Errorf("日期回显 = 本期止 %s、上期起 %s", o.Window.Current.EndDate, o.Window.Previous.StartDate)
	}

	// 上一周期没有支付：退款率是 null 而不是 0（JSON 层面核一次）。
	w := getAs(t, fx.sh.Host, "/api/v1/admin/reports/overview?period=custom&start_date=2025-03-01&end_date=2025-03-01", fx.sh.Token)
	wantStatus(t, w, http.StatusOK, "没有数据的一天")
	if !strings.Contains(w.Body.String(), `"refund_rate":null`) {
		t.Errorf("没有支付的窗口，refund_rate 应当是 null：%s", w.Body.String())
	}
}

func TestReportTrendAddsUpToTheOverview(t *testing.T) {
	fx := newReportFixture(t)
	fx.seedStandard(t)

	var tr api.ReportTrend
	fx.get(t, roleAdmin, "trend?"+rptWin, &tr)
	if tr.Granularity != "day" || len(tr.Points) != 2 {
		t.Fatalf("两天的窗口应当按天 2 个点，得到 %s × %d", tr.Granularity, len(tr.Points))
	}
	d10, d11 := tr.Points[0], tr.Points[1]
	if d10.Label != "03-10" || d10.PaidAmountCents != 5900 || d10.OrderCount != 4 || d10.RefundAmountCents != 1700 {
		t.Errorf("03-10 = %+v，想要支付 5900（O1 O8 O9 O10）4 单、退款 1700（R2 R3）", d10)
	}
	if d11.Label != "03-11" || d11.PaidAmountCents != 2000 || d11.OrderCount != 1 || d11.RefundAmountCents != 550 {
		t.Errorf("03-11 = %+v，想要支付 2000（O2）1 单、退款 550（R1 R6）", d11)
	}
	if d10.NetSalesCents+d11.NetSalesCents != 5650 {
		t.Errorf("趋势的净销售额加起来 %d，概览是 5650 —— 两处口径分叉了", d10.NetSalesCents+d11.NetSalesCents)
	}

	// 单日窗口按小时：0 点那一桶有 O1（00:00:00）与 O10（00:05），24 个桶一个不缺。
	fx.get(t, roleAdmin, "trend?period=custom&start_date=2025-03-10&end_date=2025-03-10", &tr)
	if tr.Granularity != "hour" || len(tr.Points) != 24 {
		t.Fatalf("单日窗口应当按小时 24 个点，得到 %s × %d", tr.Granularity, len(tr.Points))
	}
	if p := tr.Points[0]; p.Label != "00:00" || p.PaidAmountCents != 1400 || p.OrderCount != 2 {
		t.Errorf("00 点这一桶 = %+v，想要 O1 + O10 = 1400、2 单", p)
	}
	if p := tr.Points[9]; p.PaidAmountCents != 3000 {
		t.Errorf("09 点这一桶 = %+v，想要 O9 3000", p)
	}
	if p := tr.Points[15]; p.RefundAmountCents != 200 || p.PaidAmountCents != 0 {
		t.Errorf("15 点这一桶 = %+v，想要只有 R3 的 200 退款", p)
	}
	if p := tr.Points[23]; p.Label != "23:00" || p.PaidAmountCents != 0 {
		t.Errorf("23 点这一桶 = %+v，想要空桶（补 0，不缺席）", p)
	}
}

// 店铺时区：同一批数据，换了时区之后「哪一天」跟着变。
func TestReportsCutDaysInTheShopTimezone(t *testing.T) {
	fx := newReportFixture(t)
	// A：UTC 03-10 03:30 = 上海 03-10 11:30 = 纽约 03-09 23:30
	// B：UTC 03-11 23:30 = 上海 03-12 07:30 = 纽约 03-11 19:30
	fx.order(t, rptOrder{Name: "A", Store: fx.N1, User: "U1", Status: 20, Paid: 111,
		PaidAt: time.Date(2025, 3, 10, 3, 30, 0, 0, time.UTC)})
	fx.order(t, rptOrder{Name: "B", Store: fx.N1, User: "U1", Status: 20, Paid: 222,
		PaidAt: time.Date(2025, 3, 11, 23, 30, 0, 0, time.UTC)})

	var o api.ReportOverview
	fx.get(t, roleAdmin, "overview?"+rptWin, &o)
	if o.Current.PaidAmountCents != 111 {
		t.Errorf("上海时区：03-10 ～ 03-11 应当只有 A（111），得到 %d", o.Current.PaidAmountCents)
	}

	adminExec(t, `INSERT INTO shop_settings (merchant_id, timezone) VALUES ($1, 'America/New_York')`, fx.sh.MerchantID)
	fx.get(t, roleAdmin, "overview?"+rptWin, &o)
	if o.Window.Timezone != "America/New_York" {
		t.Fatalf("配了店铺时区之后应当按它算，回显 %q", o.Window.Timezone)
	}
	if o.Current.PaidAmountCents != 222 {
		t.Errorf("纽约时区：03-10 ～ 03-11 应当只有 B（222），得到 %d", o.Current.PaidAmountCents)
	}
	// 纽约 2025-03-10 0 点是 UTC 04:00（03-09 刚切到夏令时，UTC−4）。
	if want := time.Date(2025, 3, 10, 4, 0, 0, 0, time.UTC); !o.Window.Current.StartAt.Equal(want) {
		t.Errorf("纽约的窗口起点 = %s，想要 %s", o.Window.Current.StartAt, want)
	}

	// 写坏的时区名回落到 Asia/Shanghai，回显的也是它（界面上写的就是真正参与计算的）。
	adminExec(t, `UPDATE shop_settings SET timezone = 'Mars/Olympus' WHERE merchant_id = $1`, fx.sh.MerchantID)
	fx.get(t, roleAdmin, "overview?"+rptWin, &o)
	if o.Window.Timezone != "Asia/Shanghai" || o.Current.PaidAmountCents != 111 {
		t.Errorf("非法时区应当回落到 Asia/Shanghai：回显 %q、支付 %d", o.Window.Timezone, o.Current.PaidAmountCents)
	}

	// today：此刻付的一单算进今天，也算进趋势的当前这一小时。
	adminExec(t, `UPDATE shop_settings SET timezone = 'Asia/Shanghai' WHERE merchant_id = $1`, fx.sh.MerchantID)
	fx.order(t, rptOrder{Name: "NOW", Store: fx.N1, User: "U2", Status: 20, Paid: 333, PaidAt: time.Now()})
	fx.get(t, roleAdmin, "overview?period=today", &o)
	if o.Current.PaidAmountCents != 333 || o.Window.Period != "today" {
		t.Errorf("今天应当有刚付的 333，得到 %d（period %s）", o.Current.PaidAmountCents, o.Window.Period)
	}
	var tr api.ReportTrend
	fx.get(t, roleAdmin, "trend", &tr)
	hour := time.Now().In(shanghai).Hour()
	if tr.Granularity != "hour" || len(tr.Points) != hour+1 || tr.Points[hour].PaidAmountCents != 333 {
		t.Errorf("今天的趋势应当按小时出到当前这一小时（%d 桶），最后一桶 333：得到 %s × %d",
			hour+1, tr.Granularity, len(tr.Points))
	}
}

// ---------------------------------------------------------------------------
// 范围、筛选、租户隔离
// ---------------------------------------------------------------------------

func TestReportsAreScopedLikeTheOrderList(t *testing.T) {
	fx := newReportFixture(t)
	fx.seedStandard(t)

	cases := []struct {
		role                           permRole
		query                          string
		paid, refund, orders, buyers, n int64
	}{
		{roleOperator, "", 7900, 2250, 5, 4, 4},
		// 华北（N1 + N2）：O1 O2 O8 O10；退款 R1 R2 R3（R6 在华东）。
		{roleRegion, "", 4900, 2000, 4, 3, 3},
		// N1：O1 O2 O8；退款 R1 R2 R3。
		{roleStore, "", 4500, 2000, 3, 2, 3},
		// 筛选与范围取交集：管理员只看 E1；大区管理员带华东的门店拿到全零（不是 403）。
		{roleAdmin, fmt.Sprintf("&store_id=%d", fx.E1), 3000, 250, 1, 1, 1},
		{roleRegion, fmt.Sprintf("&store_id=%d", fx.E1), 0, 0, 0, 0, 0},
		{roleAdmin, fmt.Sprintf("&region_id=%d", fx.North), 4900, 2000, 4, 3, 3},
		{roleStore, fmt.Sprintf("&region_id=%d", fx.North), 4500, 2000, 3, 2, 3},
		{roleStore, fmt.Sprintf("&store_id=%d", fx.N2), 0, 0, 0, 0, 0},
	}
	for _, c := range cases {
		var o api.ReportOverview
		fx.get(t, c.role, "overview?"+rptWin+c.query, &o)
		wantMetrics(t, c.role.String()+c.query, o.Current, c.paid, c.refund, c.orders, c.buyers, c.n)

		var tr api.ReportTrend
		fx.get(t, c.role, "trend?"+rptWin+c.query, &tr)
		var sum int64
		for _, p := range tr.Points {
			sum += p.NetSalesCents
		}
		if sum != c.paid-c.refund {
			t.Errorf("%s%s：趋势合计 %d，概览 %d —— 两处的范围过滤分叉了", c.role, c.query, sum, c.paid-c.refund)
		}
	}

	// 门店对比：门店管理员只看得到 N1，大区管理员只看得到华北两家。
	var sc api.ReportStoreComparison
	fx.get(t, roleStore, "stores?"+rptWin, &sc)
	if len(sc.Stores) != 1 || sc.Stores[0].StoreId != fx.N1 {
		t.Errorf("门店管理员的门店对比应当只有 N1：%+v", sc.Stores)
	}
	fx.get(t, roleRegion, "stores?"+rptWin, &sc)
	if len(sc.Stores) != 2 || len(sc.Regions) != 1 || sc.Regions[0].RegionId != fx.North {
		t.Errorf("大区管理员的门店对比应当只有华北两家：%+v / %+v", sc.Stores, sc.Regions)
	}

	// 商品排行：门店管理员只算 N1 的单（O1 O2 O8）。
	var pr api.ReportProductRanking
	fx.get(t, roleStore, "products?"+rptWin, &pr)
	if len(pr.Items) != 2 || pr.Items[0].ProductId != fx.ProductID || pr.Items[0].Quantity != 5 ||
		pr.Items[0].AmountCents != 2500 || pr.Items[1].AmountCents != 2000 {
		t.Errorf("门店管理员的商品排行应当是 P1（5 件 2500）> P2（2000）：%+v", pr.Items)
	}
}

func TestReportsAreTenantIsolated(t *testing.T) {
	fx := newReportFixture(t)
	fx.seedStandard(t)

	// 另一家店在同一个窗口里有一笔大单、一笔退款、几条检索日志。
	other := newReportFixture(t)
	other.order(t, rptOrder{Name: "X", Store: other.N1, User: "U1", Status: 20, Paid: 99999, PaidAt: sh(10, 12, 0, 0)})
	other.refund(t, "X", 11111, 40, sh(10, 13, 0, 0))
	adminExec(t, `INSERT INTO search_logs (merchant_id, query, ranked_ids, trace_id, strategy, stages, created_at)
	              VALUES ($1, '别家的词', '{}', $2, 'hybrid', '{keyword}', $3)`,
		other.sh.MerchantID, "rpt-other-"+other.next(), sh(10, 12, 0, 0))

	var o api.ReportOverview
	fx.get(t, roleAdmin, "overview?"+rptWin, &o)
	wantMetrics(t, "本店（另一家店的数据不许混进来）", o.Current, 7900, 2250, 5, 4, 4)
	other.get(t, roleAdmin, "overview?"+rptWin, &o)
	wantMetrics(t, "另一家店", o.Current, 99999, 11111, 1, 1, 1)

	var sc api.ReportStoreComparison
	fx.get(t, roleAdmin, "stores?"+rptWin, &sc)
	for _, s := range sc.Stores {
		if s.StoreId == other.N1 {
			t.Errorf("门店对比里出现了别家店的门店 %d", s.StoreId)
		}
	}
	var so api.ReportSearchOverview
	fx.get(t, roleAdmin, "search?"+rptWin, &so)
	for _, q := range so.TopQueries {
		if q.Query == "别家的词" {
			t.Errorf("搜索概况里出现了别家店的搜索词")
		}
	}
}

// ---------------------------------------------------------------------------
// 商品排行、门店对比、库存预警、搜索概况
// ---------------------------------------------------------------------------

func TestReportProductRanking(t *testing.T) {
	fx := newReportFixture(t)
	fx.seedStandard(t)

	// P1：O1 2 件 1000 + O8 3 件 1500 + O10 1 件 400 = 6 件 2900，3 单，已退 3 件 1500。
	// P2：O2 1 件（2200 − 券 200 = 2000）+ O9 1 件 3000 = 2 件 5000，2 单，已退 300。
	// O5（已关闭）与 O3（窗口外）不算。
	var pr api.ReportProductRanking
	fx.get(t, roleAdmin, "products?"+rptWin, &pr)
	if pr.SortBy != "amount" || len(pr.Items) != 2 {
		t.Fatalf("默认按销售额，两件商品：%+v", pr)
	}
	p2, p1 := pr.Items[0], pr.Items[1]
	if p2.ProductId != fx.p2 || p2.Rank != 1 || p2.AmountCents != 5000 || p2.Quantity != 2 ||
		p2.OrderCount != 2 || p2.RefundedAmountCents != 300 || p2.RefundedQuantity != 0 {
		t.Errorf("第 1 名应当是 P2（5000，2 件，2 单，已退 300）：%+v", p2)
	}
	if p1.ProductId != fx.ProductID || p1.AmountCents != 2900 || p1.Quantity != 6 || p1.OrderCount != 3 ||
		p1.RefundedQuantity != 3 || p1.RefundedAmountCents != 1500 {
		t.Errorf("第 2 名应当是 P1（2900，6 件，3 单，已退 3 件 1500）：%+v", p1)
	}
	if !strings.Contains(p1.Title, "权限矩阵商品") {
		t.Errorf("标题应当取商品当前的标题，而不是订单行上的快照：%q", p1.Title)
	}

	fx.get(t, roleAdmin, "products?sort_by=quantity&"+rptWin, &pr)
	if pr.SortBy != "quantity" || pr.Items[0].ProductId != fx.ProductID {
		t.Errorf("按销量排应当是 P1 在前：%+v", pr.Items)
	}
	fx.get(t, roleAdmin, "products?limit=1&"+rptWin, &pr)
	if len(pr.Items) != 1 {
		t.Errorf("limit=1 应当只有 1 条，得到 %d", len(pr.Items))
	}

	// 类目含子孙：根类目下只有 P2（它挂在子类目上）。
	for _, c := range []struct {
		cat  int64
		want int64
	}{{fx.rootCat, fx.p2}, {fx.childCat, fx.p2}, {fx.CategoryID, fx.ProductID}} {
		fx.get(t, roleAdmin, fmt.Sprintf("products?category_id=%d&%s", c.cat, rptWin), &pr)
		if len(pr.Items) != 1 || pr.Items[0].ProductId != c.want {
			t.Errorf("类目 %d 应当只有商品 %d：%+v", c.cat, c.want, pr.Items)
		}
	}
}

func TestReportStoreComparison(t *testing.T) {
	fx := newReportFixture(t)
	fx.seedStandard(t)
	// 一家有成交之后被删掉的店（要出现，deleted = true），一家没成交就被删掉的店（不出现）。
	gone := fx.freshStore(t, fx.North)
	fx.order(t, rptOrder{Name: "G", Store: gone, User: "U6", Status: 20, Paid: 100, PaidAt: sh(11, 8, 0, 0)})
	quiet := fx.freshStore(t, fx.North)
	adminExec(t, `UPDATE stores SET deleted_at = now() WHERE id = ANY($1)`, []int64{gone, quiet})

	var sc api.ReportStoreComparison
	fx.get(t, roleAdmin, "stores?"+rptWin, &sc)
	byID := map[int64]api.ReportStoreRow{}
	var net int64
	for _, s := range sc.Stores {
		byID[s.StoreId] = s
		net += s.NetSalesCents
	}
	if _, ok := byID[quiet]; ok {
		t.Errorf("没有成交的已删除门店不该出现")
	}
	if g, ok := byID[gone]; !ok || !g.Deleted || g.PaidAmountCents != 100 {
		t.Errorf("有成交的已删除门店应当出现并标 deleted：%+v", g)
	}
	if s0, ok := byID[fx.sh.StoreID]; !ok || s0.PaidAmountCents != 0 || s0.Deleted {
		t.Errorf("没有成交的门店应当以 0 出现：%+v", s0)
	}
	n1 := byID[fx.N1]
	if n1.PaidAmountCents != 4500 || n1.RefundAmountCents != 2000 || n1.OrderCount != 3 || n1.NetSalesCents != 2500 {
		t.Errorf("N1 = %+v，想要支付 4500 退款 2000 3 单", n1)
	}
	if e1 := byID[fx.E1]; e1.NetSalesCents != 2750 || e1.RegionId != fx.East {
		t.Errorf("E1 = %+v，想要净 2750、华东", e1)
	}
	if net != 5650+100 {
		t.Errorf("门店净销售额合计 %d，想要概览的 5650 加上被删那家的 100", net)
	}
	if sc.Stores[0].StoreId != fx.E1 || sc.Stores[1].StoreId != fx.N1 {
		t.Errorf("门店应当按净销售额倒序（E1 2750 > N1 2500）：%d, %d", sc.Stores[0].StoreId, sc.Stores[1].StoreId)
	}
	// 大区：华北 = N1 2500 + N2 400 + 被删那家 100 = 3000，排在华东 2750 之前。
	if len(sc.Regions) != 3 || sc.Regions[0].RegionId != fx.North || sc.Regions[0].NetSalesCents != 3000 ||
		sc.Regions[0].StoreCount != 3 || sc.Regions[1].RegionId != fx.East {
		t.Errorf("大区合计 = %+v", sc.Regions)
	}
	fx.get(t, roleAdmin, fmt.Sprintf("stores?region_id=%d&%s", fx.East, rptWin), &sc)
	if len(sc.Stores) != 1 || sc.Stores[0].StoreId != fx.E1 {
		t.Errorf("region_id=华东 应当只有 E1：%+v", sc.Stores)
	}
}

func TestReportInventoryAlerts(t *testing.T) {
	fx := newReportFixture(t)
	s1, s2 := fx.SKUID, fx.sku2
	inv := func(store, sku int64, avail, warn int) {
		adminExec(t, `INSERT INTO inventories (merchant_id, sku_id, store_id, available_qty, warning_qty)
		              VALUES ($1, $2, $3, $4, $5)
		              ON CONFLICT (sku_id, store_id) DO UPDATE SET available_qty = EXCLUDED.available_qty,
		                                                          warning_qty = EXCLUDED.warning_qty`,
			fx.sh.MerchantID, sku, store, avail, warn)
	}
	inv(fx.N1, s1, 3, 5)  // 低于预警线 2
	inv(fx.N1, s2, 0, 0)  // 卖空，预警线 0 也算
	inv(fx.E1, s1, 1, 10) // 低于预警线 9，最缺
	inv(fx.N2, s1, 10, 5) // 正常
	inv(fx.N2, s2, 5, 5)  // 刚好等于预警线
	gone := fx.freshStore(t, fx.North)
	inv(gone, s1, 0, 9) // 门店已删：不算
	adminExec(t, `UPDATE stores SET deleted_at = now() WHERE id = $1`, gone)

	var a api.ReportInventoryAlerts
	fx.get(t, roleAdmin, "inventory-alerts", &a)
	type k struct{ store, sku int64 }
	got := []k{}
	for _, it := range a.Items {
		got = append(got, k{it.StoreId, it.SkuId})
	}
	want := []k{{fx.E1, s1}, {fx.N1, s1}, {fx.N1, s2}, {fx.N2, s2}}
	if a.Total != 4 || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("全店预警 = %d 条 %v，想要 4 条 %v（按低于预警线多少排）", a.Total, got, want)
	}
	if len(a.Items) > 0 && (a.Items[0].WarningQty != 10 || a.Items[0].AvailableQty != 1 || a.Items[0].SkuCode == "") {
		t.Errorf("第一条的字段 = %+v", a.Items[0])
	}

	for _, c := range []struct {
		role  permRole
		query string
		total int64
	}{
		{roleRegion, "", 3},
		{roleStore, "", 2},
		{roleAdmin, fmt.Sprintf("?region_id=%d", fx.East), 1},
		{roleStore, fmt.Sprintf("?store_id=%d", fx.E1), 0},
	} {
		fx.get(t, c.role, "inventory-alerts"+c.query, &a)
		if a.Total != c.total || int64(len(a.Items)) != c.total {
			t.Errorf("%s%s：预警 %d 条（items %d），想要 %d", c.role, c.query, a.Total, len(a.Items), c.total)
		}
	}
	fx.get(t, roleAdmin, "inventory-alerts?limit=1", &a)
	if a.Total != 4 || len(a.Items) != 1 {
		t.Errorf("limit=1：total 应当仍是 4、items 1 条，得到 %d / %d", a.Total, len(a.Items))
	}
}

func TestReportSearchOverview(t *testing.T) {
	fx := newReportFixture(t)
	log := func(q string, ranked string, clicked any, at time.Time) {
		adminExec(t, `INSERT INTO search_logs (merchant_id, query, ranked_ids, clicked_id, trace_id,
		                                       strategy, stages, created_at)
		              VALUES ($1, $2, $3::bigint[], $4, $5, 'hybrid', '{keyword}', $6)`,
			fx.sh.MerchantID, q, ranked, clicked, "rpt-"+fx.next(), at)
	}
	log("Nike ", "{1}", nil, sh(10, 9, 0, 0))
	log("nike", "{}", nil, sh(10, 10, 0, 0))
	log("NIKE", "{1,2}", int64(1), sh(11, 9, 0, 0))
	log("袜子", "{}", nil, sh(10, 0, 0, 0))
	log("袜子", "{}", nil, sh(11, 23, 59, 59))
	log("帽子", "{3}", nil, sh(11, 12, 0, 0))
	log("袜子", "{}", nil, sh(12, 0, 0, 0)) // 窗口终点，不算

	var so api.ReportSearchOverview
	fx.get(t, roleAdmin, "search?"+rptWin, &so)
	if so.SearchCount != 6 || so.ZeroResultCount != 3 || so.ClickCount != 1 {
		t.Errorf("搜索 %d 次、无结果 %d、有点击 %d；想要 6 / 3 / 1", so.SearchCount, so.ZeroResultCount, so.ClickCount)
	}
	if so.ZeroResultRate == nil || *so.ZeroResultRate != 0.5 {
		t.Errorf("无结果率 = %v，想要 0.5", so.ZeroResultRate)
	}
	terms := func(ts []api.ReportSearchTerm) string {
		parts := []string{}
		for _, x := range ts {
			parts = append(parts, fmt.Sprintf("%s:%d/%d", x.Query, x.SearchCount, x.ZeroResultCount))
		}
		return strings.Join(parts, " ")
	}
	if got := terms(so.TopQueries); got != "nike:3/1 袜子:2/2 帽子:1/0" {
		t.Errorf("热门搜索词 = %s，想要 nike:3/1 袜子:2/2 帽子:1/0（去空白、转小写后归并）", got)
	}
	if got := terms(so.ZeroResultQueries); got != "袜子:2/2 nike:3/1" {
		t.Errorf("无结果搜索词 = %s，想要按无结果次数排：袜子:2/2 nike:3/1", got)
	}

	fx.get(t, roleAdmin, "search?period=custom&start_date=2025-01-01&end_date=2025-01-01", &so)
	if so.SearchCount != 0 || so.ZeroResultRate != nil || len(so.TopQueries) != 0 {
		t.Errorf("没有搜索的窗口：次数 0、无结果率 null、词表为空，得到 %+v", so)
	}
}

// ---------------------------------------------------------------------------
// 窗口参数的 422
// ---------------------------------------------------------------------------

func TestReportRejectsBadWindows(t *testing.T) {
	fx := newReportFixture(t)
	for _, q := range []string{
		"period=forever",
		"period=custom",
		"period=custom&start_date=2025-03-10",
		"period=custom&start_date=2025-03-11&end_date=2025-03-10",
		"period=custom&start_date=2025-3-1&end_date=2025-03-10",
		"period=custom&start_date=2024-01-01&end_date=2025-01-01", // 367 天
	} {
		for _, path := range []string{"overview", "trend", "products", "stores", "search"} {
			w := getAs(t, fx.sh.Host, "/api/v1/admin/reports/"+path+"?"+q, fx.sh.Token)
			if typ := problemType(t, w, http.StatusUnprocessableEntity, path+"?"+q); !strings.HasSuffix(typ, "/invalid-request") {
				t.Errorf("%s?%s：type = %s", path, q, typ)
			}
		}
	}
	// 366 天刚好放行（2024 是闰年）。
	wantStatus(t, getAs(t, fx.sh.Host, "/api/v1/admin/reports/overview?period=custom&start_date=2024-01-01&end_date=2024-12-31", fx.sh.Token),
		http.StatusOK, "366 天")
	// 没有会话：401。
	wantStatus(t, getAs(t, fx.sh.Host, "/api/v1/admin/reports/overview", ""), http.StatusUnauthorized, "不带会话")
}
