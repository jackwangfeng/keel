package channeltest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/channel"
)

func binding(secret string) channel.Binding {
	s, _ := json.Marshal(Secrets{WebhookSecret: secret})
	return channel.Binding{ID: 1, Kind: Kind, Secrets: s}
}

func TestParseInboundVerifiesSignature(t *testing.T) {
	a := New()
	body := []byte(`{"id":1}`)
	mk := func(sig string) (*channel.Event, error) {
		r := httptest.NewRequest("POST", "/", strings.NewReader(string(body)))
		r.Header.Set(HeaderSignature, sig)
		r.Header.Set(HeaderEventID, "e1")
		r.Header.Set(HeaderTopic, "order")
		evs, ack, err := a.ParseInbound(binding("k"), r, body)
		if err != nil {
			return nil, err
		}
		if string(ack) != "ok" || len(evs) != 1 {
			t.Fatalf("ack=%q evs=%d", ack, len(evs))
		}
		return &evs[0], nil
	}
	ev, err := mk(Sign("k", body))
	if err != nil || ev.Kind != channel.EventOrderChanged || ev.ExternalID != "e1" {
		t.Fatalf("合法签名：ev=%+v err=%v", ev, err)
	}
	if _, err := mk(Sign("wrong", body)); !errors.Is(err, channel.ErrBadSignature) {
		t.Errorf("错的密钥签的：err=%v，期望 ErrBadSignature", err)
	}
	if _, err := mk("zz"); !errors.Is(err, channel.ErrBadSignature) {
		t.Errorf("签名不是 hex：err=%v，期望 ErrBadSignature", err)
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(string(body)))
	if _, _, err := a.ParseInbound(channel.Binding{Secrets: json.RawMessage(`{}`)}, r, body); !errors.Is(err, channel.ErrBadSignature) {
		t.Errorf("没配密钥：err=%v，期望拒绝（ErrBadSignature）", err)
	}
}

func TestPushListingsFailureAndConflictAreScripted(t *testing.T) {
	a := New()
	ctx := context.Background()
	a.FailNext(1)
	if _, err := a.PushListings(ctx, binding("k"), []channel.Listing{{StoreID: 1, SKUID: 2, Qty: 5}}); !channel.IsRetryable(err) {
		t.Fatalf("编排的失败：err=%v，期望可重试", err)
	}
	a.ConflictOnce(1, 2, 9)
	res, err := a.PushListings(ctx, binding("k"), []channel.Listing{{StoreID: 1, SKUID: 2, Qty: 5}})
	if err != nil || !res[0].Conflict || *res[0].ObservedQty != 9 {
		t.Fatalf("编排的冲突：res=%+v err=%v", res, err)
	}
	if q, ok := a.LastQty(1, 2); !ok || q != 9 {
		t.Errorf("冲突那一次不生效，渠道上应是被改成的 9：LastQty = %d,%v", q, ok)
	}
	if _, err := a.PushListings(ctx, binding("k"), []channel.Listing{{StoreID: 1, SKUID: 2, Qty: 5}}); err != nil {
		t.Fatal(err)
	}
	if q, _ := a.LastQty(1, 2); q != 5 {
		t.Errorf("重推之后 LastQty = %d，期望 5", q)
	}
}

func TestFakeOrdersAndActs(t *testing.T) {
	a := New()
	a.CapsValue.AcceptRequired, a.CapsValue.RefundNeedsApproval = true, true
	if c := a.Caps(); !c.AcceptRequired || !c.RefundNeedsApproval {
		t.Fatalf("Caps 没按配置：%+v", c)
	}
	ctx, b := context.Background(), binding("k")
	if _, err := a.FetchOrder(ctx, b, "o1"); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("没放的单：err=%v", err)
	}
	a.PutOrder(channel.ChannelOrder{ExternalOrderID: "o1", Status: channel.OrderNew, Version: 1})
	a.PutOrder(channel.ChannelOrder{ExternalOrderID: "o2", Status: channel.OrderNew, Version: 1})
	a.PutOrder(channel.ChannelOrder{ExternalOrderID: "o1", Status: channel.OrderCancelled, Version: 2})
	o, err := a.FetchOrder(ctx, b, "o1")
	if err != nil || o.Status != channel.OrderCancelled || o.Version != 2 {
		t.Fatalf("FetchOrder 应是最后放的那张：%+v err=%v", o, err)
	}
	var ids []string
	for o, err := range a.ListOrders(ctx, b, time.Time{}) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, o.ExternalOrderID)
	}
	if strings.Join(ids, ",") != "o1,o2" {
		t.Fatalf("ListOrders 顺序：%v", ids)
	}

	a.FailActNext(2, nil)
	ref := channel.OrderRef{ExternalOrderID: "o2"}
	for i := 0; i < 2; i++ {
		if err := a.Act(ctx, b, ref, channel.Action{Kind: channel.ActAccept}); !channel.IsRetryable(err) {
			t.Fatalf("第 %d 次应失败（可重试）：%v", i+1, err)
		}
	}
	a.OnAct = func(o channel.OrderRef, act channel.Action) error {
		a.PutOrder(channel.ChannelOrder{ExternalOrderID: o.ExternalOrderID, Status: channel.OrderAccepted, Version: 2})
		return nil
	}
	if err := a.Act(ctx, b, ref, channel.Action{Kind: channel.ActAccept}); err != nil {
		t.Fatalf("第 3 次应成功：%v", err)
	}
	if o, _ := a.Order("o2"); o.Status != channel.OrderAccepted {
		t.Fatalf("OnAct 没生效：%+v", o)
	}
	boom := errors.New("平台拒绝")
	a.OnAct = func(channel.OrderRef, channel.Action) error { return boom }
	if err := a.Act(ctx, b, ref, channel.Action{Kind: channel.ActReject, Reason: "缺货"}); !errors.Is(err, boom) {
		t.Fatalf("OnAct 的错误应透传：%v", err)
	}
	acts := a.Acts()
	if len(acts) != 4 || acts[0].Err == nil || acts[1].Err == nil || acts[2].Err != nil || !errors.Is(acts[3].Err, boom) {
		t.Fatalf("Acts 记录不对：%+v", acts)
	}
	if got := a.ActsOf(channel.ActReject); len(got) != 1 || got[0].Action.Reason != "缺货" || got[0].Ref != ref {
		t.Fatalf("ActsOf：%+v", got)
	}
}

func TestOrderWebhookParses(t *testing.T) {
	a := New()
	r, body := OrderWebhook("k", "e9", "o7")
	evs, _, err := a.ParseInbound(binding("k"), r, body)
	if err != nil || len(evs) != 1 || evs[0].Kind != channel.EventOrderChanged || evs[0].ExternalOrderID != "o7" || evs[0].ExternalID != "e9" {
		t.Fatalf("evs=%+v err=%v", evs, err)
	}
}
