package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// 消息通知的**读**那一半（数据模型 §16）：买家消息中心（/me/notifications）与
// 后台待办提醒（/admin/notifications）。写在 notification.go，外发在 notification_delivery.go。
//
// # 两边的越权过滤各是什么
//
//   - 买家：租户由 RLS 管；同一家店里只看得到 user_id = 令牌里那个人的（repository 那条 SQL
//     里的 user_id 谓词）。不是自己的通知，标已读回 404，与「不存在」不作区分。
//   - 员工：每条商家通知属于一家门店，范围与订单 / 售后列表同一个判据 ——
//     orderListScope（authz.go）。铃铛里看得见的，就是订单列表里看得见的那些门店的事；
//     看不见的，标已读同样 404。
//
// 已读状态：买家的在通知行上（收件人只有一个）；员工的在 notification_reads 里，
// 每个员工各一份 —— 店长点开了「新订单待发货」，不等于客服也看过了。

// ErrNotificationNotFound：这条通知不存在，或不是调用者的（买家）/ 不在调用者范围里（员工）。
// 契约：404。
var ErrNotificationNotFound = errors.New("通知不存在")

// NotificationPage 是消息中心 / 铃铛的一页。Page / PageSize 是钳制之后的值。
type NotificationPage struct {
	Items    []repository.Notification
	Page     int
	PageSize int
	Total    int64
	Unread   int64
}

// NotificationService 实现两边的读与标已读。
type NotificationService struct {
	repo tenantRunner
}

// NewNotificationService 建通知的读服务。
func NewNotificationService(r tenantRunner) *NotificationService {
	return &NotificationService{repo: r}
}

// ---------------------------------------------------------------------------
// 买家
// ---------------------------------------------------------------------------

// ListMine 实现 GET /me/notifications。
func (s *NotificationService) ListMine(ctx context.Context, page, pageSize int, unreadOnly bool) (NotificationPage, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return NotificationPage{}, err
	}
	page, pageSize = clampPaging(page, pageSize)
	out := NotificationPage{Items: []repository.Notification{}, Page: page, PageSize: pageSize}
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		if out.Total, err = tx.CountUserNotifications(ctx, id.UserID, unreadOnly); err != nil {
			return err
		}
		if out.Unread, err = tx.CountUserUnreadNotifications(ctx, id.UserID); err != nil {
			return err
		}
		out.Items, err = tx.ListUserNotifications(ctx, id.UserID, unreadOnly, int64(pageSize), offsetOf(page, pageSize))
		return err
	})
	return out, err
}

// UnreadMine 实现 GET /me/notifications/unread-count。
func (s *NotificationService) UnreadMine(ctx context.Context) (int64, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		n, err = tx.CountUserUnreadNotifications(ctx, id.UserID)
		return err
	})
	return n, err
}

// MarkMineRead 实现 POST /me/notifications/{notification_id}/read，返回标完之后的未读数。
func (s *NotificationService) MarkMineRead(ctx context.Context, notificationID int64) (int64, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		ok, err := tx.MarkUserNotificationRead(ctx, id.UserID, notificationID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: id=%d", ErrNotificationNotFound, notificationID)
		}
		n, err = tx.CountUserUnreadNotifications(ctx, id.UserID)
		return err
	})
	return n, err
}

// MarkAllMineRead 实现 POST /me/notifications/read-all，返回标完之后的未读数。
func (s *NotificationService) MarkAllMineRead(ctx context.Context) (int64, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err := tx.MarkAllUserNotificationsRead(ctx, id.UserID); err != nil {
			return err
		}
		n, err = tx.CountUserUnreadNotifications(ctx, id.UserID)
		return err
	})
	return n, err
}

// ---------------------------------------------------------------------------
// 后台
// ---------------------------------------------------------------------------

// ListForStaff 实现 GET /admin/notifications。范围由 orderListScope 收窄（任何员工都能调，
// 看到的是自己范围里的）。
func (s *NotificationService) ListForStaff(ctx context.Context, page, pageSize int, unreadOnly bool) (NotificationPage, error) {
	staff, err := requireStaff(ctx)
	if err != nil {
		return NotificationPage{}, err
	}
	page, pageSize = clampPaging(page, pageSize)
	out := NotificationPage{Items: []repository.Notification{}, Page: page, PageSize: pageSize}
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		only, err := orderListScope(ctx)
		if err != nil {
			return err
		}
		if out.Total, err = tx.CountStaffNotifications(ctx, staff.StaffID, only, unreadOnly); err != nil {
			return err
		}
		if out.Unread, err = tx.CountStaffNotifications(ctx, staff.StaffID, only, true); err != nil {
			return err
		}
		out.Items, err = tx.ListStaffNotifications(ctx, staff.StaffID, only, unreadOnly,
			int64(pageSize), offsetOf(page, pageSize))
		return err
	})
	return out, err
}

// UnreadForStaff 实现 GET /admin/notifications/unread-count。
func (s *NotificationService) UnreadForStaff(ctx context.Context) (int64, error) {
	staff, err := requireStaff(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		only, err := orderListScope(ctx)
		if err != nil {
			return err
		}
		n, err = tx.CountStaffNotifications(ctx, staff.StaffID, only, true)
		return err
	})
	return n, err
}

// MarkReadForStaff 实现 POST /admin/notifications/{notification_id}/read。
func (s *NotificationService) MarkReadForStaff(ctx context.Context, notificationID int64) (int64, error) {
	staff, err := requireStaff(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		only, err := orderListScope(ctx)
		if err != nil {
			return err
		}
		ok, err := tx.MarkStaffNotificationRead(ctx, staff.StaffID, only, notificationID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: id=%d", ErrNotificationNotFound, notificationID)
		}
		n, err = tx.CountStaffNotifications(ctx, staff.StaffID, only, true)
		return err
	})
	return n, err
}

// MarkAllReadForStaff 实现 POST /admin/notifications/read-all。
func (s *NotificationService) MarkAllReadForStaff(ctx context.Context) (int64, error) {
	staff, err := requireStaff(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		only, err := orderListScope(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.MarkAllStaffNotificationsRead(ctx, staff.StaffID, only); err != nil {
			return err
		}
		n, err = tx.CountStaffNotifications(ctx, staff.StaffID, only, true)
		return err
	})
	return n, err
}
