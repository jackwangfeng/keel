package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// 商品排行：GET /api/v1/admin/reports/products。
// 单独一个文件：它比概览多三个参数（sort_by / category_id / limit），理由见 admin_report.go。

// Products 实现 GET /api/v1/admin/reports/products。
func (h *AdminReportHandler) Products(c *gin.Context) {
	q := reportQuery(c.Query("period"), c.Query("start_date"), c.Query("end_date"),
		c.Query("store_id"), c.Query("region_id"))
	out, err := h.svc.Products(c.Request.Context(), q,
		adminListInt64(c.Query("category_id")), c.Query("sort_by"), reportLimit(c.Query("limit")))
	if err != nil {
		writeAdminListError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiReportProducts(out))
}

// ProductsCSV 实现 GET /api/v1/admin/reports/products.csv：同一组参数、同一个 service 方法
// （同一份口径与判权），写成 CSV 下载。格式规矩见 admin_report_csv.go。
func (h *AdminReportHandler) ProductsCSV(c *gin.Context) {
	q := reportQuery(c.Query("period"), c.Query("start_date"), c.Query("end_date"),
		c.Query("store_id"), c.Query("region_id"))
	out, err := h.svc.Products(c.Request.Context(), q,
		adminListInt64(c.Query("category_id")), c.Query("sort_by"), reportLimit(c.Query("limit")))
	if err != nil {
		writeAdminListError(c, err)
		return
	}
	utf, ascii := reportFileNames("商品排行", "product-ranking", out.Window)
	sendCSV(c, utf, ascii, reportProductsCSV(out))
}
