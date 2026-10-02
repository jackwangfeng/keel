// Package shopify 是 Shopify 的渠道适配器：商品源 + 销售渠道（spec §6.3）。
//
// 只依赖 internal/channel 与标准库。用到的平台接口（2026-10-02 在开发店上实测过，见第二期计划）：
//
//   - client credentials 换 token（POST /admin/oauth/access_token），token 24 小时过期：进程内按 binding 缓存，
//     用到 80% 就换新的；401 时丢掉重换一次。
//   - Admin GraphQL（/admin/api/2026-10/graphql.json）：每次响应带 extensions.cost.throttleStatus，
//     被限流（errors[].extensions.code = THROTTLED 或 HTTP 429）回 RetryableError{RateLimited}，渠道层不计失败次数。
//   - 回调：X-Shopify-Hmac-Sha256 = base64(HMAC-SHA256(client_secret, 原始请求体))。
//
// binding 约定：external_account 是店铺域名（xxx.myshopify.com），secrets 是 {"client_id","client_secret"}。
//
// 错误文本不拼 client secret / access token，也不拼平台的响应体原文（换 token 失败的响应体可能回显请求里的东西）；
// 渠道层写库之前还会再脱敏一遍（channel.RedactError）。
package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"regexp"
	"time"

	"github.com/keel/keel/internal/channel"
)

const (
	Kind       = "shopify"
	APIVersion = "2026-10"
	// listingBatch 是一次 inventorySetQuantities 的条数（二手资料称上限 250，按它取）。
	listingBatch = 250
)

// Options 是适配器的可替换部分（测试指向 shopifytest）。零值可用。
type Options struct {
	BaseURL    func(shop string) string // 默认 "https://" + shop
	HTTPClient *http.Client             // 默认 15 秒超时
	Now        func() time.Time
	PageSize   int // 拉商品每页几件，默认 25（查询成本见 catalog.go 文件头）
}

// Adapter 是 Shopify 适配器。一个进程一个，按 binding 缓存 token。
type Adapter struct {
	o      Options
	tokens tokenCache
}

func New(o Options) *Adapter {
	if o.BaseURL == nil {
		o.BaseURL = func(shop string) string { return "https://" + shop }
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.PageSize <= 0 {
		o.PageSize = 25
	}
	return &Adapter{o: o, tokens: tokenCache{m: map[int64]tokenEntry{}}}
}

func (a *Adapter) Kind() string { return Kind }

func (a *Adapter) Caps() channel.Caps {
	return channel.Caps{
		Roles:             channel.RoleCatalogSource | channel.RoleOutlet,
		CatalogDirection:  channel.DirIn,
		Delivery:          channel.DeliveryExpress | channel.DeliveryLocal,
		OutOfOrderInbound: true,
		ListingBatch:      listingBatch,
		PricePerStore:     false, // 价格挂在变体上，全店一个价
	}
}

type secrets struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

var shopDomain = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*\.myshopify\.com$`)

// parseBinding 取店铺域名与凭据。配错了（域名不像、凭据缺）是 ErrCredentials：重试也不会好，该停推、喊人。
func parseBinding(b channel.Binding) (string, secrets, error) {
	if !shopDomain.MatchString(b.ExternalAccount) {
		return "", secrets{}, fmt.Errorf("%w：external_account 应是 xxx.myshopify.com", channel.ErrCredentials)
	}
	var s secrets
	if err := json.Unmarshal(b.Secrets, &s); err != nil || s.ClientID == "" || s.ClientSecret == "" {
		return "", secrets{}, fmt.Errorf("%w：secrets 缺 client_id / client_secret", channel.ErrCredentials)
	}
	return b.ExternalAccount, s, nil
}

// 不做的销售渠道动作（拉单补漏、对账在第五期）；商品方向是进，不往 Shopify 建商品。订单见 order.go。

func (a *Adapter) PushCatalog(context.Context, channel.Binding, []channel.CatalogItem) error {
	return channel.ErrUnsupported
}

func (a *Adapter) ListOrders(context.Context, channel.Binding, time.Time) iter.Seq2[channel.ChannelOrder, error] {
	return func(yield func(channel.ChannelOrder, error) bool) {
		yield(channel.ChannelOrder{}, channel.ErrUnsupported)
	}
}

func (a *Adapter) ListListings(context.Context, channel.Binding, channel.StoreLink) iter.Seq2[channel.Listing, error] {
	return func(yield func(channel.Listing, error) bool) { yield(channel.Listing{}, channel.ErrUnsupported) }
}

var (
	_ channel.Adapter           = (*Adapter)(nil)
	_ channel.Outlet            = (*Adapter)(nil)
	_ channel.CatalogSource     = (*Adapter)(nil)
	_ channel.CatalogItemSource = (*Adapter)(nil)
	_ channel.WebhookInstaller  = (*Adapter)(nil)
)
