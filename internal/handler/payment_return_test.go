package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// 多收款：少发生 + 兜住（00150）。
//
// 2026-09-28 破坏性测试：同一单能拿到任意多套都能付的支付参数；第二笔到账、取消 / 关单之后才到的、
// 金额不符的，都只落一行支付单、订单不认、钱不退、后台看不到。

type returnRow struct {
	Reason, Status int16
	Amount         int64
	Refunded       bool
}

// returnsOf 绕过 RLS 读一笔订单的多收款退回单。
func returnsOf(t *testing.T, orderNo string) []returnRow {
	t.Helper()
	rows, err := adminSession(t).Query(context.Background(), `
		SELECT r.reason, r.status, r.amount_cents, r.channel_refund_id IS NOT NULL
		  FROM payment_returns r JOIN orders o ON o.id = r.order_id
		 WHERE o.order_no = $1 ORDER BY r.id`, orderNo)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []returnRow
	for rows.Next() {
		var r returnRow
		if err := rows.Scan(&r.Reason, &r.Status, &r.Amount, &r.Refunded); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func cleanupPaymentExtras(t *testing.T, orderNo string) {
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM payment_returns WHERE order_id IN (SELECT id FROM orders WHERE order_no = $1)`,
			`DELETE FROM payment_intents WHERE order_id IN (SELECT id FROM orders WHERE order_no = $1)`,
		} {
			if _, err := admin(t).Exec(context.Background(), q, orderNo); err != nil {
				t.Errorf("清理多收款失败: %v", err)
			}
		}
	})
}

// ① 换渠道又付了一次：两套参数都能付（换渠道之前已经发出去的那套挡不住），第二笔到账原路退回，订单只认第一笔。
// 同渠道再发起复用同一个流水号；换渠道旧的作废。买家订单详情上看得到「多付的已退回」。
func TestDuplicatePaymentAfterSwitchingChannelIsReturned(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "dup-pay")
	cleanupPaymentExtras(t, no)
	payable := payableOf(t, no)

	_, wechat, _ := intentOf(t, no, "wechat", tok, "pi-"+uniqueKey())
	_, alipay, _ := intentOf(t, no, "alipay", tok, "pi-"+uniqueKey())
	if w := followSettle(t, hostA, alipay); w.Code != http.StatusOK {
		t.Fatalf("支付宝那笔入账：%d %s", w.Code, w.Body.String())
	}
	if w := followSettle(t, hostA, wechat); w.Code != http.StatusOK {
		t.Fatalf("旧的微信那笔也付了，回调应 200：%d %s", w.Code, w.Body.String())
	}
	if st, paid, _ := paidStateOf(t, no); st != 20 || paid != payable {
		t.Fatalf("订单应只认一笔：status=%d paid=%d（应付 %d）", st, paid, payable)
	}
	got := returnsOf(t, no)
	if len(got) != 1 || got[0].Reason != repository.PaymentReturnDuplicate || got[0].Status != repository.PaymentReturnReturned ||
		got[0].Amount != payable || !got[0].Refunded {
		t.Fatalf("第二笔应当以「重复支付」原路退回（沙箱当场退回）：%+v", got)
	}
	var st [2]int16
	if err := admin(t).QueryRow(context.Background(), `
		SELECT (SELECT status FROM payment_intents WHERE channel = 1 AND order_id = o.id),
		       (SELECT status FROM payment_intents WHERE channel = 2 AND order_id = o.id)
		  FROM orders o WHERE o.order_no = $1`, no).Scan(&st[0], &st[1]); err != nil {
		t.Fatal(err)
	}
	if st[0] != repository.PaymentIntentSuperseded || st[1] != repository.PaymentIntentSettled {
		t.Fatalf("微信意图应已作废（2）、支付宝已入账（3）：%v", st)
	}

	var d api.OrderDetail
	decodeInto(t, getAuth(t, hostA, "/api/v1/orders/"+no, tok), http.StatusOK, "订单详情", &d)
	if d.PaymentReturns == nil || len(*d.PaymentReturns) != 1 {
		t.Fatalf("买家订单详情应带 payment_returns：%+v", d.PaymentReturns)
	}
	pr := (*d.PaymentReturns)[0]
	if pr.Status != 40 || pr.Reason != 1 || int64(pr.AmountCents) != payable || pr.ReturnedAt == nil || pr.Attempts != nil ||
		pr.LastError != nil {
		t.Fatalf("买家看到的退回单：%+v（不该带后台排查字段）", pr)
	}
}

// ② 取消之后才到账：订单不动（90），钱原路退回（原因：订单已取消或关闭）。
func TestPaymentArrivingAfterCancelIsReturned(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "late-pay")
	cleanupPaymentExtras(t, no)
	_, wechat, _ := intentOf(t, no, "wechat", tok, "pi-"+uniqueKey())
	wantStatus(t, orderAction(t, hostA, no, "cancel", tok, "c-"+uniqueKey()), http.StatusOK, "取消")
	if w := followSettle(t, hostA, wechat); w.Code != http.StatusOK {
		t.Fatalf("取消后才到的回调应 200：%d %s", w.Code, w.Body.String())
	}
	if st, paid, _ := paidStateOf(t, no); st != 90 || paid != 0 {
		t.Fatalf("订单应停在 90、实收 0：%d / %d", st, paid)
	}
	if got := returnsOf(t, no); len(got) != 1 || got[0].Reason != repository.PaymentReturnOrderClosed ||
		got[0].Status != repository.PaymentReturnReturned {
		t.Fatalf("取消后到账应以「订单已取消或关闭」原路退回：%+v", got)
	}
}

// ③ 金额与应付不符：订单停在待支付，那笔钱退回；之后付对了照常入账。
func TestMismatchedAmountIsReturnedAndTheOrderCanStillBePaid(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "short-pay")
	cleanupPaymentExtras(t, no)
	payable := payableOf(t, no)
	if w := notifyPayment(t, hostA, "shop-a", "wechat", payload(no, "short-"+uniqueKey(), payable-1)); w.Code != http.StatusOK {
		t.Fatalf("金额不符的回调应 200：%d", w.Code)
	}
	if got := returnsOf(t, no); len(got) != 1 || got[0].Reason != repository.PaymentReturnAmountMismatch ||
		got[0].Amount != payable-1 || got[0].Status != repository.PaymentReturnReturned {
		t.Fatalf("金额不符的到账应原路退回（退的是到账额）：%+v", got)
	}
	if st, _, _ := paidStateOf(t, no); st != 10 {
		t.Fatalf("订单应仍是待支付：%d", st)
	}
	_, settle, _ := intentOf(t, no, "wechat", tok, "pi-"+uniqueKey())
	wantStatus(t, followSettle(t, hostA, settle), http.StatusOK, "付对了")
	if st, paid, _ := paidStateOf(t, no); st != 20 || paid != payable {
		t.Fatalf("付对之后应入账：%d / %d", st, paid)
	}
	if got := returnsOf(t, no); len(got) != 1 {
		t.Fatalf("入账那一笔不该被退：%+v", got)
	}
}

// ④ 兜底扫描：一笔订单不认、却没开退回单的到账（历史数据，或任何别的路径漏掉的）被补开并退回。
func TestSweepBackfillsReturnsForUnacceptedPayments(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "sweep-pay")
	cleanupPaymentExtras(t, no)
	_, settle, _ := intentOf(t, no, "wechat", tok, "pi-"+uniqueKey())
	wantStatus(t, followSettle(t, hostA, settle), http.StatusOK, "入账")
	// 模拟历史上那种：直接落一行成功支付、没有退回单。
	adminExec(t, `INSERT INTO payments (merchant_id, payment_no, order_id, channel, amount_cents, status, channel_txn_id, paid_at)
		SELECT merchant_id, 'hist-'||order_no, id, 2, payable_cents, 1, 'hist-'||order_no, now() FROM orders WHERE order_no = $1`, no)
	var mid int64
	if err := admin(t).QueryRow(context.Background(), `SELECT merchant_id FROM orders WHERE order_no = $1`, no).Scan(&mid); err != nil {
		t.Fatal(err)
	}
	svc := service.NewPaymentReturnService(repository.New(testPool), service.PaymentConfig{Sandbox: true}, nil)
	n, err := svc.SweepMerchant(tenant.NewContext(context.Background(), mid))
	if err != nil || n < 1 {
		t.Fatalf("兜底扫描应补开一张：n=%d err=%v", n, err)
	}
	if got := returnsOf(t, no); len(got) != 1 || got[0].Reason != repository.PaymentReturnDuplicate ||
		got[0].Status != repository.PaymentReturnReturned {
		t.Fatalf("补开的应以「重复支付」退回：%+v", got)
	}
	// 再扫一轮：不重复开。
	if n, _ := svc.SweepMerchant(tenant.NewContext(context.Background(), mid)); n != 0 {
		t.Fatalf("第二轮不该再开：%d", n)
	}
}

// ⑤ 同一单并发发起支付（同渠道、不同钥匙）：只有一个有效意图、同一个流水号。
func TestConcurrentPaymentIntentsShareOneTxn(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "conc-pay")
	cleanupPaymentExtras(t, no)
	var wg sync.WaitGroup
	txns := make([]string, 6)
	for i := range txns {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := createIntent(t, hostA, no, "wechat", tok, fmt.Sprintf("pi-%d-%s", i, uniqueKey()))
			var in api.PaymentIntent
			if w.Code == http.StatusCreated && json.Unmarshal(w.Body.Bytes(), &in) == nil {
				txns[i] = in.PaymentNo
			}
		}(i)
	}
	wg.Wait()
	for _, x := range txns {
		if x == "" || x != txns[0] {
			t.Fatalf("并发发起支付应拿到同一个流水号：%v", txns)
		}
	}
	var active int
	if err := admin(t).QueryRow(context.Background(), `SELECT count(*) FROM payment_intents pi JOIN orders o ON o.id = pi.order_id
		WHERE o.order_no = $1`, no).Scan(&active); err != nil || active != 1 {
		t.Fatalf("应只有一个支付意图：%d %v", active, err)
	}
}

// ⑥ 真实渠道的退回回调走 /webhooks/refunds/{channel}，按 PR 前缀分派：30 → 40；同一个流水号重放 200 不重复。
func TestPaymentReturnChannelCallbackSettlesIt(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "cb-pay")
	cleanupPaymentExtras(t, no)
	payable := payableOf(t, no)
	notifyPayment(t, hostA, "shop-a", "wechat", payload(no, "cb-"+uniqueKey(), payable-1))
	// 把沙箱当场退回的那张拨回 30，模拟真实渠道：提交了、等回调。
	var rno string
	if err := admin(t).QueryRow(context.Background(), `UPDATE payment_returns r SET status = 30, returned_at = NULL, channel_refund_id = NULL
		FROM orders o WHERE o.id = r.order_id AND o.order_no = $1 RETURNING r.return_no`, no).Scan(&rno); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"refund_no":%q,"channel_refund_id":"REAL-%s","amount_cents":%d}`, rno, rno, payable-1)
	cb := func() *http.Response {
		w := postJSON(t, hostA, "/api/v1/webhooks/refunds/wechat", body, "",
			map[string]string{service.SignatureHeader: sign(seedSecret("wechat", "shop-a"), []byte(body))})
		return w.Result()
	}
	if r := cb(); r.StatusCode != http.StatusOK {
		t.Fatalf("退回单的渠道回调应 200：%d", r.StatusCode)
	}
	if got := returnsOf(t, no); len(got) != 1 || got[0].Status != repository.PaymentReturnReturned {
		t.Fatalf("回调后应已退回：%+v", got)
	}
	if r := cb(); r.StatusCode != http.StatusOK {
		t.Fatalf("同一个流水号重放应 200：%d", r.StatusCode)
	}
}

// ⑦ 后台「多收款退回」：全店范围的员工看得到，带订单号与原支付流水号。
func TestAdminPaymentReturnList(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "pr-admin")
	o := cs.twoLineOrder(t, b, nil)
	cs.pay(t, o.OrderNo, o.PayableCents)
	body := payload(o.OrderNo, "again-"+o.OrderNo, o.PayableCents)
	wantStatus(t, notifyPaymentSigned(t, cs.Host, "wechat", body, sign(couponWebhookSecret(cs.adminShop), []byte(body))),
		http.StatusOK, "重复付款的回调")
	var page struct {
		api.PageMeta
		Items []api.PaymentReturn `json:"items"`
	}
	decodeInto(t, getAs(t, cs.Host, "/api/v1/admin/payment-returns", cs.Token), http.StatusOK, "多收款退回列表", &page)
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("应有一张退回单：%+v", page)
	}
	it := page.Items[0]
	if it.OrderNo == nil || *it.OrderNo != o.OrderNo || it.PaymentTxnId == nil || *it.PaymentTxnId != "again-"+o.OrderNo ||
		it.Status != 40 || it.Reason != 1 || it.Attempts == nil {
		t.Fatalf("列表项：%+v", it)
	}
	decodeInto(t, getAs(t, cs.Host, "/api/v1/admin/payment-returns?status=10", cs.Token), http.StatusOK, "按状态筛", &page)
	if page.Total != 0 {
		t.Fatalf("status=10 应为空：%+v", page)
	}
}
