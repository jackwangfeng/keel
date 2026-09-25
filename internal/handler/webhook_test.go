package handler_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/service"
)

// 支付渠道异步回调（Task 7）的行为测试。
//
// 这条接口是这个系统里**唯一一条未认证的写接口**，所以这一组测试的重心不在
// 「happy path 走得通」，而在那两个只有未认证入口才会遇到的问题：
//
//	① 它怎么确定租户 —— 答案是 Host，和别的每一条接口一样，没有破例。
//	  守它的是 TestPaymentWebhookCannotSettleAnotherTenantsOrder。
//	② 它怎么防伪造 —— 答案是每租户每渠道一把密钥的 HMAC 验签。
//	  守它的是 BadSignature 与 NoSecret 两条，后者尤其重要：
//	  **没配密钥必须是拒绝，不是跳过验签。**
//
// 核对事实一律走管理员连接读 orders / payments，不靠再打一次 HTTP。

// seedSecret 是种子里给 shop-a / shop-b 配的回调密钥（db/seed/dev.sql）。
//
// 测试自己拼这个字符串而不是从库里读：从库里读的话，「种子写错了密钥」与
// 「验签用错了密钥」会互相抵消，两边一起错的时候测试照样绿。
func seedSecret(channel, merchantCode string) string {
	return fmt.Sprintf("seed-%s-secret-%s", channel, merchantCode)
}

// sign 按服务端同一个算法算签名：hex(HMAC-SHA256(secret, 原始字节))。
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// payload 拼一份规范回调报文。
func payload(orderNo, txnID string, amountCents int64) string {
	return fmt.Sprintf(`{"order_no":%q,"channel_txn_id":%q,"amount_cents":%d}`,
		orderNo, txnID, amountCents)
}

// notifyPayment 打一次**正确签名**的回调。签名用 merchantCode 那家店的密钥。
//
// merchantCode 与 host 分开传，不是冗余：这两者对不上（拿 A 的密钥签、
// 往 B 的 Host 上发）正是伪造的样子，而那条测试要能构造出来。
func notifyPayment(t *testing.T, host, merchantCode, channel, body string) *httptest.ResponseRecorder {
	t.Helper()
	return notifyPaymentSigned(t, host, channel, body, sign(seedSecret(channel, merchantCode), []byte(body)))
}

// notifyPaymentSigned 打一次带任意签名的回调。
func notifyPaymentSigned(t *testing.T, host, channel, body, signature string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/webhooks/payments/"+channel, strings.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set(service.SignatureHeader, signature)
	}
	w := httptest.NewRecorder()
	testEngine.ServeHTTP(w, req)
	return w
}

// paymentRow 是 payments 里的一行（按 channel_txn_id 反查）。
type paymentRow struct {
	PaymentNo   string
	OrderNo     string
	Channel     int16
	AmountCents int64
	Status      int16
	HasPayload  bool
}

// paymentOf 按渠道流水号读支付单。查不到时返回零值（OrderNo 为空串）。
//
// 走管理员连接跨租户查：这一组里有一条测试要断言「**没有**在别家店下落库」，
// 而一个带租户过滤的查询看不到那种情况 —— 它会把「写到别家去了」报成「没写」。
func paymentOf(t *testing.T, txnID string) paymentRow {
	t.Helper()
	var p paymentRow
	var payload []byte
	err := admin(t).QueryRow(context.Background(), `
		SELECT p.payment_no, o.order_no, p.channel, p.amount_cents, p.status, p.notify_payload
		  FROM payments p JOIN orders o ON o.id = p.order_id
		 WHERE p.channel_txn_id = $1`, txnID).
		Scan(&p.PaymentNo, &p.OrderNo, &p.Channel, &p.AmountCents, &p.Status, &payload)
	if err == pgx.ErrNoRows {
		return paymentRow{}
	}
	if err != nil {
		t.Fatalf("按流水号 %s 读支付单失败: %v", txnID, err)
	}
	p.HasPayload = len(payload) > 0
	return p
}

