package service

// 提案种类 channel_stock_rule：AI 员工提「把一个销售渠道在一家门店的库存分配规则改成这组值」
// （docs/superpowers/specs/2026-10-03-ai-channel-allocation-design.md §4.2–4.5）。
//
//   - 依据核对两次：每条改动带着 AI 看到的当前生效规则（prev）。提案时不一致 → ErrProposalStale（409，AI 看的是旧数，
//     让它重调 channel_allocation_review）；执行时在同一个事务里再核一次，不一致 → 整条执行失败、结果写清是哪一格，
//     规则保持人改过的值（Review Focus 2）。
//   - 试算：提案时按当时的 keel 可售算每格对外可售数从几变成几，写在 payload.preview 里（后台详情直接展示；
//     批的时候可售可能已经变了，所以叫「提案时的试算」，执行结果不重算）。
//   - 执行：逐条 upsert 门店级 / 门店 × SKU 级的规则，入队这一格（binding × 门店）的重算推送 —— 与后台改规则
//     （ChannelService.UpsertStockRule）同一条链路。重放时规则已经是目标值就当成功（幂等）。
//   - 复盘：执行前后各 7 天，每格比挂零 / 缺货拒单 / 卖出 / 渠道净收入，判据见 channelStockRuleVerdict。
//
// 渠道层关着（s.channels 为 nil）时提案拒收（ErrChannelsDisabled → 409），执行失败。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/repository"
)

const (
	ProposalKindChannelStockRule = "channel_stock_rule"

	channelRuleMaxChanges = 20
	channelRuleMaxSafety  = 100000
	channelPreviewMax     = 40
	// 复盘：窗口里 keel 自己没货（挂零时段 held = false）超过这么多小时的格子不计 —— 缺的是货，不是分配。
	channelOutcomeStockoutHours = 48
)

var (
	// ErrProposalStale：提案依据的规则（prev）与当前生效的不一致。契约 409。
	ErrProposalStale = errors.New("提案依据的规则已经变了")
	// ErrChannelsDisabled：渠道层没开（KEEL_CHANNELS 关着），渠道类提案不收。契约 409。
	ErrChannelsDisabled = errors.New("没有启用的销售渠道（渠道层没开）")
)

// SetChannels 接上渠道层（装配时调用；KEEL_CHANNELS 关着时不调，channel_stock_rule 提案拒收）。
func (s *AgentProposalService) SetChannels(ch *ChannelService) { s.channels = ch }

// ChannelRuleSnapshot 是一格当前生效的规则（channel.ResolveStockRuleLevel 的结果）。Level：binding | store | sku | default。
type ChannelRuleSnapshot struct {
	RatioBP   int32  `json:"ratio_bp"`
	SafetyQty int32  `json:"safety_qty"`
	CapQty    *int32 `json:"cap_qty,omitempty"`
	Level     string `json:"level"`
}

func (a ChannelRuleSnapshot) sameValues(ratio, safety int32, capQty *int32) bool {
	return a.RatioBP == ratio && a.SafetyQty == safety && equalInt32Ptr(a.CapQty, capQty)
}

func (a ChannelRuleSnapshot) String() string {
	c := "不封顶"
	if a.CapQty != nil {
		c = fmt.Sprintf("封顶 %d", *a.CapQty)
	}
	return fmt.Sprintf("比例 %s、安全库存 %d、%s（%s 级）", bpPercent(a.RatioBP), a.SafetyQty, c, a.Level)
}

// ChannelStockRuleChange 是一条改动：SKUID 为空 = 门店级（这家门店在这个渠道上的默认分配）。
type ChannelStockRuleChange struct {
	SKUID     *int64              `json:"sku_id,omitempty"`
	RatioBP   int32               `json:"ratio_bp"`
	SafetyQty int32               `json:"safety_qty"`
	CapQty    *int32              `json:"cap_qty,omitempty"`
	Prev      ChannelRuleSnapshot `json:"prev"`
}

func (c ChannelStockRuleChange) cell() string {
	if c.SKUID == nil {
		return "门店级"
	}
	return "SKU " + strconv.FormatInt(*c.SKUID, 10)
}

// targetLevel 是这条改动写进去之后那一格的生效级别。
func (c ChannelStockRuleChange) targetLevel() string {
	if c.SKUID == nil {
		return channel.RuleLevelStore
	}
	return channel.RuleLevelSKU
}

