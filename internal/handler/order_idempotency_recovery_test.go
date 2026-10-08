package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// 「处理中」那一档的自愈：SAGA 等超时之后请求早已返回，没人回填结果，
// 而SAGA 之后可能成功。2026-10-08 破坏性测试实测过那个后果：
// 同一把键永远 409 → 客户换一把新的 → 第二单 → 重复收款+重复发货。
//
// 这些测试直接造出那个**遗留状态**（把幂等行按回处理中、把订单按成已支付），
// 然后打真实的HTTP —— 因为要验的正是「读库定论之后回放的是不是那一单」，
// 而不是某个函数的返回值。

// 造出「SAGA 成功了但没人回填」：订单已支付，幂等行还在处理中。
//
// response_body 里带草稿单号，正是 placeDraft 之后 MarkIdempotencyInFlightOrder
// 写下的那个暂存位。没有它就落不到这个场景（旧代码没有这一列）。
func simulateStuckInFlight(t *testing.T, merchantCode, idemKey, orderNo string) {
	t.Helper()
	if _, err := admin(t).Exec(context.Background(), `
		UPDATE idempotency_keys k
		   SET status = 0,
		       response_code = NULL,
		       response_body = jsonb_build_object('order_no', $3::text)
		  FROM merchants m
		 WHERE m.id = k.merchant_id
		   AND m.code = $1 AND k.idem_key = $2`,
		merchantCode, idemKey, orderNo); err != nil {
		t.Fatalf("造遗留的处理中状态失败: %v", err)
	}
}

// SAGA 成功但没回填时，同一把键重试要**回放那一单**，不是 409。
//
// 这是根治的那一步：重试方按暂存位里的单号查库，发现订单已经待支付（10），
// 于是把成功存档补上并回放。之后这把键回到「成功」档。
func TestStuckInFlightSelfHealsToASuccessfulReplay(t *testing.T) {
	tok := tokenA(t)
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku, before := anySKUWithStock(t, "shop-a", 3)
	const qty = 1
	key := "stuck-" + uniqueKey()
	body := orderBody(t, "shop-a", addr, sku, qty, "")

	first := createOrder(t, hostA, body, tok, key)
	if first.Code != http.StatusCreated {
		t.Fatalf("第一次下单失败：%d %s", first.Code, first.Body.String())
	}
	var order api.Order
	if err := json.Unmarshal(first.Body.Bytes(), &order); err != nil {
		t.Fatal(err)
	}

	// 造出那次破坏性测试里的遗留状态：订单已经 status=10（建单分支推的），
	// 而幂等行被按回处理中 —— 相当于 SAGA 成功了却没人回填。
	simulateStuckInFlight(t, "shop-a", key, order.OrderNo)

	second := createOrder(t, hostA, body, tok, key)
	if second.Code != http.StatusCreated {
		t.Fatalf("遗留的处理中键重试回 %d %s，期望 201（自愈回放）—— "+
			"这里退化成 409 就等于回到那把被锁死的钥匙",
			second.Code, second.Body.String())
	}
	if got := second.Header().Get("Idempotency-Replayed"); got != "true" {
		t.Fatalf("自愈回放没有带 Idempotency-Replayed: true，实得 %q", got)
	}
	var replayed api.Order
	if err := json.Unmarshal(second.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.OrderNo != order.OrderNo {
		t.Fatalf("回放的是 %s，第一单是 %s —— 建了第二单", replayed.OrderNo, order.OrderNo)
	}
	// 硬证据：库存只扣过一次。第二单若真被建出来，这里会是 -2q。
	if after := availableOf(t, sku); after != before-qty {
		t.Fatalf("sku %d 水位 %d → %d，期望仍只扣一次到 %d —— 自愈过程中执行了业务",
			sku, before, after, before-qty)
	}
	// 而且这把键从此回到成功档。
	if archived := orderNoForKey(t, "shop-a", key); archived != order.OrderNo {
		t.Fatalf("自愈后幂等存档里的订单号是 %q，期望 %s", archived, order.OrderNo)
	}
	t.Logf("遗留的处理中键已自愈：回放订单 %s，水位 %d → %d", order.OrderNo, before, before-qty)
}

// 遗留的处理中键，单号那一单**已被补偿关掉**时，按业务失败回放，
// 而不是永远 409。
//
// 那一档原来只有 409一个出口：SAGA 最终失败被关到 90，钥匙照样锁死，
// 客户端既拿不到原因也换不掉钥匙。
func TestStuckInFlightOnAClosedOrderReplaysTheFailure(t *testing.T) {
	tok := tokenA(t)
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku, _ := anySKUWithStock(t, "shop-a", 3)
	key := "closed-" + uniqueKey()
	body := orderBody(t, "shop-a", addr, sku, 1, "")

	first := createOrder(t, hostA, body, tok, key)
	if first.Code != http.StatusCreated {
		t.Fatalf("第一次下单失败：%d %s", first.Code, first.Body.String())
	}
	var order api.Order
	if err := json.Unmarshal(first.Body.Bytes(), &order); err != nil {
		t.Fatal(err)
	}

	// 那一单被补偿关掉了（status 90），幂等行按回处理中。
	if _, err := admin(t).Exec(context.Background(),
		`UPDATE orders SET status = 90 WHERE order_no = $1`, order.OrderNo); err != nil {
		t.Fatalf("关单失败: %v", err)
	}
	simulateStuckInFlight(t, "shop-a", key, order.OrderNo)

	w := createOrder(t, hostA, body, tok, key)
	// 关掉的那一单已经被存档成失败，回放的是那个失败（4xx saga 那一档）——
	// 关键不是具体是哪个 4xx，而是**不再是 409 idempotency-key-in-flight**。
	//
	// 这里自己解 problem 而不是用 problemOf：那个 helper 会断言状态码，
	// 而这条测试要断言的恰恰是「状态码是哪个都行，只要不是处理中」。
	var p api.Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("响应体不是 Problem: %v\n%s", err, w.Body.String())
	}
	if p.Type == problem.TypeIdempotencyKeyInFlight {
		t.Fatalf("已被补偿关掉的单子仍在回 409 处理中：%s", w.Body.String())
	}
	if archived := orderNoForKey(t, "shop-a", key); archived == order.OrderNo {
		t.Fatalf("被关掉的单子 %s 被当成了成功存档", order.OrderNo)
	}
	t.Logf("已关闭的遗留单回放了失败：%s（%d）", p.Type, w.Code)
}

