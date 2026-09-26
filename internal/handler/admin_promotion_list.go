package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// GET /admin/promotions（营销活动列表）。
//
// 单独一个文件，理由同 admin_coupon_list.go：contract_test.go 按文件核对 query 参数，
// admin_promotion.go 里其余三条接口一个 query 参数都没有。

func (h *AdminPromotionHandler) List(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	status, ok := optInt16Query(c, c.Query("status"), "status 只能是 0 或 1")
	if !ok {
		return
	}
	promoType, ok := optInt16Query(c, c.Query("promotion_type"), "promotion_type 只能是 1 到 5")
	if !ok {
		return
	}
	out, err := h.svc.List(c.Request.Context(), status, promoType, page, pageSize)
	if err != nil {
		writePromotionError(c, err)
		return
	}
	items := make([]api.AdminPromotion, 0, len(out.Items))
	for _, v := range out.Items {
		items = append(items, apiAdminPromotion(v))
	}
	c.JSON(http.StatusOK, struct {
		api.PageMeta
		Items []api.AdminPromotion `json:"items"`
	}{
		PageMeta: api.PageMeta{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total)},
		Items:    items,
	})
}

// optInt16Query 把一个可选的 smallint 查询参数（已经读出来的原始值）解析出来；
// 空串为 nil，解析不了报 422。参数名在调用处写成 c.Query("…") 字面量：
// contract_test.go 按字面量核对 handler 读了契约里的哪些 query 参数。
func optInt16Query(c *gin.Context, v, msg string) (*int16, bool) {
	if v == "" {
		return nil, true
	}
	n, err := strconv.ParseInt(v, 10, 16)
	if err != nil {
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, msg)
		return nil, false
	}
	s := int16(n)
	return &s, true
}
