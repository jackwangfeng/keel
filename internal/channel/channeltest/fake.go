// Package channeltest 是测试用的假渠道：一个按 channel.Adapter 写的、行为可编排的适配器。
// 生产装配（internal/app）从不登记它；它只在测试里出现，用来跑通渠道层的全链路，
// 不依赖任何真实平台。
package channeltest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
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

// TopicOrder 是订单回调的主题；正文里的 order_id（字符串）是渠道订单号。
const TopicOrder = "order"

// OrderWebhook 造一条签好名的订单回调（正文 {"order_id": externalOrderID}）。eventID 是去重键。
func OrderWebhook(secret, eventID, externalOrderID string) (*http.Request, []byte) {
	body, _ := json.Marshal(map[string]string{"order_id": externalOrderID})
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	r.Header.Set(HeaderSignature, Sign(secret, body))
	r.Header.Set(HeaderEventID, eventID)
	r.Header.Set(HeaderTopic, TopicOrder)
	return r, body
}

// TopicRequest 是平台申请的回调主题；正文 {"order_id": …, "request": channel.OrderRequest}。
const TopicRequest = "request"

// RequestWebhook 造一条签好名的平台申请回调（取消 / 部分退款 / 缺货调整）。eventID 是去重键。
func RequestWebhook(secret, eventID, externalOrderID string, req channel.OrderRequest) (*http.Request, []byte) {
	body, _ := json.Marshal(map[string]any{"order_id": externalOrderID, "request": req})
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	r.Header.Set(HeaderSignature, Sign(secret, body))
	r.Header.Set(HeaderEventID, eventID)
	r.Header.Set(HeaderTopic, TopicRequest)
	return r, body
}

// TopicOrderPayload 是带完整订单的回调主题（推送带完整状态的渠道，Caps.OutOfOrderInbound = false）：
// 正文就是一张 channel.ChannelOrder 的 JSON，事件的 Order 填它。
const TopicOrderPayload = "order_payload"

// OrderPayloadWebhook 造一条签好名、正文带整张订单的回调。eventID 是去重键。
func OrderPayloadWebhook(secret, eventID string, o channel.ChannelOrder) (*http.Request, []byte) {
	body, _ := json.Marshal(o)
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	r.Header.Set(HeaderSignature, Sign(secret, body))
	r.Header.Set(HeaderEventID, eventID)
	r.Header.Set(HeaderTopic, TopicOrderPayload)
	return r, body
}

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
// CapsValue 可在测试里随意配（AcceptRequired、RefundNeedsApproval……），渠道层只按它分支。
type Adapter struct {
	CapsValue channel.Caps

	// OnPush 非空时在每次 PushListings 处理之前调用（不持锁）：测试用它在「推送进行中」制造并发变化。
	OnPush func(ls []channel.Listing)

	mu       sync.Mutex
	pushes   [][]channel.Listing
	failNext int                // 接下来几次 PushListings 整批返回可重试错误
	failErr  error              // 编排的失败用哪个错误（nil = 一个普通的可重试错误）
	conflict map[[2]int64]int32 // (门店, SKU) → 渠道上「被人改过」的现值，下一次推送报一次冲突
	applied  map[[2]int64]int32 // 渠道上当前的可售数（只有生效的推送改它；冲突的那一次是被人改成的数）
	prices   map[[2]int64]int64 // 渠道上当前的价格（同 applied）

	// OnAct 非空时在每次 Act 记录之后调用（不持锁），返回值作为 Act 的结果（编排的失败优先）：
	// 测试用它在平台上「做出」动作的效果（比如接单后 PutOrder 一张 OrderAccepted 的单）。
	OnAct func(o channel.OrderRef, a channel.Action) error

	acts       []ActCall
	actFail    int   // 接下来几次 Act 失败
	actFailErr error // 失败用哪个错误（nil = 一个普通的可重试错误）
	orders     map[string]channel.ChannelOrder
	orderOrder []string // PutOrder 的先后（ListOrders 按它）
	fetches    int      // FetchOrder 被调了几次

	// FetchErr 非空时 FetchOrder 一律返回它（照「不支持回读」的渠道，如 channel.ErrUnsupported）。
	FetchErr error
}

