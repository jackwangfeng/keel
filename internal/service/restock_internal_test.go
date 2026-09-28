package service

import (
	"testing"
	"time"
)

// 补货计算的纯函数（docs/AI经营-M9设计.md §5）：每一步都能手算核对。
func TestComputeRestockLine(t *testing.T) {
	today := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name                                string
		avail                               int32
		sold                                int64
		lookback, existing, stockout, cover int
		wantOK                              bool
		wantAvg                             float64
		wantEff, wantSuggest                int
		wantConf, wantStockoutDate          string
	}{
		// 14 天卖 28 件 → 日均 2；可售 6 → 3 天后卖断；覆盖 14 天要 28，补 22 → 取到 25。
		{"常规", 6, 28, 14, 100, 0, 14, true, 2, 14, 25, "normal", "2026-10-01"},
		// 同样卖 28 件，但其中 7 天断货：分母 7 → 日均 4；补 4×14−6 = 50。按日历天平均只会补 22 → 25。
		{"断货天不进分母", 6, 28, 14, 100, 7, 14, true, 4, 7, 50, "normal", "2026-09-29"},
		// 上架才 3 天：分母 3，置信 low。
		{"刚上架", 10, 9, 14, 3, 0, 14, true, 3, 3, 35, "low", "2026-10-01"},
		// 没卖过：不出结果。
		{"没卖过", 10, 0, 14, 100, 0, 14, false, 0, 0, 0, "", ""},
		// 库存够：建议 0。
		{"库存够", 100, 14, 14, 100, 0, 14, true, 1, 14, 0, "normal", "2027-01-06"},
		// 已卖空：今天卖断。
		{"已卖空", 0, 14, 14, 100, 0, 14, true, 1, 14, 15, "normal", "2026-09-28"},
		// 需求很大：上限 1000。
		{"封顶", 0, 1400, 14, 100, 0, 14, true, 100, 14, 1000, "normal", "2026-09-28"},
		// 回看期内天天断货（卖出的都在断货前）：分母按 1 算，不除以 0，置信 low。
		{"全断货", 0, 5, 14, 100, 14, 14, true, 5, 0, 70, "low", "2026-09-28"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, ok := computeRestockLine(c.avail, c.sold, c.lookback, c.existing, c.stockout, c.cover, today)
			if ok != c.wantOK {
				t.Fatalf("ok=%v，期望 %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if l.DailyAvg != c.wantAvg || l.EffectiveDays != c.wantEff || l.Suggested != c.wantSuggest ||
				l.Confidence != c.wantConf || l.StockoutDate != c.wantStockoutDate {
				t.Fatalf("得到 日均 %v 有效 %d 建议 %d 置信 %s 卖断 %s；期望 %v %d %d %s %s",
					l.DailyAvg, l.EffectiveDays, l.Suggested, l.Confidence, l.StockoutDate,
					c.wantAvg, c.wantEff, c.wantSuggest, c.wantConf, c.wantStockoutDate)
			}
		})
	}
}

func TestSkuLabel(t *testing.T) {
	if got := skuLabel("D-M", `{"颜色":"黑","尺码":"M"}`); got != "尺码：M / 颜色：黑" {
		t.Errorf("skuLabel = %q", got)
	}
	if got := skuLabel("D-M", `{}`); got != "D-M" {
		t.Errorf("空规格 skuLabel = %q", got)
	}
}
