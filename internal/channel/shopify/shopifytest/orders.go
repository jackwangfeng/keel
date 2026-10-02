package shopifytest

// 订单部分：订单、fulfillment order、fulfillment、退款与订单回调。形状照 Admin GraphQL 2026-10
// （第三期计划「已核实的事实」）：
//
//   - 下单时照真实平台从分到的 location 的 available 里减掉数量（只减 tracked 且在该 location 有库存的变体）。
//   - 每条行分到一个 location；同一 location 的行进同一张 fulfillment order。
//   - fulfillmentCreate 只认 OPEN / IN_PROGRESS 的 FO，行数量不得超过剩余；全发完 FO 变 CLOSED，发了一部分变 IN_PROGRESS。
//     fulfillmentOrderLineItems 不给 = 该 FO 的全部剩余行（同真实平台）。
//   - 每次变化 updatedAt 加 1 秒（全店一个时钟，单调）；FreezeClock 之后不再加（同一秒里的多次变化）。
//
// 以下是近似、未在开发店上实测：取消后 FO 变 CLOSED、行的 currentQuantity 归 0；
// currentTotalPriceSet = 下单总价 − 已退金额（下限 0）；displayFulfillmentStatus 只取 UNFULFILLED / PARTIALLY_FULFILLED / FULFILLED。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// OrderSpec 是往模拟店里放的一张订单。金额是「元」字符串（"12.34"），空 = 0。
type OrderSpec struct {
	Name      string // 空 = "#1001" 起自动编号
	Location  string // 行没写 Location 时分到这个 location
	Lines     []OrderLineSpec
	Financial string // displayFinancialStatus，空 = PAID
	Shipping  string
	Discount  string
	Tax       string
	Currency  string   // 空 = USD
	Address   *Address // nil = shippingAddress 为 null
	Phone     string
	Email     string
	Test      bool
}

// OrderLineSpec 是一行。Price 空 = 变体当前价。Location 空 = OrderSpec.Location。
type OrderLineSpec struct {
	Variant  string
	Qty      int32
	Price    string
	Location string
}

// Address 是收货地址（MailingAddress 的子集）。
type Address struct {
	Name, Phone, Address1, Address2, City, Province, Zip, CountryCode string
}

// RefundLine 是退款里的一行。LineItem 可以是行 gid，也可以是变体 gid（取该变体的第一行）。
type RefundLine struct {
	LineItem string
	Qty      int32
}

// Tracking 是一条 fulfillment 的物流信息。
type Tracking struct {
	Company, Number string
}

// FulfillmentOrderInfo 是一张 FO 的概况（测试断言用）。
type FulfillmentOrderInfo struct {
	ID, Location, Status string
}

type simOrder struct {
	gid, name               string
	createdAt, updatedAt    time.Time
	cancelledAt             *time.Time
	financial, currency     string
	shipping, discount, tax int64
	refunded                int64
	test                    bool
	addr                    *Address
	phone, email            string
	lines                   []*lineItem
	fos                     []*fulfillmentOrder
	fulfillments            []*fulfillment
	refunds                 []*refund
}

type lineItem struct {
	gid, title, variantGID, location string
	sku                              string
	qty, current                     int32
	price                            int64
}

type fulfillmentOrder struct {
	gid, location, status string
	lines                 []*foLine
}

type foLine struct {
	gid       string
	li        *lineItem
	remaining int32
}

type fulfillment struct {
	gid      string
	tracking Tracking
	at       time.Time
}

type refund struct {
	gid    string
	at     time.Time
	amount int64
	lines  []refundLineRec
}

type refundLineRec struct {
	li          *lineItem
	qty         int32
	restockType string
}

var simEpoch = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// tick 推进全店时钟 1 秒并返回新时刻（FreezeClock 之后不推进）。
func (s *Server) tick() time.Time {
	if s.clock.IsZero() {
		s.clock = simEpoch
	}
	if !s.frozen {
		s.clock = s.clock.Add(time.Second)
	}
	return s.clock
}

// FreezeClock 让全店时钟停住（再推进一次到新的一秒后停）：之后的变化 updatedAt 都相同。
// 真实平台的 updatedAt 只到秒（开发店实测 "2026-10-02T09:37:29Z"），同一秒里的两次变化就是这样。
func (s *Server) FreezeClock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tick()
	s.frozen = true
}

func numID(gid string) int64 {
	n, _ := strconv.ParseInt(gid[strings.LastIndex(gid, "/")+1:], 10, 64)
	return n
}

