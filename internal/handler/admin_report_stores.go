package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// 门店 / 大区对比：GET /api/v1/admin/reports/stores。
// 单独一个文件：它没有 store_id（本来就按门店展开），理由见 admin_report.go。

// Stores 实现 GET /api/v1/admin/reports/stores。
func (h *AdminReportHandler) Stores(c *gin.Context) {
	q := reportQuery(c.Query("period"), c.Query("start_date"), c.Query("end_date"),
		"", c.Query("region_id"))
	out, err := h.svc.Stores(c.Request.Context(), q)
	if err != nil {
		writeAdminListError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiReportStores(out))
}

// StoresCSV 实现 GET /api/v1/admin/reports/stores.csv：同一组参数、同一个 service 方法，写成 CSV 下载。
func (h *AdminReportHandler) StoresCSV(c *gin.Context) {
	q := reportQuery(c.Query("period"), c.Query("start_date"), c.Query("end_date"),
		"", c.Query("region_id"))
	out, err := h.svc.Stores(c.Request.Context(), q)
	if err != nil {
		writeAdminListError(c, err)
		return
	}
	utf, ascii := reportFileNames("门店对比", "store-comparison", out.Window)
	sendCSV(c, utf, ascii, reportStoresCSV(out))
}
