package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
)

// 门店库存列表单独一个文件：它的 query 参数集合是 {page, page_size,
// low_stock_only}，与另外三条列表都不同（admin_region_list.go 的文件头
// 写了完整推理）。

// ListStoreInventories 实现 GET /api/v1/admin/stores/{store_id}/inventories。
func (h *AdminStoreHandler) ListStoreInventories(c *gin.Context) {
	id, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))

	lowStockOnly := false
	if v, err := strconv.ParseBool(c.Query("low_stock_only")); err == nil {
		lowStockOnly = v
	}

	out, err := h.svc.ListStoreInventories(c.Request.Context(), id, lowStockOnly, page, pageSize)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	// **缺行显示成 0，不是漏掉**：一家刚开的店在录库存之前每个 SKU 都缺行，
	// 漏掉它们会让后台看起来「这家店一个 SKU 都没有」，
	// 而那与「开店即营业」正面冲突。那件事由 SQL 的 LEFT JOIN 做，
	// 这里只是把已经补齐的行装成契约类型。
	items := make([]api.AdminInventory, 0, len(out.Items))
	for _, in := range out.Items {
		items = append(items, apiStoreInventory(in))
	}
	c.JSON(http.StatusOK, struct {
		api.PageMeta
		Items []api.AdminInventory `json:"items"`
	}{
		PageMeta: api.PageMeta{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total)},
		Items:    items,
	})
}