// countPaymentsFor 数一笔订单名下有几行支付单。
func countPaymentsFor(t *testing.T, orderNo string) int {
	t.Helper()
	var n int
	if err := admin(t).QueryRow(context.Background(), `
		SELECT count(*) FROM payments p JOIN orders o ON o.id = p.order_id
		 WHERE o.order_no = $1`, orderNo).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// paidCentsOf 读订单的实收金额。
func paidCentsOf(t *testing.T, orderNo string) int64 {
	t.Helper()
	var cents int64
	if err := admin(t).QueryRow(context.Background(),
		`SELECT paid_cents FROM orders WHERE order_no = $1`, orderNo).Scan(&cents); err != nil {
		t.Fatalf("读订单 %s 的实收失败: %v", orderNo, err)
	}
	return cents
}

// happy path：下单 → 支付回调 → 订单 20 + 支付单落库。
//
// 四条断言，各守一件事：
//
//	· 订单到 20、paid_cents 对 —— 状态机真的走了；
//	· 支付单落库、金额与渠道对 —— 钱有痕迹；
//	· notify_payload 非空       —— 原始报文永久保留（数据模型 §5），
//	  少了它，对账时「渠道到底推了什么」无从查起；
//	· 库存**不动**              —— 契约明写「库存与优惠券在此步无动作」，
//	  它们在下单的 SAGA 正向阶段就扣完了。写一次 Confirm 进来的话这条会红。
func TestPaymentWebhookSettlesTheOrder(t *testing.T) {
	const qty = 1
	orderNo, sku, before := placeRealOrder(t, hostA, "shop-a", seedAddressA, qty)
	payable := payableOf(t, orderNo)
	txn := "ok-" + uniqueKey()

	w := notifyPayment(t, hostA, "shop-a", "wechat", payload(orderNo, txn, payable))
	if w.Code != http.StatusOK {
		t.Fatalf("支付回调返回 %d，期望 200：%s", w.Code, w.Body.String())
	}

	if got := orderStatusOf(t, orderNo); got != 20 {
		t.Fatalf("订单 %s 的状态是 %d，期望 20 已支付", orderNo, got)
	}
	if got := paidCentsOf(t, orderNo); got != payable {
		t.Fatalf("订单实收是 %d，期望 %d", got, payable)
	}

	p := paymentOf(t, txn)
	if p.OrderNo != orderNo {
		t.Fatalf("按流水号 %s 查不到支付单 —— 订单推到了 20 而钱没有任何痕迹", txn)
	}
	if p.Channel != 1 {
		t.Fatalf("支付单的 channel 是 %d，期望 1 微信 —— "+
			"写反了的话对账时会去支付宝那边找这笔钱", p.Channel)
	}
	if p.AmountCents != payable || p.Status != 1 {
		t.Fatalf("支付单是 %+v，期望金额 %d、status 1", p, payable)
	}
	if !p.HasPayload {
		t.Fatal("支付单的 notify_payload 是空的 —— 原始回调报文要永久保留（数据模型 §5），" +
			"没有它，渠道那边推了什么在事后无从查起")
	}

	if got := availableOf(t, sku); got != before-qty {
		t.Fatalf("sku %d 的水位在支付回调之后从 %d 变成了 %d —— "+
			"契约明写「库存与优惠券在此步无动作」，它们在下单时就扣完了",
			sku, before-qty, got)
	}
	if logs := inventoryLogsOf(t, orderNo); len(logs) != 1 {
		t.Fatalf("库存流水有 %d 行，期望仍是 1 行（只有下单那次扣减）：%+v", len(logs), logs)
	}
	t.Logf("订单 %s：10 → 20，实收 %d，支付单 %s，水位不动（%d）",
		orderNo, payable, p.PaymentNo, before-qty)
}

// 签名不对：401，而且**一个字节都没写进库**。
//
// 只断言状态码是不够的：一个「先入账再验签」的实现照样能回 401，
// 而钱已经入了。所以三条一起：订单不动、支付单没有、水位不动。
func TestPaymentWebhookRejectsABadSignature(t *testing.T) {
	orderNo, sku, before := placeRealOrder(t, hostA, "shop-a", seedAddressA, 1)
	payable := payableOf(t, orderNo)
	txn := "badsig-" + uniqueKey()
	body := payload(orderNo, txn, payable)

	for name, sig := range map[string]string{
		"签名是别人的密钥算的": sign("not-the-right-secret", []byte(body)),
		"签名是一串垃圾":    "deadbeef",
		"根本没带签名":     "",
	} {
		w := notifyPaymentSigned(t, hostA, "wechat", body, sig)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("[%s] 返回 %d，期望 401：%s", name, w.Code, w.Body.String())
		}
		// 契约：「签名验证失败返回 401，且**不得泄露任何内部状态**」。
		if w.Body.Len() != 0 {
			t.Fatalf("[%s] 401 带了响应体 %q —— 它迟早会把「这家店配没配支付」"+
				"写进去，那就是一个探测器", name, w.Body.String())
		}
	}

	if got := orderStatusOf(t, orderNo); got != 10 {
		t.Fatalf("订单 %s 的状态是 %d，期望仍是 10 —— 验签没过却把订单推了", orderNo, got)
	}
	if p := paymentOf(t, txn); p.OrderNo != "" {
		t.Fatalf("验签没过却落了一行支付单：%+v", p)
	}
	if got := availableOf(t, sku); got != before-1 {
		t.Fatalf("sku %d 的水位被动了：%d → %d", sku, before-1, got)
	}

	// 阳性对照：同一份报文配上**对的**签名必须成功。
	// 没有它，上面三种「失败」也可能只是因为这条接口根本不工作。
	if w := notifyPaymentSigned(t, hostA, "wechat", body,
		sign(seedSecret("wechat", "shop-a"), []byte(body))); w.Code != http.StatusOK {
		t.Fatalf("对的签名返回 %d，期望 200 —— 上面那三条断言没有区分力：%s",
			w.Code, w.Body.String())
	}
	if got := orderStatusOf(t, orderNo); got != 20 {
		t.Fatalf("对的签名也没把订单推到 20（当前 %d）", got)
	}
}

