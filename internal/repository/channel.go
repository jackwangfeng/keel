package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 渠道适配层（00301，数据模型文档第十七节）的读写。编排在 service/channel*.go。
//
// secrets 只有 ChannelBindingSecrets / SetChannelBindingSecrets 两个方法碰：ChannelBinding 结构体里
// 根本没有这个字段，于是任何把 binding 原样序列化出去的代码都泄露不了它。

// binding 的角色位与状态（与 00301 的 CHECK 同一套数）。
const (
	ChannelRoleCatalogSource int16 = 1
	ChannelRoleStockSource   int16 = 2
	ChannelRoleOutlet        int16 = 4

	ChannelBindingActive       int16 = 1
	ChannelBindingDisabled     int16 = 2
	ChannelBindingCredentialsX int16 = 3 // 凭据失效
)

// 回调事件的处理状态（channel_inbound_events.status）。
const (
	ChannelEventPending int16 = 0
	ChannelEventDone    int16 = 1
	ChannelEventIgnored int16 = 2
	ChannelEventFailed  int16 = 3
)

var (
	// ErrChannelNotFound：binding / 规则 / 映射在当前租户下不存在。
	ErrChannelNotFound = errors.New("渠道对象在当前租户下不存在")
	// ErrChannelDuplicate：同一个商家、同一个渠道、同一个外部账号已经有 binding，
	// 或者一个渠道门店已经映射给了别的 keel 门店。
	ErrChannelDuplicate = errors.New("渠道对象重复")
	// ErrChannelRefInvalid：引用的门店 / SKU 不存在（外键）。
	ErrChannelRefInvalid = errors.New("渠道规则或映射引用的门店 / SKU 不存在")
)

