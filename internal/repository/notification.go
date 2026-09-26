package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 消息通知（数据模型 §16，迁移 00053）在 repository 边界上的那一面。
//
// 写入只有一个入口 InsertNotification，而它只会在一个租户事务里被调用 ——
// 做状态变化的那一个（outbox）。merchant_id 由列默认值 current_merchant() 填，
// NewNotification 里没有那个字段，「往别家店名下写一条通知」编译不出来。

const (
	// NotificationAudienceBuyer / NotificationAudienceMerchant 是 notifications.audience。
	NotificationAudienceBuyer    int16 = 1
	NotificationAudienceMerchant int16 = 2

	// NotificationDeliverySent / Skipped / Failed 是 notification_deliveries.status。
	NotificationDeliverySent    int16 = 1
	NotificationDeliverySkipped int16 = 2
	NotificationDeliveryFailed  int16 = 3

	// QueueNotificationDelivery 是外发渠道投递的队列名（jobs.queue）。
	QueueNotificationDelivery = "notification.deliver"
)

// NewNotification 是写一条通知要给的列。**没有 MerchantID**，理由见文件头。
//
// UserID 与 StoreID 恰好给一个（chk_notification_audience）：发给买家给 UserID，
// 发给商家给 StoreID（后台按员工的门店范围过滤）。
type NewNotification struct {
	Audience   int16
	UserID     *int64
	StoreID    *int64
	Kind       string
	Title      string
	Body       string
	TargetType string
	OrderNo    *string
	RefundNo   *string
	SKUID      *int64
	DedupeKey  string
}

// Notification 是消息中心 / 铃铛里的一条。ReadAt 对买家是这一行的 read_at，
// 对员工是 notification_reads 里**他自己**的那一格。
type Notification struct {
	ID         int64
	Kind       string
	Title      string
	Body       string
	TargetType string
	OrderNo    *string
	RefundNo   *string
	StoreID    *int64
	SKUID      *int64
	ReadAt     *time.Time
	CreatedAt  time.Time
}

// NotificationForDelivery 是外发 worker 读回的那一条（多了收件人两列与 audience）。
type NotificationForDelivery struct {
	Notification
	Audience int16
	UserID   *int64
}

// AutoConfirmReminder 是「自动确认收货即将到期」的一个候选。
type AutoConfirmReminder struct {
	ID        int64
	OrderNo   string
	UserID    int64
	ShippedAt time.Time
}

// InventoryAlert 是库存预警要的上下文。
type InventoryAlert struct {
	WarningQty   int32
	ProductTitle string
	SpecValues   string
	StoreName    string
}

// NotificationTx 是通知这一面。
type NotificationTx interface {
	// InsertNotification 写一条通知。inserted 为 false 表示 dedupe_key 撞上了 ——
	// 同一件事已经通知过，**正常路径**，调用方不再入外发任务。
	InsertNotification(ctx context.Context, n NewNotification) (id int64, inserted bool, err error)

	ListUserNotifications(ctx context.Context, userID int64, unreadOnly bool, limit, offset int64) ([]Notification, error)
	CountUserNotifications(ctx context.Context, userID int64, unreadOnly bool) (int64, error)
	CountUserUnreadNotifications(ctx context.Context, userID int64) (int64, error)
	// MarkUserNotificationRead 返回 false 表示这条通知不存在或不是这个买家的。
	MarkUserNotificationRead(ctx context.Context, userID, id int64) (bool, error)
	MarkAllUserNotificationsRead(ctx context.Context, userID int64) (int64, error)

	ListStaffNotifications(ctx context.Context, staffID int64, only ScopeFilter, unreadOnly bool,
		limit, offset int64) ([]Notification, error)
	CountStaffNotifications(ctx context.Context, staffID int64, only ScopeFilter, unreadOnly bool) (int64, error)
	// MarkStaffNotificationRead 返回 false 表示这条通知不存在或不在这个员工的范围里。
	MarkStaffNotificationRead(ctx context.Context, staffID int64, only ScopeFilter, id int64) (bool, error)
	MarkAllStaffNotificationsRead(ctx context.Context, staffID int64, only ScopeFilter) (int64, error)

	// GetNotificationForDelivery 取外发要投递的那一条；不存在（保留期删掉了）返回 ErrNotificationNotFound。
	GetNotificationForDelivery(ctx context.Context, id int64) (NotificationForDelivery, error)
	// FinishedDeliveryChannels 这条通知已经有定论（发出 / 跳过）的渠道。
	FinishedDeliveryChannels(ctx context.Context, id int64) (map[string]bool, error)
	InsertNotificationDelivery(ctx context.Context, notificationID int64, channel string,
		status int16, attempt int32, detail string) error

	// PurgeExpiredNotifications 删 cutoff 之前的通知，一次至多 batch 条。
	PurgeExpiredNotifications(ctx context.Context, cutoff time.Time, batch int32) (int64, error)

	// ListAutoConfirmReminders 「自动确认收货即将到期」的候选（还没提醒过的）。
	ListAutoConfirmReminders(ctx context.Context, remindBefore time.Time, batch int32) ([]AutoConfirmReminder, error)
	// InventoryAlert 库存预警的上下文；这一行库存不存在返回 ErrNotificationNotFound。
	InventoryAlert(ctx context.Context, skuID, storeID int64) (InventoryAlert, error)
}