// 这家店没配这个渠道的密钥：**拒绝**，不是跳过验签。
//
// 这是这类代码最经典的洞，而且它不报错、不告警：「没配密钥就放行」会让每一个
// 新建的、还没接支付的店都成为一条伪造入口。种子里 shop-c 刻意没配密钥
// （db/seed/dev.sql 写明了它是这条断言的靶子）。
//
// 把 verify 里那个 `if secret == ""` 分支删掉（让空密钥继续往下走算 HMAC），
// 这条测试当场红 —— 空密钥是一把合法的 HMAC 密钥，攻击者用空串一样签得出来。
func TestPaymentWebhookRefusesWhenTheShopHasNoSecret(t *testing.T) {
	const hostC = "custom.example.net" // shop-c 的自定义域名（种子）
	body := payload("does-not-matter", "nosecret-"+uniqueKey(), 100)

	// 用空密钥签 —— 这正是「没配密钥就放行」时攻击者会做的事。
	if w := notifyPaymentSigned(t, hostC, "wechat", body, sign("", []byte(body))); w.Code != http.StatusUnauthorized {
		t.Fatalf("没配密钥的店返回 %d，期望 401 —— "+
			"「没配密钥就跳过验签」会让每一家还没接支付的店都成为伪造入口：%s",
			w.Code, w.Body.String())
	}
	// 阳性对照：这个 Host 本身是解析得出租户的（不然上面那个 401 可能只是 404
	// 走错了分支）。shop-c 有商品，/products 能正常返回。
	if w := do(t, hostC, "/api/v1/products"); w.Code != http.StatusOK {
		t.Fatalf("shop-c 的 Host 解析不出租户（/products 返回 %d）—— "+
			"上面那条 401 证明不了任何关于密钥的事", w.Code)
	}
	t.Logf("shop-c 没配 wechat 密钥：回调被拒（401），而这个 Host 本身是可解析的")
}

