package handler_test

// 渠道回调入口（handler/webhook_channel.go、service/channel_inbound.go）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/channeltest"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

func postChannelWebhook(t *testing.T, host string, bindingID int64, eventID, topic, body, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/webhooks/channels/%d", bindingID), strings.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(channeltest.HeaderSignature, sig)
	req.Header.Set(channeltest.HeaderEventID, eventID)
	req.Header.Set(channeltest.HeaderTopic, topic)
	w := httptest.NewRecorder()
	testEngine.ServeHTTP(w, req)
	return w
}

func newFakeBinding(t *testing.T, merchantID int64, account string, status int16) repository.ChannelBinding {
	t.Helper()
	ctx := tenant.NewContext(context.Background(), merchantID)
	b, err := testChannels.CreateBinding(ctx, service.ChannelBindingCreate{Channel: channeltest.Kind,
		ExternalAccount: account, Name: "假渠道", Roles: channel.RoleOutlet, Status: status})
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := json.Marshal(channeltest.Secrets{WebhookSecret: "hook-secret"})
	if err := testChannels.SetSecrets(ctx, b.ID, sec); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestChannelWebhook(t *testing.T) {
	cs := newCouponShop(t)
	other := newCouponShop(t)
	t.Cleanup(func() {
		adminExec(t, `DELETE FROM channel_merchants WHERE merchant_id = ANY($1)`, []int64{cs.MerchantID, other.MerchantID})
	})
	b := newFakeBinding(t, cs.MerchantID, "acct", repository.ChannelBindingActive)
	off := newFakeBinding(t, cs.MerchantID, "acct-off", repository.ChannelBindingDisabled)
	foreign := newFakeBinding(t, other.MerchantID, "acct", repository.ChannelBindingActive)
	body := `{"order":"A1"}`
	good := channeltest.Sign("hook-secret", []byte(body))
	events := func(bindingID int64) int64 {
		return adminQueryInt64(t, `SELECT count(*) FROM channel_inbound_events WHERE binding_id = $1`, bindingID)
	}
	jobs := func() int64 {
		return adminQueryInt64(t, `SELECT count(*) FROM jobs WHERE merchant_id = $1 AND queue = $2`, cs.MerchantID, service.QueueChannelInbound)
	}

	t.Run("合法回调_200带ack_入库入队", func(t *testing.T) {
		w := postChannelWebhook(t, cs.Host, b.ID, "e-1", "order", body, good)
		if w.Code != http.StatusOK || w.Body.String() != "ok" {
			t.Fatalf("状态 %d、体 %q，期望 200 ok", w.Code, w.Body.String())
		}
		if events(b.ID) != 1 || jobs() != 1 {
			t.Fatalf("入库 %d 行、入队 %d 条，期望各 1", events(b.ID), jobs())
		}
	})

	t.Run("重复投递_仍200_不重复入库", func(t *testing.T) {
		w := postChannelWebhook(t, cs.Host, b.ID, "e-1", "order", body, good)
		if w.Code != http.StatusOK || events(b.ID) != 1 || jobs() != 1 {
			t.Fatalf("状态 %d、入库 %d、入队 %d，期望 200 / 1 / 1", w.Code, events(b.ID), jobs())
		}
	})

	for _, c := range []struct {
		name    string
		binding int64
		sig     string
	}{
		{"验签失败", b.ID, channeltest.Sign("wrong", []byte(body))},
		{"binding不存在", 999999999, good},
		{"别家店的binding", foreign.ID, good},
	} {
		t.Run(c.name+"_空401_不入库", func(t *testing.T) {
			before := events(b.ID) + events(foreign.ID)
			w := postChannelWebhook(t, cs.Host, c.binding, "e-x", "order", body, c.sig)
			if w.Code != http.StatusUnauthorized || w.Body.Len() != 0 {
				t.Fatalf("状态 %d、体 %q，期望空的 401", w.Code, w.Body.String())
			}
			if after := events(b.ID) + events(foreign.ID); after != before {
				t.Fatalf("被拒的回调入了库（%d → %d）", before, after)
			}
		})
	}

	t.Run("停用的binding_只留档不处理", func(t *testing.T) {
		before := jobs()
		w := postChannelWebhook(t, cs.Host, off.ID, "e-2", "order", body, good)
		if w.Code != http.StatusOK {
			t.Fatalf("状态 %d，期望 200", w.Code)
		}
		if n := adminQueryInt64(t, `SELECT count(*) FROM channel_inbound_events WHERE binding_id = $1 AND status = 2`, off.ID); n != 1 {
			t.Fatalf("停用 binding 的回调没有以「忽略」留档：%d", n)
		}
		if jobs() != before {
			t.Fatal("停用 binding 的回调入了处理队列")
		}
	})

	// 第三期起订单事件有处理器（channel_order.go）；这条回调的正文没有 order_id，处理器丢弃它、事件标完成。
	t.Run("订单事件没带订单号_处理完不建单", func(t *testing.T) {
		if err := testChannels.Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
		var status int64
		var msg string
		row := admin(t).QueryRow(context.Background(),
			`SELECT status, COALESCE(error, '') FROM channel_inbound_events WHERE binding_id = $1 AND external_event_id = 'e-1'`, b.ID)
		if err := row.Scan(&status, &msg); err != nil {
			t.Fatal(err)
		}
		if status != int64(repository.ChannelEventDone) || msg != "" {
			t.Fatalf("事件处理后 status=%d error=%q，期望完成", status, msg)
		}
		if n := adminQueryInt64(t, `SELECT count(*) FROM channel_orders WHERE binding_id = $1`, b.ID); n != 0 {
			t.Fatalf("没带订单号的回调建了 %d 张渠道单", n)
		}
	})
}
