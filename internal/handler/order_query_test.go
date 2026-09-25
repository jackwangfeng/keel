package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/keel/keel/internal/api"
)

// 买家侧两条读接口的行为测试：GET /orders 与 GET /orders/{order_no}。
//
// # 这一组的重心：同一家店里，A 买家读不到 B 买家的订单
//
// 租户隔离（shop-a 读不到 shop-b）由 RLS 挡着，别的测试已经守住了。
// 这一组守的是 **RLS 管不到的那一层**：策略里只有 current_merchant()，
// 它认不出买家，所以「这一单是不是你的」全靠查询里那个 user_id 条件。
//
// 靶子是种子里 shop-a 的第二个买家（13800000005，db/seed/dev.sql 里写明了它
// 为什么存在）。跨店的那个买家当不了靶子：删掉 user_id 条件，他照样被 RLS 挡着，
// 于是断言会在被测逻辑已经失效的情况下保持绿色。

const (
	seedPhone2   = "13800000005"
	seedAddress2 = "A 店第二个收件人"
)

// tokenA2 登录 shop-a 的**第二个**买家。
func tokenA2(t *testing.T) string {
	t.Helper()
	return login(t, hostA, seedPhone2, seedPassword).AccessToken
}

// getAuth 带令牌发一个 GET。auth_test.go 里那个 post 只管 POST，
// do 又不带 Authorization —— 这两条读接口两样都要。
func getAuth(t *testing.T, host, path, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	testEngine.ServeHTTP(w, req)
	return w
}

// orderListResp 只声明断言要用到的字段，但 PageMeta 那三个一个都不能少：
// 契约的 200 响应里它们是必填的（同 product_test.go 的 listResp）。
type orderListResp struct {
	Page     int         `json:"page"`
	PageSize int         `json:"page_size"`
	Total    int64       `json:"total"`
	Items    []api.Order `json:"items"`
}

// myOrders 打一次「我的订单」。
func myOrders(t *testing.T, bearer, query string) (*httptest.ResponseRecorder, orderListResp) {
	t.Helper()
	w := getAuth(t, hostA, "/api/v1/orders"+query, bearer)
	var body orderListResp
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应不是预期结构: %v\n%s", err, w.Body.String())
		}
	}
	return w, body
}

// orderDetail 打一次订单详情。
func orderDetail(t *testing.T, bearer, orderNo string) (*httptest.ResponseRecorder, api.OrderDetail) {
	t.Helper()
	w := getAuth(t, hostA, "/api/v1/orders/"+orderNo, bearer)
	var body api.OrderDetail
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应不是 OrderDetail: %v\n%s", err, w.Body.String())
		}
	}
	return w, body
}

// userIDOf 按商家 code + 手机号取买家 id（绕过 RLS）。
func userIDOf(t *testing.T, merchantCode, phone string) int64 {
	t.Helper()
	var id int64
	err := admin(t).QueryRow(context.Background(), `
		SELECT u.id FROM users u JOIN merchants m ON m.id = u.merchant_id
		 WHERE m.code = $1 AND u.phone = $2 AND u.deleted_at IS NULL`,
		merchantCode, phone).Scan(&id)
	if err != nil {
		t.Fatalf("取 %s 的买家 %s 失败: %v", merchantCode, phone, err)
	}
	return id
}