// 幂等：同一个渠道流水号推两次，只入账一次。
//
// 兜底的是 uk_payments_channel_txn（00014）。契约明写「重复回调时写入冲突，
// 直接返回 200」—— 所以两次都必须是 200，而不是第二次报错。
//
// 断言要同时看两处：支付单只有一行（不然对账会把一笔钱算两遍），
// 以及 paid_cents 没有翻倍（不然 chk_amount 的恒等式与退款上限都会歪）。
func TestPaymentWebhookIsIdempotentOnTheChannelTxnID(t *testing.T) {
	orderNo, _, _ := placeRealOrder(t, hostA, "shop-a", seedAddressA, 1)
	payable := payableOf(t, orderNo)
	txn := "dup-" + uniqueKey()
	body := payload(orderNo, txn, payable)

	for i := 1; i <= 3; i++ {
		w := notifyPayment(t, hostA, "shop-a", "wechat", body)
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 次回调返回 %d，期望 200（无论首次还是重复）：%s",
				i, w.Code, w.Body.String())
		}
	}

	if n := countPaymentsFor(t, orderNo); n != 1 {
		t.Fatalf("订单 %s 名下有 %d 行支付单，期望 1 行 —— "+
			"uk_payments_channel_txn 没兜住，对账会把同一笔钱算 %d 遍", orderNo, n, n)
	}
	if got := paidCentsOf(t, orderNo); got != payable {
		t.Fatalf("订单实收是 %d，期望 %d —— 重复回调把金额累加了", got, payable)
	}
	if got := orderStatusOf(t, orderNo); got != 20 {
		t.Fatalf("订单状态是 %d，期望 20", got)
	}
	t.Logf("同一个流水号 %s 推了 3 次：支付单 1 行，实收 %d", txn, payable)
}

// **租户是 Host 定的，而且跨不过去。**
//
// 这条是「webhook 怎么确定租户」那个问题的守卫。构造的是最像伪造的那种请求：
// 一份**签名完全正确**的 shop-b 回调（用 shop-b 自己的密钥签、打到 shop-b 的
// Host 上），但报文里带的是 **shop-a 的订单号**。
//
// 正确的行为是：shop-b 的租户上下文里根本看不到那笔订单（RLS 把行挡住），
// 于是订单不动、支付单不落库。
//
// 为什么这条比「签名错了会被拒」更要紧：签名那条守的是「外人进不来」，
// 这条守的是「一个**合法的**商家改不了别人的订单」。后者是多租户系统里
// 更容易漏掉的一半 —— shop-b 是有密钥的，它的每一个请求都验得过。
func TestPaymentWebhookCannotSettleAnotherTenantsOrder(t *testing.T) {
	orderNo, sku, before := placeRealOrder(t, hostA, "shop-a", seedAddressA, 1)
	payable := payableOf(t, orderNo)
	txn := "cross-" + uniqueKey()

	// shop-b 的 Host + shop-b 的密钥 + shop-a 的订单号。签名是**对的**。
	w := notifyPayment(t, hostB, "shop-b", "wechat", payload(orderNo, txn, payable))
	if w.Code != http.StatusOK {
		// 200 是契约里表达得了的唯一「已受理」；服务端会留一条 Error 日志。
		t.Fatalf("跨租户回调返回 %d，期望 200：%s", w.Code, w.Body.String())
	}

	if got := orderStatusOf(t, orderNo); got != 10 {
		t.Fatalf("shop-a 的订单 %s 被 shop-b 的回调推到了 %d —— "+
			"一家商家可以把别人的订单标成已支付", orderNo, got)
	}
	if got := paidCentsOf(t, orderNo); got != 0 {
		t.Fatalf("shop-a 的订单实收变成了 %d", got)
	}
	if p := paymentOf(t, txn); p.OrderNo != "" {
		t.Fatalf("落了一行支付单：%+v —— 它挂在哪个租户名下都是错的", p)
	}
	if got := availableOf(t, sku); got != before-1 {
		t.Fatalf("sku %d 的水位被动了：%d → %d", sku, before-1, got)
	}

	// 阳性对照：同一个订单号配上 shop-a 的 Host 与密钥必须成功。
	// 没有它，上面的「什么都没发生」也可能只是因为这个订单号本来就不可用。
	ownTxn := "cross-own-" + uniqueKey()
	if w := notifyPayment(t, hostA, "shop-a", "wechat",
		payload(orderNo, ownTxn, payable)); w.Code != http.StatusOK {
		t.Fatalf("shop-a 自己的回调返回 %d：%s", w.Code, w.Body.String())
	}
	if got := orderStatusOf(t, orderNo); got != 20 {
		t.Fatalf("shop-a 自己的回调也没把订单推到 20（当前 %d）—— "+
			"上面那条跨租户断言没有区分力", got)
	}
	t.Logf("shop-b 拿着正确签名也动不了 shop-a 的订单 %s；shop-a 自己一次就推到了 20", orderNo)
}

