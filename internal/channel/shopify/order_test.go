package shopify_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/shopify"
	"github.com/keel/keel/internal/channel/shopify/shopifytest"
)

type orderRig struct {
	rig
	loc, loc2 string
	va, vb    string
}

func newOrderRig(t *testing.T) orderRig {
	t.Helper()
	r := orderRig{rig: newRig(t)}
	r.loc, r.loc2 = r.sim.AddLocation("仓 A"), r.sim.AddLocation("仓 B")
	pg := r.sim.AddProduct(shopifytest.Product{Title: "茶具", Variants: []shopifytest.Variant{
		{SKU: "A", Price: "392.50", Tracked: true, Levels: map[string]int32{r.loc: 10, r.loc2: 10}},
		{SKU: "B", Price: "0.95", Tracked: true, Levels: map[string]int32{r.loc: 10, r.loc2: 10}},
	}})
	vs := r.sim.VariantIDs(pg)
	r.va, r.vb = vs[0], vs[1]
	return r
}

func (r orderRig) spec() shopifytest.OrderSpec {
	return shopifytest.OrderSpec{Location: r.loc, Shipping: "10.00", Discount: "5.00", Tax: "62.88", Test: true,
		Lines: []shopifytest.OrderLineSpec{{Variant: r.va, Qty: 2}, {Variant: r.vb, Qty: 1}},
		Address: &shopifytest.Address{Name: "Ann Lee", Phone: "+12125550100", Address1: "1 Main St", Address2: "Apt 2",
			City: "New York", Province: "New York", Zip: "10001", CountryCode: "US"}}
}

func TestFetchOrderNormalizes(t *testing.T) {
	r := newOrderRig(t)
	ctx := context.Background()
	id := r.sim.AddOrder(r.spec())
	o, err := r.a.FetchOrder(ctx, r.b, id)
	if err != nil {
		t.Fatal(err)
	}
	if o.ExternalOrderID != id || o.ExternalOrderName != "#1001" || !o.Test || o.Status != channel.OrderNew {
		t.Fatalf("订单 = %+v", o)
	}
	if o.ExternalStoreID != r.loc || o.StoreError != "" {
		t.Fatalf("门店 = %q / %q，期望 %s", o.ExternalStoreID, o.StoreError, r.loc)
	}
	if o.Version != r.sim.UpdatedAt(id).UnixMilli() || o.Version == 0 {
		t.Fatalf("Version = %d，期望 updatedAt 毫秒 %d", o.Version, r.sim.UpdatedAt(id).UnixMilli())
	}
	want := channel.OrderAmounts{GoodsCents: 78595, FreightCents: 1000, MerchantSubsidyCents: 500, TaxCents: 6288,
		BuyerPaidCents: 79095, MerchantReceivableCents: 79095}
	if o.Amounts != want {
		t.Fatalf("金额 = %+v\n期望 %+v", o.Amounts, want)
	}
	lids := r.sim.LineItemIDs(id)
	if len(o.Lines) != 2 || o.Lines[0] != (channel.OrderLine{ExternalLineID: lids[0], ExternalSKUID: r.va, Title: "茶具", Qty: 2, PriceCents: 39250}) ||
		o.Lines[1].ExternalSKUID != r.vb || o.Lines[1].PriceCents != 95 {
		t.Fatalf("行 = %+v", o.Lines)
	}
	rc := o.Receiver
	if rc.Name != "Ann Lee" || rc.Phone != "+12125550100" || rc.Address != "1 Main St Apt 2" || rc.City != "New York" ||
		rc.Zip != "10001" || rc.Country != "US" {
		t.Fatalf("收货人 = %+v", rc)
	}
	if len(o.Raw) == 0 || len(o.Shipments) != 0 || len(o.Refunds) != 0 {
		t.Fatalf("Raw / Shipments / Refunds = %d / %+v / %+v", len(o.Raw), o.Shipments, o.Refunds)
	}
}

// 审查修复 3：价内税的店（taxesIncluded）—— 行价已经含税，BuyerPaid 就是顾客付的总价，税额只记下来、打上 TaxesIncluded。
func TestFetchOrderTaxesIncluded(t *testing.T) {
	r := newOrderRig(t)
	sp := r.spec()
	sp.TaxesIncluded = true
	id := r.sim.AddOrder(sp)
	o, err := r.a.FetchOrder(context.Background(), r.b, id)
	if err != nil {
		t.Fatal(err)
	}
	want := channel.OrderAmounts{GoodsCents: 78595, FreightCents: 1000, MerchantSubsidyCents: 500, TaxCents: 6288,
		BuyerPaidCents: 79095, MerchantReceivableCents: 79095, TaxesIncluded: true}
	if o.Amounts != want {
		t.Fatalf("金额 = %+v\n期望 %+v", o.Amounts, want)
	}
}

