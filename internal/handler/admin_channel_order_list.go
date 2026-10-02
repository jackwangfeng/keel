package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
)

// 单独一个文件：渠道订单里只有列表读 query 参数（binding_id / store_id / status / exception_only / page / page_size），
// 契约测试按文件清点 handler 读了哪些参数。

type channelOrderListResponse struct {
	api.PageMeta
	Items []api.ChannelOrder `json:"items"`
}

// positiveQueryID 读一个可选的正整数 query 参数；不合法时写 422 并返回 ok = false。
func positiveQueryID(c *gin.Context, name, raw string) (*int64, bool) {
	if raw == "" {
		return nil, true
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, name+" 不是正整数")
		return nil, false
	}
	return &v, true
}

// ListChannelOrders 实现 GET /api/v1/admin/channel-orders。
func (h *AdminChannelHandler) ListChannelOrders(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	binding, ok := positiveQueryID(c, "binding_id", c.Query("binding_id"))
	if !ok {
		return
	}
	store, ok := positiveQueryID(c, "store_id", c.Query("store_id"))
	if !ok {
		return
	}
	f := repository.ChannelOrderFilter{BindingID: binding, StoreID: store}
	if v := c.Query("status"); v != "" {
		n, err := strconv.ParseInt(v, 10, 16)
		if err != nil || !api.ChannelOrderStatus(n).Valid() {
			problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "status 不是渠道单状态（1~7）")
			return
		}
		st := int16(n)
		f.Status = &st
	}
	if v := c.Query("exception_only"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "exception_only 只能是 true / false")
			return
		}
		f.ExceptionOnly = b
	}
	out, err := h.svc.ListChannelOrders(c.Request.Context(), f, page, pageSize)
	if err != nil {
		writeChannelOrderError(c, err)
		return
	}
	items := make([]api.ChannelOrder, 0, len(out.Items))
	for _, v := range out.Items {
		items = append(items, apiChannelOrder(v))
	}
	c.JSON(http.StatusOK, channelOrderListResponse{
		PageMeta: api.PageMeta{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total)},
		Items:    items,
	})
}
