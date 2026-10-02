package shopify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/keel/keel/internal/channel"
)

// 回调头（Shopify 文档；Event-Id 在同一事件的重投之间不变，用它去重，没有时退回 Webhook-Id）。
const (
	HeaderHMAC      = "X-Shopify-Hmac-Sha256"
	HeaderTopic     = "X-Shopify-Topic"
	HeaderShop      = "X-Shopify-Shop-Domain"
	HeaderEventID   = "X-Shopify-Event-Id"
	HeaderWebhookID = "X-Shopify-Webhook-Id"
)

// Sign 算回调签名（测试与模拟平台用）。
func Sign(clientSecret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(clientSecret))
	m.Write(body)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// ParseInbound 验签并规整一次回调。验签失败、店铺域名对不上、没配 client_secret 都是 ErrBadSignature。
// ack 为 nil：Shopify 只看 2xx。
func (a *Adapter) ParseInbound(b channel.Binding, r *http.Request, body []byte) ([]channel.Event, []byte, error) {
	var s secrets
	if err := json.Unmarshal(b.Secrets, &s); err != nil || s.ClientSecret == "" {
		return nil, nil, channel.ErrBadSignature
	}
	got, err := base64.StdEncoding.DecodeString(r.Header.Get(HeaderHMAC))
	want, _ := base64.StdEncoding.DecodeString(Sign(s.ClientSecret, body))
	if err != nil || !hmac.Equal(got, want) {
		return nil, nil, channel.ErrBadSignature
	}
	if !strings.EqualFold(r.Header.Get(HeaderShop), b.ExternalAccount) {
		return nil, nil, channel.ErrBadSignature
	}
	id := r.Header.Get(HeaderEventID)
	if id == "" {
		id = r.Header.Get(HeaderWebhookID)
	}
	topic := r.Header.Get(HeaderTopic)
	var p struct {
		GID string          `json:"admin_graphql_api_id"`
		ID  json.RawMessage `json:"id"`
	}
	ev := channel.Event{ExternalID: id, Topic: topic, Payload: json.RawMessage(body)}
	switch {
	case topic == "products/create" || topic == "products/update" || topic == "products/delete":
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, nil, fmt.Errorf("Shopify %s 回调解不开：%w", topic, err)
		}
		gid := p.GID
		if gid == "" && len(p.ID) > 0 {
			gid = "gid://shopify/Product/" + strings.Trim(string(p.ID), `"`)
		}
		if gid == "" {
			return nil, nil, fmt.Errorf("Shopify %s 回调里没有商品 ID", topic)
		}
		ev.Kind, ev.ExternalItemIDs = channel.EventCatalogChanged, []string{gid}
	case topic == "inventory_levels/update":
		ev.Kind = channel.EventStockChanged
	case strings.HasPrefix(topic, "orders/"):
		_ = json.Unmarshal(body, &p)
		ev.Kind, ev.ExternalOrderID = channel.EventOrderChanged, p.GID
	default:
		ev.Kind = channel.EventIgnored
	}
	return []channel.Event{ev}, nil, nil
}