// 金额对不上：**支付单落库、订单不动**。
//
// 系统不替用户认账。少付了就该少付着，多付了要退差额 —— 两种都要人介入。
// 但钱的痕迹不能丢，所以支付单照样落库，这是本轮唯一一处「事务提交了，
// 但只提交了一半业务」的地方（service/payment.go 的 outcome 那条出口）。
//
// 把金额校验删掉，第一条断言（订单仍是 10）当场红。
// 把支付单那次 INSERT 挪到校验之后，第二条（支付单在）当场红。
func TestPaymentWebhookRefusesToSettleOnAnAmountMismatch(t *testing.T) {
	orderNo, _, _ := placeRealOrder(t, hostA, "shop-a", seedAddressA, 1)
	payable := payableOf(t, orderNo)
	txn := "amount-" + uniqueKey()

	w := notifyPayment(t, hostA, "shop-a", "wechat", payload(orderNo, txn, payable-1))
	if w.Code != http.StatusOK {
		t.Fatalf("金额不符的回调返回 %d，期望 200（重推也不会对上，让渠道停下来）：%s",
			w.Code, w.Body.String())
	}

	if got := orderStatusOf(t, orderNo); got != 10 {
		t.Fatalf("订单 %s 的状态是 %d，期望仍是 10 —— 应付 %d、到账 %d，"+
			"系统替用户认了这笔账", orderNo, got, payable, payable-1)
	}
	if got := paidCentsOf(t, orderNo); got != 0 {
		t.Fatalf("订单实收变成了 %d", got)
	}
	p := paymentOf(t, txn)
	if p.OrderNo != orderNo {
		t.Fatal("支付单没落库 —— 钱到账了而系统里没有任何痕迹，这笔钱事后查不到")
	}
	if p.AmountCents != payable-1 {
		t.Fatalf("支付单记的金额是 %d，期望**渠道实际到账的** %d —— "+
			"记成应付金额的话，差额这件事就从库里消失了", p.AmountCents, payable-1)
	}
	t.Logf("应付 %d、到账 %d：订单停在 10，支付单 %s 记着真实到账额",
		payable, payable-1, p.PaymentNo)
}

