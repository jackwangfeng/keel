package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/repository"
)

// 后台订单列表：GET /api/v1/admin/orders。
//
// 单独一个文件：它有九个 query 参数，而 contract_test.go 的参数对账按文件解析
// （同 order_list.go 的文件头）。参数名必须以字符串字面量出现在 c.Query 里，
// 不能包进小助手 —— 那条对账用 AST 读这些字面量。

type adminOrderListResponse struct {
	api.PageMeta
	Items []api.AdminOrderSummary `json:"items"`
}

// ListOrders 实现 GET /api/v1/admin/orders。
//
// 范围（门店）由 service 按调用者的身份收窄，这里没有任何一个参数能放宽它：
// store_id 只能在范围之内再收窄（交集），越出范围拿到的是空页。
func (h *AdminOrderHandler) ListOrders(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))

	from, err := adminListTime(c.Query("created_from"), "created_from")
	if err != nil {
		writeAdminListError(c, err)
		return
	}
	to, err := adminListTime(c.Query("created_to"), "created_to")
	if err != nil {
		writeAdminListError(c, err)
		return
	}
	f := repository.AdminOrderFilter{
		Status:      parseSmallint(c.Query("status")),
		StoreID:     adminListInt64(c.Query("store_id")),
		CreatedFrom: from,
		CreatedTo:   to,
		OrderNo:     adminListText(c.Query("order_no")),
		Phone:       adminListText(c.Query("phone")),
	}

	out, err := h.svc.ListOrders(c.Request.Context(), f, page, pageSize)
	if err != nil {
		writeAdminListError(c, err)
		return
	}
	items := make([]api.AdminOrderSummary, 0, len(out.Items))
	for _, o := range out.Items {
		items = append(items, apiAdminOrderSummary(o))
	}
	c.JSON(http.StatusOK, adminOrderListResponse{
		PageMeta: api.PageMeta{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total)},
		Items:    items,
	})
}
