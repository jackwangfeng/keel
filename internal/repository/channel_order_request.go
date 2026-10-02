package repository

// 平台发起的申请（channel_order_requests，00320；第三期 Task 6）与截止扫描要的两条读。
// 编排在 service/channel_order_request.go。

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 申请的状态（00320 的 status 列）。
const (
	ChannelRequestPending   int16 = 1 // 待处理
	ChannelRequestAgreed    int16 = 2 // 已同意（员工或自动策略）
	ChannelRequestRejected  int16 = 3 // 已拒绝
	ChannelRequestTimedOut  int16 = 4 // 超时自动同意（平台按自己的规则处理，keel 等平台事实）
	ChannelRequestWithdrawn int16 = 5 // 平台已撤销
)

// ChannelOrderRequest 是 channel_order_requests 的一行。
type ChannelOrderRequest struct {
	ID, ChannelOrderID int64
	ExternalRequestID  string
	Kind               int16
	Lines              json.RawMessage
	AmountCents        int64
	Reason             string
	Status             int16
	Deadline           *time.Time
	DecidedBy          *int64
	DecidedAt          *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// NewChannelOrderRequest 是第一次见到的申请。
type NewChannelOrderRequest struct {
	ChannelOrderID    int64
	ExternalRequestID string
	Kind              int16
	Lines             json.RawMessage
	AmountCents       int64
	Reason            string
	Status            int16
	Deadline          *time.Time
}

// ExpiredChannelRequest 是过了截止还待处理的申请（带上发通知要的渠道单信息）。
type ExpiredChannelRequest struct {
	ID, ChannelOrderID int64
	Kind               int16
	AmountCents        int64
	StoreID            *int64
	OrderNo            *string
	ExternalOrderName  string
}

// AcceptReminder 是该提醒接单的渠道单。
type AcceptReminder struct {
	ChannelOrderID    int64
	StoreID           int64
	ExternalOrderName string
	AcceptDeadline    time.Time
}

// ChannelOrderRequestTx 是申请流的语句（并进 ChannelTx）。
type ChannelOrderRequestTx interface {
	LockChannelOrderByExternal(ctx context.Context, bindingID int64, externalOrderID string) (ChannelOrder, error)
	// InsertOrLockChannelOrderRequest：第一次见到就建（inserted = true），否则锁住已有的那一行。
	InsertOrLockChannelOrderRequest(ctx context.Context, r NewChannelOrderRequest) (ChannelOrderRequest, bool, error)
	LockChannelOrderRequest(ctx context.Context, id int64) (ChannelOrderRequest, error)
	ListChannelOrderRequests(ctx context.Context, channelOrderID int64) ([]ChannelOrderRequest, error)
	// DecideChannelOrderRequest 处置一个待处理的申请；返回 false = 它已经不是待处理了。
	DecideChannelOrderRequest(ctx context.Context, id int64, status int16, decidedBy *int64) (bool, error)
	ExpiredChannelOrderRequests(ctx context.Context, limit int32) ([]ExpiredChannelRequest, error)
	DueAcceptReminders(ctx context.Context, limit int32) ([]AcceptReminder, error)
}

func channelRequestFrom(r db.ChannelOrderRequest) ChannelOrderRequest {
	return ChannelOrderRequest{ID: r.ID, ChannelOrderID: r.ChannelOrderID, ExternalRequestID: r.ExternalRequestID,
		Kind: r.Kind, Lines: r.Lines, AmountCents: r.AmountCents, Reason: r.Reason, Status: r.Status,
		Deadline: optTime(r.Deadline), DecidedBy: r.DecidedBy, DecidedAt: optTime(r.DecidedAt),
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time}
}

// channelRequestRow 把各条查询的行（列相同）转成 db.ChannelOrderRequest（没有 merchant_id）。
func channelRequestRow(id, coID int64, ext string, kind int16, lines []byte, amount int64, reason string, status int16,
	deadline, decidedAt, createdAt, updatedAt pgtype.Timestamptz, decidedBy *int64) ChannelOrderRequest {
	return channelRequestFrom(db.ChannelOrderRequest{ID: id, ChannelOrderID: coID, ExternalRequestID: ext, Kind: kind,
		Lines: lines, AmountCents: amount, Reason: reason, Status: status, Deadline: deadline, DecidedBy: decidedBy,
		DecidedAt: decidedAt, CreatedAt: createdAt, UpdatedAt: updatedAt})
}

func (t tenantTx) LockChannelOrderByExternal(ctx context.Context, bindingID int64, externalOrderID string) (ChannelOrder, error) {
	r, err := t.q.LockChannelOrderByExternal(ctx, db.LockChannelOrderByExternalParams{BindingID: bindingID,
		ExternalOrderID: externalOrderID})
	if err != nil {
		return ChannelOrder{}, notFound(err)
	}
	return channelOrderFrom(db.GetChannelOrderRow(r)), nil
}

func (t tenantTx) InsertOrLockChannelOrderRequest(ctx context.Context, n NewChannelOrderRequest) (ChannelOrderRequest, bool, error) {
	r, err := t.q.InsertChannelOrderRequest(ctx, db.InsertChannelOrderRequestParams{ChannelOrderID: n.ChannelOrderID,
		ExternalRequestID: n.ExternalRequestID, Kind: n.Kind, Lines: jsonArrayOrEmpty(n.Lines), AmountCents: n.AmountCents,
		Reason: n.Reason, Status: n.Status, Deadline: optTS(n.Deadline)})
	if err == nil {
		return channelRequestRow(r.ID, r.ChannelOrderID, r.ExternalRequestID, r.Kind, r.Lines, r.AmountCents, r.Reason,
			r.Status, r.Deadline, r.DecidedAt, r.CreatedAt, r.UpdatedAt, r.DecidedBy), true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ChannelOrderRequest{}, false, channelWriteErr(err)
	}
	l, err := t.q.LockChannelOrderRequestByExternal(ctx, db.LockChannelOrderRequestByExternalParams{
		ChannelOrderID: n.ChannelOrderID, ExternalRequestID: n.ExternalRequestID})
	if err != nil {
		return ChannelOrderRequest{}, false, notFound(err)
	}
	return channelRequestRow(l.ID, l.ChannelOrderID, l.ExternalRequestID, l.Kind, l.Lines, l.AmountCents, l.Reason,
		l.Status, l.Deadline, l.DecidedAt, l.CreatedAt, l.UpdatedAt, l.DecidedBy), false, nil
}

func (t tenantTx) LockChannelOrderRequest(ctx context.Context, id int64) (ChannelOrderRequest, error) {
	l, err := t.q.LockChannelOrderRequest(ctx, id)
	if err != nil {
		return ChannelOrderRequest{}, notFound(err)
	}
	return channelRequestRow(l.ID, l.ChannelOrderID, l.ExternalRequestID, l.Kind, l.Lines, l.AmountCents, l.Reason,
		l.Status, l.Deadline, l.DecidedAt, l.CreatedAt, l.UpdatedAt, l.DecidedBy), nil
}

func (t tenantTx) ListChannelOrderRequests(ctx context.Context, channelOrderID int64) ([]ChannelOrderRequest, error) {
	rows, err := t.q.ListChannelOrderRequests(ctx, channelOrderID)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelOrderRequest, 0, len(rows))
	for _, l := range rows {
		out = append(out, channelRequestRow(l.ID, l.ChannelOrderID, l.ExternalRequestID, l.Kind, l.Lines, l.AmountCents,
			l.Reason, l.Status, l.Deadline, l.DecidedAt, l.CreatedAt, l.UpdatedAt, l.DecidedBy))
	}
	return out, nil
}