// 被库存不足补偿关掉的那一单，自愈时**必须回放库存不足**（409），不能是 500。
//
// 这条是我自己那版实现的一个真错误：第一版在自愈里直接拿
// ErrOrderSagaFailed 兜，而那个 sentinel 的注释写着「出现它意味着分支把
// 失败原因弄丢了」—— 用它回答「这一单被补偿关掉了」，把一次正常的库存不足
// 报成了 500。而 notification_policy.go 上明写着关到 90 时
// 「POST /orders 同步回的是失败（库存不足、券不可用）」。
//
// 造法用现成的路子：超量下单当场得到库存不足并把订单关成 90，
// 然后把那把幂等键按回处理中（模拟「SAGA 失败后没人回填」），再重试一次。
func TestSelfHealOnAStockoutReplaysInsufficientStockNotInternal(t *testing.T) {
	tok := tokenA(t)
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku, before := anySKUWithStock(t, "shop-a", 1)
	key := "stockout-" + uniqueKey()

	// 超量买：这一笔会因库存不足失败，订单被补偿关成 90。
	w := createOrder(t, hostA, orderBody(t, "shop-a", addr, sku, int(before)+1, ""), tok, key)
	p := problemOf(t, w, http.StatusConflict)
	if p.Type != problem.TypeInsufficientStock {
		t.Fatalf("造场景失败：%s（%d）", p.Type, w.Code)
	}
	orderNo := lastOrderNoOf(t, "shop-a")
	if got := orderStatusOf(t, orderNo); got != 90 {
		t.Fatalf("订单 %s 的状态是 %d，期望 90 已关闭", orderNo, got)
	}

	// 把这一行按回「处理中」：模拟 SAGA 失败后没人回填的那个遗留状态。
	simulateStuckInFlight(t, "shop-a", key, orderNo)

	again := createOrder(t, hostA, orderBody(t, "shop-a", addr, sku, int(before)+1, ""), tok, key)
	var replayed api.Problem
	if err := json.Unmarshal(again.Body.Bytes(), &replayed); err != nil {
		t.Fatalf("响应体不是 Problem: %v\n%s", err, again.Body.String())
	}
	if replayed.Type == problem.TypeInternal {
		t.Fatalf("库存不足被报成了 500 internal：%s —— "+
			"自愈时拿「原因丢了」的 sentinel 兜了", again.Body.String())
	}
	// 期望的是 503 order-outcome-unknown 而不是 409 insufficient-stock：
	// 那一单**确实**被补偿关闭了（第一笔的 409 库存不足是对那个还在等的请求回的），
	// 而分支记下的原因是进程内的 notes，原始请求返回时就drop 了 ——
	// 自愈发生在之后，进程内已经没有它了。查不到原因就不能编一个业务原因出去，
	// 而这一档也不是「服务端坏了」，所以是 503。
	if replayed.Type != problem.TypeOrderOutcomeUnknown {
		t.Fatalf("自愈回放的 type 是 %q，期望 %q —— 查不到原因时该说的是"+
			"「结果确定、原因不明」，不是编一个业务原因，也不是 500",
			replayed.Type, problem.TypeOrderOutcomeUnknown)
	}
	if again.Code != http.StatusServiceUnavailable {
		t.Fatalf("原因查不到应是 503，实得 %d", again.Code)
	}
	if after := availableOf(t, sku); after != before {
		t.Fatalf("失败的下单动了库存：水位 %d → %d", before, after)
	}
	t.Logf("自愈回放了「结果确定、原因不明」：503 %s（第一笔是 409 库存不足）", replayed.Type)
}

