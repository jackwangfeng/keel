package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// 下单主链路的行为测试。
//
// 它们打的是**真实的那一套**：真路由、真令牌、真库、真 dtmrs 协调器
// （sqlite 落在临时目录，见 main_test.go）。核对事实一律走管理员连接直接读库，
// 不靠再打一次 HTTP —— 后者只能证明同一段代码前后自洽。

const seedAddressA = "A 店收件人"

// tokenA 登录 shop-a 的种子买家。
func tokenA(t *testing.T) string {
	t.Helper()
	return login(t, hostA, seedPhone, seedPassword).AccessToken
}

// 试算与真下单必须算出同一笔钱。
//
// 这是本任务的一条硬要求，也是 service/pricing.go 存在的全部理由：
// 两条路各算一份，用户会看到一个价、付另一个价，而两边各自都自洽、
// 各自的测试都绿。这条测试是那份共用实现的守卫 —— 把 Create 改成自己算一遍
// （哪怕只是少减一次优惠），它当场红。
func TestPreviewAndCreateAgreeOnTheMoney(t *testing.T) {
	tok := tokenA(t)
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku, _ := anySKUWithStock(t, "shop-a", 3)
	body := orderBody(t, "shop-a", addr, sku, 2, "")

	pw := previewOrder(t, hostA, body, tok)
	if pw.Code != http.StatusOK {
		t.Fatalf("试算失败：%d %s", pw.Code, pw.Body.String())
	}
	var preview api.OrderPreview
	if err := json.Unmarshal(pw.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}

	cw := createOrder(t, hostA, body, tok, "money-"+uniqueKey())
	if cw.Code != http.StatusCreated {
		t.Fatalf("下单失败：%d %s", cw.Code, cw.Body.String())
	}
	var order api.Order
	if err := json.Unmarshal(cw.Body.Bytes(), &order); err != nil {
		t.Fatal(err)
	}

	if preview.PayableCents != order.PayableCents {
		t.Fatalf("试算应付 %d，成交应付 %d —— 两条路径没有共用同一份定价",
			preview.PayableCents, order.PayableCents)
	}
	if preview.GoodsAmountCents != *order.GoodsAmountCents {
		t.Fatalf("试算商品额 %d，订单商品额 %d",
			preview.GoodsAmountCents, *order.GoodsAmountCents)
	}
	// 阳性对照：金额不能是 0，否则「两个 0 相等」什么也证明不了。
	if preview.PayableCents <= 0 {
		t.Fatalf("试算应付是 %d —— 这条断言没有区分力", preview.PayableCents)
	}
	t.Logf("试算与成交都是 %d 分", order.PayableCents)
}

// 下单真的把库存扣了，而且留下了一行可对账的流水。
//
// 两条断言缺一不可：只看水位的话，把 AppendInventoryLog 删掉测试照样绿；
// 只看流水的话，把 DeductInventory 换成一句空操作、流水照写，测试也照样绿。
func TestCreateDeductsInventory(t *testing.T) {
	tok := tokenA(t)
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku, before := anySKUWithStock(t, "shop-a", 4)
	const qty = 3

	w := createOrder(t, hostA, orderBody(t, "shop-a", addr, sku, qty, ""), tok, "deduct-"+uniqueKey())
	if w.Code != http.StatusCreated {
		t.Fatalf("下单失败：%d %s", w.Code, w.Body.String())
	}
	var order api.Order
	if err := json.Unmarshal(w.Body.Bytes(), &order); err != nil {
		t.Fatal(err)
	}

	if after := availableOf(t, sku); after != before-qty {
		t.Fatalf("sku %d 下单前水位 %d，下单后 %d，期望 %d —— 库存没有被真的扣减",
			sku, before, after, before-qty)
	}
	logs := inventoryLogsOf(t, order.OrderNo)
	if len(logs) != 1 {
		t.Fatalf("订单 %s 的库存流水有 %d 行，期望 1 行：%+v", order.OrderNo, len(logs), logs)
	}
	if logs[0].BizType != 1 || logs[0].ChangeQty != -qty ||
		logs[0].Before != before || logs[0].After != before-qty {
		t.Fatalf("流水对不上：%+v（期望 biz_type=1 change=-%d before=%d after=%d）",
			logs[0], qty, before, before-qty)
	}
	if order.Status != 10 {
		t.Fatalf("订单状态是 %d，期望 10 待支付", order.Status)
	}
	if got := orderStatusOf(t, order.OrderNo); got != 10 {
		t.Fatalf("库里订单 %s 的状态是 %d，期望 10 —— 建单分支没有生效", order.OrderNo, got)
	}
	t.Logf("订单 %s：水位 %d → %d，流水 %+v", order.OrderNo, before, before-qty, logs[0])
}

