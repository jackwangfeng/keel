package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
)

// GET /refunds（我的退款单）。三个 query 参数（page / page_size / status）
// 全都实现了，所以它单独一个文件：contract_test.go 的 query 参数对账按文件解析，
// 与 refund.go 里那四条「没有 query 参数」的登记放在一起会互相干扰。

type refundListResponse struct {
	api.PageMeta
	Items []api.Refund `json:"items"`
}

// ListMine 实现 GET /api/v1/refunds。
func (h *RefundHandler) ListMine(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	list, err := h.svc.ListMine(c.Request.Context(), page, pageSize, parseSmallint(c.Query("status")))
	if err != nil {
		writeRefundError(c, err)
		return
	}
	c.JSON(http.StatusOK, refundListResponse{
		PageMeta: api.PageMeta{Page: list.Page, PageSize: list.PageSize, Total: int(list.Total)},
		Items:    apiRefunds(list.Items),
	})
}
