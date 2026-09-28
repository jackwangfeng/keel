package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 多收款：支付意图与退回单（00150，service/payment_intent.go、service/payment_return.go）。

// PaymentIntentStatus：1 有效 / 2 已作废（换了渠道）/ 3 已入账。
const (
	PaymentIntentActive     int16 = 1
	PaymentIntentSuperseded int16 = 2
	PaymentIntentSettled    int16 = 3
)

// PaymentReturnReason：1 重复支付 / 2 订单已取消或关闭 / 3 金额与应付不符。
const (
	PaymentReturnDuplicate      int16 = 1
	PaymentReturnOrderClosed    int16 = 2
	PaymentReturnAmountMismatch int16 = 3
)

// PaymentReturnStatus：10 待提交 / 30 退回中 / 40 已退回。
const (
	PaymentReturnPending   int16 = 10
	PaymentReturnSubmitted int16 = 30
	PaymentReturnReturned  int16 = 40
)

// ErrPaymentReturnNotFound：本店没有这张退回单。
var ErrPaymentReturnNotFound = errors.New("多收款退回单不存在")

// PaymentIntent 是一次发起支付留下的意图。
type PaymentIntent struct {
	ID           int64
	OrderID      int64
	Channel      int16
	ChannelTxnID string
	AmountCents  int64
	Status       int16
	CreatedAt    time.Time
}

// PaymentReturn 是一张多收款退回单（带订单号与原支付的渠道流水号，给后台与买家看）。
type PaymentReturn struct {
	ID              int64
	ReturnNo        string
	PaymentID       int64
	OrderID         int64
	OrderNo         string
	PaymentTxnID    *string
	Channel         int16
	AmountCents     int64
	Reason          int16
	Status          int16
	ChannelRefundID *string
	Attempts        int32
	LastError       *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ReturnedAt      *time.Time
}

// UnacceptedPayment 是兜底扫描找到的一笔「渠道成功、订单不认、还没开退回单」的到账。
type UnacceptedPayment struct {
	PaymentID    int64
	OrderStatus  int16
	OrderPaid    bool
	AmountCents  int64
	PayableCents int64
}

// PaymentReturnTx 是多收款那一面。
type PaymentReturnTx interface {
	FindActivePaymentIntent(ctx context.Context, orderID int64) (PaymentIntent, bool, error)
	InsertPaymentIntent(ctx context.Context, orderID int64, channel int16, txnID string, amountCents int64) error
	SupersedePaymentIntent(ctx context.Context, id int64) error
	MarkPaymentIntentSettled(ctx context.Context, channel int16, txnID string) error

	// InsertPaymentReturn 给一笔到账开退回单；已有（同一笔支付）时返回 false。
	InsertPaymentReturn(ctx context.Context, returnNo string, paymentID int64, reason int16) (bool, error)
	ListUnacceptedPayments(ctx context.Context, limit int32) ([]UnacceptedPayment, error)
	ListPaymentReturnsToSubmit(ctx context.Context, limit int32) ([]string, error)
	LockPaymentReturnByNo(ctx context.Context, returnNo string) (PaymentReturn, error)
	MarkPaymentReturnSubmitted(ctx context.Context, id int64) error
	MarkPaymentReturnAttemptFailed(ctx context.Context, id int64, lastError string) error
	// SettlePaymentReturn 记退回成功（10 / 30 → 40）；已是 40 时返回 false。
	SettlePaymentReturn(ctx context.Context, id int64, channelRefundID string, payload []byte) (bool, error)
	ListPaymentReturns(ctx context.Context, status *int16, limit, offset int32) ([]PaymentReturn, int64, error)
	ListPaymentReturnsForOrder(ctx context.Context, orderID int64) ([]PaymentReturn, error)
}

func (t tenantTx) FindActivePaymentIntent(ctx context.Context, orderID int64) (PaymentIntent, bool, error) {
	r, err := t.q.FindActivePaymentIntent(ctx, orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentIntent{}, false, nil
	}
	if err != nil {
		return PaymentIntent{}, false, err
	}
	return PaymentIntent{ID: r.ID, OrderID: r.OrderID, Channel: r.Channel, ChannelTxnID: r.ChannelTxnID,
		AmountCents: r.AmountCents, Status: r.Status, CreatedAt: r.CreatedAt.Time}, true, nil
}

func (t tenantTx) InsertPaymentIntent(ctx context.Context, orderID int64, channel int16, txnID string, amountCents int64) error {
	return t.q.InsertPaymentIntent(ctx, db.InsertPaymentIntentParams{OrderID: orderID, Channel: channel,
		ChannelTxnID: txnID, AmountCents: amountCents})
}

