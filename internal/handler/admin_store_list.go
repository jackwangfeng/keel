package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
)

// 门店列表单独一个文件：它比大区列表多一个 region_id，而参数对账是按**文件**
// 做的（admin_region_list.go 的文件头写了完整推理）。

// ListStores 实现 GET /api/v1/admin/stores。
func (h *AdminStoreHandler) ListStores(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))

	// region_id 解析不出正整数就按没传处理，与 page 一致。
	var regionID *int64
	if raw := c.Query("region_id"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v > 0 {
			regionID = &v
		}
	}
	includeDeleted := false
	if v, err := strconv.ParseBool(c.Query("include_deleted")); err == nil {
		includeDeleted = v
	}

	out, err := h.svc.ListStores(c.Request.Context(), regionID, includeDeleted, page, pageSize)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	items := make([]api.AdminStore, 0, len(out.Items))
	for _, s := range out.Items {
		items = append(items, apiAdminStore(s))
	}
	// AdminStoreList 是契约里少数几个**有名字**的分页信封之一，因为它多一个
	// has_default —— 所以这里用生成类型，不内嵌 PageMeta。
	//
	// has_default 是「这家商家有没有默认门店」，不是「本页里有没有」：
	// 默认店可能在第三页上，而后台首页要据此决定挂不挂那条提示 ——
	// 没有默认店时，所有未授权定位的访客都会拿到 match_type = none
	// （不在服务范围），而那看起来像「商品没上架」。
	c.JSON(http.StatusOK, api.AdminStoreList{
		Page:       out.Page,
		PageSize:   out.PageSize,
		Total:      int(out.Total),
		HasDefault: out.HasDefault,
		Items:      items,
	})
}
