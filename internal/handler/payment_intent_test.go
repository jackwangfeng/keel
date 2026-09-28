package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// 发起支付（POST /orders/{order_no}/payments）的行为测试。
//
// # 这一组要证明的不是「接口回了 201」，而是两件相反的事
//
//	① 沙箱支付**真的走完了生产那条路**：它交出来的那份回调，经过真实的
//	  /webhooks/payments/{channel}、真实的验签、真实的 settle，把订单从 10 推到 20。
//	  守它的是 TestSandboxPaymentSettlesThroughTheRealWebhookPath ——
//	  核对事实走管理员连接直接读 orders / payments，不再打一次 HTTP。
//
//	② 它**没有**绕开任何一道闸门。改一个字节，验签就该拒；订单不是自己的，
//	  就该 404；订单已经付过，就该 409。守它的是后面那几条。
//
// 如果沙箱是在 CreateIntent 里直接把订单改成 20，①会绿而②里的每一条都会红 ——
// 因为那条捷径上没有验签、没有金额校验、没有状态机。

// createIntent 打一次发起支付。
func createIntent(t *testing.T, host, orderNo, channel, bearer, idemKey string) *httptest.ResponseRecorder {
	t.Helper()
	return postJSON(t, host, "/api/v1/orders/"+orderNo+"/payments",
		`{"channel":"`+channel+`"}`, bearer, map[string]string{"Idempotency-Key": idemKey})
}

// sandboxSettle 是 PaymentIntent.payload 里那份「照着打一次就入账」的回调。
type sandboxSettle struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// intentOf 打一次发起支付并把响应解开，顺带做几条**每次都该成立**的断言：
// 它是沙箱、它标着 KEEL-SANDBOX-、它带着一份可回推的回调。
func intentOf(t *testing.T, orderNo, channel, bearer, idemKey string) (api.PaymentIntent, sandboxSettle, *httptest.ResponseRecorder) {
	t.Helper()
	w := createIntent(t, hostA, orderNo, channel, bearer, idemKey)
	if w.Code != http.StatusCreated {
		t.Fatalf("发起支付失败：%d %s", w.Code, w.Body.String())
	}
	var intent api.PaymentIntent
	if err := json.Unmarshal(w.Body.Bytes(), &intent); err != nil {
		t.Fatalf("响应不是 PaymentIntent: %v\n%s", err, w.Body.String())
	}
	if intent.Payload == nil {
		t.Fatalf("响应里没有 payload：%s", w.Body.String())
	}
	raw, err := json.Marshal((*intent.Payload)["settle"])
	if err != nil {
		t.Fatal(err)
	}
	var settle sandboxSettle
	if err := json.Unmarshal(raw, &settle); err != nil {
		t.Fatalf("payload.settle 不是预期结构: %v\n%s", err, raw)
	}
	return intent, settle, w
}

// followSettle 把 payload.settle 原样打出去 —— **原样**是这条助手的全部意义。
//
// 它不重新序列化 body，也不自己算签名：HMAC 算的是字节，而重新序列化会得到
// 语义相同、字节不同的 JSON。测试自己算一遍签名的话，验的就只是
// 「我算的和服务端算的一样」，而不是「服务端交给客户端的那一份真的能用」。
func followSettle(t *testing.T, host string, s sandboxSettle) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(s.Method, s.URL, strings.NewReader(s.Body))
	req.Host = host
	for k, v := range s.Headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	testEngine.ServeHTTP(w, req)
	return w
}

// paidStateOf 绕过 RLS 读一笔订单的资金状态。
func paidStateOf(t *testing.T, orderNo string) (status int16, paidCents int64, hasPaidAt bool) {
	t.Helper()
	err := admin(t).QueryRow(context.Background(), `
		SELECT status, paid_cents, paid_at IS NOT NULL FROM orders WHERE order_no = $1`,
		orderNo).Scan(&status, &paidCents, &hasPaidAt)
	if err != nil {
		t.Fatalf("读订单 %s 失败: %v", orderNo, err)
	}
	return status, paidCents, hasPaidAt
}

