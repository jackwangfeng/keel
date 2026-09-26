package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// 库存预警：GET /api/v1/admin/reports/inventory-alerts。
// 单独一个文件：它没有时间窗口（库存是现状），理由见 admin_report.go。

// InventoryAlerts 实现 GET /api/v1/admin/reports/inventory-alerts。
func (h *AdminReportHandler) InventoryAlerts(c *gin.Context) {
	out, err := h.svc.InventoryAlerts(c.Request.Context(),
		adminListInt64(c.Query("store_id")), adminListInt64(c.Query("region_id")),
		reportLimit(c.Query("limit")))
	if err != nil {
		writeAdminListError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiReportAlerts(out))
}