type ChannelBinding struct {
	ID              int64
	Channel         string
	ExternalAccount string
	Name            string
	Roles           int16
	Status          int16
	Config          json.RawMessage
	HasSecrets      bool // 配过凭据没有（只有这个标志；值只经 ChannelBindingSecrets 读）
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// IsActiveOutlet：启用中的销售渠道。
func (b ChannelBinding) IsActiveOutlet() bool {
	return b.Status == ChannelBindingActive && b.Roles&ChannelRoleOutlet != 0
}

// IsActiveCatalogSource：启用中、且当商品源（商品从它进 keel）。
func (b ChannelBinding) IsActiveCatalogSource() bool {
	return b.Status == ChannelBindingActive && b.Roles&ChannelRoleCatalogSource != 0
}

type ChannelBindingInput struct {
	Channel, ExternalAccount, Name string
	Roles, Status                  int16
	Config                         json.RawMessage
}

// ChannelBindingPatch 里为 nil 的字段不改。
type ChannelBindingPatch struct {
	Name          *string
	Roles, Status *int16
	Config        json.RawMessage
}

// OutletBinding 是一家门店映射过的启用中的销售渠道 binding，连同它在渠道上的门店 ID。
type OutletBinding struct {
	ChannelBinding
	ExternalStoreID string
}

type ChannelStoreLink struct {
	BindingID, StoreID int64
	ExternalStoreID    string
	CreatedAt          time.Time
}

type ChannelStockRule struct {
	ID, BindingID      int64
	StoreID, SKUID     *int64
	RatioBP, SafetyQty int32
	CapQty             *int32
	UpdatedAt          time.Time
}

type ChannelPriceRule struct {
	ID, BindingID int64
	SKUID         *int64
	MarkupBP      int32
	FixedCents    *int64
	UpdatedAt     time.Time
}

type ChannelListing struct {
	BindingID, StoreID, SKUID int64
	PublishedQty              int32
	PublishedCents            int64
	Version                   int64
	PushedAt                  time.Time
	LastError                 *string
	SKUCode, ProductTitle     string // 只有 ListChannelListingsPage（后台）填
}

type ChannelInboundEventInput struct {
	BindingID       int64
	ExternalEventID string
	Topic           string
	Payload         json.RawMessage
	Status          int16
}

type ChannelInboundEvent struct {
	ID, BindingID   int64
	ExternalEventID string
	Topic           string
	Payload         json.RawMessage
	Status          int16
	Error           *string
	ReceivedAt      time.Time
	ProcessedAt     *time.Time
}

// ChannelTx 是渠道表在一个租户事务里的读写。
type ChannelTx interface {
	ChannelOrderTx
	ChannelOrderRequestTx

	CreateChannelBinding(ctx context.Context, in ChannelBindingInput) (ChannelBinding, error)
	GetChannelBinding(ctx context.Context, id int64) (ChannelBinding, error)
	ListChannelBindings(ctx context.Context) ([]ChannelBinding, error)
	UpdateChannelBinding(ctx context.Context, id int64, p ChannelBindingPatch) (ChannelBinding, error)
	// ChannelBindingSecrets 只给适配器用（验签、调平台 API），不许出现在任何响应里。
	ChannelBindingSecrets(ctx context.Context, id int64) (json.RawMessage, error)
	SetChannelBindingSecrets(ctx context.Context, id int64, secrets json.RawMessage) error
	CountActiveOutletBindings(ctx context.Context) (int64, error)
	ListActiveOutletBindingsForStore(ctx context.Context, storeID int64) ([]OutletBinding, error)

	UpsertChannelStoreLink(ctx context.Context, l ChannelStoreLink) error
	DeleteChannelStoreLink(ctx context.Context, bindingID, storeID int64) error
	ListChannelStoreLinks(ctx context.Context, bindingID int64) ([]ChannelStoreLink, error)

	UpsertChannelStockRule(ctx context.Context, r ChannelStockRule) (ChannelStockRule, error)
	DeleteChannelStockRule(ctx context.Context, bindingID, id int64) error
	ListChannelStockRules(ctx context.Context, bindingID int64) ([]ChannelStockRule, error)
	// ChannelStockRulesForStore：渠道级 + 这家门店的规则（算对外可售数用）。
	ChannelStockRulesForStore(ctx context.Context, bindingID, storeID int64) ([]ChannelStockRule, error)

	UpsertChannelPriceRule(ctx context.Context, r ChannelPriceRule) (ChannelPriceRule, error)
	DeleteChannelPriceRule(ctx context.Context, bindingID, id int64) error
	ListChannelPriceRules(ctx context.Context, bindingID int64) ([]ChannelPriceRule, error)

	ChannelListings(ctx context.Context, bindingID, storeID int64, skuIDs []int64) ([]ChannelListing, error)
	// RecordChannelListing 记下一次成功推送的值，返回新版本号。
	RecordChannelListing(ctx context.Context, l ChannelListing) (int64, error)
	SetChannelListingError(ctx context.Context, bindingID, storeID, skuID int64, msg string) error
	ListChannelListingsPage(ctx context.Context, bindingID int64, storeID *int64, errorsOnly bool, limit, offset int32) ([]ChannelListing, error)

	// InsertChannelInboundEvent：重复的外部事件 ID 返回 inserted=false、不报错。
	InsertChannelInboundEvent(ctx context.Context, in ChannelInboundEventInput) (id int64, inserted bool, err error)
	GetChannelInboundEvent(ctx context.Context, id int64) (ChannelInboundEvent, error)
	MarkChannelInboundEvent(ctx context.Context, id int64, status int16, errMsg *string) error

	// ChannelSyncRev 是「开关渠道」消息的版本（数据库时钟的微秒数，单调）。
	ChannelSyncRev(ctx context.Context) (int64, error)
	// ChannelSKUOffers：这家门店这批 SKU 的就近生效价与能不能卖。查不到的 SKU 不在结果里。
	ChannelSKUOffers(ctx context.Context, storeID int64, skuIDs []int64) (map[int64]SKUOffer, error)
	UpsertChannelItemLink(ctx context.Context, l ChannelItemLink) error
	ChannelItemLinks(ctx context.Context, bindingID int64, kind int16, keelIDs []int64) (map[int64]ChannelItemLink, error)
	LinkedSKUIDsPage(ctx context.Context, bindingID, after int64, limit int32) ([]int64, error)
	// ChannelItemLinkByExternal 按外部 ID 反查映射；没有返回 ErrChannelNotFound。
	ChannelItemLinkByExternal(ctx context.Context, bindingID int64, kind int16, externalID string) (ChannelItemLink, error)
	DeleteChannelItemLink(ctx context.Context, bindingID int64, kind int16, keelID int64) error
	// ChannelSKUsByCodes 按货号找 SKU（含已删的，Deleted 标出来），键是货号。
	ChannelSKUsByCodes(ctx context.Context, codes []string) (map[string]CodedSKU, error)
	// LockChannelMerchant 在本事务里拿这家店的渠道启停锁（提交即释放）。
	LockChannelMerchant(ctx context.Context) error
	ChannelSKUExists(ctx context.Context, skuID int64) (bool, error)
	// ChannelManagedProducts：这批商品里由启用中的商品源管理的那些 → 渠道（kind）。不在结果里 = keel 自己管。
	ChannelManagedProducts(ctx context.Context, productIDs []int64) (map[int64]string, error)
}

// channel_item_links.kind
const (
	ChannelItemProduct int16 = 1
	ChannelItemSKU     int16 = 2
)

// CodedSKU 是按货号找到的一个 SKU。
type CodedSKU struct {
	ID, ProductID int64
	Code          string
	Deleted       bool
}

type ChannelItemLink struct {
	BindingID  int64
	Kind       int16
	KeelID     int64
	ExternalID string
	Extra      json.RawMessage
	SyncedAt   time.Time
}

// SKUOffer 是一个 SKU 在一家门店的就近生效价与能不能卖的三个因素。
type SKUOffer struct {
	PriceCents       int64
	SKUActive        bool // SKU 启用且没删
	ProductLive      bool // 商品没删
	ProductPublished bool // 商品已上架
}

// Sellable：这个 SKU 能不能在渠道上卖。catalogOwned 为真（binding 本身就是这件商品的商品源，比如 Shopify）时
// 不看 keel 的上架状态 —— 上下架归商品源管（拉商品时同步过来），否则 keel 里一件还没上架的商品
// 会把商品源上的现货推成 0。
func (o SKUOffer) Sellable(catalogOwned bool) bool {
	return o.SKUActive && o.ProductLive && (o.ProductPublished || catalogOwned)
}

func bindingFrom(id int64, channel, account, name string, roles, status int16, config []byte, hasSecrets bool,
	created, updated pgtype.Timestamptz) ChannelBinding {
	return ChannelBinding{ID: id, Channel: channel, ExternalAccount: account, Name: name, Roles: roles,
		Status: status, Config: json.RawMessage(config), HasSecrets: hasSecrets, CreatedAt: created.Time, UpdatedAt: updated.Time}
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrChannelNotFound
	}
	return err
}

