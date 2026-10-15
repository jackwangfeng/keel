package service

import "testing"

// 补货复盘的四个数按文档口径手算核对。
func TestRestockReview(t *testing.T) {
	i32 := func(v int32) *int32 { return &v }
	// 日均 4 → 7 天预测 28；实际卖 21（每天 3）→ 准确率 1 − 7/28 = 0.75。
	// 执行前可售 8：第 1 天剩 5、第 2 天 2、第 3 天 −1 → 不补的话第 3–7 天一天结束时都 ≤ 0，断 5 天；
	// 实际断 0 天 → 少断 5 天。多卖 21 − 8 = 13 件，均价 210 元 / 21 件 = 10 元 → 130 元。
	var o ProposalOutcome
	restockReview(&o, &RestockForecast{DailyAvg: 4}, i32(8), []int64{3, 3, 3, 3, 3, 3, 3}, 21, 21000, 0)
	if *o.ForecastQty != 28 || *o.ForecastAccuracy != 0.75 {
		t.Fatalf("预测：%v %v", *o.ForecastQty, *o.ForecastAccuracy)
	}
	if *o.CounterfactualStockoutDays != 5 || *o.AvoidedStockoutDays != 5 || *o.ExtraSoldQty != 13 || *o.ExtraSoldCents != 13000 {
		t.Fatalf("反事实：断 %d 少断 %d 多卖 %d 件 %d 分", *o.CounterfactualStockoutDays, *o.AvoidedStockoutDays,
			*o.ExtraSoldQty, *o.ExtraSoldCents)
	}

	// 卖得比预测多一倍以上：准确率截在 0，不出负数。实际也断了 2 天：少断 = 不补会断的 − 2。
	o = ProposalOutcome{}
	restockReview(&o, &RestockForecast{DailyAvg: 1}, i32(2), []int64{5, 5, 5, 0, 0, 0, 0}, 15, 1500, 2)
	if *o.ForecastAccuracy != 0 || *o.CounterfactualStockoutDays != 7 || *o.AvoidedStockoutDays != 5 {
		t.Fatalf("截断 / 少断：%v %d %d", *o.ForecastAccuracy, *o.CounterfactualStockoutDays, *o.AvoidedStockoutDays)
	}

	// 老提案（没有测算快照）、执行结果里也没有执行前可售：只有原来的指标，新字段全缺席。
	o = ProposalOutcome{}
	restockReview(&o, nil, nil, []int64{1, 1, 1, 1, 1, 1, 1}, 7, 700, 0)
	if o.ForecastAccuracy != nil || o.ExtraSoldQty != nil || o.AvoidedStockoutDays != nil {
		t.Fatalf("没有快照时不该编数：%+v", o)
	}

	// 原本货就够：执行前可售 100，7 天卖 10 → 不补也不断，多卖 0。
	o = ProposalOutcome{}
	restockReview(&o, &RestockForecast{DailyAvg: 0}, i32(100), []int64{2, 1, 1, 2, 1, 2, 1}, 10, 1000, 0)
	if o.ForecastAccuracy != nil || *o.CounterfactualStockoutDays != 0 || *o.ExtraSoldQty != 0 {
		t.Fatalf("货够的情形：%+v", o)
	}
}
