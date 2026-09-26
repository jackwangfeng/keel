package service

import (
	"fmt"
	"slices"

	"github.com/keel/keel/internal/repository"
)

// 运费怎么算。**全仓库只有这一份实现**（数据模型 §7「运费怎么算」）。
//
// 试算（POST /orders/preview）、下单（POST /orders）、购物车（GET /cart）、
// 「本单可用券」里的包邮券，四处都走 freightContext.quote。它是纯函数：模板、
// 规则、每一行的重量与挂的模板由调用方先取齐（freight.go 的 loadFreightContext），
// 这里不碰数据库、不看时钟，于是每一条规则都能在没有库的单元测试里逐分核对
// （freight_calc_test.go）。
//
// # 在计价顺序里的位置（与营销活动那一段约定好的，不能挪）
//
//	商品原价 → 营销活动优惠 → 优惠券（门槛按活动后金额判）→ 运费 → 包邮券抵运费
//
// 运费这一段的输入是「优惠后的应付商品金额 + 每一行的件数（重量由 SKU 查）+
// 收货地址归到的省 + 履约门店」（FreightRequest），满额包邮就比那个金额。
// 它不知道金额是怎么减出来的 —— 营销活动接进来之后，调用方减完活动与券再把
// 结果递进来即可，这一段一个字不用改。
//
// # 几条定死的口径
//
//   - 一行用哪个模板：商品单独挂的 → 履约门店的门店模板 → 全店默认 → 没有（这一行不计运费）。
//   - 同一个模板的行合成一组计费；**不同模板的组各自计费、求和**。不做淘宝那种
//     「首费取最大、其余只收续费」的跨模板合并：那条规则的前提是「同一个包裹」，
//     而本系统一单一个包裹、模板又是商家自己配的，求和是商家看得懂、对得上的口径；
//     绝大多数店只有一个模板，两种算法在那时逐分相等。
//   - 包邮条件按**整单**判：满额比整单优惠后应付商品金额，满件比整单件数（送得到的那些）。
//     不按模板分组判，理由同上 —— 买家看到的「满 99 包邮」说的是这一单，不是这一单的某一部分。
//   - 一组命中哪条规则：收货省在哪条规则的 region_codes 里就是哪条，都不在就是默认规则。
//     地址归不到省时直接用默认规则 —— 但模板设了不配送地区时，归不到省就判不了
//     送不送得到，那一组整组报 province_unknown，而不是假装送得到。
//   - 首件（首重）以内收首费，超出部分每满一个续件（续重）单位收一次续费，不足一个单位按一个算。
//     按重量计费时一组的重量是 Σ(weight_gram × 件数)；没填重量（0 克）的 SKU 按 0 克算，
//     整组只收首重费 —— 一组只要有货，首费就要收。

// 计费方式（freight_templates.charge_mode）。
const (
	freightChargeByPiece  int16 = 1
	freightChargeByWeight int16 = 2
)

// 包邮原因（契约 FreightFreeReason）。
const (
	freightFreeThreshold  = "threshold"
	freightFreeQuantity   = "quantity"
	freightFreeNoTemplate = "no_template"
)

// 送不到的原因（契约 FreightUndeliverableLine.reason_code）。
const (
	undeliverableRegionExcluded  = "region_excluded"
	undeliverableProvinceUnknown = "province_unknown"
)

// FreightItem 是运费计算的一行输入：哪个 SKU、几件。重量与模板由 loadFreightContext 查。
type FreightItem struct {
	SKUID    int64
	Quantity int32
}

// FreightRuleSnapshot 是一条规则的对外形状（契约 FreightRule）。它也进订单快照，
// 所以 json tag 一旦定下就不能再动（同 order.go 的 receiverSnapshot）。
type FreightRuleSnapshot struct {
	RegionCodes        []string `json:"region_codes"`
	FirstUnit          int32    `json:"first_unit"`
	FirstFeeCents      int64    `json:"first_fee_cents"`
	AdditionalUnit     int32    `json:"additional_unit"`
	AdditionalFeeCents int64    `json:"additional_fee_cents"`
	FreeThresholdCents int64    `json:"free_threshold_cents"`
	FreeQuantity       int32    `json:"free_quantity"`
}

