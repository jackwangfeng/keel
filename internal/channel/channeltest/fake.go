// Package channeltest 是测试用的假渠道：一个按 channel.Adapter 写的、行为可编排的适配器。
// 生产装配（internal/app）从不登记它；它只在测试里出现，用来跑通渠道层的全链路，
// 不依赖任何真实平台。
package channeltest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"sync"
	"time"

	"github.com/keel/keel/internal/channel"
)

// Kind 是假渠道的名字。
const Kind = "fake"

// 回调头：签名是 hex(HMAC-SHA256(secrets.webhook_secret, 原始请求体))。
const (
	HeaderSignature = "X-Fake-Signature"
	HeaderEventID   = "X-Fake-Event-Id"
	HeaderTopic     = "X-Fake-Topic"
)

// Secrets 是假渠道 binding 的 secrets 形状。
type Secrets struct {
	WebhookSecret string `json:"webhook_secret"`
}

// Sign 给测试造一个合法签名。
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Adapter 是假渠道。零值可用（销售渠道、不需要接单）；字段可在测试里改。
type Adapter struct {
	CapsValue channel.Caps

	mu       sync.Mutex
	pushes   [][]channel.Listing
	failNext int                    // 接下来几次 PushListings 整批返回可重试错误
	conflict map[[2]int64]int32     // (门店, SKU) → 渠道上「被人改过」的现值，下一次推送报一次冲突
	actions  []channel.Action
}

func New() *Adapter {
	return &Adapter{CapsValue: channel.Caps{Roles: channel.RoleOutlet, Delivery: channel.DeliveryExpress}}
}

func (a *Adapter) Kind() string       { return Kind }
func (a *Adapter) Caps() channel.Caps { return a.CapsValue }

func (a *Adapter) ParseInbound(b channel.Binding, r *http.Request, body []byte) ([]channel.Event, []byte, error) {
	var s Secrets
	if err := json.Unmarshal(b.Secrets, &s); err != nil || s.WebhookSecret == "" {
		// 没配密钥 = 拒绝，不是跳过（同支付回调）
		return nil, nil, channel.ErrBadSignature
	}
	got, err := hex.DecodeString(r.Header.Get(HeaderSignature))
	if err != nil || !hmac.Equal(got, mustHex(Sign(s.WebhookSecret, body))) {
		return nil, nil, channel.ErrBadSignature
	}
	id := r.Header.Get(HeaderEventID)
	if id == "" {
		return nil, nil, errors.New("假渠道回调缺事件 ID")
	}
	topic := r.Header.Get(HeaderTopic)
	kind := channel.EventIgnored
	if topic == "order" {
		kind = channel.EventOrderChanged
	}
	return []channel.Event{{ExternalID: id, Kind: kind, Topic: topic, Payload: json.RawMessage(body)}}, []byte("ok"), nil
}

func mustHex(s string) []byte { b, _ := hex.DecodeString(s); return b }

// FailNext 让接下来 n 次 PushListings 整批返回可重试错误。
func (a *Adapter) FailNext(n int) { a.mu.Lock(); a.failNext = n; a.mu.Unlock() }

// ConflictOnce 让下一次推送 (store, sku) 报一次 CAS 冲突，渠道上的现值是 observed。
func (a *Adapter) ConflictOnce(store, sku int64, observed int32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conflict == nil {
		a.conflict = map[[2]int64]int32{}
	}
	a.conflict[[2]int64{store, sku}] = observed
}

// Pushes 是收到过的全部 PushListings 批次（拷贝）。
func (a *Adapter) Pushes() [][]channel.Listing {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([][]channel.Listing, len(a.pushes))
	copy(out, a.pushes)
	return out
}

// LastQty 是最近一次推给 (store, sku) 的可售数；没推过返回 false。
func (a *Adapter) LastQty(store, sku int64) (int32, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.pushes) - 1; i >= 0; i-- {
		for _, l := range a.pushes[i] {
			if l.StoreID == store && l.SKUID == sku {
				return l.Qty, true
			}
		}
	}
	return 0, false
}

func (a *Adapter) PushListings(_ context.Context, _ channel.Binding, ls []channel.Listing) ([]channel.ListingResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failNext > 0 {
		a.failNext--
		return nil, &channel.RetryableError{Err: errors.New("假渠道：编排的失败")}
	}
	cp := append([]channel.Listing(nil), ls...)
	a.pushes = append(a.pushes, cp)
	out := make([]channel.ListingResult, len(ls))
	for i, l := range ls {
		out[i] = channel.ListingResult{StoreID: l.StoreID, SKUID: l.SKUID}
		if obs, ok := a.conflict[[2]int64{l.StoreID, l.SKUID}]; ok {
			delete(a.conflict, [2]int64{l.StoreID, l.SKUID})
			o := obs
			out[i].Conflict, out[i].ObservedQty = true, &o
			out[i].Err = errors.New("假渠道：CAS 冲突")
		}
	}
	return out, nil
}

func (a *Adapter) PushCatalog(context.Context, channel.Binding, []channel.CatalogItem) error {
	return channel.ErrUnsupported
}

func (a *Adapter) Act(_ context.Context, _ channel.Binding, _ channel.OrderRef, act channel.Action) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.actions = append(a.actions, act)
	return nil
}

func (a *Adapter) FetchOrder(context.Context, channel.Binding, string) (channel.ChannelOrder, error) {
	return channel.ChannelOrder{}, channel.ErrUnsupported
}

func (a *Adapter) ListOrders(context.Context, channel.Binding, time.Time) iter.Seq2[channel.ChannelOrder, error] {
	return func(func(channel.ChannelOrder, error) bool) {}
}

func (a *Adapter) ListListings(context.Context, channel.Binding, channel.StoreLink) iter.Seq2[channel.Listing, error] {
	return func(func(channel.Listing, error) bool) {}
}

var (
	_ channel.Adapter = (*Adapter)(nil)
	_ channel.Outlet  = (*Adapter)(nil)
)
