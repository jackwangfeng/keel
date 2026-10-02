package channel

import "testing"

func i64(v int64) *int64 { return &v }
func i32(v int32) *int32 { return &v }

func TestPublishedQty(t *testing.T) {
	for _, c := range []struct {
		name      string
		available int32
		rule      StockRule
		want      int32
	}{
		{"没有规则 = 全量", 9, DefaultStockRule, 9},
		{"可售为负（超卖过）按 0", -3, DefaultStockRule, 0},
		{"比例向下取整", 9, StockRule{RatioBP: 7000}, 6},
		{"减安全库存", 10, StockRule{RatioBP: 10000, SafetyQty: 3}, 7},
		{"安全库存大于可售 → 0", 2, StockRule{RatioBP: 10000, SafetyQty: 3}, 0},
		{"上限", 100, StockRule{RatioBP: 10000, CapQty: i32(20)}, 20},
		{"上限 0 = 渠道下架", 100, StockRule{RatioBP: 10000, CapQty: i32(0)}, 0},
		{"比例 0", 100, StockRule{RatioBP: 0}, 0},
		{"大数不溢出", 2_000_000_000, StockRule{RatioBP: 10000}, 2_000_000_000},
	} {
		if got := PublishedQty(c.available, c.rule); got != c.want {
			t.Errorf("%s：PublishedQty(%d, %+v) = %d，期望 %d", c.name, c.available, c.rule, got, c.want)
		}
	}
}

func TestResolveStockRuleMostSpecificWins(t *testing.T) {
	rules := []StockRule{
		{RatioBP: 9000},                                  // 渠道级
		{StoreID: i64(1), RatioBP: 8000},                 // 门店 1
		{StoreID: i64(1), SKUID: i64(7), RatioBP: 5000},  // 门店 1 × SKU 7
		{StoreID: i64(2), RatioBP: 1000},                 // 别的门店
	}
	for _, c := range []struct {
		store, sku int64
		want       int32
	}{{1, 7, 5000}, {1, 8, 8000}, {3, 7, 9000}} {
		if got := ResolveStockRule(rules, c.store, c.sku).RatioBP; got != c.want {
			t.Errorf("门店 %d SKU %d：比例 %d，期望 %d", c.store, c.sku, got, c.want)
		}
	}
	if got := ResolveStockRule(nil, 1, 1); got != DefaultStockRule {
		t.Errorf("没有规则：%+v，期望默认（全量）", got)
	}
}

func TestPublishedPrice(t *testing.T) {
	for _, c := range []struct {
		name string
		base int64
		rule PriceRule
		want int64
	}{
		{"没有规则 = 原价", 1999, PriceRule{}, 1999},
		{"加价 15%，四舍五入到分", 1999, PriceRule{MarkupBP: 1500}, 2299}, // 2298.85
		{"加价 0.5 分进位", 10, PriceRule{MarkupBP: 500}, 11},          // 10.5
		{"固定价优先于加价", 1999, PriceRule{MarkupBP: 1500, FixedCents: i64(2500)}, 2500},
		{"降价", 1000, PriceRule{MarkupBP: -1000}, 900},
	} {
		if got := PublishedPrice(c.base, c.rule); got != c.want {
			t.Errorf("%s：PublishedPrice(%d, %+v) = %d，期望 %d", c.name, c.base, c.rule, got, c.want)
		}
	}
}

func TestResolvePriceRuleSKUOverridesChannel(t *testing.T) {
	rules := []PriceRule{{MarkupBP: 1500}, {SKUID: i64(7), FixedCents: i64(999)}}
	if got := ResolvePriceRule(rules, 7); got.FixedCents == nil || *got.FixedCents != 999 {
		t.Errorf("SKU 7：%+v，期望固定价 999", got)
	}
	if got := ResolvePriceRule(rules, 8); got.MarkupBP != 1500 || got.FixedCents != nil {
		t.Errorf("SKU 8：%+v，期望渠道级加价 1500", got)
	}
}
