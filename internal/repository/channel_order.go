package repository

// 渠道订单（00320，第三期）：channel_orders 的读写、渠道单的 keel 订单草稿、接单后的推送基线扣减。
// 编排在 service/channel_order.go 与 channel_order_saga.go。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 渠道单的规整状态（与 channel.OrderStatus 同一套数，00320 的 status 列）。
const (
	ChannelOrderPendingPayment int16 = 1
	ChannelOrderNew            int16 = 2
	ChannelOrderAccepted       int16 = 3
	ChannelOrderShipped        int16 = 4
	ChannelOrderCompleted      int16 = 5
	ChannelOrderCancelled      int16 = 6
	ChannelOrderRejected       int16 = 7
)

// ChannelOrder 是 channel_orders 的一行。Amounts / Lines / Receiver 是 JSONB 原样（形状见 00320 的注释，
// 由 service 定义与解析）。
type ChannelOrder struct {
	ID, BindingID     int64
	ExternalOrderID   string
	ExternalOrderName string
	StoreID           *int64
	OrderNo           *string
	PlatformStatus    string
	Status            int16
	Exception         *string
	AcceptDeadline    *time.Time
	DeliveryMode      int16
	Amounts           json.RawMessage
	Lines             json.RawMessage
	Receiver          json.RawMessage
	Version           int64
	LastPayload       json.RawMessage
	Test              bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ChannelOrderSnapshot 是平台快照里要落进 channel_orders 的部分。
type ChannelOrderSnapshot struct {
	BindingID         int64
	ExternalOrderID   string
	ExternalOrderName string
	StoreID           *int64
	PlatformStatus    string
	Status            int16
	AcceptDeadline    *time.Time
	DeliveryMode      int16
	Amounts           json.RawMessage
	Lines             json.RawMessage
	Receiver          json.RawMessage
	Version           int64
	LastPayload       json.RawMessage
	Test              bool
}

// ChannelOrderState 是 keel 这一侧对渠道单的处置（四列一起写）。
type ChannelOrderState struct {
	Status         int16
	OrderNo        *string
	Exception      *string
	AcceptDeadline *time.Time
}

// ChannelOrderFilter 是后台渠道单列表的筛选（BindingID / StoreID / Status 为空 = 不限）。
type ChannelOrderFilter struct {
	BindingID     *int64
	StoreID       *int64
	Status        *int16
	ExceptionOnly bool
	// Only 是员工的范围（零值不限）；store_id 空的渠道单只在不限时出现。
	Only          ScopeFilter
	Limit, Offset int32
}

// ChannelOrderRef 是一张渠道单的来源说明（后台订单的「来自 Shopify #1001」）。
type ChannelOrderRef struct {
	Channel           string // 渠道种类（binding.channel，如 shopify）
	BindingName       string
	ExternalOrderName string
}

// ChannelOrderSKU 是建 keel 订单行要的 SKU 快照。
type ChannelOrderSKU struct {
	ID, ProductID int64
	Title         string
	SpecValues    []byte
	ImageURL      *string
}

// NewChannelOrderDraft 是渠道单的 keel 订单草稿（status 0、source 1、user_id 空）。
type NewChannelOrderDraft struct {
	OrderNo              string
	ChannelOrderID       int64
	StoreID              int64
	GoodsAmountCents     int64
	FreightCents         int64
	FreightDiscountCents int64
	DiscountCents        int64 // = promotion_discount（平台补贴 + 商家补贴）
	PayableCents         int64
	ReceiverSnapshot     []byte
	Remark               *string
	ExpireAt             time.Time
}

// ChannelOrderTx 是渠道订单的那几条语句（并进 ChannelTx）。
type ChannelOrderTx interface {
	// InsertOrLockChannelOrder：第一次见到这张单就建（inserted = true），否则锁住已有的那一行（FOR UPDATE）。
	// 并发的两条回调里只有一条 inserted。
	InsertOrLockChannelOrder(ctx context.Context, s ChannelOrderSnapshot) (o ChannelOrder, inserted bool, err error)
	LockChannelOrder(ctx context.Context, id int64) (ChannelOrder, error)
	GetChannelOrder(ctx context.Context, id int64) (ChannelOrder, error)
	UpdateChannelOrderSnapshot(ctx context.Context, id int64, s ChannelOrderSnapshot) error
	TouchChannelOrderPayload(ctx context.Context, id int64, payload json.RawMessage) error
	// SetChannelOrderKeelBasis / GetChannelOrderKeelBasis：建 keel 订单时的依据（00326，形状由 service 定）。
	SetChannelOrderKeelBasis(ctx context.Context, id int64, basis json.RawMessage) error
	GetChannelOrderKeelBasis(ctx context.Context, id int64) (json.RawMessage, error)
	SetChannelOrderState(ctx context.Context, id int64, st ChannelOrderState) error
	ListChannelOrders(ctx context.Context, f ChannelOrderFilter) ([]ChannelOrder, error)
	CountChannelOrders(ctx context.Context, f ChannelOrderFilter) (int64, error)
	// ChannelOrderRefs：后台订单列表 / 详情里渠道单的来源说明（渠道、账号名、平台单号），按渠道单 id。
	ChannelOrderRefs(ctx context.Context, ids []int64) (map[int64]ChannelOrderRef, error)
	DecrementChannelListingBaseline(ctx context.Context, bindingID, storeID, skuID int64, qty int32) error
	ChannelOrderSKUs(ctx context.Context, skuIDs []int64) (map[int64]ChannelOrderSKU, error)
	CreateChannelOrderDraft(ctx context.Context, d NewChannelOrderDraft) (Order, error)
}

func channelOrderFrom(r db.GetChannelOrderRow) ChannelOrder {
	return ChannelOrder{ID: r.ID, BindingID: r.BindingID, ExternalOrderID: r.ExternalOrderID,
		ExternalOrderName: r.ExternalOrderName, StoreID: r.StoreID, OrderNo: r.OrderNo, PlatformStatus: r.PlatformStatus,
		Status: r.Status, Exception: r.Exception, AcceptDeadline: optTime(r.AcceptDeadline), DeliveryMode: r.DeliveryMode,
		Amounts: r.Amounts, Lines: r.Lines, Receiver: r.Receiver, Version: r.Version, LastPayload: r.LastPayload,
		Test: r.Test, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time}
}

func (t tenantTx) InsertOrLockChannelOrder(ctx context.Context, s ChannelOrderSnapshot) (ChannelOrder, bool, error) {
	r, err := t.q.InsertChannelOrder(ctx, db.InsertChannelOrderParams{BindingID: s.BindingID,
		ExternalOrderID: s.ExternalOrderID, ExternalOrderName: s.ExternalOrderName, StoreID: s.StoreID,
		PlatformStatus: s.PlatformStatus, Status: s.Status, AcceptDeadline: optTS(s.AcceptDeadline),
		DeliveryMode: s.DeliveryMode, Amounts: jsonOrEmpty(s.Amounts), Lines: jsonArrayOrEmpty(s.Lines),
		Receiver: jsonOrEmpty(s.Receiver), Version: s.Version, LastPayload: jsonOrEmpty(s.LastPayload), Test: s.Test})
	if err == nil {
		return channelOrderFrom(db.GetChannelOrderRow(r)), true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ChannelOrder{}, false, channelWriteErr(err)
	}
	l, err := t.q.LockChannelOrderByExternal(ctx, db.LockChannelOrderByExternalParams{BindingID: s.BindingID,
		ExternalOrderID: s.ExternalOrderID})
	if err != nil {
		return ChannelOrder{}, false, notFound(err)
	}
	return channelOrderFrom(db.GetChannelOrderRow(l)), false, nil
}

func (t tenantTx) LockChannelOrder(ctx context.Context, id int64) (ChannelOrder, error) {
	r, err := t.q.LockChannelOrder(ctx, id)
	if err != nil {
		return ChannelOrder{}, notFound(err)
	}
	return channelOrderFrom(db.GetChannelOrderRow(r)), nil
}

func (t tenantTx) GetChannelOrder(ctx context.Context, id int64) (ChannelOrder, error) {
	r, err := t.q.GetChannelOrder(ctx, id)
	if err != nil {
		return ChannelOrder{}, notFound(err)
	}
	return channelOrderFrom(r), nil
}

func (t tenantTx) UpdateChannelOrderSnapshot(ctx context.Context, id int64, s ChannelOrderSnapshot) error {
	return channelWriteErr(t.q.UpdateChannelOrderSnapshot(ctx, db.UpdateChannelOrderSnapshotParams{
		ExternalOrderName: s.ExternalOrderName, StoreID: s.StoreID, PlatformStatus: s.PlatformStatus, Status: s.Status,
		AcceptDeadline: optTS(s.AcceptDeadline), DeliveryMode: s.DeliveryMode, Amounts: jsonOrEmpty(s.Amounts),
		Lines: jsonArrayOrEmpty(s.Lines), Receiver: jsonOrEmpty(s.Receiver), Version: s.Version,
		LastPayload: jsonOrEmpty(s.LastPayload), Test: s.Test, ID: id}))
}

func (t tenantTx) TouchChannelOrderPayload(ctx context.Context, id int64, payload json.RawMessage) error {
	return t.q.TouchChannelOrderPayload(ctx, db.TouchChannelOrderPayloadParams{LastPayload: jsonOrEmpty(payload), ID: id})
}

func (t tenantTx) SetChannelOrderKeelBasis(ctx context.Context, id int64, basis json.RawMessage) error {
	return t.q.SetChannelOrderKeelBasis(ctx, db.SetChannelOrderKeelBasisParams{KeelBasis: jsonOrEmpty(basis), ID: id})
}

func (t tenantTx) GetChannelOrderKeelBasis(ctx context.Context, id int64) (json.RawMessage, error) {
	b, err := t.q.GetChannelOrderKeelBasis(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("渠道单 %d: %w", id, ErrChannelNotFound)
	}
	return b, err
}

func (t tenantTx) SetChannelOrderState(ctx context.Context, id int64, st ChannelOrderState) error {
	return channelWriteErr(t.q.SetChannelOrderState(ctx, db.SetChannelOrderStateParams{Status: st.Status,
		OrderNo: st.OrderNo, Exception: st.Exception, AcceptDeadline: optTS(st.AcceptDeadline), ID: id}))
}

func (t tenantTx) ListChannelOrders(ctx context.Context, f ChannelOrderFilter) ([]ChannelOrder, error) {
	rows, err := t.q.ListChannelOrders(ctx, db.ListChannelOrdersParams{BindingID: f.BindingID, StoreID: f.StoreID,
		Status: f.Status, ExceptionOnly: f.ExceptionOnly, OnlyRegionIds: f.Only.RegionIDs, OnlyStoreIds: f.Only.StoreIDs,
		Lim: f.Limit, Off: f.Offset})
	if err != nil {
		return nil, err
	}
	out := make([]ChannelOrder, 0, len(rows))
	for _, r := range rows {
		out = append(out, channelOrderFrom(db.GetChannelOrderRow(r)))
	}
	return out, nil
}

func (t tenantTx) CountChannelOrders(ctx context.Context, f ChannelOrderFilter) (int64, error) {
	return t.q.CountChannelOrders(ctx, db.CountChannelOrdersParams{BindingID: f.BindingID, StoreID: f.StoreID,
		Status: f.Status, ExceptionOnly: f.ExceptionOnly, OnlyRegionIds: f.Only.RegionIDs, OnlyStoreIds: f.Only.StoreIDs})
}

func (t tenantTx) ChannelOrderRefs(ctx context.Context, ids []int64) (map[int64]ChannelOrderRef, error) {
	out := map[int64]ChannelOrderRef{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := t.q.ChannelOrderRefs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ID] = ChannelOrderRef{Channel: r.Channel, BindingName: r.BindingName, ExternalOrderName: r.ExternalOrderName}
	}
	return out, nil
}

