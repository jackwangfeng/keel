// Package demotakeout 是演示站用的「演示外卖（模拟）」渠道：一个按外卖平台的样子声明能力、但背后没有真实平台的
// 销售渠道（spec docs/superpowers/specs/2026-10-03-ai-channel-allocation-design.md §7.2）。演示站的模拟器
// 签名推订单回调进来；keel 推出去的可售数就是这个模拟平台上的数。
//
// 只在 KEEL_CHANNEL_DEMO=on 时登记（internal/app/channels.go），生产部署默认不编进注册表。
// 只依赖标准库与 internal/channel。
//
// # 回调格式
//
// POST /api/v1/webhooks/channels/{binding_id}（与别的渠道同一个入口），头：
//
//	X-Demo-Signature: hex(HMAC-SHA256(secrets.secret, 原始请求体))
//	X-Demo-Event-Id:  事件去重键（同一个 binding 下唯一；重推同一条用同一个）
//
// 正文是一张规整好的订单（Order），推送带完整状态，所以收单直接用它、不回读：
//
//	{"order_id":"D-1","order_name":"#0001","store_id":"demo-store-1","status":"new","version":1,
//	 "placed_at":"2026-10-03T12:00:00Z",
//	 "lines":[{"line_id":"1","sku_id":"demo-sku-1","title":"拿铁","qty":2,"price_cents":1500}],
//	 "freight_cents":300,"platform_subsidy_cents":0,"merchant_subsidy_cents":0,"commission_cents":594,
//	 "receiver":{"name":"张三","phone":"138****0000","address":"演示路 1 号"}}
//
// 金额由行算：goods = Σ qty × price_cents；实付 = goods + 运费 − 平台补贴 − 商家补贴；
// 商家应收 = goods + 运费 − 商家补贴 − 佣金。同一张单状态变化时 version 加一（旧版本不覆盖新版本）。
package demotakeout

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"time"

	"github.com/keel/keel/internal/channel"
)

const (
	// Kind 是 channel_bindings.channel 的值。
	Kind = "demo_takeout"
	// DisplayName 是后台显示名。
	DisplayName = "演示外卖（模拟）"

	HeaderSignature = "X-Demo-Signature"
	HeaderEventID   = "X-Demo-Event-Id"

	// Topic 是订单回调在留档里的主题（这个渠道只有这一种回调）。
	Topic = "order"

	// AcceptTimeout 是接单时限：正文没给 accept_deadline 时，新单的截止 = placed_at + 它。
	AcceptTimeout = 5 * time.Minute
)

// 订单状态（正文 status 的取值）。
const (
	StatusNew       = "new"
	StatusAccepted  = "accepted"
	StatusShipped   = "shipped"
	StatusCompleted = "completed"
	StatusCancelled = "cancelled"
	StatusRejected  = "rejected"
)

var statuses = map[string]channel.OrderStatus{
	StatusNew: channel.OrderNew, StatusAccepted: channel.OrderAccepted, StatusShipped: channel.OrderShipped,
	StatusCompleted: channel.OrderCompleted, StatusCancelled: channel.OrderCancelled, StatusRejected: channel.OrderRejected,
}

// Secrets 是 binding secrets 的形状。
type Secrets struct {
	Secret string `json:"secret"`
}

// Order 是回调正文（线上格式）。
type Order struct {
	OrderID   string    `json:"order_id"`             // 必填，渠道订单号
	OrderName string    `json:"order_name,omitempty"` // 给人看的单号，缺省同 order_id
	StoreID   string    `json:"store_id"`             // 渠道门店 ID（binding 的门店映射里的 external_store_id）
	Status    string    `json:"status"`               // new / accepted / shipped / completed / cancelled / rejected
	Version   int64     `json:"version"`              // 必填，正数，单调
	PlacedAt  time.Time `json:"placed_at"`            // 必填，RFC 3339
	// AcceptDeadline 缺省时新单取 placed_at + 5 分钟。
	AcceptDeadline       *time.Time `json:"accept_deadline,omitempty"`
	Lines                []Line     `json:"lines"` // 至少一行
	FreightCents         int64      `json:"freight_cents,omitempty"`
	PlatformSubsidyCents int64      `json:"platform_subsidy_cents,omitempty"`
	MerchantSubsidyCents int64      `json:"merchant_subsidy_cents,omitempty"`
	CommissionCents      int64      `json:"commission_cents,omitempty"`
	Receiver             Receiver   `json:"receiver,omitempty"`
	Test                 bool       `json:"test,omitempty"`
}

