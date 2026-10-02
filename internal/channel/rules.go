package channel

import "math"

// StockRule 是一条库存分配规则（channel_stock_rules）：
//
//	对外可售数 = clamp( floor(真实可售 × RatioBP / 10000) − SafetyQty, 0, CapQty )
//
// StoreID / SKUID 为空表示不限；最具体的生效（门店 × SKU > 门店 > 渠道）。
type StockRule struct {
	StoreID   *int64 `json:"store_id,omitempty"`
	SKUID     *int64 `json:"sku_id,omitempty"`
	RatioBP   int32  `json:"ratio_bp"`
	SafetyQty int32  `json:"safety_qty"`
	CapQty    *int32 `json:"cap_qty,omitempty"`
}

// 规则来自哪一级（ResolveStockRuleLevel）：渠道级 / 门店级 / 门店 × SKU 级 / 一条都没配（DefaultStockRule）。
const (
	RuleLevelBinding = "binding"
	RuleLevelStore   = "store"
	RuleLevelSKU     = "sku"
	RuleLevelDefault = "default"
)

// DefaultStockRule 是没配任何规则时的分配：全量、不留安全库存、不封顶。
var DefaultStockRule = StockRule{RatioBP: 10000}

// ResolveStockRule 挑出对 (storeID, skuID) 最具体的那条规则；一条都不适用时返回 DefaultStockRule。
func ResolveStockRule(rules []StockRule, storeID, skuID int64) StockRule {
	r, _ := ResolveStockRuleLevel(rules, storeID, skuID)
	return r
}

// ResolveStockRuleLevel 同 ResolveStockRule，另外给出生效的那条来自哪一级（RuleLevel*）。
func ResolveStockRuleLevel(rules []StockRule, storeID, skuID int64) (StockRule, string) {
	best, bestRank, level := DefaultStockRule, -1, RuleLevelDefault
	for _, r := range rules {
		rank := 0
		if r.StoreID != nil {
			if *r.StoreID != storeID {
				continue
			}
			rank++
		}
		if r.SKUID != nil {
			if *r.SKUID != skuID {
				continue
			}
			rank++
		}
		if rank > bestRank {
			best, bestRank = r, rank
			switch {
			case r.SKUID != nil:
				level = RuleLevelSKU
			case r.StoreID != nil:
				level = RuleLevelStore
			default:
				level = RuleLevelBinding
			}
		}
	}
	best.StoreID, best.SKUID = nil, nil
	return best, level
}

// PublishedQty 按规则算对外可售数。真实可售为负（超卖过）按 0。
func PublishedQty(available int32, r StockRule) int32 {
	if available <= 0 {
		return 0
	}
	q := int64(available)*int64(r.RatioBP)/10000 - int64(r.SafetyQty)
	if q < 0 {
		q = 0
	}
	if r.CapQty != nil && q > int64(*r.CapQty) {
		q = int64(*r.CapQty)
	}
	if q > math.MaxInt32 {
		q = math.MaxInt32
	}
	return int32(q)
}

// PriceRule 是一条渠道价格规则（channel_price_rules）。SKUID 为空是渠道级加价；
// 非空是 SKU 级覆盖，FixedCents 优先于 MarkupBP。
type PriceRule struct {
	SKUID      *int64
	MarkupBP   int32
	FixedCents *int64
}

// ResolvePriceRule：SKU 级覆盖优先，否则渠道级，都没有就不加价。
func ResolvePriceRule(rules []PriceRule, skuID int64) PriceRule {
	var channelLevel *PriceRule
	for i := range rules {
		r := rules[i]
		if r.SKUID == nil {
			channelLevel = &rules[i]
			continue
		}
		if *r.SKUID == skuID {
			r.SKUID = nil
			return r
		}
	}
	if channelLevel != nil {
		return *channelLevel
	}
	return PriceRule{}
}

// PublishedPrice 按规则算推给渠道的价格（分）。加价四舍五入到分（半分进位）。
func PublishedPrice(baseCents int64, r PriceRule) int64 {
	if r.FixedCents != nil {
		return *r.FixedCents
	}
	n := baseCents * int64(10000+r.MarkupBP)
	q, rem := n/10000, n%10000
	if rem*2 >= 10000 {
		q++
	}
	return q
}
