package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// GET /admin/notifications（后台铃铛）。三个 query 参数（page / page_size / unread_only）
// 全都实现了，单独一个文件的理由同 admin_refund_list.go。范围由 service 按员工收窄。

// ListForStaff 实现 GET /api/v1/admin/notifications。
func (h *NotificationHandler) ListForStaff(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	unreadOnly, _ := strconv.ParseBool(c.Query("unread_only"))
	out, err := h.svc.ListForStaff(c.Request.Context(), page, pageSize, unreadOnly)
	if err != nil {
		writeNotificationError(c, err)
		return
	}
	c.JSON(http.StatusOK, notificationListOf(out))
}
