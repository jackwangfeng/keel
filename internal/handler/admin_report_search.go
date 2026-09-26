package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// 搜索概况：GET /api/v1/admin/reports/search。
// 单独一个文件：它没有门店维度（检索日志上没有门店），理由见 admin_report.go。
// 只放全店范围的人，判据在 service（requireMerchantWide）。

// Search 实现 GET /api/v1/admin/reports/search。
func (h *AdminReportHandler) Search(c *gin.Context) {
	q := reportQuery(c.Query("period"), c.Query("start_date"), c.Query("end_date"), "", "")
	out, err := h.svc.Search(c.Request.Context(), q, reportLimit(c.Query("limit")))
	if err != nil {
		writeAdminListError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiReportSearch(out))
}
