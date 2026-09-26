package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 消息通知：买家消息中心（/me/notifications）与后台待办提醒（/admin/notifications）。
//
// 两份列表各自一个文件（notification_list.go / admin_notification_list.go）：
// contract_test.go 的 query 参数对账按文件解析，这个文件里的六条接口一个 query 参数都没有。
// handler 只做参数、鉴权后的调用与响应封装；判权（收件人 / 门店范围）全在 service。

// NotificationHandler 两边共用一个：它们读的是同一张表、同一个形状，差别只在 service 里的过滤。
type NotificationHandler struct {
	svc *service.NotificationService
}

// NewNotificationHandler 建通知的 handler。
func NewNotificationHandler(svc *service.NotificationService) *NotificationHandler {
	return &NotificationHandler{svc: svc}
}

type notificationListResponse struct {
	api.PageMeta
	Items       []api.Notification `json:"items"`
	UnreadCount int                `json:"unread_count"`
}

func apiNotification(n repository.Notification) api.Notification {
	return api.Notification{
		Id:    n.ID,
		Kind:  api.NotificationKind(n.Kind),
		Title: n.Title,
		Body:  n.Body,
		Target: api.NotificationTarget{
			Type:     api.NotificationTargetType(n.TargetType),
			OrderNo:  n.OrderNo,
			RefundNo: n.RefundNo,
			StoreId:  n.StoreID,
			SkuId:    n.SKUID,
		},
		ReadAt:    n.ReadAt,
		CreatedAt: n.CreatedAt,
	}
}

func notificationListOf(p service.NotificationPage) notificationListResponse {
	items := make([]api.Notification, 0, len(p.Items))
	for _, n := range p.Items {
		items = append(items, apiNotification(n))
	}
	return notificationListResponse{
		PageMeta:    api.PageMeta{Page: p.Page, PageSize: p.PageSize, Total: int(p.Total)},
		Items:       items,
		UnreadCount: int(p.Unread),
	}
}

func writeUnread(c *gin.Context, n int64, err error) {
	if err != nil {
		writeNotificationError(c, err)
		return
	}
	c.JSON(http.StatusOK, api.NotificationUnreadCount{UnreadCount: int(n)})
}

// notificationPathID 取路径里的 notification_id。非正整数回 404：契约里这条的错误集合是
// 404 + default，`/notifications/abc` 也确实不可能是任何一条通知。
func notificationPathID(c *gin.Context) (int64, bool) {
	id, ok := parsePositiveID(c.Param("notification_id"))
	if !ok {
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "通知不存在")
		return 0, false
	}
	return id, true
}

func writeNotificationError(c *gin.Context, err error) {
	if writePermissionError(c, err) {
		return
	}
	if errors.Is(err, service.ErrNotificationNotFound) {
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "通知不存在")
		return
	}
	_ = c.Error(err)
	problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "服务内部错误")
}

// ---------------------------------------------------------------------------
// 买家
// ---------------------------------------------------------------------------

// UnreadMine 实现 GET /api/v1/me/notifications/unread-count。
func (h *NotificationHandler) UnreadMine(c *gin.Context) {
	n, err := h.svc.UnreadMine(c.Request.Context())
	writeUnread(c, n, err)
}

// MarkMineRead 实现 POST /api/v1/me/notifications/{notification_id}/read。
func (h *NotificationHandler) MarkMineRead(c *gin.Context) {
	id, ok := notificationPathID(c)
	if !ok {
		return
	}
	n, err := h.svc.MarkMineRead(c.Request.Context(), id)
	writeUnread(c, n, err)
}

// MarkAllMineRead 实现 POST /api/v1/me/notifications/read-all。
func (h *NotificationHandler) MarkAllMineRead(c *gin.Context) {
	n, err := h.svc.MarkAllMineRead(c.Request.Context())
	writeUnread(c, n, err)
}

// ---------------------------------------------------------------------------
// 后台
// ---------------------------------------------------------------------------

// UnreadForStaff 实现 GET /api/v1/admin/notifications/unread-count。
func (h *NotificationHandler) UnreadForStaff(c *gin.Context) {
	n, err := h.svc.UnreadForStaff(c.Request.Context())
	writeUnread(c, n, err)
}

// MarkReadForStaff 实现 POST /api/v1/admin/notifications/{notification_id}/read。
func (h *NotificationHandler) MarkReadForStaff(c *gin.Context) {
	id, ok := notificationPathID(c)
	if !ok {
		return
	}
	n, err := h.svc.MarkReadForStaff(c.Request.Context(), id)
	writeUnread(c, n, err)
}

// MarkAllReadForStaff 实现 POST /api/v1/admin/notifications/read-all。
func (h *NotificationHandler) MarkAllReadForStaff(c *gin.Context) {
	n, err := h.svc.MarkAllReadForStaff(c.Request.Context())
	writeUnread(c, n, err)
}