func (t tenantTx) DecideChannelOrderRequest(ctx context.Context, id int64, status int16, decidedBy *int64) (bool, error) {
	n, err := t.q.DecideChannelOrderRequest(ctx, db.DecideChannelOrderRequestParams{Status: status, DecidedBy: decidedBy, ID: id})
	if err != nil {
		return false, channelWriteErr(err)
	}
	return n == 1, nil
}

func (t tenantTx) ExpiredChannelOrderRequests(ctx context.Context, limit int32) ([]ExpiredChannelRequest, error) {
	rows, err := t.q.ExpiredChannelOrderRequests(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ExpiredChannelRequest, 0, len(rows))
	for _, r := range rows {
		out = append(out, ExpiredChannelRequest{ID: r.ID, ChannelOrderID: r.ChannelOrderID, Kind: r.Kind,
			AmountCents: r.AmountCents, StoreID: r.StoreID, OrderNo: r.OrderNo, ExternalOrderName: r.ExternalOrderName})
	}
	return out, nil
}

func (t tenantTx) DueAcceptReminders(ctx context.Context, limit int32) ([]AcceptReminder, error) {
	rows, err := t.q.DueAcceptReminders(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]AcceptReminder, 0, len(rows))
	for _, r := range rows {
		out = append(out, AcceptReminder{ChannelOrderID: r.ID, StoreID: r.StoreID, ExternalOrderName: r.ExternalOrderName,
			AcceptDeadline: r.AcceptDeadline.Time})
	}
	return out, nil
}