func ruleSnapshotOf(r repository.FreightRule) FreightRuleSnapshot {
	codes := r.RegionCodes
	if codes == nil {
		codes = []string{}
	}
	return FreightRuleSnapshot{
		RegionCodes: codes, FirstUnit: r.FirstUnit, FirstFeeCents: r.FirstFeeCents,
		AdditionalUnit: r.AdditionalUnit, AdditionalFeeCents: r.AdditionalFeeCents,
		FreeThresholdCents: r.FreeThresholdCents, FreeQuantity: r.FreeQuantity,
	}
}

// FreightGroup 是同一个模板的一组商品及其运费（契约 FreightGroup）。
type FreightGroup struct {
	TemplateID   *int64               `json:"template_id,omitempty"`
	TemplateName string               `json:"template_name,omitempty"`
	ChargeMode   int16                `json:"charge_mode,omitempty"`
	SKUIDs       []int64              `json:"sku_ids"`
	Units        int64                `json:"units"`
	Rule         *FreightRuleSnapshot `json:"rule,omitempty"`
	FeeCents     int64                `json:"fee_cents"`
	FreeReason   string               `json:"free_reason,omitempty"`
}

// FreightBreakdown 是运费的明细（契约 FreightBreakdown）。试算与购物车现算，
// 订单上存的是下单那一刻的这一份（orders.freight_snapshot）。
type FreightBreakdown struct {
	ProvinceCode         string         `json:"province_code,omitempty"`
	FreightCents         int64          `json:"freight_cents"`
	FreightDiscountCents int64          `json:"freight_discount_cents"`
	Groups               []FreightGroup `json:"groups"`
}

// FreightUndeliverable 是送不到的一行（契约 FreightUndeliverableLine）。
type FreightUndeliverable struct {
	SKUID      int64
	ReasonCode string
	Reason     string
}

// freightContext 是一次计价要的全部运费素材：门店、每一行的重量与挂的模板、
// 可能用到的模板（连同规则）。由 loadFreightContext 取齐。
type freightContext struct {
	store     repository.StoreScope
	info      map[int64]repository.SKUFreightInfo
	templates map[int64]repository.FreightTemplate
}

// templateFor 按「商品单独挂的 → 门店模板 → 全店默认」挑一行用的模板；都没有返回 nil。
func (fc freightContext) templateFor(skuID int64) *repository.FreightTemplate {
	if in, ok := fc.info[skuID]; ok && in.TemplateID != nil {
		if t, ok := fc.templates[*in.TemplateID]; ok {
			return &t
		}
	}
	var def *repository.FreightTemplate
	for id := range fc.templates {
		t := fc.templates[id]
		if t.StoreID != nil && *t.StoreID == fc.store.StoreID {
			return &t
		}
		if t.StoreID == nil && t.IsDefault {
			def = &t
		}
	}
	return def
}

// ruleFor 挑一组命中的规则：省在哪条规则里就是哪条，都不在（或归不到省）就是默认规则。
func ruleFor(t repository.FreightTemplate, provinceCode string) (repository.FreightRule, bool) {
	var def *repository.FreightRule
	for i := range t.Rules {
		r := t.Rules[i]
		if r.IsDefault() {
			def = &r
			continue
		}
		if provinceCode != "" && slices.Contains(r.RegionCodes, provinceCode) {
			return r, true
		}
	}
	if def == nil {
		return repository.FreightRule{}, false
	}
	return *def, true
}

// freightFee 是一组按一条规则算出来的运费（不看包邮）：
// 首费 + ⌈max(0, units − 首件) ÷ 续件单位⌉ × 续费。
//
// 只对非空的组调用（组是按行分出来的，没有空组）。units 可以是 0 —— 按重量计费、
// 这一组的 SKU 都没填重量 —— 那时照收首费：一组只要有货，首费就要收。
func freightFee(r repository.FreightRule, units int64) int64 {
	fee := r.FirstFeeCents
	if over := units - int64(r.FirstUnit); over > 0 && r.AdditionalUnit > 0 {
		steps := (over + int64(r.AdditionalUnit) - 1) / int64(r.AdditionalUnit)
		fee += steps * r.AdditionalFeeCents
	}
	return fee
}

// freightGroupKey 是分组的键：模板 id；没有模板的行归到 0。
type freightGroupAcc struct {
	tpl   *repository.FreightTemplate
	items []FreightItem
}

