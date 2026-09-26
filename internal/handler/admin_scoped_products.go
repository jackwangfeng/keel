package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/repository"
)

// 门店维度与大区维度的「商品可见性 + 生效价」列表。
//
// 这两条**可以**共用一个文件，而上面那两条列表不行：参数对账是按文件做的，
// 而这两条声明的 query 参数集合逐字相同（page / page_size / listed）。
// 再往这个文件里加第三条带别的参数的路由，这两条就都会红。

// ListStoreProducts 实现 GET /api/v1/admin/stores/{store_id}/products。
func (h *AdminStoreHandler) ListStoreProducts(c *gin.Context) {
	id, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	page, pageSize, listed := scopedProductQuery(c)

	out, err := h.svc.ListStoreProducts(c.Request.Context(), id, listed, page, pageSize)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	writeScopedListings(c, out.Page, out.PageSize, out.Total, out.Items)
}

// ListRegionProducts 实现 GET /api/v1/admin/regions/{region_id}/products。
func (h *AdminStoreHandler) ListRegionProducts(c *gin.Context) {
	id, ok := pathID(c, "region_id")
	if !ok {
		return
	}
	page, pageSize, listed := scopedProductQuery(c)

	out, err := h.svc.ListRegionProducts(c.Request.Context(), id, listed, page, pageSize)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	writeScopedListings(c, out.Page, out.PageSize, out.Total, out.Items)
}

// scopedProductQuery 读这两条共用的三个参数。
//
// 抽出来是安全的：它在**同一个文件**里，所以 queryParamsReadByHandler
// 照样解析得到这三个 c.Query —— 挪进别的文件就会让这两条被判成
// 「契约里有 page，handler 没读它」。
func scopedProductQuery(c *gin.Context) (page, pageSize int, listed *bool) {
	page, _ = strconv.Atoi(c.Query("page"))
	pageSize, _ = strconv.Atoi(c.Query("page_size"))

	// listed **刻意没有默认值**，契约在那个参数上写着理由：不传即两者都要，
	// 而缺省被代入会静默改变「返回哪些行」。所以只有真的传了一个能解析出
	// 布尔的值才去筛 —— `?listed=abc` 与不传一样是「不按上架状态筛」。
	if v, err := strconv.ParseBool(c.Query("listed")); err == nil {
		listed = &v
	}
	return page, pageSize, listed
}

func writeScopedListings(c *gin.Context, page, pageSize int, total int64,
	rows []repository.ScopedListing) {

	items := make([]api.ScopedProductListing, 0, len(rows))
	for _, l := range rows {
		items = append(items, apiScopedListing(l))
	}
	c.JSON(http.StatusOK, struct {
		api.PageMeta
		Items []api.ScopedProductListing `json:"items"`
	}{
		PageMeta: api.PageMeta{Page: page, PageSize: pageSize, Total: int(total)},
		Items:    items,
	})
}
