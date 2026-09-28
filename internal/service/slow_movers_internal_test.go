package service

import "testing"

// 滞销计算的纯函数（docs/AI经营-M10M11设计.md §2）：每一步都能手算核对。
func TestComputeSlowMoverLine(t *testing.T) {
	cases := []struct {
		name                         string
		avail                        int32
		sold                         int64
		lookback, existing, stockout int
		minAvail                     int32
		wantOK                       bool
		wantAvg                      float64
		wantEff                      int
		wantDaysNil                  bool
		wantDays                     float64
	}{
		// 30 天卖 15 件 → 日均 0.5；可售 60 → 周转 120 天。
		{"常规", 60, 15, 30, 100, 0, 10, true, 0.5, 30, false, 120},
		// 从没卖出去过：周转天数 null，排最前。
		{"从没卖过", 20, 0, 30, 100, 0, 10, true, 0, 30, true, 0},
		// 可售数不够 min_available：不出结果。
		{"可售不够", 5, 0, 30, 100, 0, 10, false, 0, 0, false, 0},
		// 可售恰好等于 min_available：出结果（≥ 是闭区间）。
		{"可售恰好达标", 10, 0, 30, 100, 0, 10, true, 0, 30, true, 0},
		// 上架才 5 天：有效天数按上架天数收窄。
		{"刚上架", 20, 5, 30, 5, 0, 10, true, 1, 5, false, 20},
		// 断货天数去分母：30 天卖 10 件，其中 20 天断货 → 分母 10，日均 1，周转 20 天。
		{"断货天不进分母", 20, 10, 30, 100, 20, 10, true, 1, 10, false, 20},
		// 回看期内天天断货：分母按 1 算，不除以 0。
		{"全断货", 20, 5, 30, 100, 30, 10, true, 5, 0, false, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, ok := computeSlowMoverLine(c.avail, c.sold, c.lookback, c.existing, c.stockout, c.minAvail)
			if ok != c.wantOK {
				t.Fatalf("ok=%v，期望 %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if l.DailyAvg != c.wantAvg || l.EffectiveDays != c.wantEff {
				t.Fatalf("得到 日均 %v 有效天 %d；期望 %v %d", l.DailyAvg, l.EffectiveDays, c.wantAvg, c.wantEff)
			}
			if c.wantDaysNil {
				if l.DaysOfStock != nil {
					t.Fatalf("days_of_stock 应为 null，得到 %v", *l.DaysOfStock)
				}
				return
			}
			if l.DaysOfStock == nil || *l.DaysOfStock != c.wantDays {
				t.Fatalf("days_of_stock = %v，期望 %v", l.DaysOfStock, c.wantDays)
			}
		})
	}
}

// 排序：从没卖出去过的（days_of_stock 为 null）排最前，其余按周转天数降序。
func TestSlowMoverSortOrder(t *testing.T) {
	d1, d2 := 5.0, 50.0
	lines := []SlowMoverLine{{SKUID: 1, DaysOfStock: &d1}, {SKUID: 2, DaysOfStock: nil}, {SKUID: 3, DaysOfStock: &d2}}
	sortSlowMoverLines(lines)
	if lines[0].SKUID != 2 || lines[1].SKUID != 3 || lines[2].SKUID != 1 {
		t.Fatalf("排序不对：%+v", lines)
	}
}