func (t tenantTx) SupersedePaymentIntent(ctx context.Context, id int64) error {
	return t.q.SupersedePaymentIntent(ctx, id)
}

func (t tenantTx) MarkPaymentIntentSettled(ctx context.Context, channel int16, txnID string) error {
	return t.q.MarkPaymentIntentSettled(ctx, db.MarkPaymentIntentSettledParams{Channel: channel, ChannelTxnID: txnID})
}

func (t tenantTx) InsertPaymentReturn(ctx context.Context, returnNo string, paymentID int64, reason int16) (bool, error) {
	n, err := t.q.InsertPaymentReturn(ctx, db.InsertPaymentReturnParams{ReturnNo: returnNo, Reason: reason, PaymentID: paymentID})
	return n == 1, err
}

func (t tenantTx) ListUnacceptedPayments(ctx context.Context, limit int32) ([]UnacceptedPayment, error) {
	rows, err := t.q.ListUnacceptedPayments(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]UnacceptedPayment, 0, len(rows))
	for _, r := range rows {
		out = append(out, UnacceptedPayment(r))
	}
	return out, nil
}

func (t tenantTx) ListPaymentReturnsToSubmit(ctx context.Context, limit int32) ([]string, error) {
	rows, err := t.q.ListPaymentReturnsToSubmit(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ReturnNo)
	}
	return out, nil
}

func (t tenantTx) LockPaymentReturnByNo(ctx context.Context, returnNo string) (PaymentReturn, error) {
	r, err := t.q.LockPaymentReturnByNo(ctx, returnNo)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentReturn{}, fmt.Errorf("%s: %w", returnNo, ErrPaymentReturnNotFound)
	}
	if err != nil {
		return PaymentReturn{}, err
	}
	return paymentReturnOf(db.ListPaymentReturnsRow(r)), nil
}

func (t tenantTx) MarkPaymentReturnSubmitted(ctx context.Context, id int64) error {
	return t.q.MarkPaymentReturnSubmitted(ctx, id)
}

func (t tenantTx) MarkPaymentReturnAttemptFailed(ctx context.Context, id int64, lastError string) error {
	return t.q.MarkPaymentReturnAttemptFailed(ctx, db.MarkPaymentReturnAttemptFailedParams{ID: id, LastError: &lastError})
}

func (t tenantTx) SettlePaymentReturn(ctx context.Context, id int64, channelRefundID string, payload []byte) (bool, error) {
	n, err := t.q.SettlePaymentReturn(ctx, db.SettlePaymentReturnParams{ID: id, ChannelRefundID: &channelRefundID,
		NotifyPayload: payload})
	if isUniqueViolation(err, "uk_payment_returns_channel_refund") {
		return false, fmt.Errorf("channel_refund_id %s: %w", channelRefundID, ErrDuplicateChannelRefund)
	}
	return n == 1, err
}

func (t tenantTx) ListPaymentReturns(ctx context.Context, status *int16, limit, offset int32) ([]PaymentReturn, int64, error) {
	rows, err := t.q.ListPaymentReturns(ctx, db.ListPaymentReturnsParams{Status: status, PageLimit: limit, PageOffset: offset})
	if err != nil {
		return nil, 0, err
	}
	total, err := t.q.CountPaymentReturns(ctx, status)
	if err != nil {
		return nil, 0, err
	}
	out := make([]PaymentReturn, 0, len(rows))
	for _, r := range rows {
		out = append(out, paymentReturnOf(r))
	}
	return out, total, nil
}

func (t tenantTx) ListPaymentReturnsForOrder(ctx context.Context, orderID int64) ([]PaymentReturn, error) {
	rows, err := t.q.ListPaymentReturnsForOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	out := make([]PaymentReturn, 0, len(rows))
	for _, r := range rows {
		out = append(out, paymentReturnOf(db.ListPaymentReturnsRow(r)))
	}
	return out, nil
}

func paymentReturnOf(r db.ListPaymentReturnsRow) PaymentReturn {
	return PaymentReturn{ID: r.ID, ReturnNo: r.ReturnNo, PaymentID: r.PaymentID, OrderID: r.OrderID, OrderNo: r.OrderNo,
		PaymentTxnID: r.PaymentTxnID, Channel: r.Channel, AmountCents: r.AmountCents, Reason: r.Reason, Status: r.Status,
		ChannelRefundID: r.ChannelRefundID, Attempts: r.Attempts, LastError: r.LastError, CreatedAt: r.CreatedAt.Time,
		UpdatedAt: r.UpdatedAt.Time, ReturnedAt: optTime(r.ReturnedAt)}
}
