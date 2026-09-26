package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
)

// 员工列表单独一个文件，与 order_list.go 同一个理由的加强版。
//
// 那边是为了让文件别太长；这里是**闸门要求的**：
// contract_test.go 的 queryParamsReadByHandler 按 HandlerFile 解析整份源码里
// 的 c.Query 调用，而 /admin/auth/* 那几条登记着 NoQueryParams
// （契约里它们一个 query 参数都没有）。和这条放在同一个文件里的话，
// 那三条会被判成「handler 读了一个契约里没有的参数」——
// 而那条断言正是用来抓真实漂移的，不该为文件划分让路。

// ListStaff 实现 GET /api/v1/admin/staff。
func (h *AdminAuthHandler) ListStaff(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))

	out, err := h.svc.ListStaff(c.Request.Context(), page, pageSize)
	if err != nil {
		writeStaffError(c, err)
		return
	}
	items := make([]api.Staff, 0, len(out.Items))
	for _, st := range out.Items {
		items = append(items, apiStaff(st))
	}
	// 契约里这条响应是 PageMeta 与 {items} 的 allOf，生成器把它拆成了两个类型，
	// 所以这里手写一个内联结构体。字段名逐字对着契约 —— 与商品列表同一处理。
	c.JSON(http.StatusOK, struct {
		api.PageMeta
		Items []api.Staff `json:"items"`
	}{
		PageMeta: api.PageMeta{
			Page:     out.Page,
			PageSize: out.PageSize,
			Total:    int(out.Total),
		},
		Items: items,
	})
}