// ChannelStockPreview 是提案时的试算：这个 SKU 在这个渠道上的对外可售数，按当时的 keel 可售从 BeforeQty 变成 AfterQty。
type ChannelStockPreview struct {
	SKUID     int64 `json:"sku_id"`
	Available int32 `json:"available"`
	BeforeQty int32 `json:"before_qty"`
	AfterQty  int32 `json:"after_qty"`
}

// ChannelStockRulePayload 是 channel_stock_rule 提案的执行参数。BindingName / StoreName / Preview 由 keel 在提案时填。
type ChannelStockRulePayload struct {
	BindingID   int64                    `json:"binding_id"`
	BindingName string                   `json:"binding_name,omitempty"`
	StoreID     int64                    `json:"store_id"`
	StoreName   string                   `json:"store_name,omitempty"`
	Changes     []ChannelStockRuleChange `json:"changes"`
	Preview     []ChannelStockPreview    `json:"preview,omitempty"`
}

// ruleSnapshot 是 (store, sku) 一格当前生效的规则；skuID 为空时只看渠道级与门店级（门店级改动的那一格）。
func ruleSnapshot(rules []channel.StockRule, storeID int64, skuID *int64) ChannelRuleSnapshot {
	var sku int64 // 0 不会命中任何门店 × SKU 级规则
	if skuID != nil {
		sku = *skuID
	}
	r, level := channel.ResolveStockRuleLevel(rules, storeID, sku)
	return ChannelRuleSnapshot{RatioBP: r.RatioBP, SafetyQty: r.SafetyQty, CapQty: r.CapQty, Level: level}
}

// applyChanges 返回把改动写进去之后的规则表（试算用）。
func applyChanges(rules []channel.StockRule, storeID int64, changes []ChannelStockRuleChange) []channel.StockRule {
	out := slices.Clone(rules)
	for _, c := range changes {
		nr := channel.StockRule{StoreID: &storeID, SKUID: c.SKUID, RatioBP: c.RatioBP, SafetyQty: c.SafetyQty, CapQty: c.CapQty}
		replaced := false
		for i, r := range out {
			if r.StoreID != nil && *r.StoreID == storeID && equalInt64Ptr(r.SKUID, c.SKUID) {
				out[i], replaced = nr, true
				break
			}
		}
		if !replaced {
			out = append(out, nr)
		}
	}
	return out
}

// channelTarget 核对 binding 是启用中的销售渠道、且映射了这家门店；返回它。
func channelTarget(ctx context.Context, tx repository.Tx, bindingID, storeID int64) (repository.OutletBinding, error) {
	outs, err := tx.ListActiveOutletBindingsForStore(ctx, storeID)
	if err != nil {
		return repository.OutletBinding{}, err
	}
	for _, b := range outs {
		if b.ID == bindingID {
			return b, nil
		}
	}
	b, err := tx.GetChannelBinding(ctx, bindingID)
	switch {
	case errors.Is(err, repository.ErrChannelNotFound):
		return repository.OutletBinding{}, badProposal("binding_id=%d 不存在", bindingID)
	case err != nil:
		return repository.OutletBinding{}, err
	case b.Status != repository.ChannelBindingActive:
		return repository.OutletBinding{}, badProposal("渠道「%s」没在启用中", b.Name)
	case b.Roles&repository.ChannelRoleOutlet == 0:
		return repository.OutletBinding{}, badProposal("渠道「%s」不是销售渠道", b.Name)
	}
	return repository.OutletBinding{}, badProposal("门店 %d 没有映射到渠道「%s」", storeID, b.Name)
}

