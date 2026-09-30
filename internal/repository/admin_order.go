package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 后台订单与退款单的读（GET /admin/orders、GET /admin/refunds 及各自的详情，迁移 00035）
// 在 repository 边界上的那一面。
//
// 与买家侧读（order_query.go）的实质差别：**没有 user_id**。那边的 user_id 是越权过滤
// （同一家店的 A 买家不能读 B 买家的单）；这边的越权过滤是门店范围（ScopeFilter），
// 由 service/authz.go 的 orderListScope 给出，这一层只负责把它原样放进 SQL。
// 租户仍然只由 RLS 管（check_query_tenancy.py）。

// AdminOrderFilter 是后台订单列表的筛选条件（契约 GET /admin/orders 的 query 参数）。
// 全部是可空指针：nil = 不筛。
type AdminOrderFilter struct {
	Status      *int16
	StoreID     *int64
	CreatedFrom *time.Time
	CreatedTo   *time.Time
	OrderNo     *string
	Phone       *string
}

// AdminRefundFilter 是后台退款单列表的筛选条件。
type AdminRefundFilter struct {
	Status      *int16
	StoreID     *int64
	CreatedFrom *time.Time
	CreatedTo   *time.Time
}

// AdminOrder 是后台视角的一笔订单：买家侧 Order 的全部，加上两份下单时快照
// （JSONB 原样的字节，由 service 解开）与「有没有在途售后」。
type AdminOrder struct {
	Order
	ReceiverSnapshot []byte
	StoreSnapshot    []byte
	HasOpenRefund    bool
}

// StaffRef 是审核记录里的「谁」。Name 在读不到那一行员工时为 nil
// （平台级员工在租户作用域里被 RLS 挡住）。
type StaffRef struct {
	ID   int64
	Name *string
}

// AdminRefund 是后台视角的一张退款单：买家侧 Refund 的全部，加上所属订单的状态与
// 门店快照、审核记录（00035）。
type AdminRefund struct {
	Refund
	OrderStatus int16
	// OrderShippedAt 是所属订单的发货时间，没发过货为 nil。「发没发货」看它，不看 OrderStatus：
	// 50 / 60 是未发货的整单退款（2026-09-28 AI 店长把 50 当成了已发货）。
	OrderShippedAt *time.Time
	StoreSnapshot  []byte
	AuditedBy      *StaffRef
	ReceivedAt     *time.Time
	ReceivedBy     *StaffRef
}

// AdminOrderTx 是后台订单与售后的读这一面。
type AdminOrderTx interface {
	// AdminListOrders / AdminCountOrders 共用同一套谓词（db/queries/admin_orders.sql）。
	AdminListOrders(ctx context.Context, f AdminOrderFilter, only ScopeFilter, limit, offset int64) ([]AdminOrder, error)
	AdminCountOrders(ctx context.Context, f AdminOrderFilter, only ScopeFilter) (int64, error)
	// AdminFindOrderByNo 按单号取本租户的一笔订单（不含创建中的草稿）。查不到返回 ErrOrderNotFound。
	AdminFindOrderByNo(ctx context.Context, orderNo string) (AdminOrder, error)
	// StoreRegionAnyState 这家门店此刻所属的大区，含已软删的门店。查不到返回 ErrCatalogNotFound。
	StoreRegionAnyState(ctx context.Context, storeID int64) (int64, error)

	AdminListRefunds(ctx context.Context, f AdminRefundFilter, only ScopeFilter, limit, offset int64) ([]AdminRefund, error)
	AdminCountRefunds(ctx context.Context, f AdminRefundFilter, only ScopeFilter) (int64, error)
	// AdminFindRefundByNo 按编号取本租户的一张退款单。查不到返回 ErrRefundNotFound。
	AdminFindRefundByNo(ctx context.Context, refundNo string) (AdminRefund, error)
	// AdminListOrderRefunds 一个订单的全部退款单（带审核记录），按申请时间倒序。
	AdminListOrderRefunds(ctx context.Context, orderID int64) ([]AdminRefund, error)
}

