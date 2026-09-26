package handler_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// 退货超时未寄回自动关闭（service/return_timeout.go，00059 / 00060）。

// approvedReturn 下单、发货、申请退货退款一件、审核通过（停在 20），把审核时间拨到 auditedAt。
func approvedReturn(t *testing.T, cs couponShop, b couponBuyer, auditedAt time.Time) (orderNo string, r api.Refund) {
	t.Helper()
	o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	wantStatus(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusCreated, "发货")
	_, lines := cs.lines(t, b, o.OrderNo)
	r = cs.mustApply(t, b, o.OrderNo, refundBody(2, [2]int64{lines[cs.DressSKU].Id, 1}))
	if got := cs.mustApprove(t, r.RefundNo); got.Status != 20 {
		t.Fatalf("退货退款审核通过应到 20，实得 %d", got.Status)
	}
	adminExec(t, `UPDATE refunds SET audited_at = $2 WHERE refund_no = $1`, r.RefundNo, auditedAt)
	return o.OrderNo, r
}

func buyerRefund(t *testing.T, cs couponShop, b couponBuyer, refundNo string) api.Refund {
	t.Helper()
	var out api.Refund
	decodeInto(t, getAuth(t, cs.Host, "/api/v1/refunds/"+refundNo, b.Token), http.StatusOK, "售后详情", &out)
	return out
}

// 口径：从审核时间起超过 N 天（店铺设置）、没填寄回物流的关到 60；恰好 N 天不关（严格小于）；
// 填过物流的不管多久都不关。关掉之后在途件数释放、买家收到一条通知，再跑一轮不重复。
func TestReturnTimeoutClosesOverdueReturnsOnly(t *testing.T) {
	cs := newCouponShop(t)
	setShopPreference(t, cs.MerchantID, "return_ship_days", 3)
	b := cs.newBuyer(t, "return-timeout")
	now := time.Now().UTC().Truncate(time.Second)
	days := 3 * 24 * time.Hour

	overdueOrder, overdue := approvedReturn(t, cs, b, now.Add(-days-time.Second)) // 过了一秒
	_, boundary := approvedReturn(t, cs, b, now.Add(-days))                       // 恰好 N 天
	_, shipped := approvedReturn(t, cs, b, now.Add(-10*24*time.Hour))             // 早就过了，但填了物流
	wantStatus(t, returnShipment(t, cs, shipped.RefundNo, b.Token, `{"carrier_code":"yto","tracking_no":"YT-late"}`,
		"rs-"+uniqueKey()), http.StatusOK, "填寄回物流")

	// 寄回截止时间：等寄回的单 = 审核时间 + N 天；填过物流的没有。
	if got := buyerRefund(t, cs, b, overdue.RefundNo); got.ReturnDeadlineAt == nil ||
		!got.ReturnDeadlineAt.Equal(now.Add(-time.Second)) {
		t.Fatalf("return_deadline_at = %v，期望 %s（审核时间 + 3 天）", got.ReturnDeadlineAt, now.Add(-time.Second))
	}
	if got := buyerRefund(t, cs, b, shipped.RefundNo); got.ReturnDeadlineAt != nil {
		t.Fatalf("填过寄回物流的单不该有 return_deadline_at：%v", got.ReturnDeadlineAt)
	}

	svc := service.NewReturnTimeoutService(repository.New(testPool), service.SweepConfig{}, nil).
		WithClock(func() time.Time { return now })
	rep, err := svc.ExpireOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Expired < 1 || rep.Failed != 0 {
		t.Fatalf("这一轮的报告是 %+v，期望至少关掉 1 张、没有失败", rep)
	}
	if st := refundStatusOf(t, overdue.RefundNo); st != 60 {
		t.Fatalf("审核后超过 3 天没寄回的售后单是 %d，期望 60", st)
	}
	if st := refundStatusOf(t, boundary.RefundNo); st != 20 {
		t.Fatalf("审核后恰好 3 天的售后单被关成了 %d —— 口径是「超过」N 天", st)
	}
	if st := refundStatusOf(t, shipped.RefundNo); st != 20 {
		t.Fatalf("填过寄回物流的售后单被关成了 %d —— 货已经在路上", st)
	}

	// 在途件数释放：这一行又能申请售后了。
	d, lines := cs.lines(t, b, overdueOrder)
	if it := lines[cs.DressSKU]; it.RefundingQty == nil || *it.RefundingQty != 0 {
		t.Fatalf("关掉之后这一行的在途件数是 %v，期望 0（订单详情 %+v）", it.RefundingQty, d.Status)
	}
	// 买家收到一条，而且只有一条；再跑一轮不重复、不再动任何单。
	if n := countNotif(myNotifications(t, cs.Host, b.Token, "").Items, "refund_return_expired", overdue.RefundNo); n != 1 {
		t.Fatalf("买家收到 %d 条 refund_return_expired，期望 1", n)
	}
	if _, err := svc.ExpireOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countNotif(myNotifications(t, cs.Host, b.Token, "").Items, "refund_return_expired", overdue.RefundNo); n != 1 {
		t.Fatalf("第二轮之后买家有 %d 条 refund_return_expired，期望仍是 1", n)
	}
	if st := refundStatusOf(t, boundary.RefundNo); st != 20 {
		t.Fatalf("第二轮（时钟没动）把恰好 3 天的单关成了 %d", st)
	}
	cs.mustApply(t, b, overdueOrder, refundBody(2, [2]int64{lines[cs.DressSKU].Id, 1}))

	// 天数按每一轮当时的店铺设置算：改成 2 天之后，恰好 3 天的那张下一轮就关。
	setShopPreference(t, cs.MerchantID, "return_ship_days", 2)
	if _, err := svc.ExpireOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := refundStatusOf(t, boundary.RefundNo); st != 60 {
		t.Fatalf("改成 2 天之后审核已 3 天的单仍是 %d，期望 60", st)
	}
}

// 扫描与买家填物流赛跑：扫到之后、关单之前买家填了物流，关单那条 UPDATE 必须影响 0 行。
// 预筛只是预筛，裁判是 ExpireReturnRefund 的谓词（「没填寄回物流」在锁之下再判一次）。
func TestReturnTimeoutLosesTheRaceToAReturnShipment(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "return-race")
	now := time.Now().UTC()
	_, r := approvedReturn(t, cs, b, now.Add(-30*24*time.Hour))
	cutoff := now.Add(-time.Duration(repository.DefaultReturnShipDays) * 24 * time.Hour)
	repo := repository.New(testPool)
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)

	var due []repository.ReturnOverdueRefund
	if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		due, err = tx.ListReturnOverdueRefunds(ctx, cutoff, 100)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var target *repository.ReturnOverdueRefund
	for i := range due {
		if due[i].RefundNo == r.RefundNo {
			target = &due[i]
		}
	}
	if target == nil {
		t.Fatalf("审核 30 天没寄回的单没被扫到：%+v", due)
	}

	// 扫描之后，买家填了物流。
	wantStatus(t, returnShipment(t, cs, r.RefundNo, b.Token, `{"carrier_code":"sf","tracking_no":"SF-race"}`,
		"rs-"+uniqueKey()), http.StatusOK, "填寄回物流")

	var ok bool
	if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		ok, err = tx.ExpireReturnRefund(ctx, target.ID, cutoff)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("买家已经填了寄回物流，关单那条 UPDATE 仍然生效了 —— 货在路上，单被关了")
	}
	if st := refundStatusOf(t, r.RefundNo); st != 20 {
		t.Fatalf("售后单是 %d，期望仍是 20", st)
	}
}