func channelWriteErr(err error) error {
	switch {
	case err == nil:
		return nil
	case isUniqueViolation(err, "uk_channel_bindings_account"), isUniqueViolation(err, "uk_channel_store_links_external"),
		isUniqueViolation(err, "uk_channel_item_links_external"):
		return ErrChannelDuplicate
	case isForeignKeyViolation(err):
		return ErrChannelRefInvalid
	}
	return notFound(err)
}

func jsonOrEmpty(b json.RawMessage) []byte {
	if len(b) == 0 {
		return []byte("{}")
	}
	return b
}

func (t tenantTx) CreateChannelBinding(ctx context.Context, in ChannelBindingInput) (ChannelBinding, error) {
	r, err := t.q.CreateChannelBinding(ctx, db.CreateChannelBindingParams{
		Channel: in.Channel, ExternalAccount: in.ExternalAccount, Name: in.Name,
		Roles: in.Roles, Status: in.Status, Config: jsonOrEmpty(in.Config),
	})
	if err != nil {
		return ChannelBinding{}, channelWriteErr(err)
	}
	return bindingFrom(r.ID, r.Channel, r.ExternalAccount, r.Name, r.Roles, r.Status, r.Config, r.HasSecrets, r.CreatedAt, r.UpdatedAt), nil
}

func (t tenantTx) GetChannelBinding(ctx context.Context, id int64) (ChannelBinding, error) {
	r, err := t.q.GetChannelBinding(ctx, id)
	if err != nil {
		return ChannelBinding{}, notFound(err)
	}
	return bindingFrom(r.ID, r.Channel, r.ExternalAccount, r.Name, r.Roles, r.Status, r.Config, r.HasSecrets, r.CreatedAt, r.UpdatedAt), nil
}

