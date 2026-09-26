package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
)

// GET /coupon-templates（领券中心）。
//
// 单独一个文件：contract_test.go 按文件核对「handler 读了哪些 query 参数」，
// 两条列表接口的参数集合不同，放在一起会互相串味（与 admin_region_list.go 同一个理由）。

// ListClaimable 实现 GET /api/v1/coupon-templates。
func (h *CouponHandler) ListClaimable(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	out, err := h.svc.ListClaimable(c.Request.Context(), page, pageSize)
	if err != nil {
		writeCouponError(c, err)
		return
	}
	items := make([]api.ClaimableCouponTemplate, 0, len(out.Items))
	for _, it := range out.Items {
		t := it.Template
		items = append(items, api.ClaimableCouponTemplate{
			Id:               t.Rule.TemplateID,
			Name:             t.Rule.Name,
			CouponType:       api.CouponType(t.Rule.CouponType),
			ThresholdCents:   api.Money(t.Rule.ThresholdCents),
			DiscountCents:    api.Money(t.Rule.DiscountCents),
			DiscountRate:     int(t.Rule.DiscountRate),
			MaxDiscountCents: api.Money(t.Rule.MaxDiscountCents),
			ValidMode:        api.ClaimableCouponTemplateValidMode(t.ValidMode),
			ValidStartAt:     t.ValidStartAt,
			ValidEndAt:       t.ValidEndAt,
			ValidDays:        int(t.ValidDays),
			Remaining:        it.Remaining(),
			PerUserLimit:     int(t.PerUserLimit),
			ClaimedCount:     int(t.ClaimedByMe),
			CanClaim:         t.ClaimedByMe < t.PerUserLimit,
			Scopes:           apiCouponScopes(it.Scopes),
		})
	}
	c.JSON(http.StatusOK, struct {
		api.PageMeta
		Items []api.ClaimableCouponTemplate `json:"items"`
	}{
		PageMeta: api.PageMeta{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total)},
		Items:    items,
	})
}
