package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
)

// 大区列表单独一个文件，理由与 admin_product_list.go 一字不差：**闸门要求的**，
// 不是为了让文件短。
//
// contract_test.go 的 queryParamsReadByHandler 解析整份源码里的 c.Query，
// 再与**每一条**路由声明的参数集合两向对账。这条读 {page, page_size,
// include_deleted}，而 GET /admin/stores 多一个 region_id —— 放同一个文件里，
// 这一条就会被判成「handler 读了一个契约里没有的参数」。
//
// 所以这个文件里只许有 query 参数集合恰好是这三个的路由。

// ListRegions 实现 GET /api/v1/admin/regions。
func (h *AdminStoreHandler) ListRegions(c *gin.Context) {
	// 解析失败就当没传：这三个在契约里全是 optional，`?page=abc` 与不带 page
	// 对客户端是同一件事。钳制规则在 service（业务规则，不是解析细节）。
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))

	// include_deleted 只认明确的「是」。契约说省略即不含，而一个写错的值
	// （`?include_deleted=yes`）被当成 true 的话，后台列表会静默多出一批
	// 软删大区 —— 那个方向比反过来糟：运营会以为自己没删掉。
	includeDeleted := false
	if v, err := strconv.ParseBool(c.Query("include_deleted")); err == nil {
		includeDeleted = v
	}

	out, err := h.svc.ListRegions(c.Request.Context(), page, pageSize, includeDeleted)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	items := make([]api.AdminRegion, 0, len(out.Items))
	for _, r := range out.Items {
		items = append(items, apiAdminRegion(r))
	}
	// 契约里这条响应是 PageMeta 与 {items} 的 allOf，生成器把它拆成了两个类型，
	// 所以这里内嵌 —— 与商品列表、员工列表同一处理。
	c.JSON(http.StatusOK, struct {
		api.PageMeta
		Items []api.AdminRegion `json:"items"`
	}{
		PageMeta: api.PageMeta{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total)},
		Items:    items,
	})
}