func (t tenantTx) ListChannelBindings(ctx context.Context) ([]ChannelBinding, error) {
	rows, err := t.q.ListChannelBindings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelBinding, 0, len(rows))
	for _, r := range rows {
		out = append(out, bindingFrom(r.ID, r.Channel, r.ExternalAccount, r.Name, r.Roles, r.Status, r.Config, r.HasSecrets, r.CreatedAt, r.UpdatedAt))
	}
	return out, nil
}

func (t tenantTx) UpdateChannelBinding(ctx context.Context, id int64, p ChannelBindingPatch) (ChannelBinding, error) {
	var cfg []byte
	if len(p.Config) > 0 {
		cfg = p.Config
	}
	r, err := t.q.UpdateChannelBinding(ctx, db.UpdateChannelBindingParams{
		ID: id, Name: p.Name, Roles: p.Roles, Status: p.Status, Config: cfg,
	})
	if err != nil {
		return ChannelBinding{}, channelWriteErr(err)
	}
	return bindingFrom(r.ID, r.Channel, r.ExternalAccount, r.Name, r.Roles, r.Status, r.Config, r.HasSecrets, r.CreatedAt, r.UpdatedAt), nil
}

func (t tenantTx) ChannelBindingSecrets(ctx context.Context, id int64) (json.RawMessage, error) {
	s, err := t.q.GetChannelBindingSecrets(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return json.RawMessage(s), nil
}

func (t tenantTx) SetChannelBindingSecrets(ctx context.Context, id int64, secrets json.RawMessage) error {
	n, err := t.q.SetChannelBindingSecrets(ctx, db.SetChannelBindingSecretsParams{ID: id, Secrets: jsonOrEmpty(secrets)})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrChannelNotFound
	}
	return nil
}

func (t tenantTx) CountActiveOutletBindings(ctx context.Context) (int64, error) {
	return t.q.CountActiveOutletBindings(ctx)
}

func (t tenantTx) ListActiveOutletBindingsForStore(ctx context.Context, storeID int64) ([]OutletBinding, error) {
	rows, err := t.q.ListActiveOutletBindingsForStore(ctx, storeID)
	if err != nil {
		return nil, err
	}
	out := make([]OutletBinding, 0, len(rows))
	for _, r := range rows {
		out = append(out, OutletBinding{
			ChannelBinding:  bindingFrom(r.ID, r.Channel, r.ExternalAccount, r.Name, r.Roles, r.Status, r.Config, r.HasSecrets, r.CreatedAt, r.UpdatedAt),
			ExternalStoreID: r.ExternalStoreID,
		})
	}
	return out, nil
}

func (t tenantTx) UpsertChannelStoreLink(ctx context.Context, l ChannelStoreLink) error {
	return channelWriteErr(t.q.UpsertChannelStoreLink(ctx, db.UpsertChannelStoreLinkParams{
		BindingID: l.BindingID, StoreID: l.StoreID, ExternalStoreID: l.ExternalStoreID,
	}))
}

func (t tenantTx) DeleteChannelStoreLink(ctx context.Context, bindingID, storeID int64) error {
	n, err := t.q.DeleteChannelStoreLink(ctx, db.DeleteChannelStoreLinkParams{BindingID: bindingID, StoreID: storeID})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrChannelNotFound
	}
	return nil
}

func (t tenantTx) ListChannelStoreLinks(ctx context.Context, bindingID int64) ([]ChannelStoreLink, error) {
	rows, err := t.q.ListChannelStoreLinks(ctx, bindingID)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelStoreLink, 0, len(rows))
	for _, r := range rows {
		out = append(out, ChannelStoreLink{BindingID: r.BindingID, StoreID: r.StoreID,
			ExternalStoreID: r.ExternalStoreID, CreatedAt: r.CreatedAt.Time})
	}
	return out, nil
}

func stockRuleFrom(id, binding int64, store, sku *int64, ratio, safety int32, capq *int32, upd pgtype.Timestamptz) ChannelStockRule {
	return ChannelStockRule{ID: id, BindingID: binding, StoreID: store, SKUID: sku, RatioBP: ratio,
		SafetyQty: safety, CapQty: capq, UpdatedAt: upd.Time}
}

