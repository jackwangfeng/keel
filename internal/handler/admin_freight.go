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

// 运费模板的后台接口（契约 /admin/freight-templates 那一段，00055）：
//
//	GET    /admin/freight-templates                  列表（admin_freight_list.go：它读 query 参数）
//	POST   /admin/freight-templates                  新建（Idempotency-Key 必填）
//	GET    /admin/freight-templates/{template_id}    详情
//	PUT    /admin/freight-templates/{template_id}    整体替换
//	DELETE /admin/freight-templates/{template_id}    软删
//
// 角色检查在 service/admin_freight.go（全店模板 requireMerchantWide、门店模板
// authorizeStore(storeOperate)），这里一个 if 都不写。
//
// 另外放着运费在买家侧几条接口上共用的渲染（apiFreightBreakdown 之类）。

type AdminFreightHandler struct{ svc *service.AdminFreightService }

func NewAdminFreightHandler(s *service.AdminFreightService) *AdminFreightHandler {
	return &AdminFreightHandler{svc: s}
}

// freightInputOf 把契约的 FreightTemplateInput 变成 service 的入参。
// 数值越界（int → int32）在这里挡成 422，而不是截断成一个合法的数。
func freightInputOf(c *gin.Context, raw api.FreightTemplateInput) (service.FreightTemplateInput, bool) {
	in := service.FreightTemplateInput{
		Name:       raw.Name,
		StoreID:    raw.StoreId,
		ChargeMode: int16(raw.ChargeMode),
		IsDefault:  raw.IsDefault != nil && *raw.IsDefault,
	}
	if raw.ChargeMode < -1<<15 || raw.ChargeMode >= 1<<15 {
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "charge_mode 超出范围")
		return service.FreightTemplateInput{}, false
	}
	if raw.UndeliverableRegionCodes != nil {
		in.UndeliverableRegionCodes = *raw.UndeliverableRegionCodes
	}
	for _, r := range raw.Rules {
		fu, au, fq := r.FirstUnit, r.AdditionalUnit, r.FreeQuantity
		if !int32Fits(&fu) || !int32Fits(&au) || !int32Fits(&fq) {
			problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "规则里的数值超出范围")
			return service.FreightTemplateInput{}, false
		}
		codes := r.RegionCodes
		if codes == nil {
			codes = []string{}
		}
		in.Rules = append(in.Rules, repository.FreightRule{
			RegionCodes:        codes,
			FirstUnit:          int32(fu),
			FirstFeeCents:      int64(r.FirstFeeCents),
			AdditionalUnit:     int32(au),
			AdditionalFeeCents: int64(r.AdditionalFeeCents),
			FreeThresholdCents: int64(r.FreeThresholdCents),
			FreeQuantity:       int32(fq),
		})
	}
	return in, true
}

func (h *AdminFreightHandler) Create(c *gin.Context) {
	var raw api.FreightTemplateInput
	if !bindJSON(c, &raw) {
		return
	}
	in, ok := freightInputOf(c, raw)
	if !ok {
		return
	}
	out, replayed, err := h.svc.Create(c.Request.Context(), in, idemKeyOf(c))
	if err != nil {
		writeFreightError(c, err)
		return
	}
	markReplayed(c, replayed)
	c.JSON(http.StatusCreated, apiAdminFreightTemplate(out))
}

func (h *AdminFreightHandler) Detail(c *gin.Context) {
	id, ok := pathID(c, "template_id")
	if !ok {
		return
	}
	out, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		writeFreightError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminFreightTemplate(out))
}

func (h *AdminFreightHandler) Replace(c *gin.Context) {
	id, ok := pathID(c, "template_id")
	if !ok {
		return
	}
	var raw api.FreightTemplateInput
	if !bindJSON(c, &raw) {
		return
	}
	in, ok := freightInputOf(c, raw)
	if !ok {
		return
	}
	out, err := h.svc.Replace(c.Request.Context(), id, in)
	if err != nil {
		writeFreightError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminFreightTemplate(out))
}

func (h *AdminFreightHandler) Delete(c *gin.Context) {
	id, ok := pathID(c, "template_id")
	if !ok {
		return
	}
	if err := h.svc.Delete(c.Request.Context(), id); err != nil {
		writeFreightError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// writeFreightError 把运费模板的业务错误翻成契约里的响应。
func writeFreightError(c *gin.Context, err error) {
	if writePermissionError(c, err) {
		return
	}
	switch {
	case errors.Is(err, service.ErrFreightTemplateNotFound):
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "运费模板不存在")
	case errors.Is(err, service.ErrFreightTemplateConflict):
		writeProblemDetail(c, http.StatusConflict, problem.TypeFreightTemplateConflict,
			"这家门店已经有门店运费模板", err)
	case errors.Is(err, service.ErrFreightTemplateInUse):
		writeProblemDetail(c, http.StatusConflict, problem.TypeFreightTemplateInUse,
			"还有商品挂着这个运费模板", err)
	case errors.Is(err, service.ErrFreightBadRequest):
		// detail 原样给出：运营要知道是哪一条不成立（哪个省出现了两次、缺默认规则）。
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest,
			"运费模板的配置不成立", err)
	case errors.Is(err, service.ErrIdempotencyKeyMissing):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "缺少必填的 Idempotency-Key 请求头")
	default:
		writeOrderError(c, err)
	}
}

