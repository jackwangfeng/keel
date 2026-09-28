package service

import (
	"context"
	"errors"
	"fmt"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 提案执行后的复盘与成绩单（AI 经营 M10，docs/AI经营-M10M11设计.md §4，00122）。
//
// 执行成功时定下「什么时候量」（outcomePlan）；到点由 RunProposalOutcomes 量一次，写进 outcome。
// verdict 的规则写死在这里、每条都能从 outcome 里的数复现 —— 成绩单是给店长决定「放不放手」（M11 自动执行）的，
// 它必须是一个人能手算核对的东西，而不是一个分数：
//
//	inventory_adjust  执行后 7 天：该店该 SKU 卖出件数、断货天数。
//	                  断货 ≥ 2 天 → negative（补少了 / 补晚了）；卖出 ≥ 补货量的 30% 且断货 ≤ 1 天 → positive；否则 neutral。
//	flash_price       活动结束后 3 天：活动期间参与 SKU 的件数 vs 活动前同长周期。
//	                  ≥ 1.2 倍 → positive；< 1 倍（打了折反而卖得更少）→ negative；否则 neutral。
//	coupon            执行后 7 天：领取数、使用数、使用率。使用率 ≥ 20% → positive；一张都没人用 → negative；否则 neutral。
//	product_copy      执行后 7 天：该商品件数 vs 执行前 7 天。≥ 1.2 倍 → positive；< 0.8 倍 → negative；否则 neutral。
//	refund_decision   不量效果（没有「效果」可言），执行时直接写 {verdict: neutral}。

const (
	ProposalOutcomeInterval = time.Hour
	outcomeWindow           = 7 * 24 * time.Hour
	outcomeAfterPromoEnd    = 3 * 24 * time.Hour
	verdictPositive         = "positive"
	verdictNeutral          = "neutral"
	verdictNegative         = "negative"
)

// ProposalOutcome 是写进 agent_proposals.outcome 的形状（字段按种类出现）。
type ProposalOutcome struct {
	Verdict     string `json:"verdict"`
	Explanation string `json:"explanation"`
	// 各种类的指标。
	SoldQty         *int64   `json:"sold_qty,omitempty"`
	StockoutDays    *int     `json:"stockout_days,omitempty"`
	DeltaQty        *int32   `json:"delta_qty,omitempty"`
	QtyDuring       *int64   `json:"qty_during,omitempty"`
	QtyBefore       *int64   `json:"qty_before,omitempty"`
	AmountDuring    *int64   `json:"amount_during_cents,omitempty"`
	Claimed         *int64   `json:"claimed,omitempty"`
	Used            *int64   `json:"used,omitempty"`
	UseRate         *float64 `json:"use_rate,omitempty"`
	QtyAfter        *int64   `json:"qty_after,omitempty"`
	WindowStart     string   `json:"window_start,omitempty"`
	WindowEnd       string   `json:"window_end,omitempty"`
	ComparisonStart string   `json:"comparison_start,omitempty"`
}

// outcomePlan：执行成功时决定何时量效果；不量效果的种类直接给一份结果。
func outcomePlan(kind string, payload []byte, now time.Time) (*time.Time, []byte) {
	switch kind {
	case ProposalKindRefundDecision:
		raw, _ := json.Marshal(ProposalOutcome{Verdict: verdictNeutral, Explanation: "售后审核不量效果"})
		return nil, raw
	case ProposalKindFlashPrice:
		var pl FlashPricePayload
		if json.Unmarshal(payload, &pl) == nil && pl.EndsAt.After(now) {
			due := pl.EndsAt.Add(outcomeAfterPromoEnd)
			return &due, nil
		}
	case ProposalKindCoupon:
		// 固定时段的券：可用时段结束后一天复盘（使用率要等券都过期才定型）。
		var pl CouponPayload
		if json.Unmarshal(payload, &pl) == nil && pl.ValidEndAt != nil && pl.ValidEndAt.After(now) {
			due := pl.ValidEndAt.Add(24 * time.Hour)
			return &due, nil
		}
	}
	due := now.Add(outcomeWindow)
	return &due, nil
}

func p64(v int64) *int64 { return &v }

// verdictRatio：after / before ≥ up → positive；< down → negative。before 为 0 时有卖出就算 positive。
func verdictRatio(after, before int64, up, down float64) string {
	if before == 0 {
		if after > 0 {
			return verdictPositive
		}
		return verdictNeutral
	}
	r := float64(after) / float64(before)
	switch {
	case r >= up:
		return verdictPositive
	case r < down:
		return verdictNegative
	}
	return verdictNeutral
}

// outcomeWindowClose 是一条提案的统计窗口关闭的时刻：到这之前量出来的是半截数据。
// 与 computeOutcome 用的窗口一一对应：加库存 / 改文案 / 发券 = 执行后 7 天，限时折扣 = 活动结束
// （复盘在结束后 3 天，窗口本身在结束时就关了）。量不出效果的种类返回零值（不等）。
func outcomeWindowClose(d repository.DueProposalOutcome) time.Time {
	switch d.Kind {
	case ProposalKindInventoryAdjust, ProposalKindProductCopy:
		return d.ExecutedAt.Add(outcomeWindow)
	case ProposalKindCoupon:
		var pl CouponPayload
		if json.Unmarshal(d.Payload, &pl) == nil && pl.ValidEndAt != nil {
			return max64t(*pl.ValidEndAt, d.ExecutedAt)
		}
		return d.ExecutedAt.Add(outcomeWindow)
	case ProposalKindFlashPrice:
		var pl FlashPricePayload
		if json.Unmarshal(d.Payload, &pl) == nil {
			return max64t(pl.EndsAt, d.ExecutedAt)
		}
	}
	return time.Time{}
}

// computeOutcome 量一条提案的效果（事务里跑；断货天数经库存服务）。
func computeOutcome(ctx context.Context, tx repository.Tx, inv inventory.Service, tz string,
	d repository.DueProposalOutcome) (ProposalOutcome, error) {
	day := func(t time.Time) string { return t.Format(time.RFC3339) }
	switch d.Kind {
	case ProposalKindInventoryAdjust:
		var pl InventoryAdjustPayload
		if err := json.Unmarshal(d.Payload, &pl); err != nil {
			return ProposalOutcome{}, err
		}
		from, to := d.ExecutedAt, d.ExecutedAt.Add(outcomeWindow)
		sold, _, err := tx.SKUUnitsSoldBetween(ctx, []int64{pl.SKUID}, &pl.StoreID, from, to)
		if err != nil {
			return ProposalOutcome{}, err
		}
		so, err := inv.StockoutDays(ctx, pl.StoreID, []int64{pl.SKUID}, 7, tz)
		if err != nil {
			return ProposalOutcome{}, err
		}
		n := so[pl.SKUID]
		o := ProposalOutcome{SoldQty: &sold, StockoutDays: &n, DeltaQty: &pl.Delta, WindowStart: day(from), WindowEnd: day(to)}
		switch {
		case n >= 2:
			o.Verdict, o.Explanation = verdictNegative, "补货后 7 天里仍断货 2 天及以上"
		case float64(sold) >= 0.3*float64(pl.Delta) && n <= 1:
			o.Verdict, o.Explanation = verdictPositive, "补货后 7 天没断货（≤1 天），且卖出不少于补货量的 30%"
		default:
			o.Verdict, o.Explanation = verdictNeutral, "没断货，但补的货 7 天内卖出不到三成"
		}
		return o, nil
	case ProposalKindFlashPrice:
		var pl FlashPricePayload
		if err := json.Unmarshal(d.Payload, &pl); err != nil {
			return ProposalOutcome{}, err
		}
		ids := make([]int64, 0, len(pl.Items))
		for _, it := range pl.Items {
			ids = append(ids, it.SKUID)
		}
		start := max64t(pl.StartsAt, d.ExecutedAt)
		end := pl.EndsAt
		if end.Before(start) {
			end = start
		}
		before := start.Add(-end.Sub(start))
		during, amount, err := tx.SKUUnitsSoldBetween(ctx, ids, pl.StoreID, start, end)
		if err != nil {
			return ProposalOutcome{}, err
		}
		prev, _, err := tx.SKUUnitsSoldBetween(ctx, ids, pl.StoreID, before, start)
		if err != nil {
			return ProposalOutcome{}, err
		}
		o := ProposalOutcome{QtyDuring: &during, QtyBefore: &prev, AmountDuring: &amount,
			WindowStart: day(start), WindowEnd: day(end), ComparisonStart: day(before)}
		o.Verdict = verdictRatio(during, prev, 1.2, 1.0)
		o.Explanation = map[string]string{verdictPositive: "活动期间参与商品件数是活动前同长周期的 1.2 倍及以上",
			verdictNegative: "打了折，活动期间反而比活动前卖得少", verdictNeutral: "活动期间件数与活动前相当"}[o.Verdict]
		return o, nil
	case ProposalKindCoupon:
		var res ProposalResult
		tid, ok := int64(0), false
		if raw, err := proposalResultOf(ctx, tx, d.ID); err == nil && json.Unmarshal(raw, &res) == nil {
			if v, has := res.Detail["coupon_template_id"].(float64); has {
				tid, ok = int64(v), true
			}
		}
		if !ok {
			return ProposalOutcome{Verdict: verdictNeutral, Explanation: "找不到执行时建的券模板"}, nil
		}
		claimed, used, err := tx.CouponTemplateUsage(ctx, tid)
		if err != nil {
			return ProposalOutcome{}, err
		}
		o := ProposalOutcome{Claimed: &claimed, Used: &used}
		rate := 0.0
		if claimed > 0 {
			rate = float64(used) / float64(claimed)
		}
		o.UseRate = &rate
		switch {
		case rate >= 0.2:
			o.Verdict, o.Explanation = verdictPositive, "领取的券里至少两成被用掉了"
		case used == 0:
			o.Verdict, o.Explanation = verdictNegative, "复盘窗口里一张都没被用"
		default:
			o.Verdict, o.Explanation = verdictNeutral, "有人用，但使用率不到两成"
		}
		return o, nil
	case ProposalKindProductCopy:
		var pl ProductCopyPayload
		if err := json.Unmarshal(d.Payload, &pl); err != nil {
			return ProposalOutcome{}, err
		}
		after, err := tx.ProductUnitsSoldBetween(ctx, pl.ProductID, d.ExecutedAt, d.ExecutedAt.Add(outcomeWindow))
		if err != nil {
			return ProposalOutcome{}, err
		}
		before, err := tx.ProductUnitsSoldBetween(ctx, pl.ProductID, d.ExecutedAt.Add(-outcomeWindow), d.ExecutedAt)
		if err != nil {
			return ProposalOutcome{}, err
		}
		o := ProposalOutcome{QtyAfter: &after, QtyBefore: &before, WindowStart: day(d.ExecutedAt)}
		o.Verdict = verdictRatio(after, before, 1.2, 0.8)
		o.Explanation = map[string]string{verdictPositive: "改文案后 7 天件数是之前 7 天的 1.2 倍及以上",
			verdictNegative: "改文案后 7 天件数不到之前的八成", verdictNeutral: "改文案前后件数相当"}[o.Verdict]
		return o, nil
	}
	return ProposalOutcome{Verdict: verdictNeutral, Explanation: "这种提案不量效果"}, nil
}

func max64t(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// proposalResultOf 读一条提案的执行结果（coupon 的复盘要从里面拿券模板 id）。
func proposalResultOf(ctx context.Context, tx repository.Tx, id int64) ([]byte, error) {
	p, err := tx.FindAgentProposal(ctx, id)
	if err != nil {
		return nil, err
	}
	return p.Result, nil
}

// ProposalOutcomeRepository 是复盘扫描要的仓储能力。
type ProposalOutcomeRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	ActiveMerchants(ctx context.Context) ([]int64, error)
	ShopTimezone(ctx context.Context) (string, error)
}

// ReviewProposalOutcomesOnce 按商户逐个量到点的提案，返回量了几条。单条出错只记日志、不挡别的。
func ReviewProposalOutcomesOnce(ctx context.Context, repo ProposalOutcomeRepository, inv inventory.Service,
	log *slog.Logger) (int, error) {
	merchants, err := repo.ActiveMerchants(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, m := range merchants {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		mctx := tenant.NewContext(ctx, m)
		tz, err := repo.ShopTimezone(mctx)
		if err != nil {
			return total, err
		}
		tz, _ = reportLocation(tz)
		var due []repository.DueProposalOutcome
		if err := repo.WithTenant(mctx, func(tx repository.Tx) error {
			var e error
			due, e = tx.DueAgentProposalOutcomes(mctx)
			return e
		}); err != nil {
			return total, err
		}
		for _, d := range due {
			// outcome_due_at 由执行时按窗口定（outcomePlan），正常到点时窗口已经关了。被人手工提前、
			// 或者将来改了 outcomePlan 的时候，不拿半截窗口下结论：推迟到窗口关闭再算。
			// （2026-09-28 演示站验收强制提前过一次，算出「7 天内卖出不到三成」—— 其实才过了几分钟。）
			if closeAt := outcomeWindowClose(d); time.Now().Before(closeAt) {
				if err := repo.WithTenant(mctx, func(tx repository.Tx) error {
					return tx.DeferAgentProposalOutcome(mctx, d.ID, closeAt)
				}); err != nil {
					log.ErrorContext(mctx, "AI 员工提案复盘推迟出错", "proposal_id", d.ID, "err", err)
				}
				continue
			}
			err := repo.WithTenant(mctx, func(tx repository.Tx) error {
				o, err := computeOutcome(mctx, tx, inv, tz, d)
				if err != nil {
					return err
				}
				raw, err := json.Marshal(o)
				if err != nil {
					return err
				}
				return tx.SaveAgentProposalOutcome(mctx, d.ID, raw)
			})
			if err != nil {
				log.ErrorContext(mctx, "AI 员工提案复盘出错", "proposal_id", d.ID, "err", err)
				continue
			}
			total++
		}
	}
	return total, nil
}

// RunProposalOutcomes 每 ProposalOutcomeInterval 扫一轮，直到 ctx 结束。
func RunProposalOutcomes(ctx context.Context, repo ProposalOutcomeRepository, inv inventory.Service, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	t := time.NewTicker(ProposalOutcomeInterval)
	defer t.Stop()
	for {
		if n, err := ReviewProposalOutcomesOnce(ctx, repo, inv, log); err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "AI 员工提案复盘扫描出错", "err", err)
		} else if n > 0 {
			log.InfoContext(ctx, "AI 员工提案复盘", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---------------------------------------------------------------------------
// 成绩单
// ---------------------------------------------------------------------------

// Scorecard 是一名 AI 员工近 30 天的成绩单。
type Scorecard struct {
	AgentStaffID int64
	Since        time.Time
	Kinds        []repository.ScorecardKind
	Recent       []repository.RecentOutcome
}

const scorecardWindow = 30 * 24 * time.Hour

// Scorecard 实现 GET /admin/agents/{staff_id}/scorecard（全店范围的人看；AI 员工是全店的事）。
func (s *AgentProposalService) Scorecard(ctx context.Context, agentStaffID int64) (Scorecard, error) {
	id, err := requireMerchantWide(ctx)
	if err != nil {
		return Scorecard{}, err
	}
	if id.IsAgent() {
		return Scorecard{}, ErrHumanOnly
	}
	return s.scorecard(ctx, agentStaffID)
}

// MyScorecard 是 MCP 工具 my_scorecard：AI 员工看自己的成绩单（手册要求据此调整）。
func (s *AgentProposalService) MyScorecard(ctx context.Context) (Scorecard, error) {
	id, err := requireAgent(ctx)
	if err != nil {
		return Scorecard{}, err
	}
	return s.scorecard(ctx, id.StaffID)
}

func (s *AgentProposalService) scorecard(ctx context.Context, agentStaffID int64) (Scorecard, error) {
	out := Scorecard{AgentStaffID: agentStaffID, Since: time.Now().Add(-scorecardWindow).UTC()}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err := tx.FindAgent(ctx, agentStaffID); err != nil {
			return err
		}
		var e error
		out.Kinds, out.Recent, e = tx.AgentScorecard(ctx, agentStaffID, out.Since)
		return e
	})
	if errors.Is(err, repository.ErrAgentNotFound) {
		return Scorecard{}, fmt.Errorf("%w: staff_id=%d", ErrAgentNotFound, agentStaffID)
	}
	return out, err
}