func checkChannelChanges(changes []ChannelStockRuleChange) error {
	if len(changes) < 1 || len(changes) > channelRuleMaxChanges {
		return badProposal("changes 取 1–%d 条", channelRuleMaxChanges)
	}
	seen := map[int64]bool{}
	for _, c := range changes {
		key := int64(0)
		if c.SKUID != nil {
			if *c.SKUID <= 0 {
				return badProposal("sku_id 要 > 0（门店级不给 sku_id）")
			}
			key = *c.SKUID
		}
		if seen[key] {
			return badProposal("%s 出现了两次", c.cell())
		}
		seen[key] = true
		switch {
		case c.RatioBP < 0 || c.RatioBP > 10000:
			return badProposal("%s：ratio_bp 取 0–10000（万分比）", c.cell())
		case c.SafetyQty < 0 || c.SafetyQty > channelRuleMaxSafety:
			return badProposal("%s：safety_qty 取 0–%d", c.cell(), channelRuleMaxSafety)
		case c.CapQty != nil && *c.CapQty < 0:
			return badProposal("%s：cap_qty 不给（不封顶）或 ≥ 0", c.cell())
		}
		switch c.Prev.Level {
		case channel.RuleLevelBinding, channel.RuleLevelStore, channel.RuleLevelSKU, channel.RuleLevelDefault:
		default:
			return badProposal("%s：prev.level 取 binding / store / sku / default（照抄 channel_allocation_review 的 rule_level）", c.cell())
		}
	}
	return nil
}

// ProposeChannelStockRule 是 MCP 工具 propose_channel_stock_rule。渠道分配是全店的事：要全店范围的 AI 员工。
func (s *AgentProposalService) ProposeChannelStockRule(ctx context.Context, pl ChannelStockRulePayload,
	meta ProposalMeta) (repository.AgentProposal, error) {
	id, err := requireAgent(ctx)
	if err != nil {
		return repository.AgentProposal{}, err
	}
	if _, err := requireMerchantWide(ctx); err != nil {
		return repository.AgentProposal{}, err
	}
	if s.channels == nil {
		return repository.AgentProposal{}, ErrChannelsDisabled
	}
	if err := checkChannelChanges(pl.Changes); err != nil {
		return repository.AgentProposal{}, err
	}
	if err := meta.check(); err != nil {
		return repository.AgentProposal{}, err
	}

	// 一、核对目标与依据，挑出要试算的 SKU（事务里；读库存不在 core 事务里）。
	var (
		rules   []channel.StockRule
		preview []int64
	)
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		b, err := channelTarget(ctx, tx, pl.BindingID, pl.StoreID)
		if err != nil {
			return err
		}
		st, err := tx.FindStore(ctx, pl.StoreID)
		if err != nil {
			return err
		}
		pl.BindingName, pl.StoreName = b.Name, st.Name
		rs, err := tx.ChannelStockRulesForStore(ctx, pl.BindingID, pl.StoreID)
		if err != nil {
			return err
		}
		rules = toStockRules(rs)
		storeLevel := false
		for _, c := range pl.Changes {
			if c.SKUID == nil {
				storeLevel = true
			} else {
				if _, err := tx.AdminFindSKU(ctx, *c.SKUID); err != nil {
					return badProposal("sku %d 不存在", *c.SKUID)
				}
				preview = append(preview, *c.SKUID)
			}
			cur := ruleSnapshot(rules, pl.StoreID, c.SKUID)
			if cur.Level != c.Prev.Level || !cur.sameValues(c.Prev.RatioBP, c.Prev.SafetyQty, c.Prev.CapQty) {
				return fmt.Errorf("%w：%s 现在是 %s，不是提案里写的 %s —— 重新调 channel_allocation_review 拿最新的规则",
					ErrProposalStale, c.cell(), cur, c.Prev)
			}
			if cur.sameValues(c.RatioBP, c.SafetyQty, c.CapQty) {
				return badProposal("%s 的比例、安全库存、封顶都没变，没有改动", c.cell())
			}
		}
		// 门店级的改动影响这家门店在这个渠道上没有单独规则的 SKU：拿已推过的格子试算（至多 20 个）。
		if storeLevel {
			ls, err := tx.ListChannelListingsPage(ctx, pl.BindingID, &pl.StoreID, false, 20, 0)
			if err != nil {
				return err
			}
			for _, l := range ls {
				if !slices.Contains(preview, l.SKUID) && len(preview) < channelPreviewMax {
					preview = append(preview, l.SKUID)
				}
			}
		}
		return nil
	})
	if err != nil {
		return repository.AgentProposal{}, mapProposalErr(err)
	}

	// 二、试算（事务外读 keel 可售）。
	if len(preview) > 0 {
		levels, err := s.inv.StoreStock(ctx, pl.StoreID, preview)
		if err != nil {
			return repository.AgentProposal{}, err
		}
		after := applyChanges(rules, pl.StoreID, pl.Changes)
		for _, sku := range preview {
			avail := levels[sku].Available
			pl.Preview = append(pl.Preview, ChannelStockPreview{SKUID: sku, Available: avail,
				BeforeQty: channel.PublishedQty(avail, channel.ResolveStockRule(rules, pl.StoreID, sku)),
				AfterQty:  channel.PublishedQty(avail, channel.ResolveStockRule(after, pl.StoreID, sku))})
		}
	}

	// 三、写提案。去重键：同一 binding × 门店 × 同一组格子。
	keys := make([]string, 0, len(pl.Changes))
	ids := []int64{}
	for _, c := range pl.Changes {
		if c.SKUID == nil {
			keys = append(keys, "store")
		} else {
			ids = append(ids, *c.SKUID)
		}
	}
	slices.Sort(ids)
	if len(ids) > 0 {
		keys = append(keys, joinIDs(ids))
	}
	key := fmt.Sprintf("chstock:%d:%d:%s", pl.BindingID, pl.StoreID, strings.Join(keys, ","))
	payload, err := json.Marshal(pl)
	if err != nil {
		return repository.AgentProposal{}, err
	}
	var out repository.AgentProposal
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out, e = insertProposal(ctx, tx, repository.NewAgentProposal{AgentStaffID: id.StaffID,
			Kind: ProposalKindChannelStockRule, TargetKey: key, Payload: payload, Title: truncRunes(channelRuleTitle(pl), 200),
			Evidence: strings.TrimSpace(meta.Evidence), ExpectedImpact: strings.TrimSpace(meta.ExpectedImpact)})
		return e
	})
	return s.afterPropose(ctx, out, mapProposalErr(err))
}

