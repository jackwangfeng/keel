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

// 优惠券的买家侧四条接口（契约 Coupon tag）：
//
//	GET  /coupons                              我的券
//	POST /coupons/applicable                   本单可用券
//	GET  /coupon-templates                     领券中心
//	POST /coupon-templates/{template_id}/claim 领券
//
// 计算规则、状态机、并发语义都在数据模型 §7；这一层只做绑定与渲染。

type CouponHandler struct{ svc *service.CouponService }

func NewCouponHandler(s *service.CouponService) *CouponHandler { return &CouponHandler{svc: s} }

// Applicable 实现 POST /api/v1/coupons/applicable。
func (h *CouponHandler) Applicable(c *gin.Context) {
	var raw api.CouponApplicableRequest
	if !bindJSON(c, &raw) {
		return
	}
	items, ok := lineInputs(c, raw.Items)
	if !ok {
		return
	}
	out, err := h.svc.Applicable(c.Request.Context(), items, raw.StoreId)
	if err != nil {
		writeCouponError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiApplicableCoupons(out))
}

// Claim 实现 POST /api/v1/coupon-templates/{template_id}/claim。
func (h *CouponHandler) Claim(c *gin.Context) {
	id, ok := pathID(c, "template_id")
	if !ok {
		return
	}
	out, replayed, err := h.svc.Claim(c.Request.Context(), id, idemKeyOf(c))
	if err != nil {
		writeCouponError(c, err)
		return
	}
	markReplayed(c, replayed)
	c.JSON(http.StatusCreated, apiUserCoupon(out.Coupon, out.Scopes, out.DisplayStatus))
}

// lineInputs 把契约的 OrderItemInput 收成 service 的行。与 bindOrderRequest 同一条收口规则。
func lineInputs(c *gin.Context, in []api.OrderItemInput) ([]service.LineInput, bool) {
	items := make([]service.LineInput, 0, len(in))
	for _, it := range in {
		q := it.Quantity
		if q < 0 || q > int(^uint32(0)>>1) {
			problem.Write(c, http.StatusUnprocessableEntity,
				problem.TypeInvalidRequest, "商品数量超出范围")
			return nil, false
		}
		items = append(items, service.LineInput{SKUID: it.SkuId, Quantity: int32(q)})
	}
	return items, true
}

func apiCouponScopes(in []repository.CouponScope) []api.CouponScope {
	out := make([]api.CouponScope, 0, len(in))
	for _, s := range in {
		out = append(out, api.CouponScope{
			ScopeType:  api.CouponScopeScopeType(s.ScopeType),
			TargetId:   s.TargetID,
			Include:    s.Include,
			TargetName: s.TargetName,
		})
	}
	return out
}

func apiUserCoupon(c repository.UserCoupon, scopes []repository.CouponScope, status int16) api.UserCoupon {
	return api.UserCoupon{
		Id:               c.ID,
		CouponCode:       c.Code,
		TemplateId:       c.Rule.TemplateID,
		Name:             c.Rule.Name,
		CouponType:       api.CouponType(c.Rule.CouponType),
		ThresholdCents:   api.Money(c.Rule.ThresholdCents),
		DiscountCents:    api.Money(c.Rule.DiscountCents),
		DiscountRate:     int(c.Rule.DiscountRate),
		MaxDiscountCents: api.Money(c.Rule.MaxDiscountCents),
		Status:           api.UserCouponStatus(status),
		Source:           api.UserCouponSource(c.Source),
		ValidStartAt:     c.ValidStartAt,
		ValidEndAt:       c.ValidEndAt,
		UsedAt:           c.UsedAt,
		Scopes:           apiCouponScopes(scopes),
	}
}

