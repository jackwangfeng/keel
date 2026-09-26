package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 券管理的后台接口（契约 /admin/coupon-templates 那一段）：
//
//	GET   /admin/coupon-templates                          列表（含统计）
//	POST  /admin/coupon-templates                          新建
//	GET   /admin/coupon-templates/{template_id}            详情
//	PATCH /admin/coupon-templates/{template_id}            修改 / 启停
//	PUT   /admin/coupon-templates/{template_id}/scopes     适用范围（整组替换）
//	POST  /admin/coupon-templates/{template_id}/grants     按手机号定向发放
//
// 角色检查（本期只放商家级 role 1、2）在 service.requireCouponStaff，不在这里。

type AdminCouponHandler struct{ svc *service.AdminCouponService }

func NewAdminCouponHandler(s *service.AdminCouponService) *AdminCouponHandler {
	return &AdminCouponHandler{svc: s}
}

func (h *AdminCouponHandler) Create(c *gin.Context) {
	var raw api.CouponTemplateCreateRequest
	if !bindJSON(c, &raw) {
		return
	}
	in := service.CouponTemplateInput{
		Name:             raw.Name,
		CouponType:       int16(raw.CouponType),
		ThresholdCents:   moneyOr0(raw.ThresholdCents),
		DiscountCents:    moneyOr0(raw.DiscountCents),
		DiscountRate:     int16(intOr0(raw.DiscountRate)),
		MaxDiscountCents: moneyOr0(raw.MaxDiscountCents),
		ValidMode:        int16(raw.ValidMode),
		ValidStartAt:     raw.ValidStartAt,
		ValidEndAt:       raw.ValidEndAt,
		ValidDays:        int32(intOr0(raw.ValidDays)),
		TotalCount:       int32(intOr0(raw.TotalCount)),
		PerUserLimit:     int32(intOr0(raw.PerUserLimit)),
		Claimable:        raw.Claimable != nil && *raw.Claimable,
	}
	if !int16Fits(raw.DiscountRate) || !int32Fits(raw.ValidDays) || !int32Fits(raw.TotalCount) ||
		!int32Fits(raw.PerUserLimit) {
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "数值超出范围")
		return
	}
	out, replayed, err := h.svc.Create(c.Request.Context(), in, idemKeyOf(c))
	if err != nil {
		writeCouponError(c, err)
		return
	}
	markReplayed(c, replayed)
	c.JSON(http.StatusCreated, apiAdminCouponTemplate(out))
}

func (h *AdminCouponHandler) Detail(c *gin.Context) {
	id, ok := pathID(c, "template_id")
	if !ok {
		return
	}
	out, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		writeCouponError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminCouponTemplate(out))
}

func (h *AdminCouponHandler) Update(c *gin.Context) {
	id, ok := pathID(c, "template_id")
	if !ok {
		return
	}
	var raw api.CouponTemplatePatchRequest
	if !bindJSON(c, &raw) {
		return
	}
	if !int16Fits(raw.DiscountRate) || !int32Fits(raw.ValidDays) || !int32Fits(raw.TotalCount) ||
		!int32Fits(raw.PerUserLimit) {
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "数值超出范围")
		return
	}
	p := service.CouponTemplatePatch{
		Name:             raw.Name,
		ThresholdCents:   moneyPtr(raw.ThresholdCents),
		DiscountCents:    moneyPtr(raw.DiscountCents),
		MaxDiscountCents: moneyPtr(raw.MaxDiscountCents),
		ValidStartAt:     raw.ValidStartAt,
		ValidEndAt:       raw.ValidEndAt,
		ValidDays:        int32Ptr(raw.ValidDays),
		TotalCount:       int32Ptr(raw.TotalCount),
		PerUserLimit:     int32Ptr(raw.PerUserLimit),
		Claimable:        raw.Claimable,
	}
	if raw.CouponType != nil {
		v := int16(*raw.CouponType)
		p.CouponType = &v
	}
	if raw.DiscountRate != nil {
		v := int16(*raw.DiscountRate)
		p.DiscountRate = &v
	}
	if raw.ValidMode != nil {
		v := int16(*raw.ValidMode)
		p.ValidMode = &v
	}
	if raw.Status != nil {
		v := int16(*raw.Status)
		p.Status = &v
	}
	out, err := h.svc.Update(c.Request.Context(), id, p)
	if err != nil {
		writeCouponError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminCouponTemplate(out))
}