// cents 把 "12.34" 解成 1234。空串 = 0；格式不对 panic（测试数据写错了）。
func cents(s string) int64 {
	if s == "" {
		return 0
	}
	whole, frac, _ := strings.Cut(s, ".")
	frac = (frac + "00")[:2]
	w, err1 := strconv.ParseInt(whole, 10, 64)
	f, err2 := strconv.ParseInt(frac, 10, 64)
	if err1 != nil || err2 != nil {
		panic(fmt.Sprintf("shopifytest：金额 %q 格式不对", s))
	}
	return w*100 + f
}

func money(c int64) string { return fmt.Sprintf("%d.%02d", c/100, c%100) }

// AddOrder 放一张订单，返回订单 gid。照真实平台从分到的 location 的 available 里减掉数量。
func (s *Server) AddOrder(sp OrderSpec) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.orders == nil {
		s.orders = map[string]*simOrder{}
	}
	now := s.tick()
	o := &simOrder{gid: s.id("Order"), name: sp.Name, createdAt: now, updatedAt: now, financial: sp.Financial,
		currency: sp.Currency, shipping: cents(sp.Shipping), discount: cents(sp.Discount), tax: cents(sp.Tax),
		test: sp.Test, addr: sp.Address, phone: sp.Phone, email: sp.Email}
	if o.name == "" {
		o.name = fmt.Sprintf("#%d", 1001+len(s.orderSeq))
	}
	if o.financial == "" {
		o.financial = "PAID"
	}
	if o.currency == "" {
		o.currency = "USD"
	}
	byLoc := map[string]*fulfillmentOrder{}
	for _, l := range sp.Lines {
		v := s.variants[l.Variant]
		if v == nil {
			panic(fmt.Sprintf("shopifytest：AddOrder 的变体 %s 不存在", l.Variant))
		}
		loc := l.Location
		if loc == "" {
			loc = sp.Location
		}
		price := l.Price
		if price == "" {
			price = v.v.Price
		}
		li := &lineItem{gid: s.id("LineItem"), title: s.products[v.productGID].p.Title, variantGID: v.gid, location: loc,
			sku: v.v.SKU, qty: l.Qty, current: l.Qty, price: cents(price)}
		o.lines = append(o.lines, li)
		if it := s.items[v.itemGID]; it != nil && it.tracked {
			if cur, ok := it.levels[loc]; ok {
				it.levels[loc] = cur - l.Qty
			}
		}
		fo := byLoc[loc]
		if fo == nil {
			fo = &fulfillmentOrder{gid: s.id("FulfillmentOrder"), location: loc, status: "OPEN"}
			byLoc[loc] = fo
			o.fos = append(o.fos, fo)
		}
		fo.lines = append(fo.lines, &foLine{gid: s.id("FulfillmentOrderLineItem"), li: li, remaining: l.Qty})
	}
	s.orders[o.gid] = o
	s.orderSeq = append(s.orderSeq, o.gid)
	return o.gid
}

func (s *Server) mustOrder(gid string) *simOrder {
	o := s.orders[gid]
	if o == nil {
		panic("shopifytest：没有订单 " + gid)
	}
	return o
}

func (o *simOrder) total() int64 {
	var g int64
	for _, l := range o.lines {
		g += l.price * int64(l.qty)
	}
	return g - o.discount + o.shipping + o.tax
}

// SetFinancial 改订单的 displayFinancialStatus（比如 AUTHORIZED → PAID）。
func (s *Server) SetFinancial(orderID, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.mustOrder(orderID)
	o.financial, o.updatedAt = status, s.tick()
}

// Cancel 在 Shopify 后台取消订单：已付款的全额退款（financial → REFUNDED），未付款的 VOIDED；
// 未发货的 FO 关掉；restock 为真时把未发的数量放回各自 location。
func (s *Server) Cancel(orderID string, restock bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.mustOrder(orderID)
	now := s.tick()
	o.cancelledAt, o.updatedAt = &now, now
	rf := &refund{gid: s.id("Refund"), at: now}
	unfulfilled := map[*lineItem]int32{}
	for _, fo := range o.fos {
		for _, fl := range fo.lines {
			unfulfilled[fl.li] += fl.remaining
			fl.remaining = 0
		}
		if fo.status == "OPEN" || fo.status == "IN_PROGRESS" {
			fo.status = "CLOSED"
		}
	}
	for _, li := range o.lines {
		if li.current == 0 {
			continue
		}
		rt := "NO_RESTOCK"
		if restock {
			rt = "CANCEL"
			s.restockLocked(li, unfulfilled[li])
		}
		rf.lines = append(rf.lines, refundLineRec{li: li, qty: li.current, restockType: rt})
		li.current = 0
	}
	switch o.financial {
	case "PAID", "PARTIALLY_PAID", "PARTIALLY_REFUNDED":
		rf.amount = o.total() - o.refunded
		o.refunded += rf.amount
		o.financial = "REFUNDED"
		o.refunds = append(o.refunds, rf)
	case "PENDING", "AUTHORIZED":
		o.financial = "VOIDED"
	}
}

