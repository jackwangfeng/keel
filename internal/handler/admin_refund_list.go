package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/repository"
)

// 后台退款单列表：GET /api/v1/admin/refunds。单独一个文件，理由同 admin_order_list.go。

type adminRefundListResponse struct {
	api.PageMeta
	Items []api.AdminRefund `json:"items"`
}

// ListRefunds 实现 GET /api/v1/admin/refunds。范围与订单列表同一个判据（service 收窄）。
func (h *AdminOrderHandler) ListRefunds(c *gin.Context) {
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
	dayFrom, err := adminListDay(c.Query("created_date_from"), "created_date_from")
	if err != nil {
		writeAdminListError(c, err)
		return
	}
	dayTo, err := adminListDay(c.Query("created_date_to"), "created_date_to")
	if err != nil {
		writeAdminListError(c, err)
		return
	}
	if from, to, err = h.createdRange(c, from, to, dayFrom, dayTo); err != nil {
		writeAdminListError(c, err)
		return
	}
	f := repository.AdminRefundFilter{
		Status:      parseSmallint(c.Query("status")),
		StoreID:     adminListInt64(c.Query("store_id")),
		CreatedFrom: from,
		CreatedTo:   to,
	}

	out, err := h.svc.ListRefunds(c.Request.Context(), f, page, pageSize)
	if err != nil {
		writeAdminListError(c, err)
		return
	}
	items := make([]api.AdminRefund, 0, len(out.Items))
	for _, r := range out.Items {
		items = append(items, apiAdminRefund(r))
	}
	c.JSON(http.StatusOK, adminRefundListResponse{
		PageMeta: api.PageMeta{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total)},
		Items:    items,
	})
}
