package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 同城配送（00110）与同城配送模板（00111）：
//
//	GET / PUT / DELETE /api/v1/admin/stores/{store_id}/local-delivery
//	GET / POST         /api/v1/admin/local-delivery-templates
//	PUT / DELETE       /api/v1/admin/local-delivery-templates/{template_id}

func writeLocalDeliveryError(c *gin.Context, err error) {
	if writePermissionError(c, err) {
		return
	}
	switch {
	case errors.Is(err, service.ErrCatalogBadRequest):
		// detail 原样给出：运营要知道是哪一档不成立。
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "同城配送的配置不成立", err)
	case errors.Is(err, service.ErrLocalDeliveryTemplateNotFound):
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "同城配送模板不存在")
	case errors.Is(err, service.ErrLocalDeliveryTemplateInUse):
		writeProblemDetail(c, http.StatusConflict, problem.TypeLocalDeliveryTemplateInUse, "同城配送模板正在使用", err)
	case errors.Is(err, service.ErrLocalDeliveryTemplateConflict):
		writeProblemDetail(c, http.StatusConflict, problem.TypeLocalDeliveryTemplateConflict, "已有同名的同城配送模板", err)
	case errors.Is(err, service.ErrIdempotencyKeyMissing):
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "缺少必填的 Idempotency-Key 请求头")
	case errors.Is(err, service.ErrIdempotencyInFlight), errors.Is(err, service.ErrIdempotencyKeyReused):
		writeOrderError(c, err)
	default:
		writeStoreError(c, err)
	}
}

// GetLocalDelivery 实现 GET /api/v1/admin/stores/{store_id}/local-delivery。
func (h *AdminStoreHandler) GetLocalDelivery(c *gin.Context) {
	id, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	v, err := h.svc.FindLocalDelivery(c.Request.Context(), id)
	if err != nil {
		writeLocalDeliveryError(c, err)
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
	var req api.StoreLocalDeliveryRequest
	if !bindJSON(c, &req) {
		return
	}
	in := service.StoreLocalDeliveryInput{TemplateID: req.TemplateId}
	if req.MinOrderCents != nil || req.FreeOverCents != nil || req.FeeTiers != nil {
		if req.MinOrderCents == nil || req.FreeOverCents == nil || req.FeeTiers == nil {
			problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest,
				"自定义规则要给齐 min_order_cents、free_over_cents、fee_tiers")
			return
		}
		r := ruleOf(api.LocalDeliveryConfig{MinOrderCents: *req.MinOrderCents, FreeOverCents: *req.FreeOverCents,
			FeeTiers: *req.FeeTiers})
		in.Custom = &r
	}
	v, err := h.svc.PutLocalDelivery(c.Request.Context(), id, in)
	if err != nil {
		writeLocalDeliveryError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminLocalDelivery(v))
}

// ResetLocalDelivery 实现 DELETE /api/v1/admin/stores/{store_id}/local-delivery。
func (h *AdminStoreHandler) ResetLocalDelivery(c *gin.Context) {
	id, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	v, err := h.svc.ResetLocalDelivery(c.Request.Context(), id)
	if err != nil {
		writeLocalDeliveryError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminLocalDelivery(v))
}

