package handler_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 自动确认收货定时任务（数据模型 §5 发货第三条规则，00036）。
//
// 与超时补偿那一组（sweep_test.go）同一个形状：没有 HTTP，直接调 ConfirmOnce，
// 租户由它自己从 merchants 里枚举。订单走真接口下单、付款、发货，
// 「已经发货 N 天」靠把 shipped_at 往回拨出来 —— 不等 N 天，也不改被测的时钟。
//
// 核对事实一律走管理员连接绕过 RLS。

// shippedDaysAgo 把一笔已发货订单的 shipped_at 拨到 days 天前（再多一分钟，避开边界抖动）。
func shippedDaysAgo(t *testing.T, orderNo string, days int) {
	t.Helper()
	ct, err := admin(t).Exec(context.Background(),
		`UPDATE orders SET shipped_at = now() - make_interval(days => $2) - interval '1 minute'
		  WHERE order_no = $1 AND status = 30`, orderNo, days)
	if err != nil {
		t.Fatalf("拨订单 %s 的发货时间失败: %v", orderNo, err)
	}
	if ct.RowsAffected() != 1 {
		t.Fatalf("拨订单 %s 的发货时间影响了 %d 行 —— 它不在已发货", orderNo, ct.RowsAffected())
	}
}

func newConfirmer() *service.AutoConfirmService {
	return service.NewAutoConfirmService(repository.New(testPool), service.SweepConfig{}, nil)
}

// 按店铺配置的天数确认：超过的推到 40（带 finished_at），没到的不动；
// 有在途售后的暂停，售后结束后下一轮补上。
func TestAutoConfirmFollowsTheShopSettingAndPausesForOpenRefunds(t *testing.T) {
	cs := newCouponShop(t)
	adminExec(t, `UPDATE shop_settings SET auto_confirm_days = 3 WHERE merchant_id = $1`, cs.MerchantID)
	b := cs.newBuyer(t, "autoconfirm")

	shipped := func() string {
		o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
		wantStatus(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusCreated, "发货")
		return o.OrderNo
	}
	due := shipped()
	notYet := shipped()
	withRefund := shipped()
	shippedDaysAgo(t, due, 3)
	shippedDaysAgo(t, notYet, 2)
	shippedDaysAgo(t, withRefund, 3)

	// withRefund 上挂一张退货退款申请（10 待审核）。
	_, lines := cs.lines(t, b, withRefund)
	r := cs.mustApply(t, b, withRefund, refundBody(2, [2]int64{lines[cs.DressSKU].Id, 1}))

	rep, err := newConfirmer().ConfirmOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Confirmed < 1 || rep.Failed != 0 {
		t.Fatalf("这一轮的报告是 %+v，期望至少确认 1 笔、没有失败", rep)
	}

	if got := orderTimesOf(t, due); got.Status != 40 || !got.FinishedAt {
		t.Fatalf("发货满 3 天（店铺配置 3 天）的订单是 %+v，期望 40 且有 finished_at", got)
	}
	if got := orderStatusOf(t, notYet); got != 30 {
		t.Fatalf("发货才 2 天（店铺配置 3 天）的订单被推到了 %d —— 天数没按店铺配置判", got)
	}
	if got := orderStatusOf(t, withRefund); got != 30 {
		t.Fatalf("有在途售后的订单被自动确认成了 %d —— 系统替正在退货的买家点了确认收货", got)
	}

	// 售后撤回之后，下一轮就确认（它的发货时间早就过了 3 天）。
	wantStatus(t, postWithKey(t, cs.Host, "/api/v1/refunds/"+r.RefundNo+"/cancel", "", b.Token, "c-"+uniqueKey()),
		http.StatusOK, "撤回退款申请")
	if _, err := newConfirmer().ConfirmOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := orderTimesOf(t, withRefund); got.Status != 40 || !got.FinishedAt {
		t.Fatalf("售后撤回之后的订单是 %+v，期望下一轮被确认到 40", got)
	}
	if got := orderStatusOf(t, notYet); got != 30 {
		t.Fatalf("第二轮把发货才 2 天的订单推到了 %d", got)
	}

	// 已完成的订单买家再点确认收货：409，与手动确认之后再点同一个结论。
	wantStatus(t, orderAction(t, cs.Host, due, "confirm", b.Token, "cf-"+uniqueKey()),
		http.StatusConflict, "对自动确认过的订单再确认收货")
}

// 没有 shop_settings 那一行的店（开店不写它）按列默认值 7 天走，而不是不确认。
// 同时钉住 repository.DefaultAutoConfirmDays 与库里的列默认值是同一个数。
func TestAutoConfirmDefaultsToTheColumnDefaultWithoutShopSettings(t *testing.T) {
	var colDefault string
	if err := admin(t).QueryRow(context.Background(), `
		SELECT column_default FROM information_schema.columns
		 WHERE table_name = 'shop_settings' AND column_name = 'auto_confirm_days'`).Scan(&colDefault); err != nil {
		t.Fatal(err)
	}
	if colDefault != strconv.Itoa(repository.DefaultAutoConfirmDays) {
		t.Fatalf("shop_settings.auto_confirm_days 的列默认值是 %q，repository.DefaultAutoConfirmDays 是 %d —— "+
			"没配店铺设置的店与配过的店会按不同的默认天数确认", colDefault, repository.DefaultAutoConfirmDays)
	}

	cs := newCouponShop(t)
	b := cs.newBuyer(t, "autoconfirm-default")
	shipped := func() string {
		o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
		wantStatus(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusCreated, "发货")
		return o.OrderNo
	}
	due, notYet := shipped(), shipped()
	// 付款要回调密钥（在 shop_settings.extra 里），所以先下单付款发货，再把这一行删掉。
	adminExec(t, `DELETE FROM shop_settings WHERE merchant_id = $1`, cs.MerchantID)
	shippedDaysAgo(t, due, repository.DefaultAutoConfirmDays)
	shippedDaysAgo(t, notYet, repository.DefaultAutoConfirmDays-1)

	if _, err := newConfirmer().ConfirmOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := orderStatusOf(t, due); got != 40 {
		t.Fatalf("没有店铺设置、发货满 %d 天的订单是 %d，期望 40", repository.DefaultAutoConfirmDays, got)
	}
	if got := orderStatusOf(t, notYet); got != 30 {
		t.Fatalf("没有店铺设置、发货 %d 天的订单被推到了 %d", repository.DefaultAutoConfirmDays-1, got)
	}
}

// chk_auto_confirm_days（00036）：0 天意味着「一发货就自动确认」，库里写不进去。
func TestAutoConfirmDaysMustBePositive(t *testing.T) {
	cs := newCouponShop(t)
	_, err := admin(t).Exec(context.Background(),
		`UPDATE shop_settings SET auto_confirm_days = 0 WHERE merchant_id = $1`, cs.MerchantID)
	if err == nil {
		t.Fatal("auto_confirm_days = 0 写进去了 —— 那意味着发货即确认收货，买家没有售后窗口")
	}
}