func (t tenantTx) DecrementChannelListingBaseline(ctx context.Context, bindingID, storeID, skuID int64, qty int32) error {
	return t.q.DecrementChannelListingBaseline(ctx, db.DecrementChannelListingBaselineParams{Qty: qty,
		BindingID: bindingID, StoreID: storeID, SkuID: skuID})
}

func (t tenantTx) ChannelOrderSKUs(ctx context.Context, skuIDs []int64) (map[int64]ChannelOrderSKU, error) {
	out := map[int64]ChannelOrderSKU{}
	if len(skuIDs) == 0 {
		return out, nil
	}
	rows, err := t.q.ChannelOrderSKUs(ctx, skuIDs)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ID] = ChannelOrderSKU{ID: r.ID, ProductID: r.ProductID, Title: r.Title, SpecValues: r.SpecValues, ImageURL: r.ImageUrl}
	}
	return out, nil
}

func (t tenantTx) CreateChannelOrderDraft(ctx context.Context, d NewChannelOrderDraft) (Order, error) {
	r, err := t.q.CreateChannelOrderDraft(ctx, db.CreateChannelOrderDraftParams{OrderNo: d.OrderNo,
		ChannelOrderID: d.ChannelOrderID, StoreID: d.StoreID, GoodsAmountCents: d.GoodsAmountCents,
		FreightCents: d.FreightCents, FreightDiscountCents: d.FreightDiscountCents, DiscountCents: d.DiscountCents,
		PayableCents: d.PayableCents, ReceiverSnapshot: d.ReceiverSnapshot, Remark: d.Remark,
		ExpireAt: pgtype.Timestamptz{Time: d.ExpireAt, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, fmt.Errorf("store %d: %w", d.StoreID, ErrCatalogBadReference)
	}
	if err != nil {
		return Order{}, err
	}
	cid := d.ChannelOrderID
	return Order{ID: r.ID, OrderNo: r.OrderNo, Source: OrderSourceChannel, ChannelOrderID: &cid, StoreID: r.StoreID,
		RegionID: r.RegionID, Status: r.Status, GoodsAmountCents: r.GoodsAmountCents, FreightCents: r.FreightCents,
		FreightDiscountCents: r.FreightDiscountCents, DiscountCents: r.DiscountCents, PayableCents: r.PayableCents,
		PaidCents: r.PaidCents, RefundedCents: r.RefundedCents, RefundStatus: r.RefundStatus, ExpireAt: r.ExpireAt.Time,
		CreatedAt: r.CreatedAt.Time, PromotionDiscountCents: r.PromotionDiscountCents}, nil
}

func jsonArrayOrEmpty(b json.RawMessage) []byte {
	if len(b) == 0 {
		return []byte("[]")
	}
	return b
}