func (h *AdminCouponHandler) SetScopes(c *gin.Context) {
	id, ok := pathID(c, "template_id")
	if !ok {
		return
	}
	var raw api.CouponScopesSetRequest
	if !bindJSON(c, &raw) {
		return
	}
	in := make([]repository.CouponScopeInput, 0, len(raw.Scopes))
	for _, s := range raw.Scopes {
		include := true // 契约的 default: true
		if s.Include != nil {
			include = *s.Include
		}
		in = append(in, repository.CouponScopeInput{
			ScopeType: int16(s.ScopeType), TargetID: s.TargetId, Include: include,
		})
	}
	out, err := h.svc.SetScopes(c.Request.Context(), id, in)
	if err != nil {
		writeCouponError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminCouponTemplate(out))
}

func (h *AdminCouponHandler) Grant(c *gin.Context) {
	id, ok := pathID(c, "template_id")
	if !ok {
		return
	}
	var raw api.CouponGrantRequest
	if !bindJSON(c, &raw) {
		return
	}
	out, replayed, err := h.svc.Grant(c.Request.Context(), id, raw.Phones, idemKeyOf(c))
	if err != nil {
		writeCouponError(c, err)
		return
	}
	markReplayed(c, replayed)
	items := make([]api.CouponGrantItem, 0, len(out.Items))
	for _, it := range out.Items {
		items = append(items, api.CouponGrantItem{
			Phone: it.Phone, UserId: it.UserID, UserCouponId: it.UserCouponID, CouponCode: it.CouponCode,
		})
	}
	c.JSON(http.StatusCreated, api.CouponGrantResult{
		TemplateId: out.TemplateID, Granted: len(items), Coupons: items,
	})
}

func apiAdminCouponTemplate(v service.AdminCouponView) api.AdminCouponTemplate {
	t := v.Template
	return api.AdminCouponTemplate{
		Id:               t.Rule.TemplateID,
		Name:             t.Rule.Name,
		CouponType:       api.CouponType(t.Rule.CouponType),
		ThresholdCents:   api.Money(t.Rule.ThresholdCents),
		DiscountCents:    api.Money(t.Rule.DiscountCents),
		DiscountRate:     int(t.Rule.DiscountRate),
		MaxDiscountCents: api.Money(t.Rule.MaxDiscountCents),
		ValidMode:        api.AdminCouponTemplateValidMode(t.ValidMode),
		ValidStartAt:     t.ValidStartAt,
		ValidEndAt:       t.ValidEndAt,
		ValidDays:        int(t.ValidDays),
		TotalCount:       int(t.TotalCount),
		IssuedCount:      int(t.IssuedCount),
		PerUserLimit:     int(t.PerUserLimit),
		Claimable:        t.Claimable,
		Status:           api.AdminCouponTemplateStatus(t.Status),
		Locked:           t.IssuedCount > 0,
		Scopes:           apiCouponScopes(v.Scopes),
		Stats: api.CouponTemplateStats{
			Issued: int(v.Stats.Issued), Claimed: int(v.Stats.Claimed), Granted: int(v.Stats.Granted),
			Unused: int(v.Stats.Unused), Locked: int(v.Stats.Locked), Used: int(v.Stats.Used),
			Expired: int(v.Stats.Expired),
		},
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
}

func moneyOr0(m *api.Money) int64 {
	if m == nil {
		return 0
	}
	return int64(*m)
}

func moneyPtr(m *api.Money) *int64 {
	if m == nil {
		return nil
	}
	v := int64(*m)
	return &v
}

func intOr0(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func int32Ptr(v *int) *int32 {
	if v == nil {
		return nil
	}
	n := int32(*v)
	return &n
}

// int16Fits / int32Fits：契约里是平台宽度的 int，库里是 int2 / int4。越界时报 422，
// 而不是静默截断 —— 截断会把一个荒谬的数变成一个合法的数（同 bindOrderRequest）。
func int16Fits(v *int) bool { return v == nil || (*v >= -1<<15 && *v < 1<<15) }
func int32Fits(v *int) bool { return v == nil || (*v >= -1<<31 && *v < 1<<31) }
