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

// 营销活动的后台接口（契约 /admin/promotions 那一段）：
//
//	GET    /admin/promotions                    列表（admin_promotion_list.go）
//	POST   /admin/promotions                    新建（下线状态）
//	GET    /admin/promotions/{promotion_id}     详情
//	PATCH  /admin/promotions/{promotion_id}     修改 / 上线 / 下线
//	DELETE /admin/promotions/{promotion_id}     硬删（仅下线且无成交）
//
// 角色检查（全店范围，与券管理同一行）在 service.AdminPromotionService，不在这里。
// 这里没有 SQL、没有事务：只把契约类型翻成 service 的入参，再把结果翻回来。

type AdminPromotionHandler struct {
	svc *service.AdminPromotionService
}

func NewAdminPromotionHandler(s *service.AdminPromotionService) *AdminPromotionHandler {
	return &AdminPromotionHandler{svc: s}
}

func (h *AdminPromotionHandler) Create(c *gin.Context) {
	var raw api.PromotionCreateRequest
	if !bindJSON(c, &raw) {
		return
	}
	rules, ok := promotionRulesOf(c, raw.Tiers, raw.Scopes, raw.Skus)
	if !ok {
		return
	}
	in := service.PromotionInput{
		Name:            raw.Name,
		Type:            int16(raw.PromotionType),
		StackWithCoupon: raw.StackWithCoupon == nil || *raw.StackWithCoupon, // 契约 default: true
		StartsAt:        raw.StartsAt,
		EndsAt:          raw.EndsAt,
		GiftTemplateID:  raw.GiftCouponTemplateId,
		Rules:           rules,
	}
	if raw.ThresholdUnit != nil {
		in.ThresholdUnit = int16(*raw.ThresholdUnit)
	}
	out, replayed, err := h.svc.Create(c.Request.Context(), in, idemKeyOf(c))
	if err != nil {
		writePromotionError(c, err)
		return
	}
	markReplayed(c, replayed)
	c.JSON(http.StatusCreated, apiAdminPromotion(out))
}

func (h *AdminPromotionHandler) Detail(c *gin.Context) {
	id, ok := pathID(c, "promotion_id")
	if !ok {
		return
	}
	out, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		writePromotionError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminPromotion(out))
}

func (h *AdminPromotionHandler) Update(c *gin.Context) {
	id, ok := pathID(c, "promotion_id")
	if !ok {
		return
	}
	var raw api.PromotionPatchRequest
	if !bindJSON(c, &raw) {
		return
	}
	rules, ok := promotionRulesOf(c, raw.Tiers, raw.Scopes, raw.Skus)
	if !ok {
		return
	}
	p := service.PromotionPatch{
		Name:            raw.Name,
		StackWithCoupon: raw.StackWithCoupon,
		StartsAt:        raw.StartsAt,
		EndsAt:          raw.EndsAt,
		GiftTemplateID:  raw.GiftCouponTemplateId,
		Rules:           rules,
	}
	if raw.ThresholdUnit != nil {
		v := int16(*raw.ThresholdUnit)
		p.ThresholdUnit = &v
	}
	if raw.Status != nil {
		v := int16(*raw.Status)
		p.Status = &v
	}
	out, err := h.svc.Update(c.Request.Context(), id, p)
	if err != nil {
		writePromotionError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminPromotion(out))
}