func (s *Server) restockLocked(li *lineItem, qty int32) {
	if qty <= 0 {
		return
	}
	v := s.variants[li.variantGID]
	if v == nil {
		return
	}
	if it := s.items[v.itemGID]; it != nil && it.tracked {
		if cur, ok := it.levels[li.location]; ok {
			it.levels[li.location] = cur + qty
		}
	}
}

// Refund 在 Shopify 后台退款，返回退款 gid。amount 空 = 退掉的行价之和。
// 退掉的行从 currentQuantity 与 FO 剩余里减掉（先减未发的）；FO 剩余全为 0 时关掉。
// restock 为真时未发的部分放回 location（restockType CANCEL），已发的部分算退货（RETURN，也放回）。
func (s *Server) Refund(orderID string, lines []RefundLine, amount string, restock bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.mustOrder(orderID)
	now := s.tick()
	o.updatedAt = now
	rf := &refund{gid: s.id("Refund"), at: now}
	var linesTotal int64
	for _, rl := range lines {
		li := o.findLine(rl.LineItem)
		if li == nil {
			panic("shopifytest：订单里没有行 " + rl.LineItem)
		}
		q := min(rl.Qty, li.current)
		li.current -= q
		linesTotal += li.price * int64(q)
		var fromOpen int32
		for _, fo := range o.fos {
			for _, fl := range fo.lines {
				if fl.li == li && fl.remaining > 0 && fromOpen < q {
					d := min(fl.remaining, q-fromOpen)
					fl.remaining -= d
					fromOpen += d
				}
			}
		}
		rt := "NO_RESTOCK"
		if restock {
			rt = "CANCEL"
			if fromOpen < q {
				rt = "RETURN"
			}
			s.restockLocked(li, q)
		}
		rf.lines = append(rf.lines, refundLineRec{li: li, qty: q, restockType: rt})
	}
	for _, fo := range o.fos {
		if (fo.status == "OPEN" || fo.status == "IN_PROGRESS") && fo.remaining() == 0 {
			fo.status = "CLOSED"
		}
	}
	rf.amount = linesTotal
	if amount != "" {
		rf.amount = cents(amount)
	}
	o.refunded += rf.amount
	if o.refunded >= o.total() {
		o.financial = "REFUNDED"
	} else {
		o.financial = "PARTIALLY_REFUNDED"
	}
	o.refunds = append(o.refunds, rf)
	return rf.gid
}

func (o *simOrder) findLine(id string) *lineItem {
	for _, l := range o.lines {
		if l.gid == id {
			return l
		}
	}
	for _, l := range o.lines {
		if l.variantGID == id {
			return l
		}
	}
	return nil
}

func (fo *fulfillmentOrder) remaining() int32 {
	var n int32
	for _, fl := range fo.lines {
		n += fl.remaining
	}
	return n
}

// FulfillInShopify 模拟店员在 Shopify 后台把所有可发的 FO 全部发掉（一条 fulfillment）。没有可发的返回空串。
func (s *Server) FulfillInShopify(orderID, company, number string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.mustOrder(orderID)
	var reqs []foReq
	for _, fo := range o.fos {
		if fo.status == "OPEN" || fo.status == "IN_PROGRESS" {
			reqs = append(reqs, foReq{fo: fo})
		}
	}
	if len(reqs) == 0 {
		return ""
	}
	return s.fulfillLocked(o, reqs, Tracking{Company: company, Number: number}).gid
}

type foReq struct {
	fo    *fulfillmentOrder
	lines map[*foLine]int32 // nil = 全部剩余
}