func (t tenantTx) UpsertChannelStockRule(ctx context.Context, r ChannelStockRule) (ChannelStockRule, error) {
	o, err := t.q.UpsertChannelStockRule(ctx, db.UpsertChannelStockRuleParams{
		BindingID: r.BindingID, StoreID: r.StoreID, SkuID: r.SKUID, RatioBp: r.RatioBP, SafetyQty: r.SafetyQty, CapQty: r.CapQty,
	})
	if err != nil {
		return ChannelStockRule{}, channelWriteErr(err)
	}
	return stockRuleFrom(o.ID, o.BindingID, o.StoreID, o.SkuID, o.RatioBp, o.SafetyQty, o.CapQty, o.UpdatedAt), nil
}

func (t tenantTx) DeleteChannelStockRule(ctx context.Context, bindingID, id int64) error {
	n, err := t.q.DeleteChannelStockRule(ctx, db.DeleteChannelStockRuleParams{BindingID: bindingID, ID: id})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrChannelNotFound
	}
	return nil
}

func (t tenantTx) ListChannelStockRules(ctx context.Context, bindingID int64) ([]ChannelStockRule, error) {
	rows, err := t.q.ListChannelStockRules(ctx, bindingID)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelStockRule, 0, len(rows))
	for _, o := range rows {
		out = append(out, stockRuleFrom(o.ID, o.BindingID, o.StoreID, o.SkuID, o.RatioBp, o.SafetyQty, o.CapQty, o.UpdatedAt))
	}
	return out, nil
}

func (t tenantTx) ChannelStockRulesForStore(ctx context.Context, bindingID, storeID int64) ([]ChannelStockRule, error) {
	rows, err := t.q.ListChannelStockRulesForStore(ctx, db.ListChannelStockRulesForStoreParams{BindingID: bindingID, StoreID: storeID})
	if err != nil {
		return nil, err
	}
	out := make([]ChannelStockRule, 0, len(rows))
	for _, o := range rows {
		out = append(out, stockRuleFrom(o.ID, o.BindingID, o.StoreID, o.SkuID, o.RatioBp, o.SafetyQty, o.CapQty, o.UpdatedAt))
	}
	return out, nil
}

func (t tenantTx) UpsertChannelPriceRule(ctx context.Context, r ChannelPriceRule) (ChannelPriceRule, error) {
	o, err := t.q.UpsertChannelPriceRule(ctx, db.UpsertChannelPriceRuleParams{
		BindingID: r.BindingID, SkuID: r.SKUID, MarkupBp: r.MarkupBP, FixedCents: r.FixedCents,
	})
	if err != nil {
		return ChannelPriceRule{}, channelWriteErr(err)
	}
	return ChannelPriceRule{ID: o.ID, BindingID: o.BindingID, SKUID: o.SkuID, MarkupBP: o.MarkupBp,
		FixedCents: o.FixedCents, UpdatedAt: o.UpdatedAt.Time}, nil
}

func (t tenantTx) DeleteChannelPriceRule(ctx context.Context, bindingID, id int64) error {
	n, err := t.q.DeleteChannelPriceRule(ctx, db.DeleteChannelPriceRuleParams{BindingID: bindingID, ID: id})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrChannelNotFound
	}
	return nil
}

func (t tenantTx) ListChannelPriceRules(ctx context.Context, bindingID int64) ([]ChannelPriceRule, error) {
	rows, err := t.q.ListChannelPriceRules(ctx, bindingID)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelPriceRule, 0, len(rows))
	for _, o := range rows {
		out = append(out, ChannelPriceRule{ID: o.ID, BindingID: o.BindingID, SKUID: o.SkuID, MarkupBP: o.MarkupBp,
			FixedCents: o.FixedCents, UpdatedAt: o.UpdatedAt.Time})
	}
	return out, nil
}

func listingFrom(binding, store, sku int64, qty int32, cents, ver int64, pushed pgtype.Timestamptz, lastErr *string) ChannelListing {
	return ChannelListing{BindingID: binding, StoreID: store, SKUID: sku, PublishedQty: qty, PublishedCents: cents,
		Version: ver, PushedAt: pushed.Time, LastError: lastErr}
}