func apiFreightRule(r repository.FreightRule) api.FreightRule {
	codes := r.RegionCodes
	if codes == nil {
		codes = []string{}
	}
	return api.FreightRule{
		RegionCodes:        codes,
		FirstUnit:          int(r.FirstUnit),
		FirstFeeCents:      api.Money(r.FirstFeeCents),
		AdditionalUnit:     int(r.AdditionalUnit),
		AdditionalFeeCents: api.Money(r.AdditionalFeeCents),
		FreeThresholdCents: api.Money(r.FreeThresholdCents),
		FreeQuantity:       int(r.FreeQuantity),
	}
}

func apiAdminFreightTemplate(t repository.AdminFreightTemplate) api.AdminFreightTemplate {
	rules := make([]api.FreightRule, 0, len(t.Rules))
	for _, r := range t.Rules {
		rules = append(rules, apiFreightRule(r))
	}
	und := t.UndeliverableRegionCodes
	if und == nil {
		und = []string{}
	}
	return api.AdminFreightTemplate{
		Id:                       t.ID,
		Name:                     t.Name,
		StoreId:                  t.StoreID,
		ChargeMode:               api.FreightChargeMode(t.ChargeMode),
		IsDefault:                t.IsDefault,
		Rules:                    rules,
		UndeliverableRegionCodes: und,
		ProductCount:             int(t.ProductCount),
		CreatedAt:                t.CreatedAt,
		UpdatedAt:                t.UpdatedAt,
	}
}

// ---------------------------------------------------------------------------
// 买家侧共用的渲染：试算、订单详情、购物车、后台订单详情
// ---------------------------------------------------------------------------

func apiFreightBreakdown(b service.FreightBreakdown) api.FreightBreakdown {
	groups := make([]api.FreightGroup, 0, len(b.Groups))
	for _, g := range b.Groups {
		ag := api.FreightGroup{
			TemplateId: g.TemplateID,
			SkuIds:     g.SKUIDs,
			Units:      int(g.Units),
			FeeCents:   api.Money(g.FeeCents),
		}
		if ag.SkuIds == nil {
			ag.SkuIds = []int64{}
		}
		if g.TemplateName != "" {
			name := g.TemplateName
			ag.TemplateName = &name
		}
		if g.ChargeMode != 0 {
			m := api.FreightChargeMode(g.ChargeMode)
			ag.ChargeMode = &m
		}
		if g.Rule != nil {
			r := api.FreightRule{
				RegionCodes:        g.Rule.RegionCodes,
				FirstUnit:          int(g.Rule.FirstUnit),
				FirstFeeCents:      api.Money(g.Rule.FirstFeeCents),
				AdditionalUnit:     int(g.Rule.AdditionalUnit),
				AdditionalFeeCents: api.Money(g.Rule.AdditionalFeeCents),
				FreeThresholdCents: api.Money(g.Rule.FreeThresholdCents),
				FreeQuantity:       int(g.Rule.FreeQuantity),
			}
			if r.RegionCodes == nil {
				r.RegionCodes = []string{}
			}
			ag.Rule = &r
		}
		if g.FreeReason != "" {
			fr := api.FreightFreeReason(g.FreeReason)
			ag.FreeReason = &fr
		}
		groups = append(groups, ag)
	}
	out := api.FreightBreakdown{
		FreightCents:         api.Money(b.FreightCents),
		FreightDiscountCents: api.Money(b.FreightDiscountCents),
		Groups:               groups,
	}
	if b.ProvinceCode != "" {
		pc := b.ProvinceCode
		out.ProvinceCode = &pc
	}
	return out
}

// apiFreightBreakdownOf 给试算用：试算一定带着地址，明细一定在；万一是 nil
// （priceOrder 被改坏了），回一份空明细而不是 panic —— 金额字段另有来源，不会撒谎。
func apiFreightBreakdownOf(b *service.FreightBreakdown) api.FreightBreakdown {
	if b == nil {
		return api.FreightBreakdown{Groups: []api.FreightGroup{}}
	}
	return apiFreightBreakdown(*b)
}

// apiFreightBreakdownPtr 给可选的那几处（订单详情、购物车）：nil 就整个不出现。
func apiFreightBreakdownPtr(b *service.FreightBreakdown) *api.FreightBreakdown {
	if b == nil {
		return nil
	}
	v := apiFreightBreakdown(*b)
	return &v
}

func apiUndeliverable(l service.FreightUndeliverable) api.FreightUndeliverableLine {
	return api.FreightUndeliverableLine{
		SkuId:      l.SKUID,
		ReasonCode: api.FreightUndeliverableLineReasonCode(l.ReasonCode),
		Reason:     l.Reason,
	}
}