func (h *AdminPromotionHandler) Delete(c *gin.Context) {
	id, ok := pathID(c, "promotion_id")
	if !ok {
		return
	}
	if err := h.svc.Delete(c.Request.Context(), id); err != nil {
		writePromotionError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// promotionRulesOf 把请求体里的三组规则翻成 service 的形状。nil 保持 nil（PATCH 里即「不改」）。
// 越界的整数报 422 而不是静默截断（int16Fits / int32Fits 的理由）。
func promotionRulesOf(c *gin.Context, tiers *[]api.PromotionTier, scopes *[]api.CouponScopeInput,
	skus *[]api.PromotionSkuInput) (service.PromotionRules, bool) {
	var r service.PromotionRules
	if tiers != nil {
		out := make([]repository.PromotionTier, 0, len(*tiers))
		for _, t := range *tiers {
			rate := t.DiscountRate
			if !int16Fits(&rate) {
				problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "数值超出范围")
				return r, false
			}
			out = append(out, repository.PromotionTier{
				Threshold: t.Threshold, DiscountCents: int64(t.DiscountCents), DiscountRate: int16(rate),
			})
		}
		r.Tiers = &out
	}
	if scopes != nil {
		out := make([]repository.CouponScopeInput, 0, len(*scopes))
		for _, s := range *scopes {
			include := true // 契约的 default: true
			if s.Include != nil {
				include = *s.Include
			}
			out = append(out, repository.CouponScopeInput{
				ScopeType: int16(s.ScopeType), TargetID: s.TargetId, Include: include,
			})
		}
		r.Scopes = &out
	}
	if skus != nil {
		out := make([]repository.PromotionSkuInput, 0, len(*skus))
		for _, s := range *skus {
			if !int16Fits(s.DiscountRate) || !int32Fits(s.PerUserLimit) || !int32Fits(s.StockQty) {
				problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "数值超出范围")
				return r, false
			}
			out = append(out, repository.PromotionSkuInput{
				SKUID:           s.SkuId,
				PromoPriceCents: moneyOr0(s.PromoPriceCents),
				DiscountRate:    int16(intOr0(s.DiscountRate)),
				PerUserLimit:    int32(intOr0(s.PerUserLimit)),
				StockQty:        int32(intOr0(s.StockQty)),
			})
		}
		r.Skus = &out
	}
	return r, true
}

func apiAdminPromotion(v service.AdminPromotionView) api.AdminPromotion {
	p := v.Promotion
	tiers := make([]api.PromotionTier, 0, len(v.Tiers))
	for _, t := range v.Tiers {
		tiers = append(tiers, api.PromotionTier{
			Threshold: t.Threshold, DiscountCents: api.Money(t.DiscountCents), DiscountRate: int(t.DiscountRate),
		})
	}
	skus := make([]api.PromotionSku, 0, len(v.Skus))
	for _, s := range v.Skus {
		code, title := s.SKUCode, s.Title
		skus = append(skus, api.PromotionSku{
			SkuId: s.SKUID, PromoPriceCents: api.Money(s.PromoPriceCents), DiscountRate: int(s.DiscountRate),
			PerUserLimit: int(s.PerUserLimit), StockQty: int(s.StockQty), SoldQty: int(s.SoldQty),
			SkuCode: &code, Title: &title,
		})
	}
	out := api.AdminPromotion{
		Id:              p.ID,
		Name:            p.Name,
		PromotionType:   api.PromotionType(p.Type),
		ThresholdUnit:   api.AdminPromotionThresholdUnit(p.ThresholdUnit),
		StackWithCoupon: p.StackWithCoupon,
		StartsAt:        p.StartsAt,
		EndsAt:          p.EndsAt,
		Status:          api.AdminPromotionStatus(p.Status),
		Phase:           api.AdminPromotionPhase(v.Phase),
		Tiers:           tiers,
		Scopes:          apiCouponScopes(v.Scopes),
		Skus:            skus,
		CreatedAt:       p.CreatedAt,
		UpdatedAt:       p.UpdatedAt,
	}
	if p.Type == repository.PromoNewBuyerGift {
		granted := int(v.GiftGranted)
		out.GiftCouponTemplateId = p.GiftTemplateID
		out.GiftGrantedCount = &granted
	}
	return out
}

// writePromotionError 把活动后台的业务错误翻成契约里的响应。
func writePromotionError(c *gin.Context, err error) {
	if writePermissionError(c, err) || writeInventoryUnavailable(c, err) {
		return
	}
	switch {
	case errors.Is(err, service.ErrPromotionNotFound):
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "营销活动不存在")
	case errors.Is(err, service.ErrPromotionOnline):
		writeProblemDetail(c, http.StatusConflict, problem.TypePromotionOnline, "活动上线中，改规则请先下线", err)
	case errors.Is(err, service.ErrPromotionInUse):
		writeProblemDetail(c, http.StatusConflict, problem.TypePromotionInUse, "活动已有成交或发放记录，不能删除", err)
	case errors.Is(err, service.ErrPromotionBadRequest):
		// detail 原样给出：运营要知道是哪一条不成立（「满 100 减 200」、哪个 SKU 查不到）。
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "营销活动的配置不成立", err)
	default:
		// 幂等键缺失 / 处理中 / 复用，与券管理同一个出口。
		writeCouponError(c, err)
	}
}