// 库存不足是一条**正常业务分支**：409、订单被关掉、库存一分没动。
//
// 「库存一分没动」这一条特别要紧：失败的那一次扣减和它的屏障记录在同一个事务里
// 回滚，所以这里既不该有水位变化，**也不该有任何一行流水** —— 有流水就说明
// 扣减提交过，那是一次需要补偿的既成事实，而这条路径上没有补偿会被调用。
func TestInsufficientStockClosesTheOrderAndTouchesNothing(t *testing.T) {
	tok := tokenA(t)
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku, before := anySKUWithStock(t, "shop-a", 1)

	w := createOrder(t, hostA, orderBody(t, "shop-a", addr, sku, int(before)+1, ""), tok,
		"starve-"+uniqueKey())
	p := problemOf(t, w, http.StatusConflict)
	if p.Type != problem.TypeInsufficientStock {
		t.Fatalf("problem type 是 %q，期望 %q", p.Type, problem.TypeInsufficientStock)
	}

	if after := availableOf(t, sku); after != before {
		t.Fatalf("sku %d 的水位从 %d 变成了 %d —— 一次失败的下单动了库存", sku, before, after)
	}

	// 订单必须被关掉，不能停在 0 创建中。这正是「建单排在库存前面」换来的东西：
	// 补偿只对真的执行过的分支生效，库存排在前面时建单的补偿会是一次空回滚，
	// 那笔订单就永远没人关。
	orderNo := lastOrderNoOf(t, "shop-a")
	if got := orderStatusOf(t, orderNo); got != 90 {
		t.Fatalf("最近一笔订单 %s 的状态是 %d，期望 90 已关闭 —— 全局补偿没有把它关掉",
			orderNo, got)
	}
	if logs := inventoryLogsOf(t, orderNo); len(logs) != 0 {
		t.Fatalf("订单 %s 留下了 %d 行库存流水：%+v —— 失败的扣减提交过？",
			orderNo, len(logs), logs)
	}
	t.Logf("库存不足：409 %s，订单 %s 已关闭，水位仍是 %d", p.Type, orderNo, before)
}

