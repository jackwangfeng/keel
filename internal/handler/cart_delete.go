package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// 购物车里回 204 的两条：DELETE /cart 与 DELETE /cart/items/{item_id}。
// 单独成文件的理由写在 cart.go 的文件头（query 参数对账按文件解析）。

// Clear 实现 DELETE /api/v1/cart。
func (h *CartHandler) Clear(c *gin.Context) {
	if err := h.svc.Clear(c.Request.Context()); err != nil {
		writeCartError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// DeleteItem 实现 DELETE /api/v1/cart/items/{item_id}。
func (h *CartHandler) DeleteItem(c *gin.Context) {
	itemID, ok := cartItemPathID(c)
	if !ok {
		return
	}
	if err := h.svc.DeleteItem(c.Request.Context(), itemID); err != nil {
		writeCartError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