func (s *Server) fulfillLocked(o *simOrder, reqs []foReq, t Tracking) *fulfillment {
	now := s.tick()
	for _, r := range reqs {
		for _, fl := range r.fo.lines {
			if r.lines == nil {
				fl.remaining = 0
			} else {
				fl.remaining -= r.lines[fl]
			}
		}
		if r.fo.remaining() == 0 {
			r.fo.status = "CLOSED"
		} else {
			r.fo.status = "IN_PROGRESS"
		}
	}
	f := &fulfillment{gid: s.id("Fulfillment"), tracking: t, at: now}
	o.fulfillments = append(o.fulfillments, f)
	o.updatedAt = now
	return f
}

// LineItemIDs 是订单的行 gid（按下单顺序）。
func (s *Server) LineItemIDs(orderID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, l := range s.mustOrder(orderID).lines {
		out = append(out, l.gid)
	}
	return out
}

// Fulfillments 是订单上的全部 fulfillment 的物流信息（按创建顺序）。
func (s *Server) Fulfillments(orderID string) []Tracking {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Tracking
	for _, f := range s.mustOrder(orderID).fulfillments {
		out = append(out, f.tracking)
	}
	return out
}

// FulfillmentOrders 是订单的 FO 概况。
func (s *Server) FulfillmentOrders(orderID string) []FulfillmentOrderInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []FulfillmentOrderInfo
	for _, fo := range s.mustOrder(orderID).fos {
		out = append(out, FulfillmentOrderInfo{ID: fo.gid, Location: fo.location, Status: fo.status})
	}
	return out
}

// SetFulfillmentOrderStatus 改一张 FO 的状态（ON_HOLD / SCHEDULED / INCOMPLETE / OPEN …，模拟店员在后台暂停、预约等）。
func (s *Server) SetFulfillmentOrderStatus(foGID, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fo, o := s.findFO(foGID)
	if fo == nil {
		panic("shopifytest：没有 fulfillment order " + foGID)
	}
	fo.status, o.updatedAt = status, s.tick()
}

// UpdatedAt 是订单当前的 updatedAt。
func (s *Server) UpdatedAt(orderID string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mustOrder(orderID).updatedAt
}

// FailNext 让操作 op（operationName）的下一次调用回 HTTP 503。afterApply 为真时先照常落地再回 503
// （模拟「平台做了、响应丢了」）。可以连着排多次。
func (s *Server) FailNext(op string, afterApply bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail == nil {
		s.fail = map[string][]bool{}
	}
	s.fail[op] = append(s.fail[op], afterApply)
}

// WebhookFor 造一条签好名的订单类回调（目标 "/x"），返回请求与原始正文（ParseInbound 要原样的字节）。
// orders/* 的正文含 admin_graphql_api_id（订单 gid）与数字 id；refunds/create、fulfillments/create 的正文是
// 订单上最近一条退款 / fulfillment，含数字 order_id（没有 admin_graphql_api_id 指向订单）。
// 每次调用的 X-Shopify-Event-Id / X-Shopify-Webhook-Id 都不同（要测重投就复用同一个请求的头）。
func (s *Server) WebhookFor(orderID, topic string) (*http.Request, []byte) {
	s.mu.Lock()
	o := s.mustOrder(orderID)
	var body map[string]any
	switch topic {
	case "refunds/create":
		body = map[string]any{"order_id": numID(o.gid)}
		if n := len(o.refunds); n > 0 {
			rf := o.refunds[n-1]
			body["id"], body["admin_graphql_api_id"], body["created_at"] = numID(rf.gid), rf.gid, rf.at.Format(time.RFC3339)
		}
	case "fulfillments/create":
		body = map[string]any{"order_id": numID(o.gid), "status": "success"}
		if n := len(o.fulfillments); n > 0 {
			f := o.fulfillments[n-1]
			body["id"], body["admin_graphql_api_id"], body["created_at"] = numID(f.gid), f.gid, f.at.Format(time.RFC3339)
			body["tracking_company"], body["tracking_number"] = f.tracking.Company, f.tracking.Number
		}
	default:
		var cancelled any
		if o.cancelledAt != nil {
			cancelled = o.cancelledAt.Format(time.RFC3339)
		}
		var fs any
		switch o.fulfillmentStatus() {
		case "FULFILLED":
			fs = "fulfilled"
		case "PARTIALLY_FULFILLED":
			fs = "partial"
		}
		body = map[string]any{"id": numID(o.gid), "admin_graphql_api_id": o.gid, "name": o.name,
			"created_at": o.createdAt.Format(time.RFC3339), "updated_at": o.updatedAt.Format(time.RFC3339),
			"cancelled_at": cancelled, "financial_status": strings.ToLower(o.financial), "fulfillment_status": fs,
			"test": o.test, "currency": o.currency, "total_price": money(o.total())}
	}
	s.nextID++
	evt := fmt.Sprintf("sim-evt-%d", s.nextID)
	s.mu.Unlock()
	raw, _ := json.Marshal(body)
	r := s.WebhookRequest("/x", topic, evt, raw)
	r.Header.Set("X-Shopify-Webhook-Id", "sim-wh-"+strings.TrimPrefix(evt, "sim-evt-"))
	r.Header.Set("X-Shopify-API-Version", "2026-10")
	r.Header.Set("X-Shopify-Triggered-At", time.Now().UTC().Format(time.RFC3339Nano))
	return r, raw
}