// ---------------------------------------------------------------------------
// ① 沙箱真的走完了生产那条路
// ---------------------------------------------------------------------------

// 发起支付 → 照着它给的参数回推一次 → 订单从 10 变 20，支付单落库。
//
// 核对全部走管理员连接读库：再 GET 一次订单只能证明同一段代码前后自洽，
// 而这条测试要证明的是**库里真的发生了那件事**。
func TestSandboxPaymentSettlesThroughTheRealWebhookPath(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "sandboxpay")

	intent, settle, _ := intentOf(t, no, "wechat", tok, "intent-"+uniqueKey())

	// 金额必须等于应付金额。它不是摆设：settle 会拿报文里的金额与
	// payable_cents 比，对不上就落支付单、不动订单（ErrWebhookAmountMismatch）。
	var payable int64
	if err := admin(t).QueryRow(context.Background(),
		`SELECT payable_cents FROM orders WHERE order_no = $1`, no).Scan(&payable); err != nil {
		t.Fatal(err)
	}
	if int64(intent.AmountCents) != payable {
		t.Fatalf("调起金额 %d，订单应付 %d", intent.AmountCents, payable)
	}

	// 回推之前先确认这一单还是待支付 —— 没有这一步，「回推之后是 20」
	// 也可能是因为它本来就是 20。
	if status, _, _ := paidStateOf(t, no); status != 10 {
		t.Fatalf("回推之前订单状态是 %d，期望 10", status)
	}

	w := followSettle(t, hostA, settle)
	if w.Code != http.StatusOK {
		t.Fatalf("照着沙箱参数回推，webhook 回了 %d：%s —— "+
			"沙箱交出来的那份回调过不了真实的验签/settle，那它就不是「同一条路」",
			w.Code, w.Body.String())
	}

	status, paidCents, hasPaidAt := paidStateOf(t, no)
	if status != 20 {
		t.Fatalf("回推之后订单状态是 %d，期望 20 已支付", status)
	}
	if paidCents != payable {
		t.Fatalf("paid_cents 是 %d，应付 %d", paidCents, payable)
	}
	if !hasPaidAt {
		t.Fatal("订单变成已支付了，paid_at 却是 NULL")
	}

	// 支付单：它由 settle 落库，而不是由发起支付那条接口。
	// channel_txn_id 必须等于调起时给出的 payment_no —— 那是这两条接口之间
	// 唯一的对账线索（发起支付不预落支付单，理由写在 service/payment_intent.go）。
	p := paymentOf(t, intent.PaymentNo)
	if p.OrderNo != no {
		t.Fatalf("按 channel_txn_id=%s 查不到这一单的支付单（查到的是 %q）—— "+
			"调起给出的 payment_no 与入账后的 channel_txn_id 对不上，对账断了",
			intent.PaymentNo, p.OrderNo)
	}
	if p.Status != repository.PaymentSucceeded {
		t.Fatalf("支付单状态是 %d，期望 %d 成功", p.Status, repository.PaymentSucceeded)
	}
	if p.Channel != repository.PaymentChannelWechat {
		t.Fatalf("支付单渠道是 %d，期望 %d", p.Channel, repository.PaymentChannelWechat)
	}
	if p.AmountCents != payable {
		t.Fatalf("支付单金额 %d，应付 %d", p.AmountCents, payable)
	}
	if !p.HasPayload {
		t.Fatal("支付单没有留下原始回调报文 —— 那是这笔钱唯一的证据")
	}
	t.Logf("订单 %s：10 → 20，paid_cents=%d，支付单 %s（流水 %s）",
		no, paidCents, p.PaymentNo, intent.PaymentNo)
}

