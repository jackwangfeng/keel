package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
)

// 商家列表单独一个文件：它读 page / page_size，而 admin_merchant.go 里另外三条
// 登记着 NoQueryParams。理由与 admin_staff_list.go 头上那段一字不差。

// ListMerchants 实现 GET /api/v1/admin/merchants。
func (h *AdminMerchantHandler) ListMerchants(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))

	out, err := h.svc.List(c.Request.Context(), page, pageSize)
	if err != nil {
		writeMerchantError(c, err, "只有平台级操作员能看商家目录")
		return
	}
	items := make([]api.Merchant, 0, len(out.Items))
	for _, m := range out.Items {
		items = append(items, apiMerchant(m))
	}
	c.JSON(http.StatusOK, api.MerchantList{
		Items:              items,
		Page:               out.Page,
		PageSize:           out.PageSize,
		Total:              int(out.Total),
		SingleMerchantMode: h.svc.SingleMerchantMode(),
	})
}
