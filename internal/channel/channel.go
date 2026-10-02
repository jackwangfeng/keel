// Package channel 是渠道适配层与 keel 其余部分无关的那一半：类型、适配器接口、注册表、规则计算。
// 设计见 docs/superpowers/specs/2026-10-02-channel-adapter-design.md。
//
// 这个包不碰数据库、不知道租户、不 import keel 的任何业务包：适配器（channel/shopify、channel/meituansim……）
// 只依赖它，于是写一个新渠道只要读这一个包。编排（读规则、算对外可售数、入队、回写推送状态）在
// service/channel*.go。
//
// 渠道层的通用代码按 Caps 分支，不写 if kind == "meituan"。
package channel

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Role 是 binding 能承担的角色（位，与 channel_bindings.roles 同一套数）。
type Role int16

const (
	RoleCatalogSource Role = 1 // 商品主数据源：商品进 keel
	RoleStockSource   Role = 2 // 库存数据源：外部在手进 keel（ERP）
	RoleOutlet        Role = 4 // 销售渠道：商品 / 价格 / 对外可售数出 keel，订单进 keel
)

// Direction 是商品同步方向。
type Direction int8

const (
	DirNone Direction = iota
	DirIn             // 渠道 → keel（Shopify、ERP 当商品源）
	DirOut            // keel → 渠道（美团 / 饿了么：往平台建改商品）
)

// DeliveryMode 是渠道支持的配送方式（位）。
type DeliveryMode uint8

const (
	DeliveryExpress       DeliveryMode = 1 << iota // 快递（带物流单号）
	DeliveryLocal                                  // 本地配送（商家自己送，Shopify local delivery）
	DeliveryPlatformRider                          // 平台骑手（美团专送 / 蜂鸟），状态由平台回调
	DeliverySelf                                   // 商家自配送，状态由 keel 推给平台
)

// Caps 是适配器的能力声明。渠道层的通用代码只看它，不看 Kind。
type Caps struct {
	Roles            Role
	CatalogDirection Direction
	Delivery         DeliveryMode

	AcceptRequired      bool          // 订单要商家接单（外卖平台），超时平台自动取消
	AcceptTimeout       time.Duration // 接单时限（AcceptRequired 时有意义）
	RefundNeedsApproval bool          // 平台退款要商家同意 / 拒绝
	PartialRefund       bool          // 支持按商品行部分退款
	StockoutAdjust      bool          // 支持商家发起缺货调整
	PickedAction        bool          // 有「拣货完成」动作
	OutOfOrderInbound   bool          // 回调可能乱序 / 只是提示：收到后要 FetchOrder 回读权威状态

	ListingBatch int // PushListings 单次最多几条（适配器自己再分批也行；0 = 不限）
}

// Binding 是适配器看到的 binding：一个商家接的一个渠道账号，连同它的配置与凭据。
// Secrets 只给适配器（验签、调平台 API）；渠道层不打印、不回显它。
type Binding struct {
	ID              int64
	MerchantID      int64
	Kind            string
	ExternalAccount string
	Roles           Role
	Config          json.RawMessage
	Secrets         json.RawMessage
}

// StoreLink 是 keel 门店 ↔ 渠道门店。
type StoreLink struct {
	StoreID         int64
	ExternalStoreID string
}

// EventKind 是回调规整后的类别。
type EventKind string

const (
	EventOrderChanged   EventKind = "order.changed"   // 新单 / 状态变化（乱序渠道只当提示）
	EventOrderRequest   EventKind = "order.request"   // 平台发起的取消 / 部分退款 / 缺货调整
	EventRider          EventKind = "order.rider"     // 骑手状态
	EventReminder       EventKind = "order.reminder"  // 催单
	EventCatalogChanged EventKind = "catalog.changed" // 商品源上的商品变了
	EventStockChanged   EventKind = "stock.changed"   // 渠道上的库存被人改了（对账用）
	EventIgnored        EventKind = "ignored"         // 验签通过、但这一层不关心的主题（只留档）
)

// Event 是一条规整后的回调事件。ExternalID 是去重键（Shopify 的 X-Shopify-Webhook-Id、美团的推送 ID……），
// 同一个 binding 下唯一。
type Event struct {
	ExternalID      string
	Kind            EventKind
	Topic           string // 渠道原始主题，留档用
	ExternalOrderID string
	ExternalItemIDs []string
	Payload         json.RawMessage
}