// 详情页在支付前后给出的是两件不同的事实。
//
// 这条把「发起支付」与「订单详情」两条接口接起来：付完之后详情里必须有
// paid_at 和一条 payments 记录。没有它，客户端付完钱刷新页面看到的还是
// 「待支付」，而那正是这次补齐读接口要解决的问题。
func TestOrderDetailShowsThePaymentAfterItSettles(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "detailpaid")

	_, before := orderDetail(t, tok, no)
	if before.Status != 10 || before.PaidAt != nil ||
		before.Payments == nil || len(*before.Payments) != 0 {
		t.Fatalf("付款之前详情就不对：status=%d paid_at=%v payments=%v",
			before.Status, before.PaidAt, before.Payments)
	}

	intent, settle, _ := intentOf(t, no, "wechat", tok, "detailpaid-"+uniqueKey())
	if w := followSettle(t, hostA, settle); w.Code != http.StatusOK {
		t.Fatalf("回推失败：%d %s", w.Code, w.Body.String())
	}

	_, after := orderDetail(t, tok, no)
	if after.Status != 20 {
		t.Fatalf("付款之后详情里的状态还是 %d", after.Status)
	}
	if after.PaidAt == nil {
		t.Fatal("付款之后详情里没有 paid_at —— 那一列在库里是有值的，是这条路径没读它")
	}
	if after.Payments == nil || len(*after.Payments) != 1 {
		t.Fatalf("付款之后详情里的 payments 是 %v，期望 1 条", after.Payments)
	}
	rec := (*after.Payments)[0]
	if rec.Channel == nil || *rec.Channel != "wechat" {
		t.Fatalf("支付记录的渠道是 %v，期望 wechat", rec.Channel)
	}
	if rec.Status == nil || *rec.Status != int(repository.PaymentSucceeded) {
		t.Fatalf("支付记录的状态是 %v，期望 1 成功", rec.Status)
	}
	if rec.PaidAt == nil {
		t.Fatal("支付记录里没有 paid_at")
	}
	_ = intent
}

// ---------------------------------------------------------------------------
// ② 它没有绕开任何一道闸门
// ---------------------------------------------------------------------------

// 改一个字节，验签就该拒。
//
// 这是「沙箱没有绕过验签」的直接证据：把金额从 N 改成 N+1（签名原样），
// webhook 必须回 401，订单必须一动不动。
//
// 如果沙箱是在发起支付时直接把订单推成 20，这条测试会红 ——
// 因为那时订单在回推之前就已经是 20 了。
func TestSandboxPaymentStillNeedsAValidSignature(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "tamper")
	intent, settle, _ := intentOf(t, no, "wechat", tok, "tamper-"+uniqueKey())

	// 发起支付这一步自己不许动订单。
	if status, paid, _ := paidStateOf(t, no); status != 10 || paid != 0 {
		t.Fatalf("只是发起了支付，订单就变成 status=%d paid_cents=%d 了 —— "+
			"沙箱走了捷径，绕开了 webhook", status, paid)
	}

	var tampered map[string]any
	if err := json.Unmarshal([]byte(settle.Body), &tampered); err != nil {
		t.Fatal(err)
	}
	tampered["amount_cents"] = int64(intent.AmountCents) + 1
	raw, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	bad := settle
	bad.Body = string(raw) // 签名不改

	w := followSettle(t, hostA, bad)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("改了报文、签名没改，webhook 回了 %d，期望 401：%s", w.Code, w.Body.String())
	}
	if status, paid, _ := paidStateOf(t, no); status != 10 || paid != 0 {
		t.Fatalf("验签失败之后订单却变成了 status=%d paid_cents=%d", status, paid)
	}
	if p := paymentOf(t, intent.PaymentNo); p.OrderNo != "" {
		t.Fatalf("验签失败之后却落了一行支付单：%+v", p)
	}
}