// checkPaging 与 ListUserOrders 那处同一条理由：到这里还越界只可能是上游的钳制没生效，
// 截断会把「第 1 亿页」悄悄变成某一页真实数据。
func checkPaging(limit, offset int64) error {
	if limit < 0 || limit > math.MaxInt32 {
		return fmt.Errorf("limit %d 超出范围 [0, %d]", limit, math.MaxInt32)
	}
	if offset < 0 || offset > math.MaxInt32 {
		return fmt.Errorf("offset %d 超出范围 [0, %d]", offset, math.MaxInt32)
	}
	return nil
}

// AdminListOrders 按筛选条件选三条语句之一：有单号走 ByNo、否则有手机号走 ByPhone、
// 否则走万能那条。三条的谓词逐字一致，只差「哪个条件是必填」—— 把稀疏的精确条件
// 提成必填，是为了让它在通用计划里也进得了 Index Cond（理由与实测见
// db/queries/admin_orders.sql 的「稀疏的精确条件单独成句」）。
func (t tenantTx) AdminListOrders(ctx context.Context, f AdminOrderFilter, only ScopeFilter,
	limit, offset int64) ([]AdminOrder, error) {
	if err := checkPaging(limit, offset); err != nil {
		return nil, err
	}
	var rows []db.AdminGetOrderByNoRow
	switch {
	case f.OrderNo != nil:
		rs, err := t.q.AdminListOrdersByNo(ctx, db.AdminListOrdersByNoParams{
			Status: f.Status, StoreID: f.StoreID,
			CreatedFrom: optTS(f.CreatedFrom), CreatedTo: optTS(f.CreatedTo),
			OrderNo: *f.OrderNo, Phone: f.Phone,
			OnlyRegionIds: only.RegionIDs, OnlyStoreIds: only.StoreIDs,
			PageLimit: int32(limit), PageOffset: int32(offset),
		})
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			rows = append(rows, db.AdminGetOrderByNoRow(r))
		}
	case f.Phone != nil:
		rs, err := t.q.AdminListOrdersByPhone(ctx, db.AdminListOrdersByPhoneParams{
			Status: f.Status, StoreID: f.StoreID,
			CreatedFrom: optTS(f.CreatedFrom), CreatedTo: optTS(f.CreatedTo),
			Phone:         *f.Phone,
			OnlyRegionIds: only.RegionIDs, OnlyStoreIds: only.StoreIDs,
			PageLimit: int32(limit), PageOffset: int32(offset),
		})
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			rows = append(rows, db.AdminGetOrderByNoRow(r))
		}
	default:
		rs, err := t.q.AdminListOrders(ctx, db.AdminListOrdersParams{
			Status: f.Status, StoreID: f.StoreID,
			CreatedFrom: optTS(f.CreatedFrom), CreatedTo: optTS(f.CreatedTo),
			OrderNo: f.OrderNo, Phone: f.Phone,
			OnlyRegionIds: only.RegionIDs, OnlyStoreIds: only.StoreIDs,
			PageLimit: int32(limit), PageOffset: int32(offset),
		})
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			rows = append(rows, db.AdminGetOrderByNoRow(r))
		}
	}
	out := make([]AdminOrder, 0, len(rows))
	for _, r := range rows {
		out = append(out, adminOrderFromRow(r))
	}
	return out, nil
}

