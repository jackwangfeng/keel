package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// GET /me/notifications（买家消息中心）。三个 query 参数（page / page_size / unread_only）
// 全都实现了，单独一个文件的理由同 refund_list.go：query 参数对账按文件解析。

// ListMine 实现 GET /api/v1/me/notifications。
func (h *NotificationHandler) ListMine(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	unreadOnly, _ := strconv.ParseBool(c.Query("unread_only"))
	out, err := h.svc.ListMine(c.Request.Context(), page, pageSize, unreadOnly)
	if err != nil {
		writeNotificationError(c, err)
		return
	}
	c.JSON(http.StatusOK, notificationListOf(out))
}