// Line 是订单的一行。
type Line struct {
	LineID     string `json:"line_id,omitempty"` // 缺省按行序 "1"、"2"……
	SKUID      string `json:"sku_id"`            // 渠道 SKU ID（channel_item_links.external_id）
	Title      string `json:"title,omitempty"`
	Qty        int32  `json:"qty"`         // 正数
	PriceCents int64  `json:"price_cents"` // 单价（分），非负
}

// Receiver 是收货人；电话一律当平台隐私号。
type Receiver struct {
	Name    string `json:"name,omitempty"`
	Phone   string `json:"phone,omitempty"`
	Address string `json:"address,omitempty"`
}

// Sign 是签名：hex(HMAC-SHA256(secret, body))。
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Adapter 是演示外卖适配器，无状态。
type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (*Adapter) Kind() string { return Kind }

func (*Adapter) Caps() channel.Caps {
	return channel.Caps{Roles: channel.RoleOutlet, CatalogDirection: channel.DirNone, Delivery: channel.DeliveryPlatformRider,
		AcceptRequired: true, AcceptTimeout: AcceptTimeout, OutOfOrderInbound: false, PricePerStore: true}
}

func (*Adapter) ParseInbound(b channel.Binding, r *http.Request, body []byte) ([]channel.Event, []byte, error) {
	var s Secrets
	if err := json.Unmarshal(b.Secrets, &s); err != nil || s.Secret == "" {
		return nil, nil, channel.ErrBadSignature // 没配密钥 = 拒绝
	}
	got, err := hex.DecodeString(r.Header.Get(HeaderSignature))
	want, _ := hex.DecodeString(Sign(s.Secret, body))
	if err != nil || len(got) == 0 || !hmac.Equal(got, want) {
		return nil, nil, channel.ErrBadSignature
	}
	id := r.Header.Get(HeaderEventID)
	if id == "" {
		return nil, nil, errors.New("演示外卖回调缺 " + HeaderEventID)
	}
	o, err := normalize(body)
	if err != nil {
		return nil, nil, err
	}
	ev := channel.Event{ExternalID: id, Kind: channel.EventOrderChanged, Topic: Topic, ExternalOrderID: o.ExternalOrderID,
		Payload: json.RawMessage(body), Order: &o}
	return []channel.Event{ev}, []byte(`{"code":0}`), nil
}