// 同一个 Idempotency-Key 两次，只产生一笔订单、只扣一次库存。
func TestSameIdempotencyKeyProducesExactlyOneOrder(t *testing.T) {
	tok := tokenA(t)
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku, before := anySKUWithStock(t, "shop-a", 4)
	const qty = 2
	key := "idem-" + uniqueKey()
	body := orderBody(t, "shop-a", addr, sku, qty, "")

	first := createOrder(t, hostA, body, tok, key)
	if first.Code != http.StatusCreated {
		t.Fatalf("第一次下单失败：%d %s", first.Code, first.Body.String())
	}
	if got := first.Header().Get("Idempotency-Replayed"); got != "" {
		t.Fatalf("第一次下单带了 Idempotency-Replayed: %q —— 它不该出现在首次执行上", got)
	}

	second := createOrder(t, hostA, body, tok, key)
	if second.Code != http.StatusCreated {
		t.Fatalf("重放失败：%d %s", second.Code, second.Body.String())
	}
	if got := second.Header().Get("Idempotency-Replayed"); got != "true" {
		t.Fatalf("重放没有带 Idempotency-Replayed: true，实得 %q —— "+
			"客户端分不清「我真的下了单」和「这是上次那单」", got)
	}

	var a, b api.Order
	if err := json.Unmarshal(first.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if a.OrderNo != b.OrderNo {
		t.Fatalf("两次拿到了两个订单号：%s / %s —— 幂等没有生效", a.OrderNo, b.OrderNo)
	}

	// 直接数库：只能有那一笔。
	if n := ordersWithNo(t, a.OrderNo); n != 1 {
		t.Fatalf("库里叫 %s 的订单有 %d 笔", a.OrderNo, n)
	}
	if archived := orderNoForKey(t, "shop-a", key); archived != a.OrderNo {
		t.Fatalf("幂等存档里的订单号是 %s，响应里是 %s", archived, a.OrderNo)
	}
	// 库存只该被扣一次。这一条是「重放真的什么都没做」的硬证据 ——
	// 订单号相同也可能是第二次又建了一笔然后碰巧被覆盖了。
	if after := availableOf(t, sku); after != before-qty {
		t.Fatalf("sku %d 水位 %d → %d，期望只扣一次到 %d —— 重放执行了业务",
			sku, before, after, before-qty)
	}
	if logs := inventoryLogsOf(t, a.OrderNo); len(logs) != 1 {
		t.Fatalf("订单 %s 有 %d 行库存流水，期望 1 行：%+v", a.OrderNo, len(logs), logs)
	}
	t.Logf("同一把键两次：订单 %s，水位 %d → %d", a.OrderNo, before, before-qty)
}

// 同一个键配不同的请求体 → 422，**不是**静默当成重放。
//
// 数据模型 §12：宁可显式失败，也不把不同的请求当成重放静默吞掉 ——
// 那会让用户以为下单成功了而实际什么都没发生。
func TestSameKeyDifferentBodyIsRejected(t *testing.T) {
	tok := tokenA(t)
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku, _ := anySKUWithStock(t, "shop-a", 4)
	key := "reuse-" + uniqueKey()

	if w := createOrder(t, hostA, orderBody(t, "shop-a", addr, sku, 1, ""), tok, key); w.Code != http.StatusCreated {
		t.Fatalf("第一次下单失败：%d %s", w.Code, w.Body.String())
	}
	w := createOrder(t, hostA, orderBody(t, "shop-a", addr, sku, 2, ""), tok, key)
	p := problemOf(t, w, http.StatusUnprocessableEntity)
	if p.Type != problem.TypeIdempotencyKeyReused {
		t.Fatalf("problem type 是 %q，期望 %q", p.Type, problem.TypeIdempotencyKeyReused)
	}
}

// 金额一致性：**改了 SKU 价格之后，拿旧的试算结果去下单必须被拒**（契约的 409）。
//
// 这条测试刻意去改真实的 skus.price_cents，而不是直接传一个瞎编的
// expected_payable_cents：后者证明的只是「两个数不相等会被拒」，
// 而契约里这条检查存在的理由是**价格会在两次请求之间变**。
func TestOldQuoteIsRejectedAfterAPriceChange(t *testing.T) {
	tok := tokenA(t)
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku, _ := anySKUWithStock(t, "shop-a", 4)
	body := orderBody(t, "shop-a", addr, sku, 1, "")

	pw := previewOrder(t, hostA, body, tok)
	if pw.Code != http.StatusOK {
		t.Fatalf("试算失败：%d %s", pw.Code, pw.Body.String())
	}
	var preview api.OrderPreview
	if err := json.Unmarshal(pw.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}

	// 涨价 100 分，测试结束后改回去。
	ctx := context.Background()
	conn := admin(t)
	var old int64
	if err := conn.QueryRow(ctx,
		`UPDATE skus SET price_cents = price_cents + 100 WHERE id = $1
		 RETURNING price_cents - 100`, sku).Scan(&old); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(),
			`UPDATE skus SET price_cents = $2 WHERE id = $1`, sku, old); err != nil {
			t.Errorf("恢复 sku %d 的价格失败: %v", sku, err)
		}
	})

	withOldQuote := orderBody(t, "shop-a", addr, sku, 1,
		fmt.Sprintf(`"expected_payable_cents":%d`, preview.PayableCents))
	w := createOrder(t, hostA, withOldQuote, tok, "price-"+uniqueKey())
	p := problemOf(t, w, http.StatusConflict)
	if p.Type != problem.TypePriceChanged {
		t.Fatalf("problem type 是 %q，期望 %q", p.Type, problem.TypePriceChanged)
	}

	// 阳性对照：用新价再试算一次，那个数必须能下单成功 ——
	// 否则上面的 409 也可能只是因为这条接口根本下不了单。
	pw2 := previewOrder(t, hostA, body, tok)
	var fresh api.OrderPreview
	if err := json.Unmarshal(pw2.Body.Bytes(), &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.PayableCents == preview.PayableCents {
		t.Fatalf("涨价之后试算还是 %d —— 定价没有读真实价格，这条测试没有区分力",
			fresh.PayableCents)
	}
	ok := createOrder(t, hostA, orderBody(t, "shop-a", addr, sku, 1,
		fmt.Sprintf(`"expected_payable_cents":%d`, fresh.PayableCents)), tok,
		"price-ok-"+uniqueKey())
	if ok.Code != http.StatusCreated {
		t.Fatalf("用新价下单也失败了：%d %s —— 上面那个 409 证明不了任何东西",
			ok.Code, ok.Body.String())
	}
	t.Logf("旧试算 %d 被拒（409 %s），新试算 %d 成交", preview.PayableCents, p.Type, fresh.PayableCents)
}

