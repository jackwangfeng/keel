package shopify_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/shopify"
	"github.com/keel/keel/internal/channel/shopify/shopifytest"
)

const (
	shop     = "keel-sim.myshopify.com"
	clientID = "cid-123456"
	secret   = "csecret-abcdef-123456"
)

type rig struct {
	sim *shopifytest.Server
	a   *shopify.Adapter
	b   channel.Binding
	now *time.Time
}

func newRig(t *testing.T) rig {
	t.Helper()
	sim := shopifytest.New(t, shop, clientID, secret)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	r := rig{sim: sim, now: &now}
	r.a = shopify.New(shopify.Options{BaseURL: sim.BaseURL, Now: func() time.Time { return *r.now }, PageSize: 2})
	sec, _ := json.Marshal(map[string]string{"client_id": clientID, "client_secret": secret})
	r.b = channel.Binding{ID: 7, MerchantID: 1, Kind: shopify.Kind, ExternalAccount: shop,
		Roles: channel.RoleCatalogSource | channel.RoleOutlet, Secrets: sec}
	return r
}

func noSecrets(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if s := err.Error(); strings.Contains(s, secret) || strings.Contains(s, "shpat_") {
		t.Fatalf("错误文本里有凭据：%q", s)
	}
}

func TestTokenCachedRenewedAndRecovered(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := r.a.PullCatalog(ctx, r.b, ""); err != nil {
			t.Fatal(err)
		}
	}
	if n := r.sim.Calls("tokens"); n != 1 {
		t.Fatalf("两次调用换了 %d 次 token，期望 1 次", n)
	}
	*r.now = r.now.Add(20 * time.Hour) // 超过 86399 秒的 80%
	if _, err := r.a.PullCatalog(ctx, r.b, ""); err != nil {
		t.Fatal(err)
	}
	if n := r.sim.Calls("tokens"); n != 2 {
		t.Fatalf("用到 80%% 之后没有提前续（换了 %d 次）", n)
	}
	r.sim.ExpireTokens()
	if _, err := r.a.PullCatalog(ctx, r.b, ""); err != nil {
		t.Fatalf("token 被平台作废之后没有自动重换：%v", err)
	}
	r.sim.RotateSecret("new-secret-999999")
	_, err := r.a.PullCatalog(ctx, r.b, "")
	if !errors.Is(err, channel.ErrCredentials) {
		t.Fatalf("凭据换了之后返回 %v，期望 ErrCredentials", err)
	}
	noSecrets(t, err)
}

func TestBadBindingIsCredentialsError(t *testing.T) {
	r := newRig(t)
	b := r.b
	b.ExternalAccount = "evil.example.com"
	if _, err := r.a.PullCatalog(context.Background(), b, ""); !errors.Is(err, channel.ErrCredentials) {
		t.Fatalf("域名不像 myshopify.com 时返回 %v", err)
	}
	b = r.b
	b.Secrets = json.RawMessage(`{}`)
	if _, err := r.a.PullCatalog(context.Background(), b, ""); !errors.Is(err, channel.ErrCredentials) {
		t.Fatalf("缺凭据时返回 %v", err)
	}
}

func TestThrottledIsRateLimited(t *testing.T) {
	r := newRig(t)
	r.sim.ThrottleNext(1)
	_, err := r.a.PullCatalog(context.Background(), r.b, "")
	var re *channel.RetryableError
	if !errors.As(err, &re) || !re.RateLimited || re.After < time.Second {
		t.Fatalf("限流返回 %v，期望 RateLimited 且 After ≥ 1 秒", err)
	}
	if _, err := r.a.PullCatalog(context.Background(), r.b, ""); err != nil {
		t.Fatalf("限流解除之后：%v", err)
	}
}

func TestHTTP429AndServerErrors(t *testing.T) {
	code := http.StatusTooManyRequests
	srv := newStatusServer(t, &code)
	a := shopify.New(shopify.Options{BaseURL: func(string) string { return srv }})
	sec, _ := json.Marshal(map[string]string{"client_id": clientID, "client_secret": secret})
	b := channel.Binding{ID: 1, ExternalAccount: shop, Secrets: sec}
	_, err := a.PullCatalog(context.Background(), b, "")
	var re *channel.RetryableError
	if !errors.As(err, &re) || !re.RateLimited || re.After != 3*time.Second {
		t.Fatalf("429 返回 %v，期望 RateLimited、After = Retry-After 3 秒", err)
	}
	code = http.StatusBadGateway
	_, err = a.PullCatalog(context.Background(), b, "")
	if !errors.As(err, &re) || re.RateLimited {
		t.Fatalf("502 返回 %v，期望可重试、非限流", err)
	}
}

func TestParseInbound(t *testing.T) {
	r := newRig(t)
	body := []byte(`{"id":123,"admin_graphql_api_id":"gid://shopify/Product/123","title":"x"}`)
	req := r.sim.WebhookRequest("/x", "products/update", "evt-1", body)
	evs, ack, err := r.a.ParseInbound(r.b, req, body)
	if err != nil || len(evs) != 1 || ack != nil {
		t.Fatalf("ParseInbound = %v, %v, %v", evs, ack, err)
	}
	if ev := evs[0]; ev.Kind != channel.EventCatalogChanged || ev.ExternalID != "evt-1" || ev.ExternalItemIDs[0] != "gid://shopify/Product/123" {
		t.Fatalf("事件 = %+v", ev)
	}
	del := []byte(`{"id":456}`)
	evs, _, err = r.a.ParseInbound(r.b, r.sim.WebhookRequest("/x", "products/delete", "evt-2", del), del)
	if err != nil || evs[0].ExternalItemIDs[0] != "gid://shopify/Product/456" {
		t.Fatalf("products/delete = %+v, %v", evs, err)
	}
	for topic, kind := range map[string]channel.EventKind{"inventory_levels/update": channel.EventStockChanged,
		"orders/create": channel.EventOrderChanged, "shop/update": channel.EventIgnored} {
		evs, _, err := r.a.ParseInbound(r.b, r.sim.WebhookRequest("/x", topic, "e-"+topic, body), body)
		if err != nil || evs[0].Kind != kind {
			t.Errorf("%s → %v, %v；期望 %s", topic, evs, err, kind)
		}
	}
	// 签名错、域名不符、没配 secret
	bad := r.sim.WebhookRequest("/x", "products/update", "evt-3", body)
	bad.Header.Set(shopify.HeaderHMAC, "AAAA")
	if _, _, err := r.a.ParseInbound(r.b, bad, body); !errors.Is(err, channel.ErrBadSignature) {
		t.Fatalf("签名错 → %v", err)
	}
	other := r.sim.WebhookRequest("/x", "products/update", "evt-4", body)
	other.Header.Set(shopify.HeaderShop, "other.myshopify.com")
	if _, _, err := r.a.ParseInbound(r.b, other, body); !errors.Is(err, channel.ErrBadSignature) {
		t.Fatalf("域名不符 → %v", err)
	}
	nb := r.b
	nb.Secrets = json.RawMessage(`{"client_id":"x"}`)
	if _, _, err := r.a.ParseInbound(nb, r.sim.WebhookRequest("/x", "products/update", "evt-5", body), body); !errors.Is(err, channel.ErrBadSignature) {
		t.Fatalf("没配 secret → %v", err)
	}
}