// quote 算一单的运费。goodsPayable 是**整单优惠后应付商品金额**（满额包邮比它），
// 由调用方减完营销活动与券之后递进来。
//
// 返回送不到的行时，明细只覆盖送得到的那些组 —— 试算与下单据此整单拒绝
// （422 region-not-deliverable），购物车据此给行打标、再按送得到的行重算一次。
// error 只在模板数据坏了（没有默认规则）时出现，那是写入校验被绕过，不是买家的错。
func (fc freightContext) quote(provinceCode string, items []FreightItem,
	goodsPayable int64) (FreightBreakdown, []FreightUndeliverable, error) {

	out := FreightBreakdown{ProvinceCode: provinceCode, Groups: []FreightGroup{}}

	// 分组，保持首次出现的顺序：明细里组的顺序与订单行的顺序一致，人读得懂。
	var order []int64
	groups := map[int64]*freightGroupAcc{}
	for _, it := range items {
		t := fc.templateFor(it.SKUID)
		var key int64
		if t != nil {
			key = t.ID
		}
		g, ok := groups[key]
		if !ok {
			g = &freightGroupAcc{tpl: t}
			groups[key] = g
			order = append(order, key)
		}
		g.items = append(g.items, it)
	}

	// 先判送不送得到：包邮条件按「送得到的那些」判件数，所以这一步必须在计费之前。
	var bad []FreightUndeliverable
	deliverable := make([]int64, 0, len(order))
	var totalQty int64
	for _, key := range order {
		g := groups[key]
		reasonCode, reason := "", ""
		if g.tpl != nil && len(g.tpl.UndeliverableRegionCodes) > 0 {
			switch {
			case provinceCode == "":
				reasonCode = undeliverableProvinceUnknown
				reason = fmt.Sprintf("收货地址归不到省级行政区，而运费模板「%s」设了不配送地区，"+
					"判不了送不送得到；请补全地址的省份", g.tpl.Name)
			case slices.Contains(g.tpl.UndeliverableRegionCodes, provinceCode):
				reasonCode = undeliverableRegionExcluded
				reason = fmt.Sprintf("%s不在运费模板「%s」的配送范围", provinceName(provinceCode), g.tpl.Name)
			}
		}
		if reasonCode != "" {
			for _, it := range g.items {
				bad = append(bad, FreightUndeliverable{SKUID: it.SKUID, ReasonCode: reasonCode, Reason: reason})
			}
			continue
		}
		deliverable = append(deliverable, key)
		for _, it := range g.items {
			totalQty += int64(it.Quantity)
		}
	}

	for _, key := range deliverable {
		g := groups[key]
		fg := FreightGroup{SKUIDs: make([]int64, 0, len(g.items))}
		var qty, grams int64
		for _, it := range g.items {
			fg.SKUIDs = append(fg.SKUIDs, it.SKUID)
			qty += int64(it.Quantity)
			grams += int64(fc.info[it.SKUID].WeightGram) * int64(it.Quantity)
		}
		if g.tpl == nil {
			fg.Units = qty
			fg.FreeReason = freightFreeNoTemplate
			out.Groups = append(out.Groups, fg)
			continue
		}
		id := g.tpl.ID
		fg.TemplateID = &id
		fg.TemplateName = g.tpl.Name
		fg.ChargeMode = g.tpl.ChargeMode
		rule, ok := ruleFor(*g.tpl, provinceCode)
		if !ok {
			return FreightBreakdown{}, nil, fmt.Errorf("运费模板 %d 没有默认规则（写入校验被绕过了）", id)
		}
		snap := ruleSnapshotOf(rule)
		fg.Rule = &snap
		if g.tpl.ChargeMode == freightChargeByWeight {
			fg.Units = grams
		} else {
			fg.Units = qty
		}
		switch {
		case rule.FreeThresholdCents > 0 && goodsPayable >= rule.FreeThresholdCents:
			fg.FreeReason = freightFreeThreshold
		case rule.FreeQuantity > 0 && totalQty >= int64(rule.FreeQuantity):
			fg.FreeReason = freightFreeQuantity
		default:
			fg.FeeCents = freightFee(rule, fg.Units)
		}
		out.FreightCents += fg.FeeCents
		out.Groups = append(out.Groups, fg)
	}
	return out, bad, nil
}

// freeShippingDeduction 是一张包邮券在这一单上抵多少运费：min(运费, 封顶)，封顶 0 = 不封顶。
// 运费为 0 时返回 0 —— 调用方据此判「这张券本单不可用」（用掉一张一分钱没抵的券，比不让用更糟）。
func freeShippingDeduction(maxDiscountCents, freightCents int64) int64 {
	if freightCents <= 0 {
		return 0
	}
	if maxDiscountCents > 0 && maxDiscountCents < freightCents {
		return maxDiscountCents
	}
	return freightCents
}
