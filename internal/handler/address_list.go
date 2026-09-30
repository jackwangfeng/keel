package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
)

// GET /addresses 单独成文件的理由写在 address.go 的文件头（query 参数对账按文件解析）。

// addressStoreID 读 store_id（契约 GET /addresses 的 store_id）：按哪家门店标 in_service_area。
// 解析规则与 cartStoreID 逐字一致：解析不出正整数就按没传处理（不标）；真传了一个
// 不存在的门店由 service 报 ErrStoreNotFound（422），不静默当作没传。
func addressStoreID(c *gin.Context) *int64 {
	if raw := c.Query("store_id"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v > 0 {
			return &v
		}
	}
	return nil
}

// List 实现 GET /api/v1/addresses。
func (h *AddressHandler) List(c *gin.Context) {
	list, err := h.svc.List(c.Request.Context(), addressStoreID(c))
	if err != nil {
		// ErrStoreNotFound（422）落在 writeAddressError 兜底的 writeOrderError 里。
		writeAddressError(c, err)
		return
	}
	out := make([]api.Address, 0, len(list))
	for _, a := range list {
		out = append(out, apiAddress(a))
	}
	c.JSON(http.StatusOK, out)
}