// Listing 是推给销售渠道的一条（门店, SKU）：绝对可售数 + 价格。
// PrevQty 是上次推出去的值（没推过为 nil），给有 CAS 的渠道（Shopify changeFromQuantity）用。
// IdemKey 每次推送不同、重推同一次相同（binding:store:sku:version）。
type Listing struct {
	StoreID, SKUID  int64
	ExternalStoreID string
	ExternalSKUID   string
	Extra           json.RawMessage // channel_item_links.extra（如 Shopify inventoryItem gid）
	Qty             int32
	PrevQty         *int32
	PriceCents      int64
	IdemKey         string
}

// ListingResult 是一条 Listing 的推送结果。Err 为空即成功。
// Conflict 为真表示渠道上的数和 PrevQty 对不上（被人改过）：ObservedQty 是渠道上的现值，
// 渠道层会以 keel 的值重推（对销售渠道 keel 是权威）并记一条差异。
type ListingResult struct {
	StoreID, SKUID int64
	Err            error
	Conflict       bool
	ObservedQty    *int32
}

// OrderRef 定位渠道上的一张订单。
type OrderRef struct {
	ExternalOrderID string
	ExternalStoreID string
}

// ActionKind 是 keel 对渠道订单做的动作。
type ActionKind string

const (
	ActAccept         ActionKind = "accept"
	ActReject         ActionKind = "reject"
	ActPicked         ActionKind = "picked"
	ActShip           ActionKind = "ship"          // 带物流单号（快递 / Shopify fulfillmentCreate）
	ActDeliveryStatus ActionKind = "delivery"      // 自配送状态
	ActAgreeRequest   ActionKind = "agree_request" // 同意平台发起的取消 / 退款
	ActRejectRequest  ActionKind = "reject_request"
	ActStockout       ActionKind = "stockout" // 商家发起缺货调整
)

// Action 是一个动作。各字段按 Kind 取用，其余为空。
type Action struct {
	Kind              ActionKind
	Reason            string
	TrackingCompany   string
	TrackingNo        string
	DeliveryStatus    string
	ExternalRequestID string
	Lines             []ActionLine // 缺货调整 / 部分退款涉及的行
	IdemKey           string
}

type ActionLine struct {
	ExternalSKUID string
	Qty           int32
}

// ChannelOrder 是渠道上一张订单的权威状态（第三期充实）。
type ChannelOrder struct {
	ExternalOrderID string
	ExternalStoreID string
	PlatformStatus  string
	Raw             json.RawMessage
}

// CatalogPage 是商品源的一页（第二期充实）。
type CatalogPage struct {
	Items      []CatalogItem
	NextCursor string // 空 = 没有下一页
}

// CatalogItem 是商品源上的一件商品（含规格），或推给渠道的一件商品。
type CatalogItem struct {
	ExternalID string
	Title      string
	Raw        json.RawMessage
}

// OnHand 是库存源（ERP）上一个（门店, SKU）的在手数。
type OnHand struct {
	ExternalStoreID, ExternalSKUID string
	Qty                            int32
	AsOf                           time.Time
}

// SaleDoc 是回写给 ERP 的一笔线上销售（虚拟收银流水 / 单据导入 / 中间表）。
type SaleDoc struct {
	DocID           string // 幂等键：同一个 DocID 推多次，ERP 侧只入账一次
	ExternalStoreID string
	Lines           []ActionLine
	At              time.Time
}

// 适配器返回的错误分类。渠道层按它们决定重试、告警或停推。
var (
	// ErrUnsupported：这个适配器不支持这个动作（Caps 没声明的能力被调用了）。
	ErrUnsupported = errors.New("渠道不支持这个动作")
	// ErrBadSignature：回调验签失败。入口回 401、不落库。
	ErrBadSignature = errors.New("渠道回调验签失败")
	// ErrCredentials：凭据失效且续不上。binding 标「凭据失效」、停推送、告警，不无限重试。
	ErrCredentials = errors.New("渠道凭据失效")
)

// RetryableError 包一个可以重试的失败（网络、5xx、限流）。After 非零是平台给的建议等待时间。
// 限流不计入任务的失败次数（RateLimited）。
type RetryableError struct {
	Err         error
	After       time.Duration
	RateLimited bool
}

func (e *RetryableError) Error() string {
	if e.RateLimited {
		return fmt.Sprintf("渠道限流（%s 后重试）：%v", e.After, e.Err)
	}
	return fmt.Sprintf("渠道暂时不可用：%v", e.Err)
}

func (e *RetryableError) Unwrap() error { return e.Err }

// IsRetryable 判断一个错误是不是可重试的。
func IsRetryable(err error) bool {
	var r *RetryableError
	return errors.As(err, &r)
}
