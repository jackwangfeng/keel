package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 订单后半程的履约维度（数据模型 §5）：取消、发货、确认收货。

// orderTransitionConstraint 是 00033 那个状态机触发器 RAISE 时带的约束名。
//
// **它是接口的一部分**：触发器以 23514 拒绝一次非法跳转，而 23514 也是每一条
// CHECK 的错误码（chk_amount、chk_refund_status……）。按约束名挑出来，
// 「状态机不许这条边」与「金额算错了」才是两个不同的 sentinel ——
// 前者是调用方的业务冲突，后者是我们自己的 bug，混在一起的话后者会被报成 409。
const orderTransitionConstraint = "order_status_transition"

// trackingNoConstraint 是运单号的租户内唯一索引（00033）。
const trackingNoConstraint = "uk_shipments_tracking"

var (
	// ErrIllegalOrderTransition：订单状态机触发器拒绝了这次跳转。
	//
	// 正常路径上撞不上它 —— 服务层的每一条条件 UPDATE 都带着预期的起点状态，
	// 失配时影响 0 行，根本走不到触发器。撞上它说明某条语句的谓词被改坏了，
	// 或者有人绕过服务层写库。调用方按业务冲突处理（契约里对应的那个 409），
	// 同时留一条日志。
	ErrIllegalOrderTransition = errors.New("订单状态机不允许这次跳转")

	// ErrTrackingNoDuplicated：这个承运商的这个运单号在本店已经登记过。
	// 契约：409 tracking-no-duplicated（录入重复，不是新包裹）。
	ErrTrackingNoDuplicated = errors.New("运单号已被登记过")
)

// Shipment 是一个发货包裹（契约的 Shipment）。
type Shipment struct {
	ID          int64
	CarrierCode string
	TrackingNo  string
	Status      int16
	ShippedAt   time.Time
	DeliveredAt *time.Time
}

// NewShipment 是落一个包裹要写的列。没有 MerchantID —— 那一列的默认值是
// current_merchant()（00033），调用方没有那个参数可以传错。
type NewShipment struct {
	OrderID     int64
	CarrierCode string
	TrackingNo  string
	// CreatedBy 是发货的操作员（staff.id）。审计字段，单列外键（§5）。
	CreatedBy int64
}

// FulfillmentTx 是履约这一面。
//
// 三个状态推进都返回 (bool, error)：false 表示「这一单已经不在预期的起点状态上」，
// 调用方据此回 409；error 只留给真的出了错的情形。
type FulfillmentTx interface {
	// CancelPendingOrder 买家取消：10 → 90。userID 是越权过滤。
	CancelPendingOrder(ctx context.Context, orderNo string, userID int64) (bool, error)

	// ConfirmOrderReceipt 买家确认收货：30 → 40。
	ConfirmOrderReceipt(ctx context.Context, orderNo string, userID int64) (bool, error)

	// ShipOrder 后台发货：20 → 30。
	ShipOrder(ctx context.Context, orderID int64) (bool, error)

	// InsertShipment 落一个包裹。运单号重复返回 ErrTrackingNoDuplicated。
	InsertShipment(ctx context.Context, s NewShipment) (Shipment, error)

	// ListOrderShipments 这一单的包裹。
	ListOrderShipments(ctx context.Context, orderID int64) ([]Shipment, error)

	// ListAutoConfirmableOrders 发货早于 cutoff、仍在 30、没有在途售后的订单，
	// 按发货时间从早到晚，至多 limit 笔（自动确认收货的扫描，00036）。
	ListAutoConfirmableOrders(ctx context.Context, cutoff time.Time, limit int32) ([]AutoConfirmCandidate, error)

	// OrderHasOpenRefund 这一单此刻有没有 10 / 20 / 30 的退款单。
	// 在订单行锁之下调用才有意义（申请退款也先锁订单行）。
	OrderHasOpenRefund(ctx context.Context, orderID int64) (bool, error)
}