// 传了券**不会被静默忽略**：两条接口都返回一个明确的 501。
//
// 静默忽略 = 用户以为用了券、实际按原价成交。这是钱的问题，而且客户端没有
// 任何办法发现。
//
// 这条测试同时守着 contract_test.go 里那笔挂账的**反方向**：券真的实现了之后，
// 这里会红，逼人回去把 NotYetImplementedBody 那一行删掉（清单那侧只能做
// 「清单 → 契约」一个方向的机械对账）。
func TestCouponIsRejectedNotSilentlyIgnored(t *testing.T) {
	tok := tokenA(t)
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku, _ := anySKUWithStock(t, "shop-a", 2)
	withCoupon := orderBody(t, "shop-a", addr, sku, 1, `"user_coupon_id":123`)

	for _, tc := range []struct {
		name         string
		contractPath string
		send         func() *httptest.ResponseRecorder
	}{
		{"preview", "/orders/preview", func() *httptest.ResponseRecorder {
			return previewOrder(t, hostA, withCoupon, tok)
		}},
		{"create", "/orders", func() *httptest.ResponseRecorder {
			return createOrder(t, hostA, withCoupon, tok, "coupon-"+uniqueKey())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := problemOf(t, tc.send(), http.StatusNotImplemented)
			if p.Type != problem.TypeNotImplemented {
				t.Fatalf("problem type 是 %q，期望 %q", p.Type, problem.TypeNotImplemented)
			}
			r := routeOf(t, http.MethodPost, tc.contractPath)
			if _, listed := r.NotYetImplementedBody["user_coupon_id"]; !listed {
				t.Fatalf("%s 的 NotYetImplementedBody 里没有 user_coupon_id —— "+
					"券实现了就把这条测试一起改掉，别让挂账烂在那里", tc.contractPath)
			}
		})
	}

	// 阳性对照：同样的请求去掉券就能成功。否则上面的 501 也可能只是因为
	// 这个请求体本身就是坏的。
	if w := previewOrder(t, hostA, orderBody(t, "shop-a", addr, sku, 1, ""), tok); w.Code != http.StatusOK {
		t.Fatalf("去掉券之后试算仍然失败：%d %s", w.Code, w.Body.String())
	}
}

// 运费字段**整个不出现**，而不是 0。
//
// 「算出来是 0」意味着这单包邮，「没算」意味着这个数还会变 —— 对客户端是两件
// 不同的事，而契约里 freight_cents 是可选字段，所以「没算」的诚实形状就是缺席。
//
// 这条测试是 contract_test.go 里 NotYetImplementedResponse 那笔挂账的反方向：
// 真的实现了运费，这里会红。
func TestFreightIsAbsentNotZero(t *testing.T) {
	tok := tokenA(t)
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku, _ := anySKUWithStock(t, "shop-a", 2)
	body := orderBody(t, "shop-a", addr, sku, 1, "")

	check := func(t *testing.T, contractPath string, raw []byte) {
		t.Helper()
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("响应不是 JSON 对象: %v\n%s", err, raw)
		}
		if _, present := m["freight_cents"]; present {
			t.Fatalf("响应里出现了 freight_cents（%s）—— 运费真的实现了？"+
				"那就把 contract_test.go 里 %s 的 NotYetImplementedResponse 一行删掉",
				m["freight_cents"], contractPath)
		}
		// 阳性对照：别的金额字段必须在。整个响应体是空对象的话，
		// 「freight_cents 不在里面」是句废话。
		if _, ok := m["payable_cents"]; !ok {
			t.Fatalf("响应里连 payable_cents 都没有 —— 这条断言没有区分力：%s", raw)
		}
		r := routeOf(t, http.MethodPost, contractPath)
		if _, listed := r.NotYetImplementedResponse["freight_cents"]; !listed {
			t.Fatalf("%s 的 NotYetImplementedResponse 里没有 freight_cents", contractPath)
		}
	}

	pw := previewOrder(t, hostA, body, tok)
	if pw.Code != http.StatusOK {
		t.Fatalf("试算失败：%d %s", pw.Code, pw.Body.String())
	}
	check(t, "/orders/preview", pw.Body.Bytes())

	cw := createOrder(t, hostA, body, tok, "freight-"+uniqueKey())
	if cw.Code != http.StatusCreated {
		t.Fatalf("下单失败：%d %s", cw.Code, cw.Body.String())
	}
	check(t, "/orders", cw.Body.Bytes())
}

