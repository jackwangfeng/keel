package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/repository"
)

// 后台商品列表单独一个文件，与 admin_staff_list.go 同一个理由 ——
// **闸门要求的**，不是为了让文件别太长。
//
// contract_test.go 的 queryParamsReadByHandler 按 HandlerFile 解析整份源码里
// 的 c.Query 调用，而这 16 条里另外 15 条登记着 NoQueryParams（契约里它们
// 一个 query 参数都没有）。和它们放在同一个文件里的话，那 15 条会被判成
// 「handler 读了一个契约里没有的参数」—— 而那条断言正是用来抓真实漂移的。

// ListProducts 实现 GET /api/v1/admin/products。
//
// 契约里这条有五个 query 参数，**五个都实现了**，所以 routes 表里那一行
// 既不写 NoQueryParams 也不挂账 —— 对账测试会两个方向都核一遍。
func (h *AdminCatalogHandler) ListProducts(c *gin.Context) {
	// 解析失败就当没传：这五个在契约里全是 optional，`?page=abc` 与不带 page
	// 对客户端是同一件事。钳制规则在 service（业务规则，不是解析细节）。
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))

	var f repository.ProductFilter

	// status **刻意没有默认值**，契约在那个参数上写着理由：
	// 缺省被代入会静默改变「返回哪些行」。所以只有真的传了一个合法取值
	// 才去筛 —— 传 `?status=abc` 与不传一样是「不按状态筛」，
	// 而不是「按 status = 0 草稿筛」。
	if raw := c.Query("status"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 16); err == nil && v >= 0 && v <= 2 {
			st := int16(v)
			f.Status = &st
		}
	}
	if raw := c.Query("category_id"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v > 0 {
			f.CategoryID = &v
		}
	}
	// include_deleted 只认明确的「是」。契约说省略即不含，
	// 而一个写错的值（`?include_deleted=yes`）被当成 true 的话，
	// 后台列表会静默多出一批软删商品 —— 那个方向比反过来糟。
	if v, err := strconv.ParseBool(c.Query("include_deleted")); err == nil {
		f.IncludeDeleted = v
	}

	out, err := h.svc.ListProducts(c.Request.Context(), page, pageSize, f)
	if err != nil {
		writeCatalogError(c, err)
		return
	}
	items := make([]api.AdminProduct, 0, len(out.Items))
	for _, p := range out.Items {
		items = append(items, apiAdminProduct(p))
	}
	// 契约里这条响应是 PageMeta 与 {items} 的 allOf，生成器把它拆成了两个类型，
	// 所以这里内嵌 —— 与商品列表、员工列表同一处理。
	c.JSON(http.StatusOK, struct {
		api.PageMeta
		Items []api.AdminProduct `json:"items"`
	}{
		PageMeta: api.PageMeta{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total)},
		Items:    items,
	})
}

// 这个文件里不该出现别的 handler：再加一条**带** query 参数的接口是可以的，
// 加一条不带 query 参数的接口会让它在对账里被判成读了不该读的参数。
