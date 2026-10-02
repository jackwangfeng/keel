package channeltest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

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
	if q, ok := a.LastQty(1, 2); !ok || q != 5 {
		t.Errorf("LastQty = %d,%v", q, ok)
	}
}