// apiApplicableCoupons 渲染「本单可用券」。POST /coupons/applicable 与试算的
// applicable_coupons 共用 —— 同一份结果、同一段渲染。
func apiApplicableCoupons(in []service.ApplicableCoupon) []api.ApplicableCoupon {
	out := make([]api.ApplicableCoupon, 0, len(in))
	for _, a := range in {
		u := apiUserCoupon(a.Coupon, a.Scopes, a.Coupon.Status)
		out = append(out, api.ApplicableCoupon{
			ApplicableDiscountCents: api.Money(a.DiscountCents),
			Id:                      u.Id,
			CouponCode:              u.CouponCode,
			TemplateId:              u.TemplateId,
			Name:                    u.Name,
			CouponType:              u.CouponType,
			ThresholdCents:          u.ThresholdCents,
			DiscountCents:           u.DiscountCents,
			DiscountRate:            u.DiscountRate,
			MaxDiscountCents:        u.MaxDiscountCents,
			Status:                  api.ApplicableCouponStatus(u.Status),
			Source:                  api.ApplicableCouponSource(u.Source),
			ValidStartAt:            u.ValidStartAt,
			ValidEndAt:              u.ValidEndAt,
			UsedAt:                  u.UsedAt,
			Scopes:                  u.Scopes,
		})
	}
	return out
}

// writeCouponError 把券相关的业务错误翻成契约里的响应。买家侧与后台共用：
// 同一个 sentinel 在两边是同一个 type。没对上的交给 writeOrderError
// （本单可用券的定价错误与试算一模一样），再没对上就是 500。
func writeCouponError(c *gin.Context, err error) {
	// 分级权限的两种 403（role-forbidden / out-of-scope）走统一的翻译，与商品、门店那些
	// 后台接口同一个出口 —— 同一件事只有一种拒绝类型（见 admin_coupon.go 文件头）。
	if writePermissionError(c, err) {
		return
	}
	switch {
	case errors.Is(err, service.ErrCouponNotApplicable):
		writeProblemDetail(c, http.StatusConflict, problem.TypeCouponNotApplicable, "这张优惠券本单不可用", err)
	case errors.Is(err, service.ErrCouponTemplateNotFound):
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "券模板不存在")
	case errors.Is(err, service.ErrCouponSoldOut):
		writeProblemDetail(c, http.StatusConflict, problem.TypeCouponSoldOut, "券已经发完了", err)
	case errors.Is(err, service.ErrCouponClaimLimitReached):
		writeProblemDetail(c, http.StatusConflict, problem.TypeCouponClaimLimitReached, "已达每人限领", err)
	case errors.Is(err, service.ErrCouponClaimEnded):
		writeProblemDetail(c, http.StatusConflict, problem.TypeCouponClaimEnded, "这批券的活动已结束", err)
	case errors.Is(err, service.ErrCouponTemplateLocked):
		writeProblemDetail(c, http.StatusConflict, problem.TypeCouponTemplateLocked, "这批券已经发出过，券面与范围不能再改", err)
	case errors.Is(err, service.ErrCouponTemplateDisabled):
		problem.Write(c, http.StatusConflict, problem.TypeCouponTemplateDisabled, "券模板已停用")
	case errors.Is(err, service.ErrCouponBadRequest):
		// detail 原样给出：运营要知道是哪一条不成立（「满 100 减 200」、哪几个手机号查不到）。
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "券的配置不成立", err)
	case errors.Is(err, service.ErrIdempotencyKeyMissing):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "缺少必填的 Idempotency-Key 请求头")
	default:
		writeOrderError(c, err)
	}
}

// writeProblemDetail 写一个带 detail 的 problem：title 是固定的一句话（客户端可以直接显示），
// detail 是这一次的具体原因（门槛差多少、哪几个手机号查不到）。
// 原因放进 detail 而不是 title：title 按 RFC 9457 对同一个 type 应当不变。
func writeProblemDetail(c *gin.Context, status int, kind, title string, err error) {
	detail := err.Error()
	problem.WriteValue(c, status, api.Problem{Type: kind, Title: title, Status: status, Detail: &detail})
}
