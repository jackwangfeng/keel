package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
)

// 经营概览与销售趋势：GET /api/v1/admin/reports/overview、GET /api/v1/admin/reports/trend。
// 两条的 query 参数一模一样（period / start_date / end_date / store_id / region_id），
// 所以共用一个文件（理由见 admin_report.go 的文件头）。

// Overview 实现 GET /api/v1/admin/reports/overview。
func (h *AdminReportHandler) Overview(c *gin.Context) {
	q := reportQuery(c.Query("period"), c.Query("start_date"), c.Query("end_date"),
		c.Query("store_id"), c.Query("region_id"))
	out, err := h.svc.Overview(c.Request.Context(), q)
	if err != nil {
		writeAdminListError(c, err)
		return
	}
	c.JSON(http.StatusOK, api.ReportOverview{
		Window:   apiReportWindow(out.Window),
		Current:  apiReportMetrics(out.Current),
		Previous: apiReportMetrics(out.Previous),
	})
}

// Trend 实现 GET /api/v1/admin/reports/trend。
func (h *AdminReportHandler) Trend(c *gin.Context) {
	q := reportQuery(c.Query("period"), c.Query("start_date"), c.Query("end_date"),
		c.Query("store_id"), c.Query("region_id"))
	out, err := h.svc.Trend(c.Request.Context(), q)
	if err != nil {
		writeAdminListError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiReportTrend(out))
}
