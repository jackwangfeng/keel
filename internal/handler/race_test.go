package handler_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// 超时补偿任务与支付回调**撞车**。
//
// 这条竞态是本轮接上支付回调之后才第一次存在的，所以它单独一个文件：
// 在任务 6 落地、任务 7 还没落地的那一刻，它连构造都构造不出来。
//
// 撞车的处理不在应用层加锁。两条 UPDATE 都带着 `status = 10`：
//
//	关单   UPDATE orders SET status = 90 WHERE order_no = $1 AND status = 10 AND expire_at < now()
//	支付   UPDATE orders SET status = 20, ... WHERE order_no = $1 AND status = 10
//
// 撞在同一行上时由行锁排队，**恰好一个的 rows_affected 是 1**。
// 下面两条测试分别断言输掉的那一边做了什么、没做什么 —— 两条都要，
// 因为「谁赢了」这件事在两个方向上的正确行为**不对称**：
// 补偿输了要什么都不做，而支付输了要把钱记下来。

// 竞态一：支付回调先到，超时任务必须空手而归。
//
// 这不是理论。订单在 expire_at 那一刻前后，用户点「已完成支付」与定时任务
// 扫到这一单，是两件真的会撞在一起的事。
//
// 撞车的处理不在应用层加锁：两条 UPDATE 都带着 `status = 10`，行锁让它们排队，
// **恰好一个的 rows_affected 是 1**。这条测试断言输掉的那一边什么都没做 ——
// 尤其是**没有回补库存**：回补了的话，一笔已付款的订单背后的货被放回了货架，
// 那是超卖。
func TestSweepLosesTheRaceWhenThePaymentArrivesFirst(t *testing.T) {
	const qty = 2
	orderNo, sku, before := placeRealOrder(t, hostA, "shop-a", seedAddressA, qty)
	expire(t, orderNo)

	// 支付回调抢先。
	if w := notifyPayment(t, hostA, "shop-a", "wechat",
		payload(orderNo, "race-paid-"+uniqueKey(), payableOf(t, orderNo))); w.Code != http.StatusOK {
		t.Fatalf("支付回调失败：%d %s", w.Code, w.Body.String())
	}
	if got := orderStatusOf(t, orderNo); got != 20 {
		t.Fatalf("支付之后订单状态是 %d，期望 20 —— 这条测试的前置没成立", got)
	}

	rep, err := newSweeper(service.SweepConfig{}).SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}

	if got := orderStatusOf(t, orderNo); got != 20 {
		t.Fatalf("订单 %s 被超时任务关成了 %d —— 用户付了钱，订单却被关了", orderNo, got)
	}
	if got := availableOf(t, sku); got != before-qty {
		t.Fatalf("sku %d 的水位是 %d，期望仍是 %d —— "+
			"超时任务把一笔已付款订单的货放回了货架，这是超卖", sku, got, before-qty)
	}
	logs := inventoryLogsOf(t, orderNo)
	if len(logs) != 1 {
		t.Fatalf("库存流水有 %d 行，期望 1 行（只有下单那次扣减）：%+v —— "+
			"多出来的那一行就是那次不该发生的回补", len(logs), logs)
	}

	// 上面那一段走的是「先支付、再扫描」，而扫描那条 SELECT 本身就带着
	// status = 10，所以它压根不会把这一单捞出来 —— 也就是说**占位那条 UPDATE
	// 的谓词没有被走到**。真正的窗口在「扫描已经把这一单捞出来了、处置还没开始」
	// 那一瞬间，而那一瞬间在一次串行的测试里造不出来。
	//
	// 这不是理论上的挑剔：把占位的谓词从 `status = 10` 放宽成 `status IN (0,10,20)`
	// 之后，上面每一条断言都照样绿（变异验证 P9 就是这么发现的）。
	//
	// 所以这里直接把处置那一步单独调一次 —— 输入正是「扫描刚刚捞到、而支付已经
	// 落地」的那个状态。它是这条竞态真正的守卫。
	tctx := tenant.NewContext(context.Background(), merchantIDOf(t, "shop-a"))
	err = repository.New(testPool).WithTenant(tctx, func(tx repository.Tx) error {
		return tx.ClaimExpiredPendingOrder(context.Background(), orderNo)
	})
	if !errors.Is(err, repository.ErrOrderNotClaimed) {
		t.Fatalf("对一笔已支付（status 20）的订单占位返回的是 %v，"+
			"期望 repository.ErrOrderNotClaimed —— "+
			"占位那条 UPDATE 的谓词里少了 status = 10，"+
			"于是扫描与处置之间到达的支付会被一次关单抹掉，而库存还会被回补一次", err)
	}
	if got := orderStatusOf(t, orderNo); got != 20 {
		t.Fatalf("那次占位把订单改成了 %d", got)
	}

	t.Logf("支付先到：订单停在 20，水位停在 %d，流水 1 行；"+
		"而直接对它占位返回 ErrOrderNotClaimed（report=%+v）", before-qty, rep)
}

// 竞态二：超时任务先到，晚到的支付必须落库但不改订单。
//
// 这是同一条竞态的另一半。订单已经被关到 90、库存已经回补，而钱真的到账了 ——
// 系统**不能**假装这笔钱没来过（那是一笔查不到的钱），也不能把订单重新开成
// 已支付（货已经放回货架卖给别人了）。
//
// 正确的处置是：支付单落库、订单不动、一条 Error 日志等人来退款。
func TestLatePaymentAfterSweepIsRecordedButDoesNotReopenTheOrder(t *testing.T) {
	const qty = 2
	orderNo, sku, before := placeRealOrder(t, hostA, "shop-a", seedAddressA, qty)
	expire(t, orderNo)

	if _, err := newSweeper(service.SweepConfig{}).SweepOnce(context.Background()); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if got := orderStatusOf(t, orderNo); got != 90 {
		t.Fatalf("扫描之后订单状态是 %d，期望 90 —— 这条测试的前置没成立", got)
	}

	txn := "late-" + uniqueKey()
	w := notifyPayment(t, hostA, "shop-a", "wechat", payload(orderNo, txn, payableOf(t, orderNo)))
	if w.Code != http.StatusOK {
		t.Fatalf("晚到的支付回调返回 %d，期望 200（让渠道停下来）：%s", w.Code, w.Body.String())
	}

	if got := orderStatusOf(t, orderNo); got != 90 {
		t.Fatalf("订单 %s 被晚到的支付重新开成了 %d —— 那批货已经放回货架了", orderNo, got)
	}
	if got := availableOf(t, sku); got != before {
		t.Fatalf("sku %d 的水位是 %d，期望仍是回补后的 %d", sku, got, before)
	}
	// 钱的痕迹必须在：支付单落库了，而且是「渠道那边成功了」的状态。
	// 少了这一条，「不改订单」就退化成「把这笔钱丢掉」。
	p := paymentOf(t, txn)
	if p.OrderNo != orderNo {
		t.Fatalf("支付单没有落库（按 channel_txn_id=%s 查不到）—— 一笔真实到账凭空消失了", txn)
	}
	if p.Status != 1 {
		t.Fatalf("支付单的 status 是 %d，期望 1 成功 —— 这一列描述的是"+
			"「渠道那边这笔支付成没成」，而不是「我们认不认这笔账」", p.Status)
	}
	t.Logf("超时先到：订单停在 90、水位停在 %d，而支付单 %s 照样落了库", before, p.PaymentNo)
}
