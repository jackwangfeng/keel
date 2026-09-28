package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 同城配送配置（00110）：GET / PUT /api/v1/admin/stores/{store_id}/local-delivery。

// GetLocalDelivery 实现 GET /api/v1/admin/stores/{store_id}/local-delivery。
func (h *AdminStoreHandler) GetLocalDelivery(c *gin.Context) {
	id, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	v, err := h.svc.FindLocalDelivery(c.Request.Context(), id)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminLocalDelivery(v))
}

// PutLocalDelivery 实现 PUT /api/v1/admin/stores/{store_id}/local-delivery。
func (h *AdminStoreHandler) PutLocalDelivery(c *gin.Context) {
	id, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	var req api.LocalDeliveryConfig
	if !bindJSON(c, &req) {
		return
	}
	in := repository.LocalDelivery{MinOrderCents: int64(req.MinOrderCents), FreeOverCents: int64(req.FreeOverCents),
		Tiers: make([]repository.DeliveryTier, 0, len(req.FeeTiers))}
	for _, t := range req.FeeTiers {
		in.Tiers = append(in.Tiers, repository.DeliveryTier{WithinM: t.WithinM, FeeCents: int64(t.FeeCents)})
	}
	v, err := h.svc.PutLocalDelivery(c.Request.Context(), id, in)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminLocalDelivery(v))
}

func apiAdminLocalDelivery(v service.LocalDeliveryView) api.AdminLocalDelivery {
	out := api.AdminLocalDelivery{Active: v.Active, MinOrderCents: api.Money(v.MinOrderCents),
		FreeOverCents: api.Money(v.FreeOverCents), FeeTiers: make([]api.DeliveryTier, 0, len(v.Tiers)),
		UpdatedAt: v.UpdatedAt}
	for _, t := range v.Tiers {
		out.FeeTiers = append(out.FeeTiers, api.DeliveryTier{WithinM: t.WithinM, FeeCents: api.Money(t.FeeCents)})
	}
	return out
}