// channelRuleTitle：「Shopify 开发店 · 示例小店：3 个商品调分配（比例 80%→90%…）」。
func channelRuleTitle(pl ChannelStockRulePayload) string {
	what := fmt.Sprintf("%d 个商品调分配", len(pl.Changes))
	if len(pl.Changes) == 1 && pl.Changes[0].SKUID == nil {
		what = "门店级调分配"
	}
	c := pl.Changes[0]
	var d string
	switch {
	case c.RatioBP != c.Prev.RatioBP:
		d = fmt.Sprintf("比例 %s→%s", bpPercent(c.Prev.RatioBP), bpPercent(c.RatioBP))
	case c.SafetyQty != c.Prev.SafetyQty:
		d = fmt.Sprintf("安全库存 %d→%d", c.Prev.SafetyQty, c.SafetyQty)
	default:
		d = "封顶 " + capText(c.Prev.CapQty) + "→" + capText(c.CapQty)
	}
	if len(pl.Changes) > 1 {
		d += "…"
	}
	return fmt.Sprintf("%s · %s：%s（%s）", pl.BindingName, pl.StoreName, what, d)
}

func capText(c *int32) string {
	if c == nil {
		return "不封顶"
	}
	return strconv.Itoa(int(*c))
}

// bpPercent：8000 → "80%"，8050 → "80.5%"。
func bpPercent(bp int32) string {
	return strconv.FormatFloat(float64(bp)/100, 'f', -1, 64) + "%"
}

