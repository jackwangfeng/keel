package service

import (
	"encoding/json"
	"testing"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/repository"
)

func i32(v int32) *int32 { return &v }

// 自动执行上限（Review Focus 3）：比例变化 ≤ max_ratio_step_bp、安全库存变化 ≤ max_units、封顶不变；永远不自动调到 0；
// max_ratio_step_bp = 0（默认）= 不自动执行。
func TestWithinChannelPolicy(t *testing.T) {
	prop := func(cs ...ChannelStockRuleChange) repository.AgentProposal {
		raw, _ := json.Marshal(ChannelStockRulePayload{BindingID: 1, StoreID: 2, Changes: cs})
		return repository.AgentProposal{Kind: ProposalKindChannelStockRule, Payload: raw}
	}
	prev := ChannelRuleSnapshot{RatioBP: 8000, SafetyQty: 2, Level: channel.RuleLevelBinding}
	pol := repository.AgentAutoPolicy{MaxRatioStepBP: 2000, MaxUnits: 3}
	cases := []struct {
		name string
		pol  repository.AgentAutoPolicy
		c    ChannelStockRuleChange
		want bool
	}{
		{"80%→70% 在上限内", pol, ChannelStockRuleChange{RatioBP: 7000, SafetyQty: 2, Prev: prev}, true},
		{"80%→100% 在上限内", pol, ChannelStockRuleChange{RatioBP: 10000, SafetyQty: 2, Prev: prev}, true},
		{"80%→0 不自动执行", pol, ChannelStockRuleChange{RatioBP: 0, SafetyQty: 2, Prev: prev}, false},
		{"80%→0 上限放到 100% 也不行", repository.AgentAutoPolicy{MaxRatioStepBP: 10000, MaxUnits: 3},
			ChannelStockRuleChange{RatioBP: 0, SafetyQty: 2, Prev: prev}, false},
		{"80%→50% 超过单次比例变化", pol, ChannelStockRuleChange{RatioBP: 5000, SafetyQty: 2, Prev: prev}, false},
		{"安全库存 +3 在上限内", pol, ChannelStockRuleChange{RatioBP: 8000, SafetyQty: 5, Prev: prev}, true},
		{"安全库存 +4 超了", pol, ChannelStockRuleChange{RatioBP: 8000, SafetyQty: 6, Prev: prev}, false},
		{"改封顶不自动执行", pol, ChannelStockRuleChange{RatioBP: 8000, SafetyQty: 2, CapQty: i32(5), Prev: prev}, false},
		{"策略默认 0 不自动执行", repository.AgentAutoPolicy{MaxUnits: 3},
			ChannelStockRuleChange{RatioBP: 7000, SafetyQty: 2, Prev: prev}, false},
	}
	for _, c := range cases {
		if got := withinPolicy(c.pol, prop(c.c)); got != c.want {
			t.Errorf("%s：withinPolicy = %v，期望 %v", c.name, got, c.want)
		}
	}
	// 多条里有一条越界 → 整条不自动执行。
	if withinPolicy(pol, prop(ChannelStockRuleChange{RatioBP: 9000, SafetyQty: 2, Prev: prev},
		ChannelStockRuleChange{SKUID: p64(9), RatioBP: 0, SafetyQty: 2, Prev: prev})) {
		t.Error("有一条调到 0，整条都不该自动执行")
	}
}

func TestChannelRuleSnapshotAndApply(t *testing.T) {
	store, other, sku := int64(2), int64(3), int64(9)
	rules := []channel.StockRule{{RatioBP: 8000}, {StoreID: &other, RatioBP: 5000}}
	if s := ruleSnapshot(rules, store, &sku); s.Level != channel.RuleLevelBinding || s.RatioBP != 8000 {
		t.Fatalf("只有渠道级时 SKU 格子应是渠道级 80%%：%+v", s)
	}
	after := applyChanges(rules, store, []ChannelStockRuleChange{{RatioBP: 9000}, {SKUID: &sku, RatioBP: 10000, SafetyQty: 1}})
	if s := ruleSnapshot(after, store, nil); s.Level != channel.RuleLevelStore || s.RatioBP != 9000 {
		t.Fatalf("门店级改动后门店格子应是 90%%：%+v", s)
	}
	if s := ruleSnapshot(after, store, &sku); s.Level != channel.RuleLevelSKU || s.RatioBP != 10000 || s.SafetyQty != 1 {
		t.Fatalf("SKU 级改动后 SKU 格子应是 100%% / 1：%+v", s)
	}
	if len(rules) != 2 {
		t.Fatal("applyChanges 不该改原规则表")
	}
	// 再改一次同一级：替换、不追加。
	again := applyChanges(after, store, []ChannelStockRuleChange{{SKUID: &sku, RatioBP: 7000}})
	if len(again) != len(after) || ruleSnapshot(again, store, &sku).RatioBP != 7000 {
		t.Fatalf("同一级的规则应被替换：%+v", again)
	}
}