func (t tenantTx) ChannelListings(ctx context.Context, bindingID, storeID int64, skuIDs []int64) ([]ChannelListing, error) {
	if len(skuIDs) == 0 {
		return []ChannelListing{}, nil
	}
	rows, err := t.q.GetChannelListings(ctx, db.GetChannelListingsParams{BindingID: bindingID, StoreID: storeID, SkuIds: skuIDs})
	if err != nil {
		return nil, err
	}
	out := make([]ChannelListing, 0, len(rows))
	for _, r := range rows {
		out = append(out, listingFrom(r.BindingID, r.StoreID, r.SkuID, r.PublishedQty, r.PublishedCents, r.Version, r.PushedAt, r.LastError))
	}
	return out, nil
}

func (t tenantTx) RecordChannelListing(ctx context.Context, l ChannelListing) (int64, error) {
	v, err := t.q.UpsertChannelListing(ctx, db.UpsertChannelListingParams{
		BindingID: l.BindingID, StoreID: l.StoreID, SkuID: l.SKUID, PublishedQty: l.PublishedQty, PublishedCents: l.PublishedCents,
	})
	return v, channelWriteErr(err)
}

func (t tenantTx) SetChannelListingError(ctx context.Context, bindingID, storeID, skuID int64, msg string) error {
	return t.q.SetChannelListingError(ctx, db.SetChannelListingErrorParams{BindingID: bindingID, StoreID: storeID, SkuID: skuID, LastError: msg})
}

func (t tenantTx) ListChannelListingsPage(ctx context.Context, bindingID int64, storeID *int64, errorsOnly bool, limit, offset int32) ([]ChannelListing, error) {
	rows, err := t.q.ListChannelListingsPage(ctx, db.ListChannelListingsPageParams{BindingID: bindingID, StoreID: storeID,
		ErrorsOnly: errorsOnly, Lim: limit, Off: offset})
	if err != nil {
		return nil, err
	}
	out := make([]ChannelListing, 0, len(rows))
	for _, r := range rows {
		l := listingFrom(r.BindingID, r.StoreID, r.SkuID, r.PublishedQty, r.PublishedCents, r.Version, r.PushedAt, r.LastError)
		l.SKUCode, l.ProductTitle = r.SkuCode, r.ProductTitle
		out = append(out, l)
	}
	return out, nil
}

func (t tenantTx) InsertChannelInboundEvent(ctx context.Context, in ChannelInboundEventInput) (int64, bool, error) {
	id, err := t.q.InsertChannelInboundEvent(ctx, db.InsertChannelInboundEventParams{
		BindingID: in.BindingID, ExternalEventID: in.ExternalEventID, Topic: in.Topic,
		Payload: jsonOrEmpty(in.Payload), Status: in.Status,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, channelWriteErr(err)
	}
	return id, true, nil
}

func (t tenantTx) GetChannelInboundEvent(ctx context.Context, id int64) (ChannelInboundEvent, error) {
	r, err := t.q.GetChannelInboundEvent(ctx, id)
	if err != nil {
		return ChannelInboundEvent{}, notFound(err)
	}
	return ChannelInboundEvent{ID: r.ID, BindingID: r.BindingID, ExternalEventID: r.ExternalEventID, Topic: r.Topic,
		Payload: json.RawMessage(r.Payload), Status: r.Status, Error: r.Error, ReceivedAt: r.ReceivedAt.Time,
		ProcessedAt: tsPtr(r.ProcessedAt)}, nil
}

func (t tenantTx) MarkChannelInboundEvent(ctx context.Context, id int64, status int16, errMsg *string) error {
	return t.q.MarkChannelInboundEvent(ctx, db.MarkChannelInboundEventParams{ID: id, Status: status, Error: errMsg})
}

// —— 库存库：开了渠道的商家（00300）

func (t invTx) SetChannelMerchant(ctx context.Context, enabled bool, rev int64) (bool, error) {
	n, err := t.q.InvSetChannelMerchant(ctx, db.InvSetChannelMerchantParams{Enabled: enabled, Rev: rev})
	return n > 0, err
}

func (t invTx) ChannelMerchantEnabled(ctx context.Context) (bool, error) {
	return t.q.InvChannelMerchantEnabled(ctx)
}

func (t tenantTx) ChannelSyncRev(ctx context.Context) (int64, error) { return t.q.ChannelSyncRev(ctx) }

func (t tenantTx) ChannelSKUOffers(ctx context.Context, storeID int64, skuIDs []int64) (map[int64]SKUOffer, error) {
	out := make(map[int64]SKUOffer, len(skuIDs))
	if len(skuIDs) == 0 {
		return out, nil
	}
	rows, err := t.q.ChannelSKUOffers(ctx, db.ChannelSKUOffersParams{StoreID: storeID, SkuIds: skuIDs})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.SkuID] = SKUOffer{PriceCents: r.PriceCents, SKUActive: r.SkuActive, ProductLive: r.ProductLive,
			ProductPublished: r.ProductPublished}
	}
	return out, nil
}