func equalInt32Ptr(a, b *int32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func equalInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// execChannelStockRule 以 AI 员工的身份执行：同一事务里先拿 binding 的规则锁（与后台改 / 删规则串行，
// 否则 READ COMMITTED 下核对 prev 与 upsert 之间提交的人工修改会被覆盖），再逐条核对 prev → upsert → 入队重算推送。
// 有一条被人改过 → 整条失败（一条都不写），结果说明是哪一格；已经是目标值的条目算已生效（重放幂等）。
func (s *AgentProposalService) execChannelStockRule(ctx context.Context, p repository.AgentProposal) (ProposalResult, error) {
	if s.channels == nil {
		return ProposalResult{}, ErrChannelsDisabled
	}
	if _, err := requireMerchantWide(ctx); err != nil { // 提案之后它的范围可能被收窄了
		return ProposalResult{}, err
	}
	var pl ChannelStockRulePayload
	if err := json.Unmarshal(p.Payload, &pl); err != nil {
		return ProposalResult{}, err
	}
	applied, already := 0, 0
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if err := tx.LockChannelBindingRules(ctx, pl.BindingID); err != nil {
			if errors.Is(err, repository.ErrChannelNotFound) {
				return badProposal("binding_id=%d 不存在", pl.BindingID)
			}
			return err
		}
		if _, err := channelTarget(ctx, tx, pl.BindingID, pl.StoreID); err != nil {
			return err
		}
		rs, err := tx.ChannelStockRulesForStore(ctx, pl.BindingID, pl.StoreID)
		if err != nil {
			return err
		}
		rules := toStockRules(rs)
		var todo []ChannelStockRuleChange
		for _, c := range pl.Changes {
			cur := ruleSnapshot(rules, pl.StoreID, c.SKUID)
			if cur.Level == c.targetLevel() && cur.sameValues(c.RatioBP, c.SafetyQty, c.CapQty) {
				already++
				continue
			}
			if cur.Level != c.Prev.Level || !cur.sameValues(c.Prev.RatioBP, c.Prev.SafetyQty, c.Prev.CapQty) {
				return fmt.Errorf("%w：%s 提案时是 %s，现在是 %s —— 有人改过，不覆盖人的修改（整条提案没有执行）",
					ErrProposalStale, c.cell(), c.Prev, cur)
			}
			todo = append(todo, c)
		}
		for _, c := range todo {
			storeID := pl.StoreID
			if _, err := tx.UpsertChannelStockRule(ctx, repository.ChannelStockRule{BindingID: pl.BindingID, StoreID: &storeID,
				SKUID: c.SKUID, RatioBP: c.RatioBP, SafetyQty: c.SafetyQty, CapQty: c.CapQty}); err != nil {
				return err
			}
			applied++
		}
		if applied == 0 {
			return nil
		}
		return s.channels.enqueueRecompute(ctx, tx, pl.BindingID, pl.StoreID)
	})
	if err != nil {
		return ProposalResult{}, err
	}
	return ProposalResult{Detail: map[string]any{"binding_id": pl.BindingID, "store_id": pl.StoreID,
		"applied": applied, "already": already}}, nil
}

// withinChannelPolicy：每条改动比例变化 ≤ max_ratio_step_bp、安全库存变化 ≤ max_units、封顶不变，且不把比例调到 0。
func withinChannelPolicy(pol repository.AgentAutoPolicy, p repository.AgentProposal) bool {
	if pol.MaxRatioStepBP <= 0 {
		return false
	}
	var pl ChannelStockRulePayload
	if json.Unmarshal(p.Payload, &pl) != nil || len(pl.Changes) == 0 {
		return false
	}
	for _, c := range pl.Changes {
		if c.RatioBP == 0 || absInt32(c.RatioBP-c.Prev.RatioBP) > pol.MaxRatioStepBP ||
			absInt32(c.SafetyQty-c.Prev.SafetyQty) > pol.MaxUnits || !equalInt32Ptr(c.CapQty, c.Prev.CapQty) {
			return false
		}
	}
	return true
}

func absInt32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// ---------------------------------------------------------------------------
// 复盘
// ---------------------------------------------------------------------------

// ChannelCellFacts 是一格（binding × 门店 × SKU）在一个窗口里的事实。NetCents = 卖出 × 单件净收入（按复盘时的价格、
// 佣金率与成本价算；成本价没填不减）。
type ChannelCellFacts struct {
	HeldZeroHours   float64 `json:"held_zero_hours"`
	EmptyZeroHours  float64 `json:"empty_zero_hours"`
	StockoutRejects int64   `json:"stockout_rejects"`
	Sold            int64   `json:"sold"`
	NetCents        int64   `json:"net_cents"`
}

// ChannelOutcomeCell 是复盘里的一格。Direction：up（这格对外放得更多）/ down / same；ExcludedReason 非空 = 不计。
type ChannelOutcomeCell struct {
	BindingID      int64            `json:"binding_id"`
	StoreID        int64            `json:"store_id"`
	SKUID          int64            `json:"sku_id"`
	Direction      string           `json:"direction"`
	Before         ChannelCellFacts `json:"before"`
	After          ChannelCellFacts `json:"after"`
	ExcludedReason string           `json:"excluded_reason,omitempty"`
}