// 「这家店没有这一行库存」≡「可售 0」，报 409 缺货 —— 而**不是**「本店不卖」。
//
// ===========================================================================
// 这条测试本轮换了靶子，原来那个被 00020 拿掉了
// ===========================================================================
//
// 它原先叫「不可见的库存行不会被翻译成库存不足」，断言的是 SKU-NOSTOCKROW
// 触发 ErrSKUNotInTenant、链路报 500。**那个行为本轮是被有意改掉的**：
// 00020 把扣减的失败从两种拆成四种，并把「这家店根本没有这一行」从
// ErrSKUNotInTenant 挪进了 ErrInsufficientStock（internal/repository/inventory.go
// 的文件头逐条写着，并注明这是数据模型 §4 里唯一一处与任务书字面不同的地方）。
//
// 理由是「开店即营业」：库存按门店分之后，「缺行」从罕见变成常态 ——
// 新店、新品、缺货清零都会缺行。把它判成「本店不卖」会让一家刚开的店在录
// 库存之前对每一件商品都回「本店不卖」，而那是产品明确不要的形态。
//
// 于是硬约束三（不可见不能伪装成缺货）在 HTTP 层**没有可达的靶子了**：
//
//	· 别家的 sku_id —— 定价那一步（ListSKUsForPricing）先把它当成不可售拒掉；
//	· 软删的 sku_id —— 同上，那条查询带着 s.deleted_at IS NULL；
//	· 别家的 store_id —— CreateOrderDraft 那条 INSERT ... SELECT FROM stores
//	  插 0 行，在 SAGA 跑起来之前就是 422。
//
// 三条都够不着扣减。硬约束三今天由
// repository/inventory_test.go 的 TestDeductInventoryTellsStarvationFromCrossTenant
// 用两家商家的真实数据守着，那里能直接调 DeductInventory。
//
// 所以这条测试改成守**替换它的那条语义**，而不是删掉：缺行 ≡ 可售 0。
// 没有它的话，把「缺行」重新判成 ErrSKUNotSoldInStore（422）这个回退
// 在整个仓库里没有任何东西会红 —— 而它的症状正是「新店什么都不卖」。
func TestMissingInventoryRowMeansZeroStockNotUnsold(t *testing.T) {
	tok := tokenA(t)
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku := skuIDOf(t, "shop-a", "SKU-NOSTOCKROW")

	// 阳性对照：这个 SKU 真的没有库存行。有的话（种子被改了），
	// 下面那个 500 就会变成一次成功下单，而这条测试会以另一种方式红。
	var rows int
	if err := admin(t).QueryRow(context.Background(),
		`SELECT count(*) FROM inventories WHERE sku_id = $1`, sku).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("SKU-NOSTOCKROW 有 %d 行库存 —— 种子里那条「刻意没有库存行」被补上了，"+
			"这条断言失去了靶子", rows)
	}

	// 试算必须成功：定价不看库存（架构 §5 论证过判定点在 SAGA 正向阶段）。
	// 这一步同时排除了「500 只是因为这个 SKU 压根查不到」这种解释。
	if w := previewOrder(t, hostA, orderBody(t, "shop-a", addr, sku, 1, ""), tok); w.Code != http.StatusOK {
		t.Fatalf("试算这个 SKU 就失败了：%d %s —— 下面那个 500 证明不了任何东西",
			w.Code, w.Body.String())
	}

	w := createOrder(t, hostA, orderBody(t, "shop-a", addr, sku, 1, ""), tok, "nostockrow-"+uniqueKey())
	p := problemOf(t, w, http.StatusConflict)
	if p.Type == problem.TypeSKUNotSoldInStore {
		t.Fatalf("返回了 422/409 %s —— 「这家店没有这一行库存」被判成了「本店不卖」。"+
			"缺行 ≡ 可售 0（数据模型 §4）：判成「不卖」会让一家刚开的店在录库存之前"+
			"对每一件商品都回「本店不卖」，而那与「开店即营业」正面冲突", p.Type)
	}
	if p.Type != problem.TypeInsufficientStock {
		t.Fatalf("problem type 是 %q，期望 %q —— 缺行要报成缺货，"+
			"客户端据此提示「暂时没货」并允许稍后再来",
			p.Type, problem.TypeInsufficientStock)
	}
	t.Logf("缺库存行报成了 409 %s：缺行 ≡ 可售 0，不是「本店不卖」", p.Type)
}