// visibleOrderCount 绕过 RLS 数一个买家名下**应当可见**的订单数
// （status <> 0，与查询里的谓词逐字一致）。
//
// 直接读库，而不是再打一次 GET /orders 去数：后者是拿被测代码去验被测代码，
// 而这一组测试守的恰恰是那条查询的条件。
func visibleOrderCount(t *testing.T, userID int64) int64 {
	t.Helper()
	var n int64
	if err := admin(t).QueryRow(context.Background(),
		`SELECT count(*) FROM orders WHERE user_id = $1 AND status <> 0`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// shopVisibleOrderCount 数**整家店**可见的订单数（同样绕过 RLS）。
func shopVisibleOrderCount(t *testing.T, merchantCode string) int64 {
	t.Helper()
	var n int64
	err := admin(t).QueryRow(context.Background(), `
		SELECT count(*) FROM orders o JOIN merchants m ON m.id = o.merchant_id
		 WHERE m.code = $1 AND o.status <> 0`, merchantCode).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// placeOrderFor 让某个买家下一单，返回单号。
func placeOrderFor(t *testing.T, bearer, addressName, tag string) string {
	t.Helper()
	addr := addressIDOf(t, "shop-a", addressName)
	sku, _ := anySKUWithStock(t, "shop-a", 2)
	w := createOrder(t, hostA, orderBody(addr, sku, 1, ""), bearer, tag+"-"+uniqueKey())
	if w.Code != http.StatusCreated {
		t.Fatalf("下单失败：%d %s", w.Code, w.Body.String())
	}
	var o api.Order
	if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	return o.OrderNo
}

// ---------------------------------------------------------------------------
// 买家之间的隔离。这一组的主角。
// ---------------------------------------------------------------------------

// 「我的订单」只返回我自己的单，而且 total 也只数我自己的。
//
// 三条断言各挡一类失效：
//
//   - items 里没有别人的单号 —— 挡「列表不过滤」；
//   - total 等于库里这个买家的可见单数 —— 挡「列表过滤了、计数没过滤」。
//     这一类最阴：页面上看不出任何异常，只有页脚那个总数在告诉每个买家
//     这家店一共有多少单，而它甚至不需要读到任何一行别人的数据；
//   - 全店可见单数**严格大于**我的 —— 阳性对照。两个数相等的话，
//     上面两条对着一个「根本没过滤」的实现也是绿的。
func TestMyOrdersOnlyShowsMyOwnOrders(t *testing.T) {
	tok1, tok2 := tokenA(t), tokenA2(t)
	mine := placeOrderFor(t, tok1, seedAddressA, "isolate1")
	theirs := placeOrderFor(t, tok2, seedAddress2, "isolate2")

	me := userIDOf(t, "shop-a", seedPhone)
	wantMine := visibleOrderCount(t, me)
	wantShop := shopVisibleOrderCount(t, "shop-a")
	if wantShop <= wantMine {
		t.Fatalf("全店可见单数 %d 不比我的 %d 多 —— 第二个买家的单没进去，"+
			"这条测试没有区分力", wantShop, wantMine)
	}

	w, list := myOrders(t, tok1, "?page_size=100")
	if w.Code != http.StatusOK {
		t.Fatalf("我的订单失败：%d %s", w.Code, w.Body.String())
	}

	seen := map[string]bool{}
	for _, o := range list.Items {
		seen[o.OrderNo] = true
	}
	if !seen[mine] {
		t.Fatalf("我自己的单 %s 不在我的订单里 —— 阳性对照失败", mine)
	}
	if seen[theirs] {
		t.Fatalf("另一个买家的单 %s 出现在我的订单里 —— "+
			"同一家店里的买家越权，RLS 管不到这一层", theirs)
	}
	if list.Total != wantMine {
		t.Fatalf("total 是 %d，库里我名下可见的订单有 %d 笔（全店 %d 笔）—— "+
			"total 没有按买家过滤的话，页脚会把全店的单量告诉每一个买家",
			list.Total, wantMine, wantShop)
	}
	t.Logf("我有 %d 笔（全店 %d 笔），列表与 total 都只给了我自己的", wantMine, wantShop)
}

// 别人的订单详情是 404，而它真的存在（由它自己的主人证明）。
//
// 阳性对照必须在同一条测试里：单独断言一个 404 证明不了任何事 ——
// 一个把所有单号都回 404 的实现（比如路由挂错了）同样能让它绿。
func TestAnotherBuyersOrderDetailIs404(t *testing.T) {
	tok1, tok2 := tokenA(t), tokenA2(t)
	theirs := placeOrderFor(t, tok2, seedAddress2, "detail-isolate")

	// 阳性对照：这一单对它自己的主人是 200。
	if w, d := orderDetail(t, tok2, theirs); w.Code != http.StatusOK {
		t.Fatalf("阳性对照失败：主人自己读 %s 回了 %d %s", theirs, w.Code, w.Body.String())
	} else if d.OrderNo != theirs {
		t.Fatalf("详情回的是 %s，请求的是 %s", d.OrderNo, theirs)
	}

	w, _ := orderDetail(t, tok1, theirs)
	if w.Code != http.StatusNotFound {
		t.Fatalf("读别人的订单 %s 回了 %d，期望 404：%s", theirs, w.Code, w.Body.String())
	}
	p := problemOf(t, w, http.StatusNotFound)
	t.Logf("如期 404：%s / %s", p.Type, p.Title)
}

// ---------------------------------------------------------------------------
// 分页：语义必须与 /products 一致
// ---------------------------------------------------------------------------

// total 是全部条数，不是本页条数；越界的页回空页而不是报错。
//
// 「total = len(items)」是这条接口最容易写出来的 bug，而且它只在
// 「总数 > 一页」时才看得出来 —— 所以这条测试先确保这个买家至少有 3 笔单，
// 再用 page_size=1 去问。
func TestMyOrdersTotalIsEveryOrderNotJustThisPage(t *testing.T) {
	tok := tokenA2(t)
	uid := userIDOf(t, "shop-a", seedPhone2)
	for visibleOrderCount(t, uid) < 3 {
		placeOrderFor(t, tok, seedAddress2, "total")
	}
	want := visibleOrderCount(t, uid)

	w, list := myOrders(t, tok, "?page=1&page_size=1")
	if w.Code != http.StatusOK {
		t.Fatalf("我的订单失败：%d %s", w.Code, w.Body.String())
	}
	if len(list.Items) != 1 {
		t.Fatalf("page_size=1 却回了 %d 条", len(list.Items))
	}
	if list.Total != want {
		t.Fatalf("total 是 %d，库里有 %d 笔 —— total 数的是本页条数，不是全部",
			list.Total, want)
	}
	if want < 3 {
		t.Fatalf("这个买家只有 %d 笔订单 —— total 与本页条数分不开，这条测试没有区分力", want)
	}

	// 越界的一页：空 items，但 total 不变，也不是错误。
	w2, far := myOrders(t, tok, "?page=9999&page_size=20")
	if w2.Code != http.StatusOK {
		t.Fatalf("越界页回了 %d，期望 200 空页：%s", w2.Code, w2.Body.String())
	}
	if len(far.Items) != 0 {
		t.Fatalf("第 9999 页回了 %d 条", len(far.Items))
	}
	if far.Total != want {
		t.Fatalf("越界页的 total 是 %d，期望 %d", far.Total, want)
	}
	t.Logf("total=%d，一页 1 条，第 9999 页是空的而不是错误", want)
}

// 分页参数的钳制与 /products 一字不差（它们共用 service.clampPaging）。
func TestMyOrdersPagingIsClampedLikeProducts(t *testing.T) {
	tok := tokenA2(t)
	for _, tc := range []struct {
		query              string
		wantPage, wantSize int
	}{
		{"", 1, 20},
		{"?page=0&page_size=0", 1, 20},
		{"?page=-3&page_size=-1", 1, 20},
		{"?page=1&page_size=100000", 1, 100},
		{"?page=abc&page_size=xyz", 1, 20},
	} {
		w, list := myOrders(t, tok, tc.query)
		if w.Code != http.StatusOK {
			t.Fatalf("%q 回了 %d：%s", tc.query, w.Code, w.Body.String())
		}
		if list.Page != tc.wantPage || list.PageSize != tc.wantSize {
			t.Fatalf("%q 回显 page=%d page_size=%d，期望 %d / %d —— "+
				"回显原始输入会让客户端按一个它其实没拿到的页长算总页数",
				tc.query, list.Page, list.PageSize, tc.wantPage, tc.wantSize)
		}
		if len(list.Items) > tc.wantSize {
			t.Fatalf("%q 回了 %d 条，超过钳制后的页长 %d", tc.query, len(list.Items), tc.wantSize)
		}
	}
}

// status 与 refund_status 两个筛选真的筛了，而且 total 跟着筛。
//
// 断言「筛出来的每一条都符合条件」加上「换一个状态就筛不到这一单」：
// 只断言前者的话，一个把筛选整个忽略的实现在这个买家只有待支付订单时照样绿。
func TestMyOrdersStatusFilterReallyFilters(t *testing.T) {
	tok := tokenA2(t)
	no := placeOrderFor(t, tok, seedAddress2, "filter")

	w, pending := myOrders(t, tok, "?status=10&page_size=100")
	if w.Code != http.StatusOK {
		t.Fatalf("按状态筛失败：%d %s", w.Code, w.Body.String())
	}
	found := false
	for _, o := range pending.Items {
		if o.Status != 10 {
			t.Fatalf("status=10 的结果里出现了状态 %d 的订单 %s", o.Status, o.OrderNo)
		}
		if o.OrderNo == no {
			found = true
		}
	}
	if !found {
		t.Fatalf("刚下的待支付订单 %s 不在 status=10 的结果里 —— 阳性对照失败", no)
	}

	// 换一个这一单不可能处于的状态：它必须消失，而 total 也要跟着变。
	_, shipped := myOrders(t, tok, "?status=30&page_size=100")
	for _, o := range shipped.Items {
		if o.OrderNo == no {
			t.Fatalf("status=30 的结果里出现了待支付的订单 %s —— 筛选被忽略了", no)
		}
	}
	if shipped.Total >= pending.Total {
		t.Fatalf("status=30 的 total 是 %d，status=10 的是 %d —— total 没跟着筛",
			shipped.Total, pending.Total)
	}

	// refund_status=0（无退款）必须能筛出东西 —— 它是一个合法且常用的筛选值，
	// 不是「不筛」。把它和零值压在一起的实现会让「我的订单-正常」这个入口失效。
	_, norefund := myOrders(t, tok, "?refund_status=0&page_size=100")
	if norefund.Total == 0 {
		t.Fatal("refund_status=0 筛出 0 笔 —— 这个买家的订单都是无退款的，" +
			"说明这个参数被当成了「不筛」之外的什么东西")
	}
}

// 状态 0「创建中」的订单不出现在任何响应里。
//
// 它是 SAGA 还没跑完的中间态。一旦漏出去，用户会看到一笔既不能支付也不能取消的
// 订单，而它可能在下一秒被补偿关掉。
//
// 靶子用管理员连接直接插：正常链路造不出一笔**停留**在 0 的订单
// （SAGA 要么把它推到 10，要么补偿到 90），而一条没有靶子的断言是空转的。
func TestDraftOrdersNeverShowUp(t *testing.T) {
	tok := tokenA(t)
	uid := userIDOf(t, "shop-a", seedPhone)
	mid := merchantIDOf(t, "shop-a")
	no := "DRAFT-" + uniqueKey()

	ctx := context.Background()
	conn := admin(t)
	if _, err := conn.Exec(ctx, `
		INSERT INTO orders (merchant_id, order_no, user_id, status, goods_amount_cents,
		                    payable_cents, receiver_snapshot, expire_at)
		VALUES ($1, $2, $3, 0, 1990, 1990, '{}'::jsonb, now() + interval '30 minutes')`,
		mid, no, uid); err != nil {
		t.Fatalf("插入草稿订单失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(),
			`DELETE FROM orders WHERE order_no = $1`, no); err != nil {
			t.Errorf("清理草稿订单 %s 失败: %v", no, err)
		}
	})

	// 阳性对照：这一行**真的在库里**，而且属于这个买家。
	// 没有它，下面两条断言对着一次失败的 INSERT 也是绿的。
	if got := orderStatusOf(t, no); got != 0 {
		t.Fatalf("草稿订单 %s 在库里的状态是 %d，期望 0 —— 靶子没造出来", no, got)
	}

	_, list := myOrders(t, tok, "?page_size=100")
	for _, o := range list.Items {
		if o.OrderNo == no {
			t.Fatalf("状态 0 的草稿订单 %s 出现在我的订单里", no)
		}
	}
	if w, _ := orderDetail(t, tok, no); w.Code != http.StatusNotFound {
		t.Fatalf("草稿订单 %s 的详情回了 %d，期望 404：%s", no, w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 详情的内容
// ---------------------------------------------------------------------------

// 详情带收货快照与订单行，而且行的内容与库里逐列对得上。
func TestOrderDetailCarriesSnapshotAndItems(t *testing.T) {
	tok := tokenA(t)
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku, _ := anySKUWithStock(t, "shop-a", 3)
	w := createOrder(t, hostA, orderBody(addr, sku, 2, ""), tok, "detail-"+uniqueKey())
	if w.Code != http.StatusCreated {
		t.Fatalf("下单失败：%d %s", w.Code, w.Body.String())
	}
	var created api.Order
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	dw, d := orderDetail(t, tok, created.OrderNo)
	if dw.Code != http.StatusOK {
		t.Fatalf("详情失败：%d %s", dw.Code, dw.Body.String())
	}

	// Order 那一部分必须与建单响应逐字段一致。详情自己抄一遍 apiOrder 的字段，
	// 抄漏一个的症状是「列表里有 payable_cents、详情里没有」——
	// 契约里它们都是可选字段，漏掉不会有任何编译错误。
	if d.PayableCents != created.PayableCents || d.Status != created.Status ||
		d.RefundStatus != created.RefundStatus {
		t.Fatalf("详情的 Order 部分与建单响应对不上：详情 %d/%d/%d，建单 %d/%d/%d",
			d.PayableCents, d.Status, d.RefundStatus,
			created.PayableCents, created.Status, created.RefundStatus)
	}

	if d.Receiver == nil {
		t.Fatal("详情里没有 receiver —— 订单详情页要展示收货信息")
	}
	if d.Receiver.ReceiverName != seedAddressA {
		t.Fatalf("收货人是 %q，期望 %q", d.Receiver.ReceiverName, seedAddressA)
	}

	if d.Items == nil || len(*d.Items) != 1 {
		t.Fatalf("详情里的订单行是 %v，期望 1 行", d.Items)
	}
	it := (*d.Items)[0]
	if it.SkuId != sku {
		t.Fatalf("订单行的 sku_id 是 %d，下单的是 %d", it.SkuId, sku)
	}
	if it.Quantity != 2 {
		t.Fatalf("订单行的数量是 %d，下单的是 2", it.Quantity)
	}

	// 与库里逐列对：下单时的快照是不是真的拍下来了。
	var wantTitle string
	var wantPrice, wantAmount int64
	var wantQty int32
	err := admin(t).QueryRow(context.Background(), `
		SELECT i.title_snapshot, i.price_cents, i.amount_cents, i.quantity
		  FROM order_items i JOIN orders o ON o.id = i.order_id
		 WHERE o.order_no = $1`, created.OrderNo).Scan(&wantTitle, &wantPrice, &wantAmount, &wantQty)
	if err != nil {
		t.Fatalf("读订单行失败: %v", err)
	}
	if it.Title != wantTitle || int64(it.PriceCents) != wantPrice ||
		it.AmountCents == nil || int64(*it.AmountCents) != wantAmount || int32(it.Quantity) != wantQty {
		t.Fatalf("详情的订单行与库里对不上：详情 %q/%d/%v/%d，库里 %q/%d/%d/%d",
			it.Title, it.PriceCents, it.AmountCents, it.Quantity,
			wantTitle, wantPrice, wantAmount, wantQty)
	}

	// 还没付钱，所以 payments 是空的（不是 nil：契约里它是数组，
	// 而「这一单还没有任何支付尝试」的诚实形状是空数组 —— 我们**查过了**）。
	if d.Payments == nil {
		t.Fatal("详情里没有 payments 这个键 —— 它是查过的，空数组才是实情")
	}
	if len(*d.Payments) != 0 {
		t.Fatalf("还没付钱，payments 却有 %d 条", len(*d.Payments))
	}

	// paid_at 必须缺席（还没付）。它是新补上的三个时间戳之一。
	if d.PaidAt != nil {
		t.Fatalf("还没付钱，paid_at 却是 %v", d.PaidAt)
	}
}

// 详情不声称自己知道退款。
//
// 退款域的三张表本轮没建，所以 refunds 是「没查过」，不是「没有退款」。
// 这条同时是 contract_test.go 那份挂账的反向守卫。
func TestOrderDetailDoesNotClaimRefundsItDoesNotHave(t *testing.T) {
	tok := tokenA(t)
	no := placeOrderFor(t, tok, seedAddressA, "refundgap")

	w := getAuth(t, hostA, "/api/v1/orders/"+no, tok)
	if w.Code != http.StatusOK {
		t.Fatalf("详情失败：%d %s", w.Code, w.Body.String())
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if _, present := m["refunds"]; present {
		t.Fatalf("响应里出现了 refunds（%s）—— 退款域真的实现了？"+
			"那就把 contract_test.go 里 /orders/{order_no} 的 "+
			"NotYetImplementedResponse 那一行删掉", m["refunds"])
	}
	r := routeOf(t, http.MethodGet, "/orders/{order_no}")
	if _, listed := r.NotYetImplementedResponse["refunds"]; !listed {
		t.Fatal("NotYetImplementedResponse 里没有 refunds —— 挂账清单烂了")
	}
	// 阳性对照：payments 在（它是查过的），所以「refunds 不在」不是因为
	// 整个响应体是空的。
	if _, ok := m["payments"]; !ok {
		t.Fatalf("响应里连 payments 都没有 —— 这条断言没有区分力：%s", w.Body.String())
	}
}

// 两条读接口都要令牌。
//
// 少了这一条，「订单只能看见自己的」那套断言全都建立在一个可以被绕过的前提上：
// 没有令牌就没有 user_id，而 service 那边 auth.FromContext 刻意不回落到任何
// 默认用户 —— 但那句话要有测试盯着，否则哪天有人给它加个「取不到就用 0」的兜底。
func TestOrderReadEndpointsNeedABearerToken(t *testing.T) {
	no := placeOrderFor(t, tokenA(t), seedAddressA, "noauth")

	for _, path := range []string{"/api/v1/orders", "/api/v1/orders/" + no} {
		w := getAuth(t, hostA, path, "")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s 不带令牌回了 %d，期望 401：%s", path, w.Code, w.Body.String())
		}
	}

	// 别家店的令牌同样不行（它在这家店不是一个买家）。
	cross := login(t, hostB, seedPhone, seedPassword).AccessToken
	w := getAuth(t, hostA, "/api/v1/orders", cross)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("拿 shop-b 的令牌读 shop-a 的订单回了 %d，期望 401：%s", w.Code, w.Body.String())
	}
}