// changeDirection：比例上调 → up；比例不变时看安全库存（降 = up），再看封顶（放宽 = up）。
func changeDirection(c ChannelStockRuleChange) string {
	switch {
	case c.RatioBP > c.Prev.RatioBP:
		return "up"
	case c.RatioBP < c.Prev.RatioBP:
		return "down"
	case c.SafetyQty < c.Prev.SafetyQty:
		return "up"
	case c.SafetyQty > c.Prev.SafetyQty:
		return "down"
	}
	switch {
	case equalInt32Ptr(c.CapQty, c.Prev.CapQty):
		return "same"
	case c.CapQty == nil || (c.Prev.CapQty != nil && *c.CapQty > *c.Prev.CapQty):
		return "up"
	}
	return "down"
}

// channelCellFacts 在事务里算 binding 在这家门店这批 SKU 上 [from, to) 的事实（与 AllocationReviewAt 同一组查询，
// 窗口显式给定）。单件净收入要的渠道价按复盘时的门店价与价格规则算。
func channelCellFacts(ctx context.Context, tx repository.Tx, bindingID, storeID int64, skuIDs []int64,
	from, to time.Time) (map[int64]ChannelCellFacts, error) {
	out := make(map[int64]ChannelCellFacts, len(skuIDs))
	sold, err := tx.ChannelSoldBySource(ctx, storeID, from, to)
	if err != nil {
		return nil, err
	}
	rejects, err := tx.ChannelStockoutRejects(ctx, storeID, from, to)
	if err != nil {
		return nil, err
	}
	zero, err := tx.ChannelZeroHours(ctx, storeID, skuIDs, from, to)
	if err != nil {
		return nil, err
	}
	unit, err := channelUnitNet(ctx, tx, bindingID, storeID, skuIDs)
	if err != nil {
		return nil, err
	}
	for _, sku := range skuIDs {
		f := ChannelCellFacts{}
		for _, r := range sold {
			if r.BindingID == bindingID && r.SKUID == sku {
				f.Sold = r.Qty
			}
		}
		for _, r := range rejects {
			if r.BindingID == bindingID && r.SKUID == sku {
				f.StockoutRejects = r.Qty
			}
		}
		for _, z := range zero {
			if z.BindingID == bindingID && z.SKUID == sku {
				f.HeldZeroHours, f.EmptyZeroHours = round2(z.HeldHours), round2(z.EmptyHours)
			}
		}
		f.NetCents = f.Sold * unit[sku]
		out[sku] = f
	}
	return out, nil
}

// channelUnitNet：单件净收入 = 渠道价 × (1 − 佣金率) − 成本价（与 channel_allocation_review 同一算法）。
func channelUnitNet(ctx context.Context, tx repository.Tx, bindingID, storeID int64, skuIDs []int64) (map[int64]int64, error) {
	b, err := tx.GetChannelBinding(ctx, bindingID)
	if err != nil {
		return nil, err
	}
	offers, err := tx.ChannelSKUOffers(ctx, storeID, skuIDs)
	if err != nil {
		return nil, err
	}
	prs, err := tx.ListChannelPriceRules(ctx, bindingID)
	if err != nil {
		return nil, err
	}
	infos, err := tx.ChannelAllocationSKUs(ctx, skuIDs)
	if err != nil {
		return nil, err
	}
	cost := map[int64]int64{}
	for _, k := range infos {
		cost[k.SKUID] = k.CostCents
	}
	prules, commission := toPriceRules(prs), parseBindingConfig(b.Config).CommissionBP
	out := map[int64]int64{}
	for _, sku := range skuIDs {
		o, ok := offers[sku]
		if !ok {
			continue
		}
		price := channel.PublishedPrice(o.PriceCents, channel.ResolvePriceRule(prules, sku))
		out[sku] = allocationUnitNet(price, commission, cost[sku])
	}
	return out, nil
}