func TestChangeDirection(t *testing.T) {
	prev := ChannelRuleSnapshot{RatioBP: 8000, SafetyQty: 2, CapQty: i32(10)}
	for _, c := range []struct {
		c    ChannelStockRuleChange
		want string
	}{
		{ChannelStockRuleChange{RatioBP: 9000, SafetyQty: 9, CapQty: i32(10), Prev: prev}, "up"},
		{ChannelStockRuleChange{RatioBP: 7000, SafetyQty: 0, CapQty: i32(10), Prev: prev}, "down"},
		{ChannelStockRuleChange{RatioBP: 8000, SafetyQty: 1, CapQty: i32(10), Prev: prev}, "up"},
		{ChannelStockRuleChange{RatioBP: 8000, SafetyQty: 3, CapQty: i32(10), Prev: prev}, "down"},
		{ChannelStockRuleChange{RatioBP: 8000, SafetyQty: 2, Prev: prev}, "up"},
		{ChannelStockRuleChange{RatioBP: 8000, SafetyQty: 2, CapQty: i32(5), Prev: prev}, "down"},
	} {
		if got := changeDirection(c.c); got != c.want {
			t.Errorf("%+v：方向 %s，期望 %s", c.c, got, c.want)
		}
	}
}

// 复盘判据（spec §4.5，Review Focus 4）：positive / negative / neutral / 断货格子不计。
func TestChannelStockRuleVerdict(t *testing.T) {
	cell := func(dir string, b, a ChannelCellFacts, excluded string) ChannelOutcomeCell {
		return ChannelOutcomeCell{SKUID: 9, Direction: dir, Before: b, After: a, ExcludedReason: excluded}
	}
	cases := []struct {
		name  string
		cells []ChannelOutcomeCell
		want  string
	}{
		{"上调后挂零减半 → positive", []ChannelOutcomeCell{cell("up",
			ChannelCellFacts{HeldZeroHours: 48, Sold: 5}, ChannelCellFacts{HeldZeroHours: 10, Sold: 5}, "")}, verdictPositive},
		{"上调后卖出 +20% → positive", []ChannelOutcomeCell{cell("up",
			ChannelCellFacts{Sold: 10}, ChannelCellFacts{Sold: 12}, "")}, verdictPositive},
		{"缺货拒单增加 → negative（哪怕卖出涨了）", []ChannelOutcomeCell{cell("up",
			ChannelCellFacts{Sold: 10}, ChannelCellFacts{Sold: 20, StockoutRejects: 1}, "")}, verdictNegative},
		{"下调后挂零多 30 小时且卖出降 → negative", []ChannelOutcomeCell{cell("down",
			ChannelCellFacts{HeldZeroHours: 0, Sold: 6}, ChannelCellFacts{HeldZeroHours: 30, Sold: 3}, "")}, verdictNegative},
		{"下调后挂零多但卖出没降 → neutral", []ChannelOutcomeCell{cell("down",
			ChannelCellFacts{Sold: 6}, ChannelCellFacts{HeldZeroHours: 30, Sold: 6}, "")}, verdictNeutral},
		{"上调后没变化 → neutral", []ChannelOutcomeCell{cell("up",
			ChannelCellFacts{HeldZeroHours: 10, Sold: 10}, ChannelCellFacts{HeldZeroHours: 9, Sold: 11}, "")}, verdictNeutral},
		{"断货格子不计：它的拒单增加不算", []ChannelOutcomeCell{
			cell("up", ChannelCellFacts{}, ChannelCellFacts{StockoutRejects: 5, EmptyZeroHours: 72}, "缺货"),
			cell("up", ChannelCellFacts{Sold: 0}, ChannelCellFacts{Sold: 2}, "")}, verdictPositive},
		{"全部不计 → neutral", []ChannelOutcomeCell{
			cell("up", ChannelCellFacts{}, ChannelCellFacts{StockoutRejects: 5}, "缺货")}, verdictNeutral},
	}
	for _, c := range cases {
		if got, why := channelStockRuleVerdict(c.cells); got != c.want || why == "" {
			t.Errorf("%s：%s（%s），期望 %s", c.name, got, why, c.want)
		}
	}
}