// ListLocalDeliveryTemplates 实现 GET /api/v1/admin/local-delivery-templates。
func (h *AdminStoreHandler) ListLocalDeliveryTemplates(c *gin.Context) {
	ts, err := h.svc.ListLocalDeliveryTemplates(c.Request.Context())
	if err != nil {
		writeLocalDeliveryError(c, err)
		return
	}
	items := make([]api.LocalDeliveryTemplate, 0, len(ts))
	for _, t := range ts {
		at := apiLocalDeliveryTemplate(t)
		n := t.StoreCount
		at.StoreCount = &n
		items = append(items, at)
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// CreateLocalDeliveryTemplate 实现 POST /api/v1/admin/local-delivery-templates。
func (h *AdminStoreHandler) CreateLocalDeliveryTemplate(c *gin.Context) {
	var req api.LocalDeliveryTemplateInput
	if !bindJSON(c, &req) {
		return
	}
	t, replayed, err := h.svc.CreateLocalDeliveryTemplate(c.Request.Context(), templateInputOf(req), idemKeyOf(c))
	if err != nil {
		writeLocalDeliveryError(c, err)
		return
	}
	markReplayed(c, replayed)
	c.JSON(http.StatusCreated, apiLocalDeliveryTemplate(t))
}

// UpdateLocalDeliveryTemplate 实现 PUT /api/v1/admin/local-delivery-templates/{template_id}。
func (h *AdminStoreHandler) UpdateLocalDeliveryTemplate(c *gin.Context) {
	id, ok := pathID(c, "template_id")
	if !ok {
		return
	}
	var req api.LocalDeliveryTemplateInput
	if !bindJSON(c, &req) {
		return
	}
	t, err := h.svc.UpdateLocalDeliveryTemplate(c.Request.Context(), id, templateInputOf(req))
	if err != nil {
		writeLocalDeliveryError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiLocalDeliveryTemplate(t))
}

// DeleteLocalDeliveryTemplate 实现 DELETE /api/v1/admin/local-delivery-templates/{template_id}。
func (h *AdminStoreHandler) DeleteLocalDeliveryTemplate(c *gin.Context) {
	id, ok := pathID(c, "template_id")
	if !ok {
		return
	}
	if err := h.svc.DeleteLocalDeliveryTemplate(c.Request.Context(), id); err != nil {
		writeLocalDeliveryError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func ruleOf(cfg api.LocalDeliveryConfig) repository.LocalDelivery {
	r := repository.LocalDelivery{MinOrderCents: int64(cfg.MinOrderCents), FreeOverCents: int64(cfg.FreeOverCents),
		Tiers: make([]repository.DeliveryTier, 0, len(cfg.FeeTiers))}
	for _, t := range cfg.FeeTiers {
		r.Tiers = append(r.Tiers, repository.DeliveryTier{WithinM: t.WithinM, FeeCents: int64(t.FeeCents)})
	}
	return r
}

func templateInputOf(req api.LocalDeliveryTemplateInput) service.LocalDeliveryTemplateInput {
	return service.LocalDeliveryTemplateInput{Name: req.Name, IsDefault: req.IsDefault,
		Rule: ruleOf(api.LocalDeliveryConfig{MinOrderCents: req.MinOrderCents, FreeOverCents: req.FreeOverCents,
			FeeTiers: req.FeeTiers})}
}

func apiTiers(ts []repository.DeliveryTier) []api.DeliveryTier {
	out := make([]api.DeliveryTier, 0, len(ts))
	for _, t := range ts {
		out = append(out, api.DeliveryTier{WithinM: t.WithinM, FeeCents: api.Money(t.FeeCents)})
	}
	return out
}

func apiLocalDeliveryTemplate(t repository.LocalDeliveryTemplate) api.LocalDeliveryTemplate {
	return api.LocalDeliveryTemplate{Id: t.ID, Name: t.Name, IsDefault: t.IsDefault,
		MinOrderCents: api.Money(t.MinOrderCents), FreeOverCents: api.Money(t.FreeOverCents), FeeTiers: apiTiers(t.Tiers),
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}

func apiAdminLocalDelivery(v service.LocalDeliveryView) api.AdminLocalDelivery {
	out := api.AdminLocalDelivery{Active: v.Active, Source: api.AdminLocalDeliverySource(v.Source),
		TemplateId: v.TemplateID, TemplateName: v.TemplateName,
		MinOrderCents: api.Money(v.Effective.MinOrderCents), FreeOverCents: api.Money(v.Effective.FreeOverCents),
		FeeTiers: apiTiers(v.Effective.Tiers), UpdatedAt: v.UpdatedAt}
	if v.Custom != nil {
		out.Custom = &api.LocalDeliveryConfig{MinOrderCents: api.Money(v.Custom.MinOrderCents),
			FreeOverCents: api.Money(v.Custom.FreeOverCents), FeeTiers: apiTiers(v.Custom.Tiers)}
	}
	return out
}