// channelStockRuleOutcome 量一条 channel_stock_rule 提案：执行前 7 天 vs 执行后 7 天，逐格。
// 门店级改动的格子取提案时试算过的 SKU（没有单独改的那些）。
func channelStockRuleOutcome(ctx context.Context, tx repository.Tx, d repository.DueProposalOutcome) (ProposalOutcome, error) {
	var pl ChannelStockRulePayload
	if err := json.Unmarshal(d.Payload, &pl); err != nil {
		return ProposalOutcome{}, err
	}
	dir := map[int64]string{}
	var skus []int64
	var storeLevel *ChannelStockRuleChange
	for i, c := range pl.Changes {
		if c.SKUID == nil {
			storeLevel = &pl.Changes[i]
			continue
		}
		dir[*c.SKUID] = changeDirection(c)
		skus = append(skus, *c.SKUID)
	}
	if storeLevel != nil {
		for _, pv := range pl.Preview {
			if _, ok := dir[pv.SKUID]; !ok {
				dir[pv.SKUID] = changeDirection(*storeLevel)
				skus = append(skus, pv.SKUID)
			}
		}
	}
	ex := d.ExecutedAt
	day := func(t time.Time) string { return t.Format(time.RFC3339) }
	o := ProposalOutcome{WindowStart: day(ex), WindowEnd: day(ex.Add(outcomeWindow)), ComparisonStart: day(ex.Add(-outcomeWindow))}
	if len(skus) == 0 {
		o.Verdict, o.Explanation, o.Cells = verdictNeutral, "没有可比的格子（门店级改动时这家门店在这个渠道上还没推过货）", []ChannelOutcomeCell{}
		return o, nil
	}
	before, err := channelCellFacts(ctx, tx, pl.BindingID, pl.StoreID, skus, ex.Add(-outcomeWindow), ex)
	if err != nil {
		return ProposalOutcome{}, err
	}
	after, err := channelCellFacts(ctx, tx, pl.BindingID, pl.StoreID, skus, ex, ex.Add(outcomeWindow))
	if err != nil {
		return ProposalOutcome{}, err
	}
	for _, sku := range skus {
		c := ChannelOutcomeCell{BindingID: pl.BindingID, StoreID: pl.StoreID, SKUID: sku, Direction: dir[sku],
			Before: before[sku], After: after[sku]}
		if c.Before.EmptyZeroHours > channelOutcomeStockoutHours || c.After.EmptyZeroHours > channelOutcomeStockoutHours {
			c.ExcludedReason = "窗口里 keel 自己断货超过 2 天：缺的是货，不是分配"
		}
		o.Cells = append(o.Cells, c)
	}
	o.Verdict, o.Explanation = channelStockRuleVerdict(o.Cells)
	return o, nil
}

// channelStockRuleVerdict 是复盘判据（spec §4.5），纯函数。不计的格子（ExcludedReason 非空）不参与。
//
//	negative：任一格缺货拒单增加；或被下调的格子挂零（keel 有货、规则算 0）多了 24 小时以上且卖出下降。
//	positive：缺货拒单都没增加，且有被上调的格子挂零减少 ≥ 30% 或卖出增加 ≥ 20%。
//	其余 neutral；全部格子都不计 → neutral 并写明原因。
func channelStockRuleVerdict(cells []ChannelOutcomeCell) (string, string) {
	counted := 0
	positive := false
	for _, c := range cells {
		if c.ExcludedReason != "" {
			continue
		}
		counted++
		b, a := c.Before, c.After
		if a.StockoutRejects > b.StockoutRejects {
			return verdictNegative, fmt.Sprintf("SKU %d 的缺货拒单从 %d 件增加到 %d 件", c.SKUID, b.StockoutRejects, a.StockoutRejects)
		}
		if c.Direction == "down" && a.HeldZeroHours-b.HeldZeroHours > 24 && a.Sold < b.Sold {
			return verdictNegative, fmt.Sprintf("SKU %d 下调后挂零从 %.0f 小时增加到 %.0f 小时，卖出从 %d 件降到 %d 件",
				c.SKUID, b.HeldZeroHours, a.HeldZeroHours, b.Sold, a.Sold)
		}
		if c.Direction == "up" {
			heldDown := b.HeldZeroHours > 0 && a.HeldZeroHours <= b.HeldZeroHours*0.7
			soldUp := (b.Sold == 0 && a.Sold > 0) || (b.Sold > 0 && float64(a.Sold) >= float64(b.Sold)*1.2)
			positive = positive || heldDown || soldUp
		}
	}
	switch {
	case counted == 0:
		return verdictNeutral, "所有格子在窗口里 keel 自己都断货超过 2 天：缺的是货，不是分配，不下结论"
	case positive:
		return verdictPositive, "缺货拒单没增加，且被上调的渠道挂零减少三成以上或卖出增加两成以上"
	}
	return verdictNeutral, "缺货拒单没增加，但挂零与卖出没有明显变化"
}
