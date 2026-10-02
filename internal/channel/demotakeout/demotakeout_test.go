package demotakeout_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/demotakeout"
)

const secret = "s3cret"

func binding() channel.Binding {
	sec, _ := json.Marshal(demotakeout.Secrets{Secret: secret})
	return channel.Binding{ID: 7, Kind: demotakeout.Kind, Roles: channel.RoleOutlet, Secrets: sec}
}

func sampleBody(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(demotakeout.Order{OrderID: "D-1", OrderName: "#0001", StoreID: "demo-store-1",
		Status: demotakeout.StatusNew, Version: 3, PlacedAt: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
		Lines: []demotakeout.Line{
			{LineID: "1", SKUID: "demo-sku-1", Title: "拿铁", Qty: 2, PriceCents: 1500},
			{LineID: "2", SKUID: "demo-sku-2", Title: "可颂", Qty: 1, PriceCents: 1200},
		},
		FreightCents: 300, PlatformSubsidyCents: 200, MerchantSubsidyCents: 100, CommissionCents: 756,
		Receiver: demotakeout.Receiver{Name: "张三", Phone: "138****0000", Address: "演示路 1 号"}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func request(body []byte, sig, eventID string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if sig != "" {
		r.Header.Set(demotakeout.HeaderSignature, sig)
	}
	if eventID != "" {
		r.Header.Set(demotakeout.HeaderEventID, eventID)
	}
	return r
}

func TestCaps(t *testing.T) {
	a := demotakeout.New()
	if a.Kind() != "demo_takeout" || demotakeout.DisplayName != "演示外卖（模拟）" {
		t.Fatalf("Kind = %q, DisplayName = %q", a.Kind(), demotakeout.DisplayName)
	}
	want := channel.Caps{Roles: channel.RoleOutlet, CatalogDirection: channel.DirNone, Delivery: channel.DeliveryPlatformRider,
		AcceptRequired: true, AcceptTimeout: 5 * time.Minute, OutOfOrderInbound: false, PricePerStore: true}
	if got := a.Caps(); got != want {
		t.Fatalf("Caps = %+v，期望 %+v", got, want)
	}
	var _ channel.Outlet = a
}

func TestParseInboundSignature(t *testing.T) {
	a := demotakeout.New()
	body := sampleBody(t)
	t.Run("对", func(t *testing.T) {
		evs, ack, err := a.ParseInbound(binding(), request(body, demotakeout.Sign(secret, body), "e-1"), body)
		if err != nil || len(evs) != 1 || len(ack) == 0 {
			t.Fatalf("evs=%v ack=%q err=%v", evs, ack, err)
		}
	})
	t.Run("错", func(t *testing.T) {
		_, _, err := a.ParseInbound(binding(), request(body, demotakeout.Sign("other", body), "e-1"), body)
		if !errors.Is(err, channel.ErrBadSignature) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("正文被改", func(t *testing.T) {
		sig := demotakeout.Sign(secret, body)
		tampered := append([]byte(nil), body...)
		tampered[len(tampered)-2] = ' '
		_, _, err := a.ParseInbound(binding(), request(tampered, sig, "e-1"), tampered)
		if !errors.Is(err, channel.ErrBadSignature) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("缺头", func(t *testing.T) {
		_, _, err := a.ParseInbound(binding(), request(body, "", "e-1"), body)
		if !errors.Is(err, channel.ErrBadSignature) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("binding 没配密钥", func(t *testing.T) {
		b := binding()
		b.Secrets = json.RawMessage(`{}`)
		_, _, err := a.ParseInbound(b, request(body, demotakeout.Sign("", body), "e-1"), body)
		if !errors.Is(err, channel.ErrBadSignature) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("缺事件 ID", func(t *testing.T) {
		_, _, err := a.ParseInbound(binding(), request(body, demotakeout.Sign(secret, body), ""), body)
		if err == nil || errors.Is(err, channel.ErrBadSignature) {
			t.Fatalf("err = %v，期望一个非验签的错误", err)
		}
	})
}

func TestParseInboundNormalizesOrder(t *testing.T) {
	a := demotakeout.New()
	body := sampleBody(t)
	evs, _, err := a.ParseInbound(binding(), request(body, demotakeout.Sign(secret, body), "e-9"), body)
	if err != nil {
		t.Fatal(err)
	}
	ev := evs[0]
	if ev.ExternalID != "e-9" || ev.Kind != channel.EventOrderChanged || ev.ExternalOrderID != "D-1" || ev.Order == nil {
		t.Fatalf("事件 = %+v", ev)
	}
	o := *ev.Order
	if o.ExternalOrderID != "D-1" || o.ExternalOrderName != "#0001" || o.ExternalStoreID != "demo-store-1" ||
		o.Status != channel.OrderNew || o.PlatformStatus != "new" || o.Version != 3 || o.Delivery != channel.DeliveryPlatformRider {
		t.Fatalf("订单 = %+v", o)
	}
	placed := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	if !o.PlacedAt.Equal(placed) || o.AcceptDeadline == nil || !o.AcceptDeadline.Equal(placed.Add(5*time.Minute)) {
		t.Fatalf("下单 %v 接单截止 %v", o.PlacedAt, o.AcceptDeadline)
	}
	if len(o.Lines) != 2 || o.Lines[0] != (channel.OrderLine{ExternalLineID: "1", ExternalSKUID: "demo-sku-1", Title: "拿铁", Qty: 2, PriceCents: 1500}) {
		t.Fatalf("行 = %+v", o.Lines)
	}
	// goods 4200；实付 4200+300−200−100 = 4200；商家应收 4200+300−100−756 = 3644
	want := channel.OrderAmounts{GoodsCents: 4200, FreightCents: 300, PlatformSubsidyCents: 200, MerchantSubsidyCents: 100,
		CommissionCents: 756, BuyerPaidCents: 4200, MerchantReceivableCents: 3644}
	if o.Amounts != want {
		t.Fatalf("金额 = %+v，期望 %+v", o.Amounts, want)
	}
	if o.Receiver.Name != "张三" || !o.Receiver.PhoneVirtual || o.Receiver.Address != "演示路 1 号" {
		t.Fatalf("收货人 = %+v", o.Receiver)
	}
	if !bytes.Equal(o.Raw, body) || !bytes.Equal(ev.Payload, body) {
		t.Fatal("Raw / Payload 不是原文")
	}
	// 事件要能原样落库再读回（渠道层把整个 Event 存进 channel_inbound_events）。
	stored, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var back channel.Event
	if err := json.Unmarshal(stored, &back); err != nil || back.Order == nil || back.Order.Amounts != want {
		t.Fatalf("读回 = %+v err=%v", back.Order, err)
	}
}

func TestParseInboundStatusesAndBadBodies(t *testing.T) {
	a := demotakeout.New()
	for st, want := range map[string]channel.OrderStatus{"new": channel.OrderNew, "accepted": channel.OrderAccepted,
		"shipped": channel.OrderShipped, "completed": channel.OrderCompleted, "cancelled": channel.OrderCancelled,
		"rejected": channel.OrderRejected} {
		body := []byte(`{"order_id":"D-2","store_id":"s","status":"` + st + `","version":1,"placed_at":"2026-10-03T12:00:00Z",` +
			`"accept_deadline":"2026-10-03T12:03:00Z","lines":[{"sku_id":"k","qty":1,"price_cents":100}]}`)
		evs, _, err := a.ParseInbound(binding(), request(body, demotakeout.Sign(secret, body), "e"), body)
		if err != nil || evs[0].Order.Status != want {
			t.Fatalf("%s: err=%v", st, err)
		}
		if d := evs[0].Order.AcceptDeadline; d == nil || !d.Equal(time.Date(2026, 10, 3, 12, 3, 0, 0, time.UTC)) {
			t.Fatalf("%s: 给了 accept_deadline 就用它，得到 %v", st, d)
		}
	}
	for name, body := range map[string]string{
		"不是 JSON": `nope`,
		"没单号":     `{"store_id":"s","status":"new","version":1,"placed_at":"2026-10-03T12:00:00Z","lines":[{"sku_id":"k","qty":1,"price_cents":1}]}`,
		"版本不是正数":  `{"order_id":"x","store_id":"s","status":"new","version":0,"placed_at":"2026-10-03T12:00:00Z","lines":[{"sku_id":"k","qty":1,"price_cents":1}]}`,
		"状态认不出":   `{"order_id":"x","store_id":"s","status":"paid","version":1,"placed_at":"2026-10-03T12:00:00Z","lines":[{"sku_id":"k","qty":1,"price_cents":1}]}`,
		"没有行":     `{"order_id":"x","store_id":"s","status":"new","version":1,"placed_at":"2026-10-03T12:00:00Z","lines":[]}`,
		"数量为 0":   `{"order_id":"x","store_id":"s","status":"new","version":1,"placed_at":"2026-10-03T12:00:00Z","lines":[{"sku_id":"k","qty":0,"price_cents":1}]}`,
		"没下单时间":   `{"order_id":"x","store_id":"s","status":"new","version":1,"lines":[{"sku_id":"k","qty":1,"price_cents":1}]}`,
		"补贴超过应付":  `{"order_id":"x","store_id":"s","status":"new","version":1,"placed_at":"2026-10-03T12:00:00Z","platform_subsidy_cents":500,"lines":[{"sku_id":"k","qty":1,"price_cents":1}]}`,
	} {
		b := []byte(body)
		if _, _, err := a.ParseInbound(binding(), request(b, demotakeout.Sign(secret, b), "e"), b); err == nil ||
			errors.Is(err, channel.ErrBadSignature) {
			t.Errorf("%s: err = %v，期望解析错误", name, err)
		}
	}
}

func TestOutletMethods(t *testing.T) {
	a := demotakeout.New()
	ctx, b := context.Background(), binding()
	ls := []channel.Listing{{StoreID: 1, SKUID: 2, Qty: 5}, {StoreID: 1, SKUID: 3, Qty: 0, PushPrice: true, PriceCents: 900}}
	res, err := a.PushListings(ctx, b, ls)
	if err != nil || len(res) != 2 || res[0].Err != nil || res[1].Err != nil || res[1].StoreID != 1 || res[1].SKUID != 3 {
		t.Fatalf("PushListings = %+v, %v", res, err)
	}
	for _, k := range []channel.ActionKind{channel.ActAccept, channel.ActReject, channel.ActPicked, channel.ActShip} {
		if err := a.Act(ctx, b, channel.OrderRef{ExternalOrderID: "D-1"}, channel.Action{Kind: k}); err != nil {
			t.Fatalf("Act(%s) = %v", k, err)
		}
	}
	if err := a.Act(ctx, b, channel.OrderRef{ExternalOrderID: "D-1"}, channel.Action{Kind: channel.ActStockout}); !errors.Is(err, channel.ErrUnsupported) {
		t.Fatalf("Act(stockout) = %v", err)
	}
	if _, err := a.FetchOrder(ctx, b, "D-1"); !errors.Is(err, channel.ErrUnsupported) {
		t.Fatalf("FetchOrder = %v", err)
	}
	if err := a.PushCatalog(ctx, b, nil); !errors.Is(err, channel.ErrUnsupported) {
		t.Fatalf("PushCatalog = %v", err)
	}
	for _, err := range a.ListOrders(ctx, b, time.Now()) {
		if !errors.Is(err, channel.ErrUnsupported) {
			t.Fatalf("ListOrders = %v", err)
		}
	}
	for _, err := range a.ListListings(ctx, b, channel.StoreLink{}) {
		if !errors.Is(err, channel.ErrUnsupported) {
			t.Fatalf("ListListings = %v", err)
		}
	}
}
