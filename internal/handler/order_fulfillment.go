package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// 买家侧履约的两条写接口：POST /orders/{order_no}/cancel 与 .../confirm。
//
// 单独一个文件：contract_test.go 的 query 参数对账按 HandlerFile 解析整份源码，
// 这两条都没有 query 参数（NoQueryParams）。它们也不读请求体 —— 要处理哪一单
// 在路径上，幂等键在请求头里。

// Cancel 实现 POST /api/v1/orders/:order_no/cancel。
func (h *OrderHandler) Cancel(c *gin.Context) {
	order, replayed, err := h.svc.Cancel(c.Request.Context(), c.Param("order_no"),
		c.GetHeader(idempotencyKeyHeader))
	if err != nil {
		writeOrderError(c, err)
		return
	}
	if replayed {
		c.Header(idempotencyReplayedHeader, "true")
	}
	c.JSON(http.StatusOK, apiOrder(order))
}

// Confirm 实现 POST /api/v1/orders/:order_no/confirm。
func (h *OrderHandler) Confirm(c *gin.Context) {
	order, replayed, err := h.svc.Confirm(c.Request.Context(), c.Param("order_no"),
		c.GetHeader(idempotencyKeyHeader))
	if err != nil {
		writeOrderError(c, err)
		return
	}
	if replayed {
		c.Header(idempotencyReplayedHeader, "true")
	}
	c.JSON(http.StatusOK, apiOrder(order))
}