// ActCall 是一次 Act 调用的记录（失败的也记，Err 是返回给调用方的错误）。
type ActCall struct {
	Ref    channel.OrderRef
	Action channel.Action
	Err    error
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
	ev := channel.Event{ExternalID: id, Kind: channel.EventIgnored, Topic: topic, Payload: json.RawMessage(body)}
	if topic == TopicOrder {
		var p struct {
			OrderID string `json:"order_id"`
		}
		_ = json.Unmarshal(body, &p)
		ev.Kind, ev.ExternalOrderID = channel.EventOrderChanged, p.OrderID
	}
	if topic == TopicOrderPayload {
		var o channel.ChannelOrder
		if err := json.Unmarshal(body, &o); err != nil {
			return nil, nil, fmt.Errorf("假渠道订单回调解不开: %w", err)
		}
		ev.Kind, ev.ExternalOrderID, ev.Order = channel.EventOrderChanged, o.ExternalOrderID, &o
	}
	if topic == TopicRequest {
		var p struct {
			OrderID string               `json:"order_id"`
			Request channel.OrderRequest `json:"request"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, nil, fmt.Errorf("假渠道申请回调解不开: %w", err)
		}
		ev.Kind, ev.ExternalOrderID, ev.Request = channel.EventOrderRequest, p.OrderID, &p.Request
	}
	return []channel.Event{ev}, []byte("ok"), nil
}

func mustHex(s string) []byte { b, _ := hex.DecodeString(s); return b }

// FailNext 让接下来 n 次 PushListings 整批返回可重试错误。
func (a *Adapter) FailNext(n int) { a.FailNextWith(n, nil) }

// FailNextWith 让接下来 n 次 PushListings 整批返回 err（nil = 一个普通的可重试错误）。
func (a *Adapter) FailNextWith(n int, err error) {
	a.mu.Lock()
	a.failNext, a.failErr = n, err
	a.mu.Unlock()
}

// LastPrice 是渠道上 (store, sku) 当前的价格（最近一次生效、且 PushPrice 为真的推送）。
func (a *Adapter) LastPrice(store, sku int64) (int64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.prices[[2]int64{store, sku}]
	return p, ok
}

// FailuresLeft 是还没用掉的编排失败次数。
func (a *Adapter) FailuresLeft() int { a.mu.Lock(); defer a.mu.Unlock(); return a.failNext }

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

// LastQty 是渠道上 (store, sku) 当前的可售数（最近一次**生效的**推送）；没推过返回 false。
func (a *Adapter) LastQty(store, sku int64) (int32, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	q, ok := a.applied[[2]int64{store, sku}]
	return q, ok
}

func (a *Adapter) PushListings(_ context.Context, _ channel.Binding, ls []channel.Listing) ([]channel.ListingResult, error) {
	if a.OnPush != nil {
		a.OnPush(ls)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failNext > 0 {
		a.failNext--
		if a.failErr != nil {
			return nil, a.failErr
		}
		return nil, &channel.RetryableError{Err: errors.New("假渠道：编排的失败")}
	}
	cp := append([]channel.Listing(nil), ls...)
	a.pushes = append(a.pushes, cp)
	out := make([]channel.ListingResult, len(ls))
	for i, l := range ls {
		out[i] = channel.ListingResult{StoreID: l.StoreID, SKUID: l.SKUID}
		k := [2]int64{l.StoreID, l.SKUID}
		if obs, ok := a.conflict[k]; ok {
			delete(a.conflict, k)
			o := obs
			out[i].Conflict, out[i].ObservedQty = true, &o
			out[i].Err = errors.New("假渠道：CAS 冲突")
			if a.applied == nil {
				a.applied = map[[2]int64]int32{}
			}
			a.applied[k] = obs // 渠道上是被人改成的那个数
			continue
		}
		if a.applied == nil {
			a.applied = map[[2]int64]int32{}
		}
		a.applied[k] = l.Qty
		if !l.PushPrice {
			continue
		}
		if a.prices == nil {
			a.prices = map[[2]int64]int64{}
		}
		a.prices[k] = l.PriceCents
	}
	return out, nil
}

func (a *Adapter) PushCatalog(context.Context, channel.Binding, []channel.CatalogItem) error {
	return channel.ErrUnsupported
}

// PutOrder 放（或替换）一张渠道上的订单，FetchOrder / ListOrders 返回它。
func (a *Adapter) PutOrder(o channel.ChannelOrder) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.orders == nil {
		a.orders = map[string]channel.ChannelOrder{}
	}
	if _, ok := a.orders[o.ExternalOrderID]; !ok {
		a.orderOrder = append(a.orderOrder, o.ExternalOrderID)
	}
	a.orders[o.ExternalOrderID] = o
}

// Order 是 PutOrder 放进去的那张单（拷贝）。
func (a *Adapter) Order(externalOrderID string) (channel.ChannelOrder, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	o, ok := a.orders[externalOrderID]
	return o, ok
}

// FailActNext 让接下来 n 次 Act 返回 err（nil = 一个普通的可重试错误）。失败的调用也记进 Acts。
func (a *Adapter) FailActNext(n int, err error) {
	a.mu.Lock()
	a.actFail, a.actFailErr = n, err
	a.mu.Unlock()
}

// Acts 是收到过的全部 Act 调用（按到达顺序，拷贝）。
func (a *Adapter) Acts() []ActCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]ActCall(nil), a.acts...)
}

// ActsOf 是某一种动作的调用（含失败的）。
func (a *Adapter) ActsOf(kind channel.ActionKind) []ActCall {
	var out []ActCall
	for _, c := range a.Acts() {
		if c.Action.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func (a *Adapter) Act(_ context.Context, _ channel.Binding, o channel.OrderRef, act channel.Action) error {
	a.mu.Lock()
	if a.actFail > 0 {
		a.actFail--
		err := a.actFailErr
		if err == nil {
			err = &channel.RetryableError{Err: errors.New("假渠道：编排的动作失败")}
		}
		a.acts = append(a.acts, ActCall{Ref: o, Action: act, Err: err})
		a.mu.Unlock()
		return err
	}
	i := len(a.acts)
	a.acts = append(a.acts, ActCall{Ref: o, Action: act})
	hook := a.OnAct
	a.mu.Unlock()
	if hook == nil {
		return nil
	}
	err := hook(o, act)
	if err != nil {
		a.mu.Lock()
		a.acts[i].Err = err
		a.mu.Unlock()
	}
	return err
}

// FetchOrder 返回 PutOrder 放进去的单；没有返回 ErrOrderNotFound（不可重试）。
func (a *Adapter) FetchOrder(_ context.Context, _ channel.Binding, id string) (channel.ChannelOrder, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fetches++
	if a.FetchErr != nil {
		return channel.ChannelOrder{}, a.FetchErr
	}
	o, ok := a.orders[id]
	if !ok {
		return channel.ChannelOrder{}, ErrOrderNotFound
	}
	return o, nil
}

// FetchCalls 是 FetchOrder 被调过的次数。
func (a *Adapter) FetchCalls() int { a.mu.Lock(); defer a.mu.Unlock(); return a.fetches }

// ErrOrderNotFound：假渠道上没有这张单。
var ErrOrderNotFound = errors.New("假渠道：没有这张订单")

// ListOrders 按 PutOrder 的先后列出全部订单（不按 day 过滤）。
func (a *Adapter) ListOrders(context.Context, channel.Binding, time.Time) iter.Seq2[channel.ChannelOrder, error] {
	a.mu.Lock()
	out := make([]channel.ChannelOrder, 0, len(a.orderOrder))
	for _, id := range a.orderOrder {
		out = append(out, a.orders[id])
	}
	a.mu.Unlock()
	return func(yield func(channel.ChannelOrder, error) bool) {
		for _, o := range out {
			if !yield(o, nil) {
				return
			}
		}
	}
}

func (a *Adapter) ListListings(context.Context, channel.Binding, channel.StoreLink) iter.Seq2[channel.Listing, error] {
	return func(func(channel.Listing, error) bool) {}
}

var (
	_ channel.Adapter = (*Adapter)(nil)
	_ channel.Outlet  = (*Adapter)(nil)
)
