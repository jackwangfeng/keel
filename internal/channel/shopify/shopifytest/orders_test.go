package shopifytest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"
)

type rig struct {
	t   *testing.T
	s   *Server
	tok string
}

func newRig(t *testing.T) *rig {
	s := New(t, "sim.myshopify.com", "cid", "sec")
	body, _ := json.Marshal(map[string]string{"grant_type": "client_credentials", "client_id": "cid", "client_secret": "sec"})
	resp, err := http.Post(s.URL+"/admin/oauth/access_token", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var tk struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tk)
	return &rig{t: t, s: s, tok: tk.AccessToken}
}

// gql 发一条 GraphQL，返回 HTTP 状态与 data。
func (r *rig) gql(op string, vars any) (int, map[string]any) {
	r.t.Helper()
	body, _ := json.Marshal(map[string]any{"query": "…", "operationName": op, "variables": vars})
	req, _ := http.NewRequest(http.MethodPost, r.s.URL+"/admin/api/2026-10/graphql.json", bytes.NewReader(body))
	req.Header.Set("X-Shopify-Access-Token", r.tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Data   map[string]any `json:"data"`
		Errors any            `json:"errors"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode == http.StatusOK && out.Errors != nil {
		r.t.Fatalf("%s：%v", op, out.Errors)
	}
	return resp.StatusCode, out.Data
}

func dig(v any, path ...string) any {
	for _, p := range path {
		switch x := v.(type) {
		case map[string]any:
			v = x[p]
		case []any:
			i, _ := strconv.Atoi(p)
			v = x[i]
		default:
			return nil
		}
	}
	return v
}

// 一件 tracked 的商品（A=7）、一件不 tracked 的（B），一个 location。
func (r *rig) shop() (loc, va, vb, itemA string) {
	loc = r.s.AddLocation("Main")
	pa := r.s.AddProduct(Product{Title: "Tea", Variants: []Variant{{SKU: "T1", Price: "12.50", Tracked: true, Levels: map[string]int32{loc: 7}}}})
	pb := r.s.AddProduct(Product{Title: "Cup", Variants: []Variant{{Price: "3.00", Levels: map[string]int32{loc: 5}}}})
	va, vb = r.s.VariantIDs(pa)[0], r.s.VariantIDs(pb)[0]
	return loc, va, vb, r.s.InventoryItem(va)
}

func TestAddOrderDecrementsAvailableAndCancelRestocks(t *testing.T) {
	r := newRig(t)
	loc, va, vb, itemA := r.shop()
	o := r.s.AddOrder(OrderSpec{Location: loc, Lines: []OrderLineSpec{{Variant: va, Qty: 2}, {Variant: vb, Qty: 1}}, Shipping: "5.00", Tax: "1.25"})
	if q, _ := r.s.Available(itemA, loc); q != 5 {
		t.Fatalf("下 2 件后 A 应 7→5，实际 %d", q)
	}
	if q, _ := r.s.Available(r.s.InventoryItem(vb), loc); q != 5 {
		t.Fatalf("不 tracked 的变体不该减，实际 %d", q)
	}
	_, d := r.gql("Order", map[string]string{"id": o})
	ord := d["order"]
	if dig(ord, "name") != "#1001" || dig(ord, "displayFinancialStatus") != "PAID" || dig(ord, "displayFulfillmentStatus") != "UNFULFILLED" ||
		dig(ord, "currentTotalPriceSet", "shopMoney", "amount") != "34.25" || dig(ord, "shippingAddress") != nil ||
		dig(ord, "lineItems", "nodes", "0", "variant", "id") != va || dig(ord, "lineItems", "nodes", "1", "sku") != nil ||
		dig(ord, "fulfillmentOrders", "nodes", "0", "assignedLocation", "location", "id") != loc {
		b, _ := json.Marshal(ord)
		t.Fatalf("order 形状不对：%s", b)
	}
	u0 := r.s.UpdatedAt(o)
	r.s.Cancel(o, true)
	if q, _ := r.s.Available(itemA, loc); q != 7 {
		t.Fatalf("取消并 restock 后 A 应回到 7，实际 %d", q)
	}
	if d := r.s.UpdatedAt(o).Sub(u0); d.Seconds() != 1 {
		t.Fatalf("每次变化 updatedAt +1s，实际 +%s", d)
	}
	_, d = r.gql("Order", map[string]string{"id": o})
	ord = d["order"]
	if dig(ord, "cancelledAt") == nil || dig(ord, "displayFinancialStatus") != "REFUNDED" || dig(ord, "fulfillmentOrders", "nodes", "0", "status") != "CLOSED" ||
		dig(ord, "refunds", "0", "totalRefundedSet", "shopMoney", "amount") != "34.25" ||
		dig(ord, "refunds", "0", "refundLineItems", "nodes", "0", "restockType") != "CANCEL" {
		b, _ := json.Marshal(ord)
		t.Fatalf("取消后：%s", b)
	}
	if _, d := r.gql("Order", map[string]string{"id": "gid://shopify/Order/1"}); d["order"] != nil {
		t.Fatalf("没有的单应回 null")
	}
}

func TestRefundPartial(t *testing.T) {
	r := newRig(t)
	loc, va, _, itemA := r.shop()
	o := r.s.AddOrder(OrderSpec{Location: loc, Lines: []OrderLineSpec{{Variant: va, Qty: 3}}, Address: &Address{Name: "Ann", City: "NYC", CountryCode: "US"}})
	r.s.Refund(o, []RefundLine{{LineItem: va, Qty: 1}}, "", true)
	if q, _ := r.s.Available(itemA, loc); q != 5 {
		t.Fatalf("退 1 件 restock：7-3+1=5，实际 %d", q)
	}
	_, d := r.gql("Order", map[string]string{"id": o})
	ord := d["order"]
	if dig(ord, "displayFinancialStatus") != "PARTIALLY_REFUNDED" || dig(ord, "totalRefundedSet", "shopMoney", "amount") != "12.50" ||
		dig(ord, "lineItems", "nodes", "0", "currentQuantity") != 2.0 || dig(ord, "fulfillmentOrders", "nodes", "0", "lineItems", "nodes", "0", "remainingQuantity") != 2.0 ||
		dig(ord, "shippingAddress", "countryCodeV2") != "US" {
		b, _ := json.Marshal(ord)
		t.Fatalf("部分退款后：%s", b)
	}
}

func TestFulfillmentCreateValidates(t *testing.T) {
	r := newRig(t)
	loc, va, _, _ := r.shop()
	o := r.s.AddOrder(OrderSpec{Location: loc, Lines: []OrderLineSpec{{Variant: va, Qty: 3}}})
	fo := r.s.FulfillmentOrders(o)[0].ID
	_, d := r.gql("Order", map[string]string{"id": o})
	fl := dig(d["order"], "fulfillmentOrders", "nodes", "0", "lineItems", "nodes", "0", "id").(string)

	create := func(foID string, lines []map[string]any) map[string]any {
		in := map[string]any{"fulfillmentOrderId": foID}
		if lines != nil {
			in["fulfillmentOrderLineItems"] = lines
		}
		_, d := r.gql("FulfillmentCreate", map[string]any{"fulfillment": map[string]any{"notifyCustomer": true,
			"trackingInfo": map[string]string{"company": "UPS", "number": "1Z"}, "lineItemsByFulfillmentOrder": []any{in}}})
		return d["fulfillmentCreate"].(map[string]any)
	}
	errsOf := func(m map[string]any) int { return len(m["userErrors"].([]any)) }

	if m := create("gid://shopify/FulfillmentOrder/1", nil); errsOf(m) != 1 || m["fulfillment"] != nil {
		t.Fatalf("不存在的 FO 应报 userError：%v", m)
	}
	if m := create(fo, []map[string]any{{"id": fl, "quantity": 4}}); errsOf(m) != 1 {
		t.Fatalf("超量应报 userError：%v", m)
	}
	if m := create(fo, []map[string]any{{"id": fl, "quantity": 1}}); errsOf(m) != 0 || dig(m, "fulfillment", "id") == nil {
		t.Fatalf("发 1 件应成功：%v", m)
	}
	if st := r.s.FulfillmentOrders(o)[0].Status; st != "IN_PROGRESS" {
		t.Fatalf("发一部分后 FO 应 IN_PROGRESS，实际 %s", st)
	}
	if m := create(fo, nil); errsOf(m) != 0 {
		t.Fatalf("不给行 = 发全部剩余：%v", m)
	}
	if st := r.s.FulfillmentOrders(o)[0].Status; st != "CLOSED" {
		t.Fatalf("发完 FO 应 CLOSED，实际 %s", st)
	}
	if m := create(fo, nil); errsOf(m) != 1 {
		t.Fatalf("CLOSED 的 FO 再发应报 userError：%v", m)
	}
	if n := len(r.s.Fulfillments(o)); n != 2 {
		t.Fatalf("应有 2 条 fulfillment，实际 %d", n)
	}
	_, d = r.gql("Order", map[string]string{"id": o})
	if dig(d["order"], "displayFulfillmentStatus") != "FULFILLED" || dig(d["order"], "fulfillments", "0", "trackingInfo", "0", "number") != "1Z" {
		t.Fatalf("发完后：%v", d["order"])
	}
}

func TestFailNextAfterApply(t *testing.T) {
	r := newRig(t)
	loc, va, _, _ := r.shop()
	o := r.s.AddOrder(OrderSpec{Location: loc, Lines: []OrderLineSpec{{Variant: va, Qty: 1}}})
	fo := r.s.FulfillmentOrders(o)[0].ID
	vars := map[string]any{"fulfillment": map[string]any{"lineItemsByFulfillmentOrder": []any{map[string]any{"fulfillmentOrderId": fo}}}}
	r.s.FailNext("FulfillmentCreate", true)
	if code, _ := r.gql("FulfillmentCreate", vars); code != http.StatusServiceUnavailable {
		t.Fatalf("应回 503，实际 %d", code)
	}
	if n := len(r.s.Fulfillments(o)); n != 1 || r.s.FulfillmentOrders(o)[0].Status != "CLOSED" {
		t.Fatalf("afterApply：应已落地 1 条，实际 %d", n)
	}
	r.s.FailNext("FulfillmentCreate", false)
	if code, _ := r.gql("FulfillmentCreate", vars); code != http.StatusServiceUnavailable {
		t.Fatalf("应回 503，实际 %d", code)
	}
	if n := len(r.s.Fulfillments(o)); n != 1 {
		t.Fatalf("不落地的失败不该多出 fulfillment，实际 %d", n)
	}
	if r.s.Calls("FulfillmentCreate") != 2 {
		t.Fatalf("Calls 应计 2 次")
	}
}

func TestWebhookFor(t *testing.T) {
	r := newRig(t)
	loc, va, _, _ := r.shop()
	o := r.s.AddOrder(OrderSpec{Location: loc, Lines: []OrderLineSpec{{Variant: va, Qty: 1}}})
	r.s.FulfillInShopify(o, "UPS", "1Z9")
	r.s.Refund(o, []RefundLine{{LineItem: va, Qty: 1}}, "1.00", false)
	num := json.Number(o[len("gid://shopify/Order/"):])
	for topic, check := range map[string]func(map[string]any) bool{
		"orders/paid": func(m map[string]any) bool {
			return m["admin_graphql_api_id"] == o && m["financial_status"] == "partially_refunded"
		},
		"refunds/create":      func(m map[string]any) bool { return m["order_id"] == num },
		"fulfillments/create": func(m map[string]any) bool { return m["order_id"] == num && m["tracking_number"] == "1Z9" },
	} {
		req, body := r.s.WebhookFor(o, topic)
		if req.Header.Get("X-Shopify-Hmac-Sha256") != r.s.Sign(body) || req.Header.Get("X-Shopify-Topic") != topic ||
			req.Header.Get("X-Shopify-Shop-Domain") != r.s.Shop || req.Header.Get("X-Shopify-Event-Id") == "" {
			t.Fatalf("%s 头不对：%v", topic, req.Header)
		}
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		_ = dec.Decode(&m)
		if !check(m) {
			t.Fatalf("%s 正文不对：%s", topic, body)
		}
	}
}
