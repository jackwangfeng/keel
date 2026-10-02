package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// 单独一个文件：渠道管理里只有这一条读 query 参数（store_id / page / page_size），
// 契约测试按文件清点 handler 读了哪些参数。

// ListListings 实现 GET /api/v1/admin/channel-bindings/{binding_id}/listings。
func (h *AdminChannelHandler) ListListings(c *gin.Context) {
	id, ok := pathID(c, "binding_id")
	if !ok {
		return
	}
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	var store *int64
	if v := c.Query("store_id"); v != "" {
		s, err := strconv.ParseInt(v, 10, 64)
		if err != nil || s <= 0 {
			problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "store_id 不是正整数")
			return
		}
		store = &s
	}
	ls, err := h.svc.Listings(c.Request.Context(), id, store, int32(pageSize), int32((page-1)*pageSize))
	if err != nil {
		writeChannelError(c, err)
		return
	}
	items := make([]api.ChannelListing, 0, len(ls))
	for _, l := range ls {
		items = append(items, api.ChannelListing{StoreId: l.StoreID, SkuId: l.SKUID, PublishedQty: l.PublishedQty,
			PublishedCents: l.PublishedCents, Version: l.Version, PushedAt: l.PushedAt, LastError: l.LastError})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}
