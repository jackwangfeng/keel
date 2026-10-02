package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 后台的渠道管理（渠道适配层，service/admin_channel.go）：
//
//	GET          /api/v1/admin/channel-kinds
//	GET / POST   /api/v1/admin/channel-bindings
//	GET / PATCH  /api/v1/admin/channel-bindings/{binding_id}
//	PUT          /api/v1/admin/channel-bindings/{binding_id}/secrets          （只写不读）
//	GET          /api/v1/admin/channel-bindings/{binding_id}/store-links
//	PUT / DELETE /api/v1/admin/channel-bindings/{binding_id}/store-links/{store_id}
//	PUT          /api/v1/admin/channel-bindings/{binding_id}/sku-links/{sku_id}
//	GET / PUT    /api/v1/admin/channel-bindings/{binding_id}/stock-rules       （PUT 按作用域覆盖）
//	DELETE       /api/v1/admin/channel-bindings/{binding_id}/stock-rules/{rule_id}
//	GET / PUT    /api/v1/admin/channel-bindings/{binding_id}/price-rules
//	DELETE       /api/v1/admin/channel-bindings/{binding_id}/price-rules/{rule_id}
//	GET          /api/v1/admin/channel-bindings/{binding_id}/listings
//
// KEEL_CHANNELS 关闭时这些路由都不注册（404），后台据此不显示「渠道」菜单。

type AdminChannelHandler struct{ svc *service.AdminChannelService }

func NewAdminChannelHandler(s *service.AdminChannelService) *AdminChannelHandler {
	return &AdminChannelHandler{svc: s}
}

func writeChannelError(c *gin.Context, err error) {
	if writePermissionError(c, err) {
		return
	}
	switch {
	case errors.Is(err, repository.ErrChannelNotFound):
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "渠道账号（或规则 / 映射）不存在")
	case errors.Is(err, repository.ErrChannelDuplicate):
		problem.Write(c, http.StatusConflict, problem.TypeChannelDuplicate, "已经接过这个渠道账号，或这个渠道门店已映射给别的门店")
	case errors.Is(err, repository.ErrChannelRefInvalid), errors.Is(err, service.ErrChannelBadRequest),
		errors.Is(err, service.ErrChannelUnknownKind), errors.Is(err, service.ErrChannelRoleUnsupported):
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "渠道配置不成立", err)
	case errors.Is(err, service.ErrIdempotencyKeyMissing):
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "缺少必填的 Idempotency-Key 请求头")
	case errors.Is(err, service.ErrIdempotencyInFlight), errors.Is(err, service.ErrIdempotencyKeyReused):
		writeOrderError(c, err)
	default:
		writeStoreError(c, err)
	}
}

func channelJSONObject(b []byte) map[string]interface{} {
	m := map[string]interface{}{}
	_ = json.Unmarshal(b, &m)
	return m
}

func rawObject(m *map[string]interface{}) json.RawMessage {
	if m == nil {
		return nil
	}
	b, _ := json.Marshal(*m)
	return b
}

func apiChannelBinding(b repository.ChannelBinding) api.ChannelBinding {
	return api.ChannelBinding{Id: b.ID, Channel: b.Channel, ExternalAccount: b.ExternalAccount, Name: b.Name,
		Roles: int32(b.Roles), Status: api.ChannelBindingStatus(b.Status), Config: channelJSONObject(b.Config),
		WebhookPath: fmt.Sprintf("/api/v1/webhooks/channels/%d", b.ID), CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt}
}

func directionOf(d channel.Direction) api.ChannelKindCatalogDirection {
	switch d {
	case channel.DirIn:
		return api.Inbound
	case channel.DirOut:
		return api.Outbound
	}
	return api.Off
}