// 同一把 Idempotency-Key 只生成一份调起凭据。
//
// 不这样的话，客户端重试一次就多一张能让这一单入账的通行证 ——
// 其中一张用掉之后，另一张会变成一条「金额对得上但订单已不在待支付」的回调，
// 那在日志里长得和一次重复付款一模一样。
func TestSandboxPaymentIntentIsIdempotent(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "idem")
	key := "payintent-" + uniqueKey()

	first, _, w1 := intentOf(t, no, "wechat", tok, key)
	if got := w1.Header().Get("Idempotency-Replayed"); got != "" {
		t.Fatalf("首次调用带了 Idempotency-Replayed: %q", got)
	}

	second, _, w2 := intentOf(t, no, "wechat", tok, key)
	if second.PaymentNo != first.PaymentNo {
		t.Fatalf("同一把钥匙给出了两个凭据：%s 与 %s", first.PaymentNo, second.PaymentNo)
	}
	if got := w2.Header().Get("Idempotency-Replayed"); got != "true" {
		t.Fatalf("重放没有带 Idempotency-Replayed: true（实得 %q）—— "+
			"客户端会把一次重放记成一次新的支付调起", got)
	}

	// 换一把钥匙：不是重放（没有 Idempotency-Replayed），但同一单同一渠道**复用同一个流水号**（00150：一单至多一个
	// 有效的支付意图 —— 之前每次一个新流水号，同一单能拿到任意多套都能付的参数，第二笔到账就是多收）。
	third, _, w3 := intentOf(t, no, "wechat", tok, "payintent-"+uniqueKey())
	if got := w3.Header().Get("Idempotency-Replayed"); got != "" {
		t.Fatalf("换了钥匙却被当成重放：%q", got)
	}
	if third.PaymentNo != first.PaymentNo {
		t.Fatalf("同一单同一渠道再次发起支付应复用流水号 %s，实得 %s", first.PaymentNo, third.PaymentNo)
	}
	// 换渠道：旧的作废，给一个新的流水号（阳性对照：凭据不是从订单号派生的）。
	fourth, _, _ := intentOf(t, no, "alipay", tok, "payintent-"+uniqueKey())
	if fourth.PaymentNo == first.PaymentNo {
		t.Fatalf("换了渠道还是同一个流水号 %s", first.PaymentNo)
	}

	// 同一把钥匙配另一个渠道 = 键被复用，契约要求 422。
	w := createIntent(t, hostA, no, "alipay", tok, key)
	p := problemOf(t, w, http.StatusUnprocessableEntity)
	if p.Type != problem.TypeIdempotencyKeyReused {
		t.Fatalf("同键不同渠道回了 %s，期望 %s", p.Type, problem.TypeIdempotencyKeyReused)
	}
}

// 已经付过的订单不能再发起支付。契约：409 order-status-not-payable。
func TestPaymentIntentRefusesAnOrderThatIsNoLongerPayable(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "twice")
	_, settle, _ := intentOf(t, no, "wechat", tok, "twice-"+uniqueKey())
	if w := followSettle(t, hostA, settle); w.Code != http.StatusOK {
		t.Fatalf("回推失败：%d %s", w.Code, w.Body.String())
	}
	if status, _, _ := paidStateOf(t, no); status != 20 {
		t.Fatalf("回推之后状态是 %d，期望 20 —— 这条测试的前提没成立", status)
	}

	w := createIntent(t, hostA, no, "wechat", tok, "twice2-"+uniqueKey())
	p := problemOf(t, w, http.StatusConflict)
	if p.Type != problem.TypeOrderStatusNotPayable {
		t.Fatalf("已支付订单再发起支付回了 %s，期望 %s", p.Type, problem.TypeOrderStatusNotPayable)
	}
}