// ErrNotificationNotFound：要读的那一行不存在（或不在本租户）。
var ErrNotificationNotFound = errors.New("通知不存在")

func (t tenantTx) InsertNotification(ctx context.Context, n NewNotification) (int64, bool, error) {
	id, err := t.q.InsertNotification(ctx, db.InsertNotificationParams{
		Audience: n.Audience, UserID: n.UserID, StoreID: n.StoreID, Kind: n.Kind,
		Title: n.Title, Body: n.Body, TargetType: n.TargetType, OrderNo: n.OrderNo,
		RefundNo: n.RefundNo, SkuID: n.SKUID, DedupeKey: n.DedupeKey,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func notificationFromRow(id int64, kind, title, body, targetType string, orderNo, refundNo *string,
	storeID, skuID *int64, readAt, createdAt pgtype.Timestamptz) Notification {
	return Notification{
		ID: id, Kind: kind, Title: title, Body: body, TargetType: targetType,
		OrderNo: orderNo, RefundNo: refundNo, StoreID: storeID, SKUID: skuID,
		ReadAt: optTime(readAt), CreatedAt: createdAt.Time,
	}
}

func (t tenantTx) ListUserNotifications(ctx context.Context, userID int64, unreadOnly bool,
	limit, offset int64) ([]Notification, error) {
	if err := checkPaging(limit, offset); err != nil {
		return nil, err
	}
	rows, err := t.q.ListUserNotifications(ctx, db.ListUserNotificationsParams{
		UserID: &userID, UnreadOnly: unreadOnly, PageLimit: int32(limit), PageOffset: int32(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Notification, 0, len(rows))
	for _, r := range rows {
		out = append(out, notificationFromRow(r.ID, r.Kind, r.Title, r.Body, r.TargetType,
			r.OrderNo, r.RefundNo, r.StoreID, r.SkuID, r.ReadAt, r.CreatedAt))
	}
	return out, nil
}

func (t tenantTx) CountUserNotifications(ctx context.Context, userID int64, unreadOnly bool) (int64, error) {
	return t.q.CountUserNotifications(ctx, db.CountUserNotificationsParams{UserID: &userID, UnreadOnly: unreadOnly})
}

func (t tenantTx) CountUserUnreadNotifications(ctx context.Context, userID int64) (int64, error) {
	return t.q.CountUserUnreadNotifications(ctx, &userID)
}

func (t tenantTx) MarkUserNotificationRead(ctx context.Context, userID, id int64) (bool, error) {
	n, err := t.q.MarkUserNotificationRead(ctx, db.MarkUserNotificationReadParams{ID: id, UserID: &userID})
	return n == 1, err
}

func (t tenantTx) MarkAllUserNotificationsRead(ctx context.Context, userID int64) (int64, error) {
	return t.q.MarkAllUserNotificationsRead(ctx, &userID)
}

func (t tenantTx) ListStaffNotifications(ctx context.Context, staffID int64, only ScopeFilter,
	unreadOnly bool, limit, offset int64) ([]Notification, error) {
	if err := checkPaging(limit, offset); err != nil {
		return nil, err
	}
	rows, err := t.q.ListStaffNotifications(ctx, db.ListStaffNotificationsParams{
		StaffID: staffID, UnreadOnly: unreadOnly,
		OnlyRegionIds: only.RegionIDs, OnlyStoreIds: only.StoreIDs,
		PageLimit: int32(limit), PageOffset: int32(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Notification, 0, len(rows))
	for _, r := range rows {
		out = append(out, notificationFromRow(r.ID, r.Kind, r.Title, r.Body, r.TargetType,
			r.OrderNo, r.RefundNo, r.StoreID, r.SkuID, r.ReadAt, r.CreatedAt))
	}
	return out, nil
}

func (t tenantTx) CountStaffNotifications(ctx context.Context, staffID int64, only ScopeFilter,
	unreadOnly bool) (int64, error) {
	return t.q.CountStaffNotifications(ctx, db.CountStaffNotificationsParams{
		StaffID: staffID, UnreadOnly: unreadOnly,
		OnlyRegionIds: only.RegionIDs, OnlyStoreIds: only.StoreIDs,
	})
}

func (t tenantTx) MarkStaffNotificationRead(ctx context.Context, staffID int64, only ScopeFilter,
	id int64) (bool, error) {
	n, err := t.q.MarkStaffNotificationRead(ctx, db.MarkStaffNotificationReadParams{
		ID: id, StaffID: staffID, OnlyRegionIds: only.RegionIDs, OnlyStoreIds: only.StoreIDs,
	})
	return n == 1, err
}

func (t tenantTx) MarkAllStaffNotificationsRead(ctx context.Context, staffID int64,
	only ScopeFilter) (int64, error) {
	return t.q.MarkAllStaffNotificationsRead(ctx, db.MarkAllStaffNotificationsReadParams{
		StaffID: staffID, OnlyRegionIds: only.RegionIDs, OnlyStoreIds: only.StoreIDs,
	})
}

func (t tenantTx) GetNotificationForDelivery(ctx context.Context, id int64) (NotificationForDelivery, error) {
	r, err := t.q.GetNotificationForDelivery(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return NotificationForDelivery{}, ErrNotificationNotFound
	}
	if err != nil {
		return NotificationForDelivery{}, err
	}
	return NotificationForDelivery{
		Notification: notificationFromRow(r.ID, r.Kind, r.Title, r.Body, r.TargetType,
			r.OrderNo, r.RefundNo, r.StoreID, r.SkuID, pgtype.Timestamptz{}, r.CreatedAt),
		Audience: r.Audience,
		UserID:   r.UserID,
	}, nil
}

func (t tenantTx) FinishedDeliveryChannels(ctx context.Context, id int64) (map[string]bool, error) {
	chs, err := t.q.ListFinishedDeliveryChannels(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(chs))
	for _, c := range chs {
		out[c] = true
	}
	return out, nil
}

func (t tenantTx) InsertNotificationDelivery(ctx context.Context, notificationID int64, channel string,
	status int16, attempt int32, detail string) error {
	var d *string
	if detail != "" {
		d = &detail
	}
	return t.q.InsertNotificationDelivery(ctx, db.InsertNotificationDeliveryParams{
		NotificationID: notificationID, Channel: channel, Status: status, Attempt: attempt, Detail: d,
	})
}

func (t tenantTx) PurgeExpiredNotifications(ctx context.Context, cutoff time.Time, batch int32) (int64, error) {
	return t.q.PurgeExpiredNotifications(ctx, db.PurgeExpiredNotificationsParams{
		Cutoff: pgtype.Timestamptz{Time: cutoff, Valid: true}, Batch: batch,
	})
}

func (t tenantTx) ListAutoConfirmReminders(ctx context.Context, remindBefore time.Time,
	batch int32) ([]AutoConfirmReminder, error) {
	rows, err := t.q.ListAutoConfirmReminders(ctx, db.ListAutoConfirmRemindersParams{
		RemindBefore: pgtype.Timestamptz{Time: remindBefore, Valid: true}, Batch: batch,
	})
	if err != nil {
		return nil, err
	}
	out := make([]AutoConfirmReminder, 0, len(rows))
	for _, r := range rows {
		out = append(out, AutoConfirmReminder{ID: r.ID, OrderNo: r.OrderNo, UserID: r.UserID,
			ShippedAt: r.ShippedAt.Time})
	}
	return out, nil
}

func (t tenantTx) InventoryAlert(ctx context.Context, skuID, storeID int64) (InventoryAlert, error) {
	r, err := t.q.GetInventoryAlert(ctx, db.GetInventoryAlertParams{SkuID: skuID, StoreID: storeID})
	if errors.Is(err, pgx.ErrNoRows) {
		return InventoryAlert{}, ErrNotificationNotFound
	}
	if err != nil {
		return InventoryAlert{}, err
	}
	return InventoryAlert{WarningQty: r.WarningQty, ProductTitle: r.ProductTitle,
		SpecValues: r.SpecValues, StoreName: r.StoreName}, nil
}
