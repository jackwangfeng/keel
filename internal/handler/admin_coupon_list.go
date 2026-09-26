package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// GET /admin/coupon-templates（券模板列表，含统计）。
//
// 单独一个文件，理由同 admin_region_list.go：contract_test.go 按文件核对 query 参数，
// admin_coupon.go 里其余五条接口一个 query 参数都没有。

func (h *AdminCouponHandler) List(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	var status *int16
	if v := c.Query("status"); v != "" {
		n, err := strconv.ParseInt(v, 10, 16)
		if err != nil {
			problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "status 只能是 0 或 1")
			return
		}
		s := int16(n)
		status = &s
	}
	out, err := h.svc.List(c.Request.Context(), status, page, pageSize)
	if err != nil {
		writeCouponError(c, err)
		return
	}
	items := make([]api.AdminCouponTemplate, 0, len(out.Items))
	for _, v := range out.Items {
		items = append(items, apiAdminCouponTemplate(v))
	}
	c.JSON(http.StatusOK, struct {
		api.PageMeta
		Items []api.AdminCouponTemplate `json:"items"`
	}{
		PageMeta: api.PageMeta{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total)},
		Items:    items,
	})
}