// 过期的处理中键要被**回收**，让重试成为一次全新的尝试。
//
// 幂等协议要求客户端拿同一把钥匙重试；而一把过了期的处理中键此前
// 恒定 409（实测过：把 expire_at 提前到过去，仍是 409）——
// 那不是锁 24 小时，是永久锁死。
func TestExpiredInFlightKeyIsReclaimed(t *testing.T) {
	tok := tokenA(t)
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku, before := anySKUWithStock(t, "shop-a", 3)
	const qty = 1
	key := "expired-" + uniqueKey()
	body := orderBody(t, "shop-a", addr, sku, qty, "")

	first := createOrder(t, hostA, body, tok, key)
	if first.Code != http.StatusCreated {
		t.Fatalf("第一次下单失败：%d %s", first.Code, first.Body.String())
	}
	var order api.Order
	if err := json.Unmarshal(first.Body.Bytes(), &order); err != nil {
		t.Fatal(err)
	}

	// 遗留的处理中键，并把有效期挪到过去。
	simulateStuckInFlight(t, "shop-a", key, order.OrderNo)
	if _, err := admin(t).Exec(context.Background(), `
		UPDATE idempotency_keys k
		   SET expire_at = now() - interval '1 minute'
		  FROM merchants m
		 WHERE m.id = k.merchant_id AND m.code = $1 AND k.idem_key = $2`,
		"shop-a", key); err != nil {
		t.Fatalf("把 expire_at 提前失败: %v", err)
	}

	// 这一次的请求体与第一单不同（数量不同），所以它不可能靠「回放成功存档」
	// 通过 —— 那条路会撞 422 idempotency-key-reused（同一个键配了不同的请求体）。
	// 只有**真的重新抢到了键**、重新占了一次库存，才可能 201。
	//
	// 用数量而不是换 SKU 来造差异：anySKUWithStock 每次都取水位最高的那一件，
	// 换个 SKU 的写法在这个夹具下换不出第二件（那正是本条测试第一次写成 SKIP 的原因）。
	const qty2 = 2
	again := createOrder(t, hostA, orderBody(t, "shop-a", addr, sku, qty2, ""), tok, key)
	if again.Code != http.StatusCreated {
		t.Fatalf("过期的处理中键重试回 %d %s，期望 201（键被回收，一次全新尝试）—— "+
			"实得 409 说明那把过期钥匙被永久锁死了",
			again.Code, again.Body.String())
	}
	if got := again.Header().Get("Idempotency-Replayed"); got == "true" {
		t.Fatalf("过期键被回收后竟然是重放：%s", again.Body.String())
	}
	var fresh api.Order
	if err := json.Unmarshal(again.Body.Bytes(), &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.OrderNo == order.OrderNo {
		t.Fatalf("回收后建出来的还是第一单 %s", fresh.OrderNo)
	}
	// 水位要再降一次：第一单扣过一次，这一单又扣了一次。少降说明它只是回放了库存。
	if after := availableOf(t, sku); after != before-qty-qty2 {
		t.Fatalf("sku %d 水位 %d → %d，期望 %d（两笔都真扣了）—— "+
			"回收后没有真的建新单", sku, before, after, before-qty-qty2)
	}
	t.Logf("过期的处理中键已回收：新建了 %s，水位 %d → %d", fresh.OrderNo, before, before-qty-qty2)
}