// normalize 把正文规整成 ChannelOrder；格式不对返回错误（渠道层应答并留日志，不处理）。
func normalize(body []byte) (channel.ChannelOrder, error) {
	var w Order
	if err := json.Unmarshal(body, &w); err != nil {
		return channel.ChannelOrder{}, fmt.Errorf("演示外卖回调正文解不开：%w", err)
	}
	st, ok := statuses[w.Status]
	switch {
	case w.OrderID == "":
		return channel.ChannelOrder{}, errors.New("演示外卖回调没有 order_id")
	case w.Version <= 0:
		return channel.ChannelOrder{}, fmt.Errorf("演示外卖订单 %s 的 version 要是正数", w.OrderID)
	case !ok:
		return channel.ChannelOrder{}, fmt.Errorf("演示外卖订单 %s 的 status %q 认不出来", w.OrderID, w.Status)
	case w.PlacedAt.IsZero():
		return channel.ChannelOrder{}, fmt.Errorf("演示外卖订单 %s 没有 placed_at", w.OrderID)
	case len(w.Lines) == 0:
		return channel.ChannelOrder{}, fmt.Errorf("演示外卖订单 %s 没有商品行", w.OrderID)
	}
	o := channel.ChannelOrder{ExternalOrderID: w.OrderID, ExternalOrderName: w.OrderName, ExternalStoreID: w.StoreID,
		PlatformStatus: w.Status, Status: st, Version: w.Version, Test: w.Test, PlacedAt: w.PlacedAt,
		AcceptDeadline: w.AcceptDeadline, Delivery: channel.DeliveryPlatformRider, Raw: json.RawMessage(body)}
	if o.ExternalOrderName == "" {
		o.ExternalOrderName = w.OrderID
	}
	if o.AcceptDeadline == nil && st == channel.OrderNew {
		d := w.PlacedAt.Add(AcceptTimeout)
		o.AcceptDeadline = &d
	}
	var goods int64
	for i, l := range w.Lines {
		if l.SKUID == "" || l.Qty <= 0 || l.PriceCents < 0 {
			return channel.ChannelOrder{}, fmt.Errorf("演示外卖订单 %s 第 %d 行要有 sku_id、正的 qty、非负的 price_cents", w.OrderID, i+1)
		}
		id := l.LineID
		if id == "" {
			id = fmt.Sprint(i + 1)
		}
		o.Lines = append(o.Lines, channel.OrderLine{ExternalLineID: id, ExternalSKUID: l.SKUID, Title: l.Title, Qty: l.Qty,
			PriceCents: l.PriceCents})
		goods += int64(l.Qty) * l.PriceCents
	}
	if w.FreightCents < 0 || w.PlatformSubsidyCents < 0 || w.MerchantSubsidyCents < 0 || w.CommissionCents < 0 {
		return channel.ChannelOrder{}, fmt.Errorf("演示外卖订单 %s 的金额不能为负", w.OrderID)
	}
	paid := goods + w.FreightCents - w.PlatformSubsidyCents - w.MerchantSubsidyCents
	if paid < 0 {
		return channel.ChannelOrder{}, fmt.Errorf("演示外卖订单 %s 的补贴超过了应付", w.OrderID)
	}
	o.Amounts = channel.OrderAmounts{GoodsCents: goods, FreightCents: w.FreightCents, PlatformSubsidyCents: w.PlatformSubsidyCents,
		MerchantSubsidyCents: w.MerchantSubsidyCents, CommissionCents: w.CommissionCents, BuyerPaidCents: paid,
		MerchantReceivableCents: goods + w.FreightCents - w.MerchantSubsidyCents - w.CommissionCents}
	o.Receiver = channel.Receiver{Name: w.Receiver.Name, Phone: w.Receiver.Phone, PhoneVirtual: w.Receiver.Phone != "",
		Address: w.Receiver.Address}
	return o, nil
}

// PushListings 直接成功：keel 的 channel_listings 就是这个模拟平台上的数。
func (*Adapter) PushListings(_ context.Context, _ channel.Binding, ls []channel.Listing) ([]channel.ListingResult, error) {
	out := make([]channel.ListingResult, len(ls))
	for i, l := range ls {
		out[i] = channel.ListingResult{StoreID: l.StoreID, SKUID: l.SKUID}
	}
	return out, nil
}

func (*Adapter) PushCatalog(context.Context, channel.Binding, []channel.CatalogItem) error {
	return channel.ErrUnsupported
}

// Act：接单 / 拒单 / 拣货完成 / 发货直接成功；别的动作 Caps 没声明，不支持。
func (*Adapter) Act(_ context.Context, _ channel.Binding, _ channel.OrderRef, a channel.Action) error {
	switch a.Kind {
	case channel.ActAccept, channel.ActReject, channel.ActPicked, channel.ActShip:
		return nil
	}
	return channel.ErrUnsupported
}

// FetchOrder 不支持：回调带完整状态，渠道层直接用载荷（Event.Order），不会回读。
func (*Adapter) FetchOrder(context.Context, channel.Binding, string) (channel.ChannelOrder, error) {
	return channel.ChannelOrder{}, channel.ErrUnsupported
}

func (*Adapter) ListOrders(context.Context, channel.Binding, time.Time) iter.Seq2[channel.ChannelOrder, error] {
	return func(yield func(channel.ChannelOrder, error) bool) {
		yield(channel.ChannelOrder{}, channel.ErrUnsupported)
	}
}

func (*Adapter) ListListings(context.Context, channel.Binding, channel.StoreLink) iter.Seq2[channel.Listing, error] {
	return func(yield func(channel.Listing, error) bool) { yield(channel.Listing{}, channel.ErrUnsupported) }
}
