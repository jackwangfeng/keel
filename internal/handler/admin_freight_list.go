package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// GET /admin/freight-templates（运费模板列表）。单独成文件，理由同 admin_coupon_list.go：
// contract_test.go 的 query 参数对账按 HandlerFile 解析整份源码，别的几条不读 query。

// List 实现 GET /api/v1/admin/freight-templates。
func (h *AdminFreightHandler) List(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	var storeID *int64
	if v := c.Query("store_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "store_id 必须是正整数")
			return
		}
		storeID = &n
	}
	out, err := h.svc.List(c.Request.Context(), storeID, page, pageSize)
	if err != nil {
		writeFreightError(c, err)
		return
	}
	items := make([]api.AdminFreightTemplate, 0, len(out.Items))
	for _, t := range out.Items {
		items = append(items, apiAdminFreightTemplate(t))
	}
	c.JSON(http.StatusOK, struct {
		api.PageMeta
		Items []api.AdminFreightTemplate `json:"items"`
	}{
		PageMeta: api.PageMeta{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total)},
		Items:    items,
	})
}