func (o *simOrder) fulfillmentStatus() string {
	var rem int32
	for _, fo := range o.fos {
		rem += fo.remaining()
	}
	switch {
	case len(o.fulfillments) == 0:
		return "UNFULFILLED"
	case rem == 0:
		return "FULFILLED"
	default:
		return "PARTIALLY_FULFILLED"
	}
}

func (o *simOrder) moneySet(c int64) map[string]any {
	return map[string]any{"shopMoney": map[string]any{"amount": money(c), "currencyCode": o.currency},
		"presentmentMoney": map[string]any{"amount": money(c), "currencyCode": o.currency}}
}

func (s *Server) orderJSON(o *simOrder) map[string]any {
	var cancelled any
	if o.cancelledAt != nil {
		cancelled = o.cancelledAt.Format(time.RFC3339)
	}
	lines := []any{}
	for _, l := range o.lines {
		var sku any
		if l.sku != "" {
			sku = l.sku
		}
		var v any
		if s.variants[l.variantGID] != nil {
			v = map[string]any{"id": l.variantGID}
		}
		lines = append(lines, map[string]any{"id": l.gid, "title": l.title, "quantity": l.qty, "currentQuantity": l.current,
			"sku": sku, "variant": v, "originalUnitPriceSet": o.moneySet(l.price)})
	}
	fos := []any{}
	for _, fo := range o.fos {
		fls := []any{}
		for _, fl := range fo.lines {
			fls = append(fls, map[string]any{"id": fl.gid, "remainingQuantity": fl.remaining, "totalQuantity": fl.li.qty,
				"lineItem": map[string]any{"id": fl.li.gid}})
		}
		fos = append(fos, map[string]any{"id": fo.gid, "status": fo.status,
			"assignedLocation": map[string]any{"location": map[string]any{"id": fo.location}},
			"lineItems":        map[string]any{"nodes": fls}})
	}
	fuls := []any{}
	for _, f := range o.fulfillments {
		fuls = append(fuls, map[string]any{"id": f.gid, "status": "SUCCESS", "createdAt": f.at.Format(time.RFC3339),
			"trackingInfo": []any{map[string]any{"company": f.tracking.Company, "number": f.tracking.Number, "url": nil}}})
	}
	refunds := []any{}
	for _, rf := range o.refunds {
		rls := []any{}
		for _, rl := range rf.lines {
			rls = append(rls, map[string]any{"quantity": rl.qty, "restockType": rl.restockType, "lineItem": map[string]any{"id": rl.li.gid}})
		}
		refunds = append(refunds, map[string]any{"id": rf.gid, "createdAt": rf.at.Format(time.RFC3339),
			"totalRefundedSet": o.moneySet(rf.amount), "refundLineItems": map[string]any{"nodes": rls}})
	}
	var addr any
	if a := o.addr; a != nil {
		addr = map[string]any{"name": a.Name, "phone": nilIfEmpty(a.Phone), "address1": a.Address1, "address2": nilIfEmpty(a.Address2),
			"city": a.City, "province": a.Province, "zip": a.Zip, "countryCodeV2": a.CountryCode}
	}
	return map[string]any{"id": o.gid, "name": o.name, "createdAt": o.createdAt.Format(time.RFC3339),
		"updatedAt": o.updatedAt.Format(time.RFC3339), "test": o.test, "cancelledAt": cancelled,
		"displayFinancialStatus": o.financial, "displayFulfillmentStatus": o.fulfillmentStatus(),
		"currentTotalPriceSet": o.moneySet(max(0, o.total()-o.refunded)), "totalShippingPriceSet": o.moneySet(o.shipping),
		"totalDiscountsSet": o.moneySet(o.discount), "totalTaxSet": o.moneySet(o.tax), "totalRefundedSet": o.moneySet(o.refunded),
		"lineItems": map[string]any{"nodes": lines}, "fulfillmentOrders": map[string]any{"nodes": fos},
		"fulfillments": fuls, "refunds": refunds, "shippingAddress": addr,
		"phone": nilIfEmpty(o.phone), "email": nilIfEmpty(o.email)}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// fulfillmentCreate 的变量形状：{"fulfillment": FulfillmentInput}。
func (s *Server) fulfillmentCreate(raw json.RawMessage) (any, error) {
	var v struct {
		F struct {
			NotifyCustomer bool `json:"notifyCustomer"`
			TrackingInfo   *struct {
				Company string `json:"company"`
				Number  string `json:"number"`
				URL     string `json:"url"`
			} `json:"trackingInfo"`
			LineItemsByFulfillmentOrder []struct {
				FulfillmentOrderID        string `json:"fulfillmentOrderId"`
				FulfillmentOrderLineItems []struct {
					ID       string `json:"id"`
					Quantity int32  `json:"quantity"`
				} `json:"fulfillmentOrderLineItems"`
			} `json:"lineItemsByFulfillmentOrder"`
		} `json:"fulfillment"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	fail := func(field []string, msg string) (any, error) {
		return map[string]any{"fulfillmentCreate": map[string]any{"fulfillment": nil,
			"userErrors": []any{map[string]any{"field": field, "message": msg}}}}, nil
	}
	in := v.F.LineItemsByFulfillmentOrder
	if len(in) == 0 {
		return fail([]string{"fulfillment", "lineItemsByFulfillmentOrder"}, "Line items by fulfillment order can't be blank.")
	}
	var o *simOrder
	var reqs []foReq
	for i, x := range in {
		field := []string{"fulfillment", "lineItemsByFulfillmentOrder", strconv.Itoa(i), "fulfillmentOrderId"}
		fo, owner := s.findFO(x.FulfillmentOrderID)
		if fo == nil {
			return fail(field, "Fulfillment order does not exist.")
		}
		if o != nil && owner != o {
			return fail(field, "All fulfillment orders must belong to the same order.")
		}
		o = owner
		if fo.status != "OPEN" && fo.status != "IN_PROGRESS" {
			return fail(field, fmt.Sprintf("Fulfillment order %s has an unfulfillable status= %s.", fo.gid, strings.ToLower(fo.status)))
		}
		r := foReq{fo: fo}
		if len(x.FulfillmentOrderLineItems) > 0 {
			r.lines = map[*foLine]int32{}
			for j, li := range x.FulfillmentOrderLineItems {
				lf := []string{"fulfillment", "lineItemsByFulfillmentOrder", strconv.Itoa(i), "fulfillmentOrderLineItems", strconv.Itoa(j)}
				var fl *foLine
				for _, c := range fo.lines {
					if c.gid == li.ID {
						fl = c
					}
				}
				if fl == nil {
					return fail(append(lf, "id"), "Fulfillment order line item does not exist.")
				}
				if li.Quantity <= 0 || r.lines[fl]+li.Quantity > fl.remaining {
					return fail(append(lf, "quantity"), "Invalid fulfillment order line item quantity requested.")
				}
				r.lines[fl] += li.Quantity
			}
		} else if fo.remaining() == 0 {
			return fail(field, "Fulfillment order has no remaining line items to fulfill.")
		}
		reqs = append(reqs, r)
	}
	var t Tracking
	if v.F.TrackingInfo != nil {
		t = Tracking{Company: v.F.TrackingInfo.Company, Number: v.F.TrackingInfo.Number}
	}
	f := s.fulfillLocked(o, reqs, t)
	return map[string]any{"fulfillmentCreate": map[string]any{"fulfillment": map[string]any{"id": f.gid, "status": "SUCCESS",
		"trackingInfo": []any{map[string]any{"company": t.Company, "number": t.Number, "url": nil}}},
		"userErrors": []any{}}}, nil
}

func (s *Server) findFO(gid string) (*fulfillmentOrder, *simOrder) {
	for _, o := range s.orders {
		for _, fo := range o.fos {
			if fo.gid == gid {
				return fo, o
			}
		}
	}
	return nil, nil
}