// 下单 SAGA 还没走完收尾分支（库存还没扣成）的单不能付（00085）。
//
// 拆分部署下库存服务不在时，订单以 10 待支付的样子在买家的订单列表里停几分钟；
// 这时收了钱，库存分支若被拒，全局补偿关不掉一张已支付的单。这里把 placed_at 抹掉
// 来造出「建单分支跑完、收尾分支还没跑」的那一刻；标记回来之后同一个请求照常放行。
func TestPaymentIntentWaitsUntilTheOrderSagaFinishes(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "unplaced")
	setPlaced := func(placed bool) {
		t.Helper()
		expr := "NULL"
		if placed {
			expr = "now()"
		}
		if _, err := admin(t).Exec(context.Background(),
			"UPDATE orders SET placed_at = "+expr+" WHERE order_no = $1", no); err != nil {
			t.Fatalf("改 placed_at 失败: %v", err)
		}
	}
	var placed bool
	if err := admin(t).QueryRow(context.Background(),
		"SELECT placed_at IS NOT NULL FROM orders WHERE order_no = $1", no).Scan(&placed); err != nil || !placed {
		t.Fatalf("走完 SAGA 的订单 placed_at 应当非空（err=%v, placed=%v）", err, placed)
	}

	setPlaced(false)
	key := "unplaced-" + uniqueKey()
	w := createIntent(t, hostA, no, "wechat", tok, key)
	p := problemOf(t, w, http.StatusConflict)
	if p.Type != problem.TypeOrderStatusNotPayable {
		t.Fatalf("收尾分支没跑完的单发起支付回了 %s，期望 %s", p.Type, problem.TypeOrderStatusNotPayable)
	}

	// 被拒的请求不占钥匙（整个事务回滚）：收尾分支跑完之后，同一把钥匙重试就能付。
	setPlaced(true)
	if w := createIntent(t, hostA, no, "wechat", tok, key); w.Code != http.StatusCreated {
		t.Fatalf("placed_at 补上之后同一把钥匙应当放行，实得 %d %s", w.Code, w.Body.String())
	}
}

// 不能替别人的订单发起支付。
//
// 这一条与「读不到别人的订单」是同一个越权面的两半，但后果不同：读到的是信息，
// 而这里交出去的是**一份能让那一单入账的签名回调**。
func TestPaymentIntentRefusesAnotherBuyersOrder(t *testing.T) {
	tok1, tok2 := tokenA(t), tokenA2(t)
	theirs := placeOrderFor(t, tok2, seedAddress2, "payisolate")

	// 阳性对照：主人自己发起支付是 201。
	if w := createIntent(t, hostA, theirs, "wechat", tok2, "payown-"+uniqueKey()); w.Code != http.StatusCreated {
		t.Fatalf("阳性对照失败：主人自己发起支付回了 %d %s", w.Code, w.Body.String())
	}

	w := createIntent(t, hostA, theirs, "wechat", tok1, "payother-"+uniqueKey())
	if w.Code != http.StatusNotFound {
		t.Fatalf("替别人的订单发起支付回了 %d，期望 404：%s", w.Code, w.Body.String())
	}
	if status, _, _ := paidStateOf(t, theirs); status != 10 {
		t.Fatalf("越权发起支付之后别人的订单变成了 %d", status)
	}
}

// 余额支付回 501，不是静默当成微信支付。
func TestBalanceChannelSaysItIsNotImplemented(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "balance")
	w := createIntent(t, hostA, no, "balance", tok, "balance-"+uniqueKey())
	p := problemOf(t, w, http.StatusNotImplemented)
	if p.Type != problem.TypeNotImplemented {
		t.Fatalf("余额支付回了 %s，期望 %s", p.Type, problem.TypeNotImplemented)
	}
	// 它必须在碰幂等键之前就被拒：一个注定被拒的请求不该占掉客户端那把钥匙。
	if status, _, _ := paidStateOf(t, no); status != 10 {
		t.Fatalf("余额支付被拒之后订单变成了 %d", status)
	}
}

// 不认识的渠道名回 422。
func TestUnknownChannelIsRejected(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "badchan")
	w := createIntent(t, hostA, no, "unionpay", tok, "badchan-"+uniqueKey())
	p := problemOf(t, w, http.StatusUnprocessableEntity)
	if p.Type != problem.TypeInvalidRequest {
		t.Fatalf("未知渠道回了 %s，期望 %s", p.Type, problem.TypeInvalidRequest)
	}
}