// AutoConfirmCandidate 是自动确认收货扫到的一笔订单。
//
// 带 UserID 是因为处置走的是买家确认收货**同一条** UPDATE（ConfirmOrderReceipt，
// 谓词里有 user_id）—— 自动确认就是「系统替买家点了确认收货」，
// 状态迁移与副作用只该有一份。
type AutoConfirmCandidate struct {
	ID      int64
	OrderNo string
	UserID  int64
}

// transitionErr 把状态机触发器的 23514 挑成 ErrIllegalOrderTransition，
// 其余错误原样返回。
func transitionErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == orderTransitionConstraint {
		return fmt.Errorf("%w: %s", ErrIllegalOrderTransition, pgErr.Message)
	}
	return err
}

func (t tenantTx) CancelPendingOrder(ctx context.Context, orderNo string, userID int64) (bool, error) {
	n, err := t.q.CancelPendingOrder(ctx, db.CancelPendingOrderParams{OrderNo: orderNo, UserID: userID})
	if err != nil {
		return false, transitionErr(err)
	}
	return n == 1, nil
}

func (t tenantTx) ConfirmOrderReceipt(ctx context.Context, orderNo string, userID int64) (bool, error) {
	n, err := t.q.ConfirmOrderReceipt(ctx, db.ConfirmOrderReceiptParams{OrderNo: orderNo, UserID: userID})
	if err != nil {
		return false, transitionErr(err)
	}
	return n == 1, nil
}

func (t tenantTx) ShipOrder(ctx context.Context, orderID int64) (bool, error) {
	n, err := t.q.ShipOrder(ctx, orderID)
	if err != nil {
		return false, transitionErr(err)
	}
	return n == 1, nil
}

func (t tenantTx) InsertShipment(ctx context.Context, s NewShipment) (Shipment, error) {
	by := s.CreatedBy
	row, err := t.q.InsertShipment(ctx, db.InsertShipmentParams{
		OrderID:     s.OrderID,
		CarrierCode: s.CarrierCode,
		TrackingNo:  s.TrackingNo,
		CreatedBy:   &by,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == trackingNoConstraint {
			return Shipment{}, fmt.Errorf("%s/%s: %w", s.CarrierCode, s.TrackingNo, ErrTrackingNoDuplicated)
		}
		return Shipment{}, err
	}
	return Shipment{
		ID:          row.ID,
		CarrierCode: row.CarrierCode,
		TrackingNo:  row.TrackingNo,
		Status:      row.Status,
		ShippedAt:   row.ShippedAt.Time,
		DeliveredAt: optTime(row.DeliveredAt),
	}, nil
}

func (t tenantTx) ListOrderShipments(ctx context.Context, orderID int64) ([]Shipment, error) {
	rows, err := t.q.ListOrderShipments(ctx, orderID)
	if err != nil {
		return nil, err
	}
	out := make([]Shipment, 0, len(rows))
	for _, r := range rows {
		out = append(out, Shipment{
			ID:          r.ID,
			CarrierCode: r.CarrierCode,
			TrackingNo:  r.TrackingNo,
			Status:      r.Status,
			ShippedAt:   r.ShippedAt.Time,
			DeliveredAt: optTime(r.DeliveredAt),
		})
	}
	return out, nil
}

func (t tenantTx) ListAutoConfirmableOrders(ctx context.Context, cutoff time.Time,
	limit int32) ([]AutoConfirmCandidate, error) {
	rows, err := t.q.ListAutoConfirmableOrders(ctx, db.ListAutoConfirmableOrdersParams{
		Cutoff:    pgtype.Timestamptz{Time: cutoff, Valid: true},
		PageLimit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]AutoConfirmCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, AutoConfirmCandidate{ID: r.ID, OrderNo: r.OrderNo, UserID: r.UserID})
	}
	return out, nil
}

func (t tenantTx) OrderHasOpenRefund(ctx context.Context, orderID int64) (bool, error) {
	return t.q.OrderHasOpenRefund(ctx, orderID)
}