// AdminCountOrders 与 AdminListOrders 同一个选择规则 —— 两边选岔了，total 数的就不是
// 列表会分出来的那批行。
func (t tenantTx) AdminCountOrders(ctx context.Context, f AdminOrderFilter, only ScopeFilter) (int64, error) {
	switch {
	case f.OrderNo != nil:
		return t.q.AdminCountOrdersByNo(ctx, db.AdminCountOrdersByNoParams{
			Status: f.Status, StoreID: f.StoreID,
			CreatedFrom: optTS(f.CreatedFrom), CreatedTo: optTS(f.CreatedTo),
			OrderNo: *f.OrderNo, Phone: f.Phone,
			OnlyRegionIds: only.RegionIDs, OnlyStoreIds: only.StoreIDs,
		})
	case f.Phone != nil:
		return t.q.AdminCountOrdersByPhone(ctx, db.AdminCountOrdersByPhoneParams{
			Status: f.Status, StoreID: f.StoreID,
			CreatedFrom: optTS(f.CreatedFrom), CreatedTo: optTS(f.CreatedTo),
			Phone:         *f.Phone,
			OnlyRegionIds: only.RegionIDs, OnlyStoreIds: only.StoreIDs,
		})
	}
	return t.q.AdminCountOrders(ctx, db.AdminCountOrdersParams{
		Status: f.Status, StoreID: f.StoreID,
		CreatedFrom: optTS(f.CreatedFrom), CreatedTo: optTS(f.CreatedTo),
		OrderNo: f.OrderNo, Phone: f.Phone,
		OnlyRegionIds: only.RegionIDs, OnlyStoreIds: only.StoreIDs,
	})
}

func (t tenantTx) AdminFindOrderByNo(ctx context.Context, orderNo string) (AdminOrder, error) {
	r, err := t.q.AdminGetOrderByNo(ctx, orderNo)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminOrder{}, fmt.Errorf("order %s: %w", orderNo, ErrOrderNotFound)
	}
	if err != nil {
		return AdminOrder{}, err
	}
	return adminOrderFromRow(r), nil
}

func (t tenantTx) StoreRegionAnyState(ctx context.Context, storeID int64) (int64, error) {
	regionID, err := t.q.StoreRegionAnyState(ctx, storeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("store %d: %w", storeID, ErrCatalogNotFound)
	}
	return regionID, err
}

func adminOrderFromRow(r db.AdminGetOrderByNoRow) AdminOrder {
	return AdminOrder{
		Order: Order{
			ID:                     r.ID,
			OrderNo:                r.OrderNo,
			UserID:                 r.UserID,
			StoreID:                r.StoreID,
			RegionID:               r.RegionID,
			Status:                 r.Status,
			GoodsAmountCents:       r.GoodsAmountCents,
			FreightCents:           r.FreightCents,
			FreightDiscountCents:   r.FreightDiscountCents,
			DiscountCents:          r.DiscountCents,
			PayableCents:           r.PayableCents,
			PaidCents:              r.PaidCents,
			RefundedCents:          r.RefundedCents,
			RefundStatus:           r.RefundStatus,
			ExpireAt:               r.ExpireAt.Time,
			CreatedAt:              r.CreatedAt.Time,
			PaidAt:                 optTime(r.PaidAt),
			ShippedAt:              optTime(r.ShippedAt),
			FinishedAt:             optTime(r.FinishedAt),
			UserCouponID:           r.UserCouponID,
			CouponName:             r.CouponName,
			PromotionDiscountCents: r.PromotionDiscountCents,
			Promotions:             r.Promotions,
		},
		ReceiverSnapshot: r.ReceiverSnapshot,
		StoreSnapshot:    r.StoreSnapshot,
		HasOpenRefund:    r.HasOpenRefund,
	}
}

