package handler

import (
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
)

// GET /admin/product-imports/template —— 下载批量导入模板。
//
// 单独一个文件，是因为三条导入接口里只有它有 query 参数（format）：contract_test.go
// 的参数对账按 handler 文件解析 c.Query，放在一起的话另外两条会被判成
// 「handler 读了契约里没有的参数」（同 admin_product_list.go 的理由）。

// Template 实现 GET /api/v1/admin/product-imports/template。
func (h *ProductImportHandler) Template(c *gin.Context) {
	tpl, err := h.svc.Template(c.Request.Context(), c.Query("format"))
	if err != nil {
		writeImportError(c, err)
		return
	}
	// 文件名两份：ASCII 的 filename 给不认 RFC 5987 的客户端，filename* 给认的
	// （浏览器都认），于是下载下来是「商品导入模板.xlsx」而不是一串百分号。
	c.Header("Content-Disposition", "attachment; filename=\""+tpl.FileName+"\"; filename*=UTF-8''"+
		url.PathEscape(tpl.FileNameUTF))
	// 模板是静态内容，但它随版本变（加一列）。不让中间层缓存，免得商家下到旧模板、
	// 填完了才发现列对不上。
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, tpl.ContentType, tpl.Data)
}