// ListKinds 实现 GET /api/v1/admin/channel-kinds。
func (h *AdminChannelHandler) ListKinds(c *gin.Context) {
	as, err := h.svc.Kinds(c.Request.Context())
	if err != nil {
		writeChannelError(c, err)
		return
	}
	items := make([]api.ChannelKind, 0, len(as))
	for _, a := range as {
		cp := a.Caps()
		items = append(items, api.ChannelKind{Channel: a.Kind(), Roles: int32(cp.Roles), CatalogDirection: directionOf(cp.CatalogDirection),
			AcceptRequired: cp.AcceptRequired, RefundNeedsApproval: cp.RefundNeedsApproval, PartialRefund: cp.PartialRefund,
			StockoutAdjust: cp.StockoutAdjust})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// List 实现 GET /api/v1/admin/channel-bindings。
func (h *AdminChannelHandler) List(c *gin.Context) {
	bs, err := h.svc.List(c.Request.Context())
	if err != nil {
		writeChannelError(c, err)
		return
	}
	items := make([]api.ChannelBinding, 0, len(bs))
	for _, b := range bs {
		items = append(items, apiChannelBinding(b))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// Create 实现 POST /api/v1/admin/channel-bindings。
func (h *AdminChannelHandler) Create(c *gin.Context) {
	var req api.ChannelBindingInput
	if !bindJSON(c, &req) {
		return
	}
	in := service.ChannelBindingCreate{Channel: req.Channel, ExternalAccount: req.ExternalAccount, Name: req.Name,
		Roles: channel.Role(req.Roles), Config: rawObject(req.Config)}
	if req.Status != nil {
		in.Status = int16(*req.Status)
	}
	b, replayed, err := h.svc.Create(c.Request.Context(), in, idemKeyOf(c))
	if err != nil {
		writeChannelError(c, err)
		return
	}
	markReplayed(c, replayed)
	c.JSON(http.StatusCreated, apiChannelBinding(b))
}

// Get 实现 GET /api/v1/admin/channel-bindings/{binding_id}。
func (h *AdminChannelHandler) Get(c *gin.Context) {
	id, ok := pathID(c, "binding_id")
	if !ok {
		return
	}
	b, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		writeChannelError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiChannelBinding(b))
}

// Patch 实现 PATCH /api/v1/admin/channel-bindings/{binding_id}。
func (h *AdminChannelHandler) Patch(c *gin.Context) {
	id, ok := pathID(c, "binding_id")
	if !ok {
		return
	}
	var req api.ChannelBindingPatch
	if !bindJSON(c, &req) {
		return
	}
	p := service.ChannelBindingUpdate{Name: req.Name, Config: rawObject(req.Config)}
	if req.Roles != nil {
		r := channel.Role(*req.Roles)
		p.Roles = &r
	}
	if req.Status != nil {
		st := int16(*req.Status)
		p.Status = &st
	}
	b, err := h.svc.Update(c.Request.Context(), id, p)
	if err != nil {
		writeChannelError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiChannelBinding(b))
}

// PutSecrets 实现 PUT /api/v1/admin/channel-bindings/{binding_id}/secrets。
func (h *AdminChannelHandler) PutSecrets(c *gin.Context) {
	id, ok := pathID(c, "binding_id")
	if !ok {
		return
	}
	var req api.PutAdminChannelBindingsBindingIdSecretsJSONBody
	if !bindJSON(c, &req) {
		return
	}
	raw, _ := json.Marshal(req)
	if err := h.svc.SetSecrets(c.Request.Context(), id, raw); err != nil {
		writeChannelError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func apiStoreLink(l repository.ChannelStoreLink) api.ChannelStoreLink {
	return api.ChannelStoreLink{StoreId: l.StoreID, ExternalStoreId: l.ExternalStoreID, CreatedAt: l.CreatedAt}
}

// ListStoreLinks 实现 GET /api/v1/admin/channel-bindings/{binding_id}/store-links。
func (h *AdminChannelHandler) ListStoreLinks(c *gin.Context) {
	id, ok := pathID(c, "binding_id")
	if !ok {
		return
	}
	ls, err := h.svc.StoreLinks(c.Request.Context(), id)
	if err != nil {
		writeChannelError(c, err)
		return
	}
	items := make([]api.ChannelStoreLink, 0, len(ls))
	for _, l := range ls {
		items = append(items, apiStoreLink(l))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// PutStoreLink 实现 PUT /api/v1/admin/channel-bindings/{binding_id}/store-links/{store_id}。
func (h *AdminChannelHandler) PutStoreLink(c *gin.Context) {
	id, ok := pathID(c, "binding_id")
	if !ok {
		return
	}
	store, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	var req api.PutAdminChannelBindingsBindingIdStoreLinksStoreIdJSONBody
	if !bindJSON(c, &req) {
		return
	}
	l, err := h.svc.PutStoreLink(c.Request.Context(), repository.ChannelStoreLink{BindingID: id, StoreID: store,
		ExternalStoreID: req.ExternalStoreId})
	if err != nil {
		writeChannelError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiStoreLink(l))
}

// DeleteStoreLink 实现 DELETE /api/v1/admin/channel-bindings/{binding_id}/store-links/{store_id}。
func (h *AdminChannelHandler) DeleteStoreLink(c *gin.Context) {
	id, ok := pathID(c, "binding_id")
	if !ok {
		return
	}
	store, ok := pathID(c, "store_id")
	if !ok {
		return
	}
	if err := h.svc.DeleteStoreLink(c.Request.Context(), id, store); err != nil {
		writeChannelError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// PutSKULink 实现 PUT /api/v1/admin/channel-bindings/{binding_id}/sku-links/{sku_id}。
func (h *AdminChannelHandler) PutSKULink(c *gin.Context) {
	id, ok := pathID(c, "binding_id")
	if !ok {
		return
	}
	sku, ok := pathID(c, "sku_id")
	if !ok {
		return
	}
	var req api.PutAdminChannelBindingsBindingIdSkuLinksSkuIdJSONBody
	if !bindJSON(c, &req) {
		return
	}
	if err := h.svc.PutSKULink(c.Request.Context(), repository.ChannelItemLink{BindingID: id, KeelID: sku,
		ExternalID: req.ExternalId, Extra: rawObject(req.Extra)}); err != nil {
		writeChannelError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func apiStockRule(r repository.ChannelStockRule) api.ChannelStockRule {
	safety := r.SafetyQty
	return api.ChannelStockRule{Id: r.ID, StoreId: r.StoreID, SkuId: r.SKUID, RatioBp: r.RatioBP, SafetyQty: safety,
		CapQty: r.CapQty, UpdatedAt: r.UpdatedAt}
}

// ListStockRules 实现 GET /api/v1/admin/channel-bindings/{binding_id}/stock-rules。
func (h *AdminChannelHandler) ListStockRules(c *gin.Context) {
	id, ok := pathID(c, "binding_id")
	if !ok {
		return
	}
	rs, err := h.svc.StockRules(c.Request.Context(), id)
	if err != nil {
		writeChannelError(c, err)
		return
	}
	items := make([]api.ChannelStockRule, 0, len(rs))
	for _, r := range rs {
		items = append(items, apiStockRule(r))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// PutStockRule 实现 PUT /api/v1/admin/channel-bindings/{binding_id}/stock-rules。
func (h *AdminChannelHandler) PutStockRule(c *gin.Context) {
	id, ok := pathID(c, "binding_id")
	if !ok {
		return
	}
	var req api.ChannelStockRuleInput
	if !bindJSON(c, &req) {
		return
	}
	r := repository.ChannelStockRule{BindingID: id, StoreID: req.StoreId, SKUID: req.SkuId, RatioBP: req.RatioBp, CapQty: req.CapQty}
	if req.SafetyQty != nil {
		r.SafetyQty = *req.SafetyQty
	}
	out, err := h.svc.PutStockRule(c.Request.Context(), r)
	if err != nil {
		writeChannelError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiStockRule(out))
}

// DeleteStockRule 实现 DELETE /api/v1/admin/channel-bindings/{binding_id}/stock-rules/{rule_id}。
func (h *AdminChannelHandler) DeleteStockRule(c *gin.Context) {
	id, ok := pathID(c, "binding_id")
	if !ok {
		return
	}
	rule, ok := pathID(c, "rule_id")
	if !ok {
		return
	}
	if err := h.svc.DeleteStockRule(c.Request.Context(), id, rule); err != nil {
		writeChannelError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func apiPriceRule(r repository.ChannelPriceRule) api.ChannelPriceRule {
	return api.ChannelPriceRule{Id: r.ID, SkuId: r.SKUID, MarkupBp: r.MarkupBP, FixedCents: r.FixedCents, UpdatedAt: r.UpdatedAt}
}

// ListPriceRules 实现 GET /api/v1/admin/channel-bindings/{binding_id}/price-rules。
func (h *AdminChannelHandler) ListPriceRules(c *gin.Context) {
	id, ok := pathID(c, "binding_id")
	if !ok {
		return
	}
	rs, err := h.svc.PriceRules(c.Request.Context(), id)
	if err != nil {
		writeChannelError(c, err)
		return
	}
	items := make([]api.ChannelPriceRule, 0, len(rs))
	for _, r := range rs {
		items = append(items, apiPriceRule(r))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// PutPriceRule 实现 PUT /api/v1/admin/channel-bindings/{binding_id}/price-rules。
func (h *AdminChannelHandler) PutPriceRule(c *gin.Context) {
	id, ok := pathID(c, "binding_id")
	if !ok {
		return
	}
	var req api.ChannelPriceRuleInput
	if !bindJSON(c, &req) {
		return
	}
	r := repository.ChannelPriceRule{BindingID: id, SKUID: req.SkuId, FixedCents: req.FixedCents}
	if req.MarkupBp != nil {
		r.MarkupBP = *req.MarkupBp
	}
	out, err := h.svc.PutPriceRule(c.Request.Context(), r)
	if err != nil {
		writeChannelError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiPriceRule(out))
}

// DeletePriceRule 实现 DELETE /api/v1/admin/channel-bindings/{binding_id}/price-rules/{rule_id}。
func (h *AdminChannelHandler) DeletePriceRule(c *gin.Context) {
	id, ok := pathID(c, "binding_id")
	if !ok {
		return
	}
	rule, ok := pathID(c, "rule_id")
	if !ok {
		return
	}
	if err := h.svc.DeletePriceRule(c.Request.Context(), id, rule); err != nil {
		writeChannelError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
