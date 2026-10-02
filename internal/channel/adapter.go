package channel

import (
	"context"
	"iter"
	"net/http"
	"time"
)

// Adapter 是每个适配器都要实现的部分：身份、能力、回调解析。
// 其余能力按角色拆成下面几个接口，适配器只实现自己能当的那几个；渠道层用类型断言取用。
type Adapter interface {
	Kind() string
	Caps() Caps
	// ParseInbound 验签并解析一次回调，产出规整后的事件；ack 是平台要求的应答体
	// （Shopify 回 200 空体、美团回 {"code":0}……）。验签失败返回 ErrBadSignature。
	// body 是原始请求体（验签要原样的字节），r 只用来读头。
	ParseInbound(b Binding, r *http.Request, body []byte) (evs []Event, ack []byte, err error)
}

// CatalogSource：商品主数据源（Shopify / ERP）。cursor 为空取第一页。
type CatalogSource interface {
	PullCatalog(ctx context.Context, b Binding, cursor string) (CatalogPage, error)
}

// CatalogItemSource：按外部 ID 拉一件商品（回调往往只给了 ID）。found = false 表示渠道上已经没有这件了（删了）。
type CatalogItemSource interface {
	PullItem(ctx context.Context, b Binding, externalID string) (item CatalogItem, found bool, err error)
}

// WebhookInstaller：把这个 binding 需要的回调订阅装到渠道上（幂等：已有同主题同地址的不重复建）。
// 平台要靠后台手工配回调的渠道不实现它。
type WebhookInstaller interface {
	EnsureWebhooks(ctx context.Context, b Binding, callbackURL string) error
}

// StockSource：库存数据源（ERP，只读从库或视图）。
type StockSource interface {
	PullOnHand(ctx context.Context, b Binding, stores []StoreLink) ([]OnHand, error)
}

// Outlet：销售渠道。
type Outlet interface {
	// PushListings 推一批（门店, SKU）的绝对可售数与价格。每条一个结果，顺序与入参一致；
	// 整批失败（网络断了）可以只返回 error。适配器自己按平台规则分批、限流。
	PushListings(ctx context.Context, b Binding, ls []Listing) ([]ListingResult, error)
	// PushCatalog 往平台建 / 改商品（CatalogDirection == DirOut 的渠道）。
	PushCatalog(ctx context.Context, b Binding, items []CatalogItem) error
	// Act 对一张渠道订单做一个动作。
	Act(ctx context.Context, b Binding, o OrderRef, a Action) error
	// FetchOrder 回读一张订单的权威状态。
	FetchOrder(ctx context.Context, b Binding, externalOrderID string) (ChannelOrder, error)
	// ListOrders / ListListings 给对账用：某一天（渠道时区）的订单、某家门店的当前可售数。
	ListOrders(ctx context.Context, b Binding, day time.Time) iter.Seq2[ChannelOrder, error]
	ListListings(ctx context.Context, b Binding, store StoreLink) iter.Seq2[Listing, error]
}

// SalesSink：线上销售回写（ERP 虚拟收银流水 / 单据导入 / 中间表），幂等按 SaleDoc.DocID。
type SalesSink interface {
	PostSale(ctx context.Context, b Binding, doc SaleDoc) error
}