// 不带 Idempotency-Key 的下单要被拒。契约把它写成 required。
func TestCreateRequiresAnIdempotencyKey(t *testing.T) {
	tok := tokenA(t)
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku, _ := anySKUWithStock(t, "shop-a", 2)

	w := postJSON(t, hostA, "/api/v1/orders", orderBody(t, "shop-a", addr, sku, 1, ""), tok, nil)
	p := problemOf(t, w, http.StatusUnprocessableEntity)
	if p.Type != problem.TypeInvalidRequest {
		t.Fatalf("problem type 是 %q，期望 %q", p.Type, problem.TypeInvalidRequest)
	}
}

// 用别家店的 address_id 下单要被拒，而且要和「地址不存在」同形。
//
// 契约里 address_id 是自增 id 对外，防越权靠的正是服务端按 user_id 强制过滤。
// 分开报的话，这个接口就成了「猜 id 探测别人有几个地址」的口子。
func TestAnotherShopsAddressIsRejected(t *testing.T) {
	tok := tokenA(t)
	foreign := addressIDOf(t, "shop-b", "B 店收件人")
	sku, _ := anySKUWithStock(t, "shop-a", 2)

	w := createOrder(t, hostA, orderBody(t, "shop-a", foreign, sku, 1, ""), tok, "addr-"+uniqueKey())
	p := problemOf(t, w, http.StatusUnprocessableEntity)
	if p.Type != problem.TypeInvalidRequest {
		t.Fatalf("problem type 是 %q，期望 %q", p.Type, problem.TypeInvalidRequest)
	}
	// 阳性对照：同样的请求换成自己的地址必须成功。
	own := addressIDOf(t, "shop-a", seedAddressA)
	if ok := createOrder(t, hostA, orderBody(t, "shop-a", own, sku, 1, ""), tok,
		"addr-ok-"+uniqueKey()); ok.Code != http.StatusCreated {
		t.Fatalf("换成自己的地址也失败了：%d %s", ok.Code, ok.Body.String())
	}
}

// 没有令牌打不了这两条接口（契约里它们继承全局 bearerAuth）。
func TestOrderEndpointsNeedABearerToken(t *testing.T) {
	for _, path := range []string{"/api/v1/orders/preview", "/api/v1/orders"} {
		w := postJSON(t, hostA, path, `{"address_id":1,"items":[]}`, "",
			map[string]string{"Idempotency-Key": "nobody"})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s 在没有令牌时回了 %d，期望 401", path, w.Code)
		}
	}
}

// uniqueKey 给幂等键一个每次都不同的后缀。
//
// 同一个包里的测试共用一个库，而幂等键的作用域是 (scope, user_id, key) ——
// 写死一个常量会让第二条测试拿到第一条的重放结果，然后以一种很难看懂的方式失败。
func uniqueKey() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }

// lastOrderNoOf 取某家店最近一笔订单的单号。
//
// 只在「下单失败、响应体里没有单号」时用。按 created_at 取最后一笔在并发跑的
// 测试里是不牢靠的，所以调用它的测试都是串行的单条断言 —— 真要并发化，
// 该做的是让失败响应也带上单号，而那要改契约。
func lastOrderNoOf(t *testing.T, merchantCode string) string {
	t.Helper()
	var no string
	err := admin(t).QueryRow(context.Background(), `
		SELECT o.order_no FROM orders o
		  JOIN merchants m ON m.id = o.merchant_id
		 WHERE m.code = $1 ORDER BY o.id DESC LIMIT 1`, merchantCode).Scan(&no)
	if err != nil {
		t.Fatalf("取 %s 最近一笔订单失败: %v", merchantCode, err)
	}
	return no
}