// 不认识的渠道名一律 401。
//
// 契约里 channel 的枚举只有 wechat 与 alipay，**没有 balance** ——
// 余额支付不产生渠道回调。给 balance 留一个入口等于开了一条
// 「用一条伪造的余额回调把订单推成已支付」的路。
func TestPaymentWebhookRejectsChannelsOutsideTheContract(t *testing.T) {
	orderNo, _, _ := placeRealOrder(t, hostA, "shop-a", seedAddressA, 1)
	payable := payableOf(t, orderNo)

	// **balance 用它自己那把（种子里配好的）密钥签。**
	//
	// 这一点是变异验证逼出来的：原先这条测试用 wechat 的密钥签一条 balance 回调，
	// 于是「被拒」既可能是白名单挡的、也可能只是因为这家店没有 balance 密钥。
	// 两件事分不开时，「把 balance 加进白名单」这种改动照样绿 —— 而那正是
	// 这条测试唯一要挡的东西。配上 balance 密钥之后，被拒只剩白名单这一个成因。
	for _, c := range []struct{ channel, secret string }{
		{"balance", seedSecret("balance", "shop-a")}, // 密钥是真的，渠道不在契约里
		{"unionpay", seedSecret("wechat", "shop-a")}, // 契约里没有这个渠道
		{"WECHAT", seedSecret("wechat", "shop-a")},   // 大小写不符
	} {
		body := payload(orderNo, "chan-"+uniqueKey(), payable)
		w := notifyPaymentSigned(t, hostA, c.channel, body, sign(c.secret, []byte(body)))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("渠道 %q 返回 %d，期望 401：%s", c.channel, w.Code, w.Body.String())
		}
	}
	if got := orderStatusOf(t, orderNo); got != 10 {
		t.Fatalf("订单被一条渠道名不合法的回调推到了 %d", got)
	}

	// 阳性对照：alipay 在契约里，而且种子给它配了密钥 —— 它必须走得通。
	// 没有它，上面三条「被拒」也可能只是因为验签整个坏了。
	body := payload(orderNo, "chan-ok-"+uniqueKey(), payable)
	if w := notifyPaymentSigned(t, hostA, "alipay", body,
		sign(seedSecret("alipay", "shop-a"), []byte(body))); w.Code != http.StatusOK {
		t.Fatalf("alipay 返回 %d，期望 200 —— 上面那三条断言没有区分力：%s",
			w.Code, w.Body.String())
	}
	if got := orderStatusOf(t, orderNo); got != 20 {
		t.Fatalf("alipay 的回调没把订单推到 20（当前 %d）", got)
	}
	t.Logf("balance（有密钥）/ unionpay / 大小写不符都被拒，而 alipay 走得通")
}

// 「不认这笔账」的四条出路必须真的留下一条 Error 日志。
//
// 支付回调把「钱到了货没扣」从一条不变量降级成了运维约定：支付单留在库里 +
// 一条日志给人看。webhook.go 与 service/payment.go 的注释都是照着这条约定写的
// （「钱可能是真的到账了，所以这条要响」）。而在挂上 app.logHandlerErrors 之前，
// 那半条约定是假的 —— 全仓库 12 处 `_ = c.Error(err)` 没有任何人 drain，
// gin 的 c.Error 只是往 c.Errors 里 append。
//
// 验收用一次真撞车量出了代价：44 笔「支付单已入账 / 订单被关到 90 / 库存已回补」，
// 合计约 ¥2205，日志里一条都没有。
//
// 所以这条测试断言的不是「代码里有没有写 c.Error」，而是**日志里真的出现了一条
// Error 记录** —— 前者在黑洞存在时也是真的。
func TestUnsettleableWebhookLeavesAnErrorInTheLog(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(prev)

	// 签名完全正确，只是订单号不存在 —— 这正是「钱可能真的到账了」那一支。
	body := payload("no-such-order-"+strconv.FormatInt(time.Now().UnixNano(), 10),
		"txn-"+strconv.FormatInt(time.Now().UnixNano(), 10), 1)
	w := notifyPayment(t, "shop-a."+baseDomain, "shop-a", "wechat", body)

	// 回 200 是对的：这种情况渠道重推也没用，让它停下来。
	if w.Code != http.StatusOK {
		t.Fatalf("状态码是 %d，期望 200", w.Code)
	}
	if got := buf.String(); !strings.Contains(got, "level=ERROR") {
		t.Fatalf("一笔认不下来的支付没有在日志里留下任何 Error 记录——"+
			"「钱可能是真的到账了，所以这条要响」这句注释就是假的。实际日志：%q", got)
	}
}