func (t tenantTx) AdminListRefunds(ctx context.Context, f AdminRefundFilter, only ScopeFilter,
	limit, offset int64) ([]AdminRefund, error) {
	if err := checkPaging(limit, offset); err != nil {
		return nil, err
	}
	rows, err := t.q.AdminListRefunds(ctx, db.AdminListRefundsParams{
		Status: f.Status, StoreID: f.StoreID,
		CreatedFrom: optTS(f.CreatedFrom), CreatedTo: optTS(f.CreatedTo),
		OnlyRegionIds: only.RegionIDs, OnlyStoreIds: only.StoreIDs,
		PageLimit: int32(limit), PageOffset: int32(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]AdminRefund, 0, len(rows))
	for _, r := range rows {
		out = append(out, adminRefundFromRow(db.AdminGetRefundByNoRow(r)))
	}
	return t.withAdminItems(ctx, out)
}

func (t tenantTx) AdminCountRefunds(ctx context.Context, f AdminRefundFilter, only ScopeFilter) (int64, error) {
	return t.q.AdminCountRefunds(ctx, db.AdminCountRefundsParams{
		Status: f.Status, StoreID: f.StoreID,
		CreatedFrom: optTS(f.CreatedFrom), CreatedTo: optTS(f.CreatedTo),
		OnlyRegionIds: only.RegionIDs, OnlyStoreIds: only.StoreIDs,
	})
}

func (t tenantTx) AdminFindRefundByNo(ctx context.Context, refundNo string) (AdminRefund, error) {
	r, err := t.q.AdminGetRefundByNo(ctx, refundNo)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminRefund{}, fmt.Errorf("refund %s: %w", refundNo, ErrRefundNotFound)
	}
	if err != nil {
		return AdminRefund{}, err
	}
	out, err := t.withAdminItems(ctx, []AdminRefund{adminRefundFromRow(r)})
	if err != nil {
		return AdminRefund{}, err
	}
	return out[0], nil
}

func (t tenantTx) AdminListOrderRefunds(ctx context.Context, orderID int64) ([]AdminRefund, error) {
	rows, err := t.q.AdminListOrderRefunds(ctx, orderID)
	if err != nil {
		return nil, err
	}
	out := make([]AdminRefund, 0, len(rows))
	for _, r := range rows {
		out = append(out, adminRefundFromRow(db.AdminGetRefundByNoRow(r)))
	}
	return t.withAdminItems(ctx, out)
}

// withAdminItems 给一批后台退款单挂上明细：复用 withItems（一次查询，不是 N 次）。
func (t tenantTx) withAdminItems(ctx context.Context, rs []AdminRefund) ([]AdminRefund, error) {
	base := make([]Refund, len(rs))
	for i := range rs {
		base[i] = rs[i].Refund
	}
	base, err := t.withItems(ctx, base)
	if err != nil {
		return nil, err
	}
	for i := range rs {
		rs[i].Refund = base[i]
	}
	return rs, nil
}

func adminRefundFromRow(r db.AdminGetRefundByNoRow) AdminRefund {
	out := AdminRefund{
		Refund: refundFromRow(db.GetRefundByNoRow{
			ID: r.ID, RefundNo: r.RefundNo, OrderID: r.OrderID, OrderNo: r.OrderNo,
			StoreID: r.StoreID, PaymentNo: r.PaymentNo, UserID: r.UserID,
			RefundType: r.RefundType, ReasonCode: r.ReasonCode, ReasonText: r.ReasonText,
			EvidenceUrls: r.EvidenceUrls, GoodsAmountCents: r.GoodsAmountCents,
			FreightCents: r.FreightCents, AmountCents: r.AmountCents, Status: r.Status,
			Channel: r.Channel, ChannelRefundID: r.ChannelRefundID, RejectReason: r.RejectReason,
			AuditedAt: r.AuditedAt, RefundedAt: r.RefundedAt,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
			ReturnCarrierCode: r.ReturnCarrierCode, ReturnTrackingNo: r.ReturnTrackingNo,
			ReturnSubmittedAt: r.ReturnSubmittedAt,
		}),
		OrderStatus:    r.OrderStatus,
		OrderShippedAt: optTime(r.OrderShippedAt),
		StoreSnapshot:  r.StoreSnapshot,
		ReceivedAt:     optTime(r.ReceivedAt),
	}
	if r.AuditedBy != nil {
		out.AuditedBy = &StaffRef{ID: *r.AuditedBy, Name: r.AuditedByName}
	}
	if r.ReceivedBy != nil {
		out.ReceivedBy = &StaffRef{ID: *r.ReceivedBy, Name: r.ReceivedByName}
	}
	return out
}