func (t tenantTx) UpsertChannelItemLink(ctx context.Context, l ChannelItemLink) error {
	return channelWriteErr(t.q.UpsertChannelItemLink(ctx, db.UpsertChannelItemLinkParams{
		BindingID: l.BindingID, Kind: l.Kind, KeelID: l.KeelID, ExternalID: l.ExternalID, Extra: jsonOrEmpty(l.Extra),
	}))
}

func (t tenantTx) ChannelItemLinks(ctx context.Context, bindingID int64, kind int16, keelIDs []int64) (map[int64]ChannelItemLink, error) {
	out := make(map[int64]ChannelItemLink, len(keelIDs))
	if len(keelIDs) == 0 {
		return out, nil
	}
	rows, err := t.q.ListChannelItemLinks(ctx, db.ListChannelItemLinksParams{BindingID: bindingID, Kind: kind, KeelIds: keelIDs})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.KeelID] = ChannelItemLink{BindingID: r.BindingID, Kind: r.Kind, KeelID: r.KeelID, ExternalID: r.ExternalID,
			Extra: json.RawMessage(r.Extra), SyncedAt: r.SyncedAt.Time}
	}
	return out, nil
}

func (t tenantTx) LinkedSKUIDsPage(ctx context.Context, bindingID, after int64, limit int32) ([]int64, error) {
	return t.q.ListLinkedSKUIDsPage(ctx, db.ListLinkedSKUIDsPageParams{BindingID: bindingID, After: after, Lim: limit})
}

func (t tenantTx) LockChannelMerchant(ctx context.Context) error { return t.q.LockChannelMerchant(ctx) }

func (t tenantTx) ChannelSKUExists(ctx context.Context, skuID int64) (bool, error) {
	return t.q.ChannelSKUExists(ctx, skuID)
}

func (t tenantTx) ChannelItemLinkByExternal(ctx context.Context, bindingID int64, kind int16, externalID string) (ChannelItemLink, error) {
	r, err := t.q.GetChannelItemLinkByExternal(ctx, db.GetChannelItemLinkByExternalParams{BindingID: bindingID, Kind: kind, ExternalID: externalID})
	if err != nil {
		return ChannelItemLink{}, notFound(err)
	}
	return ChannelItemLink{BindingID: r.BindingID, Kind: r.Kind, KeelID: r.KeelID, ExternalID: r.ExternalID,
		Extra: json.RawMessage(r.Extra), SyncedAt: r.SyncedAt.Time}, nil
}

func (t tenantTx) DeleteChannelItemLink(ctx context.Context, bindingID int64, kind int16, keelID int64) error {
	_, err := t.q.DeleteChannelItemLink(ctx, db.DeleteChannelItemLinkParams{BindingID: bindingID, Kind: kind, KeelID: keelID})
	return err
}

func (t tenantTx) ChannelSKUsByCodes(ctx context.Context, codes []string) (map[string]CodedSKU, error) {
	out := make(map[string]CodedSKU, len(codes))
	if len(codes) == 0 {
		return out, nil
	}
	rows, err := t.q.ChannelSKUsByCodes(ctx, codes)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.SkuCode] = CodedSKU{ID: r.ID, ProductID: r.ProductID, Code: r.SkuCode, Deleted: r.Deleted}
	}
	return out, nil
}

func (t tenantTx) ChannelManagedProducts(ctx context.Context, productIDs []int64) (map[int64]string, error) {
	if len(productIDs) == 0 {
		return map[int64]string{}, nil
	}
	rows, err := t.q.ChannelManagedProducts(ctx, productIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]string, len(rows))
	for _, r := range rows {
		out[r.ProductID] = r.Channel
	}
	return out, nil
}