func TestFetchOrderNoAddressAndMultiLocation(t *testing.T) {
	r := newOrderRig(t)
	ctx := context.Background()
	sp := r.spec()
	sp.Address = nil
	sp.Lines[1].Location = r.loc2
	o, err := r.a.FetchOrder(ctx, r.b, r.sim.AddOrder(sp))
	if err != nil {
		t.Fatal(err)
	}
	if o.Receiver != (channel.Receiver{}) {
		t.Fatalf("shippingAddress 为 null 时收货人应全空：%+v", o.Receiver)
	}
	if o.ExternalStoreID != "" || !strings.Contains(o.StoreError, r.loc) || !strings.Contains(o.StoreError, r.loc2) {
		t.Fatalf("多个 location：门店 %q，原因 %q", o.ExternalStoreID, o.StoreError)
	}
	if _, err := r.a.FetchOrder(ctx, r.b, "gid://shopify/Order/999999"); err == nil || channel.IsRetryable(err) {
		t.Fatalf("没有的订单 → %v，期望不可重试的错误", err)
	}
}

func TestFetchOrderStatuses(t *testing.T) {
	r := newOrderRig(t)
	ctx := context.Background()
	status := func(id string) channel.ChannelOrder {
		t.Helper()
		o, err := r.a.FetchOrder(ctx, r.b, id)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	// 待付款 → 付款 → Shopify 后台发货
	sp := r.spec()
	sp.Financial = "AUTHORIZED"
	id := r.sim.AddOrder(sp)
	v1 := status(id)
	if v1.Status != channel.OrderPendingPayment {
		t.Fatalf("AUTHORIZED → %v", v1.Status)
	}
	r.sim.SetFinancial(id, "PAID")
	v2 := status(id)
	if v2.Status != channel.OrderNew || v2.Version <= v1.Version {
		t.Fatalf("PAID → %v，版本 %d → %d", v2.Status, v1.Version, v2.Version)
	}
	r.sim.FulfillInShopify(id, "UPS", "1Z999")
	v3 := status(id)
	if v3.Status != channel.OrderShipped || len(v3.Shipments) != 1 || v3.Shipments[0].Company != "UPS" ||
		v3.Shipments[0].TrackingNo != "1Z999" || v3.Shipments[0].At.IsZero() {
		t.Fatalf("发货后 = %v / %+v", v3.Status, v3.Shipments)
	}
	// 发货后部分退款仍是已发货
	r.sim.Refund(id, []shopifytest.RefundLine{{LineItem: r.vb, Qty: 1}}, "", false)
	if o := status(id); o.Status != channel.OrderShipped || len(o.Refunds) != 1 || o.Amounts.RefundedCents != 95 {
		t.Fatalf("发货后部分退款 = %v / %+v / %d", o.Status, o.Refunds, o.Amounts.RefundedCents)
	}

	// 未发货就取消
	id2 := r.sim.AddOrder(r.spec())
	r.sim.Cancel(id2, true)
	o := status(id2)
	if o.Status != channel.OrderCancelled || len(o.Refunds) != 1 || !o.Refunds[0].Restock || o.Refunds[0].AmountCents != 79095+6288 {
		t.Fatalf("取消 = %v / %+v", o.Status, o.Refunds)
	}

	// 未发货、全额退款（没取消）→ 取消；退款行按变体记
	id3 := r.sim.AddOrder(r.spec())
	r.sim.Refund(id3, []shopifytest.RefundLine{{LineItem: r.va, Qty: 2}, {LineItem: r.vb, Qty: 1}}, "854.83", false)
	o = status(id3)
	if o.Status != channel.OrderCancelled || len(o.Refunds) != 1 || o.Refunds[0].Restock {
		t.Fatalf("全额退款 = %v / %+v", o.Status, o.Refunds)
	}
	if ls := o.Refunds[0].Lines; len(ls) != 2 || ls[0] != (channel.ActionLine{ExternalSKUID: r.va, Qty: 2}) {
		t.Fatalf("退款行 = %+v", ls)
	}
	if o.Lines[0].RefundedQty != 2 || o.Lines[0].Qty != 2 {
		t.Fatalf("行的已退数量 = %+v", o.Lines[0])
	}

	// 未发货、部分退款 → 仍是新单
	id4 := r.sim.AddOrder(r.spec())
	r.sim.Refund(id4, []shopifytest.RefundLine{{LineItem: r.vb, Qty: 1}}, "", true)
	if o := status(id4); o.Status != channel.OrderNew {
		t.Fatalf("未发货部分退款 → %v", o.Status)
	}
}

func TestActShip(t *testing.T) {
	r := newOrderRig(t)
	ctx := context.Background()
	id := r.sim.AddOrder(r.spec())
	ref := channel.OrderRef{ExternalOrderID: id, ExternalStoreID: r.loc}
	ship := channel.Action{Kind: channel.ActShip, TrackingCompany: "UPS", TrackingNo: "1Z1", IdemKey: "7:ship:1"}
	if err := r.a.Act(ctx, r.b, ref, ship); err != nil {
		t.Fatal(err)
	}
	if fs := r.sim.Fulfillments(id); len(fs) != 1 || fs[0] != (shopifytest.Tracking{Company: "UPS", Number: "1Z1"}) {
		t.Fatalf("fulfillments = %+v", fs)
	}
	for _, fo := range r.sim.FulfillmentOrders(id) {
		if fo.Status != "CLOSED" {
			t.Fatalf("FO = %+v", fo)
		}
	}
	// 重放：没有可发的 FO → 成功，不再调 fulfillmentCreate
	if err := r.a.Act(ctx, r.b, ref, ship); err != nil {
		t.Fatal(err)
	}
	if n := r.sim.Calls("FulfillmentCreate"); n != 1 || len(r.sim.Fulfillments(id)) != 1 {
		t.Fatalf("重放后 fulfillmentCreate 调了 %d 次、fulfillment %d 条", n, len(r.sim.Fulfillments(id)))
	}
}

// 审查修复 4：FO 暂停 / 预约 / 不完整（ON_HOLD / SCHEDULED / INCOMPLETE）不是「已经发过了」：回可重试（不算限流，
// 计入死信次数），原因写清是哪张 FO、什么状态，一条都不发；放开之后照常发。
func TestActShipHeldFulfillmentOrderIsRetryable(t *testing.T) {
	r := newOrderRig(t)
	ctx := context.Background()
	for _, st := range []string{"ON_HOLD", "SCHEDULED", "INCOMPLETE"} {
		id := r.sim.AddOrder(r.spec())
		fo := r.sim.FulfillmentOrders(id)[0]
		r.sim.SetFulfillmentOrderStatus(fo.ID, st)
		ref := channel.OrderRef{ExternalOrderID: id}
		ship := channel.Action{Kind: channel.ActShip, TrackingCompany: "UPS", TrackingNo: "1Z-" + st, IdemKey: "k-" + st}
		calls := r.sim.Calls("FulfillmentCreate")
		err := r.a.Act(ctx, r.b, ref, ship)
		var re *channel.RetryableError
		if !errors.As(err, &re) || re.RateLimited || !strings.Contains(err.Error(), st) {
			t.Fatalf("%s → %v，期望不算限流的可重试错误、写明状态", st, err)
		}
		if n := r.sim.Calls("FulfillmentCreate") - calls; n != 0 || len(r.sim.Fulfillments(id)) != 0 {
			t.Fatalf("%s：调了 %d 次 fulfillmentCreate", st, n)
		}
		r.sim.SetFulfillmentOrderStatus(fo.ID, "OPEN")
		if err := r.a.Act(ctx, r.b, ref, ship); err != nil {
			t.Fatalf("%s 放开之后：%v", st, err)
		}
		if fs := r.sim.Fulfillments(id); len(fs) != 1 {
			t.Fatalf("%s 放开之后 fulfillment %d 条", st, len(fs))
		}
	}
}

// 审查 6：第一次 fulfillmentCreate 平台做了、回 5xx → 重试不建第二条。
func TestActShipRetryAfterLostResponse(t *testing.T) {
	r := newOrderRig(t)
	ctx := context.Background()
	id := r.sim.AddOrder(r.spec())
	ref := channel.OrderRef{ExternalOrderID: id}
	ship := channel.Action{Kind: channel.ActShip, TrackingCompany: "UPS", TrackingNo: "1Z2", IdemKey: "k"}
	r.sim.FailNext("FulfillmentCreate", true)
	if err := r.a.Act(ctx, r.b, ref, ship); !channel.IsRetryable(err) {
		t.Fatalf("响应丢了 → %v，期望可重试", err)
	}
	if err := r.a.Act(ctx, r.b, ref, ship); err != nil {
		t.Fatal(err)
	}
	if fs := r.sim.Fulfillments(id); len(fs) != 1 {
		t.Fatalf("重试后 fulfillment %d 条，期望 1", len(fs))
	}
	// 限流
	id2 := r.sim.AddOrder(r.spec())
	r.sim.ThrottleNext(1)
	var re *channel.RetryableError
	if err := r.a.Act(ctx, r.b, channel.OrderRef{ExternalOrderID: id2}, ship); !errors.As(err, &re) || !re.RateLimited {
		t.Fatalf("限流 → %v", err)
	}
}

func TestActShipUserErrorsAndUnsupported(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/oauth/access_token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"shpat_x","expires_in":86399}`))
	})
	mux.HandleFunc("POST /admin/api/2026-10/graphql.json", func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		if strings.Contains(string(raw), `"operationName":"Order"`) {
			_, _ = w.Write([]byte(`{"data":{"order":{"id":"gid://shopify/Order/1","fulfillmentOrders":{"nodes":[
				{"id":"gid://shopify/FulfillmentOrder/1","status":"OPEN","assignedLocation":{"location":{"id":"gid://shopify/Location/1"}}}]}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"fulfillmentCreate":{"fulfillment":null,"userErrors":[{"field":["fulfillment"],"message":"Merchant managed location needs scope."}]}}}`))
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	r := newRig(t)
	a := shopify.New(shopify.Options{BaseURL: func(string) string { return s.URL }})
	ref := channel.OrderRef{ExternalOrderID: "gid://shopify/Order/1"}
	err := a.Act(context.Background(), r.b, ref, channel.Action{Kind: channel.ActShip, TrackingNo: "1"})
	if err == nil || channel.IsRetryable(err) || !strings.Contains(err.Error(), "Merchant managed location needs scope.") {
		t.Fatalf("userErrors → %v", err)
	}
	for _, k := range []channel.ActionKind{channel.ActAccept, channel.ActReject, channel.ActPicked, channel.ActDeliveryStatus,
		channel.ActAgreeRequest, channel.ActRejectRequest, channel.ActStockout} {
		if err := a.Act(context.Background(), r.b, ref, channel.Action{Kind: k}); !errors.Is(err, channel.ErrUnsupported) {
			t.Errorf("%s → %v，期望 ErrUnsupported", k, err)
		}
	}
}

func TestParseInboundOrderTopics(t *testing.T) {
	r := newOrderRig(t)
	id := r.sim.AddOrder(r.spec())
	r.sim.Refund(id, []shopifytest.RefundLine{{LineItem: r.vb, Qty: 1}}, "", false)
	r.sim.FulfillInShopify(id, "UPS", "1Z3")
	for _, topic := range []string{"orders/create", "orders/paid", "orders/updated", "orders/cancelled", "refunds/create", "fulfillments/create"} {
		req, body := r.sim.WebhookFor(id, topic)
		evs, _, err := r.a.ParseInbound(r.b, req, body)
		if err != nil || len(evs) != 1 {
			t.Fatalf("%s → %v, %v", topic, evs, err)
		}
		if ev := evs[0]; ev.Kind != channel.EventOrderChanged || ev.ExternalOrderID != id || ev.Topic != topic || ev.ExternalID == "" {
			t.Errorf("%s → %+v，期望订单 %s", topic, ev, id)
		}
	}
	body := []byte(`{"id":5}`)
	if _, _, err := r.a.ParseInbound(r.b, r.sim.WebhookRequest("/x", "refunds/create", "e-x", body), body); err == nil {
		t.Fatal("refunds/create 没有 order_id 应报错")
	}
}

func TestEnsureWebhooksOrderTopics(t *testing.T) {
	r := newRig(t)
	const cb = "https://demo.example/cb"
	if err := r.a.EnsureWebhooks(context.Background(), r.b, cb); err != nil {
		t.Fatal(err)
	}
	have := map[string]int{}
	for _, w := range r.sim.Webhooks() {
		have[w.Topic]++
	}
	for _, tp := range []string{"ORDERS_CREATE", "ORDERS_UPDATED", "ORDERS_CANCELLED", "ORDERS_PAID", "REFUNDS_CREATE", "FULFILLMENTS_CREATE"} {
		if have[tp] != 1 {
			t.Errorf("%s 装了 %d 条", tp, have[tp])
		}
	}
}
