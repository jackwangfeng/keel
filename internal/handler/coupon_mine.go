package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
)

// GET /coupons（我的券）。
//
// 单独一个文件：contract_test.go 按文件核对「handler 读了哪些 query 参数」，
// 两条列表接口的参数集合不同，放在一起会互相串味（与 admin_region_list.go 同一个理由）。

// ListMine 实现 GET /api/v1/coupons。
func (h *CouponHandler) ListMine(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	out, err := h.svc.ListMine(c.Request.Context(), c.Query("status"), page, pageSize)
	if err != nil {
		writeCouponError(c, err)
		return
	}
	items := make([]api.UserCoupon, 0, len(out.Items))
	for _, it := range out.Items {
		items = append(items, apiUserCoupon(it.Coupon, it.Scopes, it.DisplayStatus))
	}
	c.JSON(http.StatusOK, struct {
		api.PageMeta
		Items []api.UserCoupon `json:"items"`
	}{
		PageMeta: api.PageMeta{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total)},
		Items:    items,
	})
}