// 发起支付要令牌，而且要 Idempotency-Key。
func TestPaymentIntentNeedsATokenAndAnIdempotencyKey(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "payauth")

	if w := createIntent(t, hostA, no, "wechat", "", "k-"+uniqueKey()); w.Code != http.StatusUnauthorized {
		t.Fatalf("不带令牌回了 %d，期望 401：%s", w.Code, w.Body.String())
	}
	w := createIntent(t, hostA, no, "wechat", tok, "")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("不带 Idempotency-Key 回了 %d，期望 422：%s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 沙箱是显式的、可关的
// ---------------------------------------------------------------------------

// 响应本身就说明它不是真支付。
//
// 判据是「任何人看到响应都不会以为这是真的微信支付」，所以这条测试断言的是
// **响应里肉眼可见的那两样东西**，不是某个内部状态。
func TestSandboxIntentIsUnmistakablyNotARealPayment(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "mark")
	intent, settle, w := intentOf(t, no, "wechat", tok, "mark-"+uniqueKey())

	if !strings.HasPrefix(intent.PaymentNo, "KEEL-SANDBOX-") {
		t.Fatalf("payment_no 是 %q —— 它没有沙箱前缀，看上去就像一个真实支付单号",
			intent.PaymentNo)
	}
	if v, ok := (*intent.Payload)["sandbox"]; !ok || v != true {
		t.Fatalf("payload.sandbox 是 %v，期望 true —— 客户端据此知道这不是真钱", v)
	}
	notice, _ := (*intent.Payload)["sandbox_notice"].(string)
	if !strings.Contains(notice, "沙箱") || !strings.Contains(notice, "不是真实") {
		t.Fatalf("payload.sandbox_notice 是 %q —— 它要讲人话，"+
			"看到响应的人里没有谁应该先去查一张枚举表才知道这不是真钱", notice)
	}

	// 真实的微信 JSAPI 调起参数里那几个键一个都不该出现：
	// 它们的存在会让客户端真的去调 wx.requestPayment。
	for _, fake := range []string{"prepay_id", "paySign", "nonceStr", "appId"} {
		if _, ok := (*intent.Payload)[fake]; ok {
			t.Fatalf("payload 里出现了 %q —— 那是真实微信调起参数的键，"+
				"客户端会拿它去真的调起支付", fake)
		}
	}

	// settle 那一份必须打得通（上面几条只看字面，这一条看它是不是真的）。
	if settle.Method != http.MethodPost || !strings.HasSuffix(settle.URL, "/webhooks/payments/wechat") {
		t.Fatalf("payload.settle 指向的不是真实的回调路径：%+v", settle)
	}
	if settle.Headers[service.SignatureHeader] == "" {
		t.Fatalf("payload.settle 里没有签名头 %s", service.SignatureHeader)
	}
	t.Logf("payment_no=%s\npayload=%s", intent.PaymentNo, w.Body.String())
}

// 关掉沙箱之后这条接口回 501，而不是回一个假装成功的支付参数。
//
// 它单独装一套路由（Sandbox: false），不动包级那个 testEngine —— 换掉后者会让
// 别的测试在一个它们没预期的配置上跑。
func TestPaymentIntentIsRefusedWhenSandboxIsOff(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "sandboxoff")

	off := app.Router(testPool,
		tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}),
		testSigner, testOrders, service.PaymentConfig{Sandbox: false}, conceptEmbedder{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders/"+no+"/payments",
		strings.NewReader(`{"channel":"wechat"}`))
	req.Host = hostA
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Idempotency-Key", "sandboxoff-"+uniqueKey())
	w := httptest.NewRecorder()
	off.ServeHTTP(w, req)

	p := problemOf(t, w, http.StatusNotImplemented)
	if p.Type != problem.TypeNotImplemented {
		t.Fatalf("关掉沙箱之后回了 %s，期望 %s", p.Type, problem.TypeNotImplemented)
	}

	// 阳性对照：同一笔订单在开着沙箱的那套路由上是 201。
	// 没有它，这个 501 也可能只是因为订单本身有问题。
	if ok := createIntent(t, hostA, no, "wechat", tok, "sandboxon-"+uniqueKey()); ok.Code != http.StatusCreated {
		t.Fatalf("阳性对照失败：沙箱开着时回了 %d %s", ok.Code, ok.Body.String())
	}
	t.Logf("关掉沙箱：%d %s", w.Code, p.Title)
}
