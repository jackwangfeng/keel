package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// 自动执行策略（AI 经营 M11，docs/AI经营-M10M11设计.md §6，00130）。
//
// 店长按 AI 员工 × 提案种类放开：命中策略、且在单笔上限与今天的条数上限之内的提案，写入后当场以 AI 员工身份执行，
// 与人批准走同一段执行代码（runClaimed）。超限的一律照常进待处理队列 —— 策略只会让事情更快，不会让没权的事发生：
// 执行时照样按 AI 员工的身份与范围判权，失败照样记成执行失败。售后审核不许自动执行（资金动作）。

// ErrAutoPolicyBadRequest：策略参数不成立。契约 422。
var ErrAutoPolicyBadRequest = errors.New("自动执行策略不成立")

// autoKinds 是能自动执行的种类。
var autoKinds = map[string]bool{ProposalKindInventoryAdjust: true, ProposalKindFlashPrice: true,
	ProposalKindCoupon: true, ProposalKindProductCopy: true}

// ListAutoPolicies 实现 GET /admin/agents/{staff_id}/auto-policies：四种都列出来，没配过的给默认（关）。
func (s *AgentProposalService) ListAutoPolicies(ctx context.Context, agentStaffID int64) ([]repository.AgentAutoPolicy, error) {
	if _, err := requireShopAdmin(ctx); err != nil {
		return nil, err
	}
	var out []repository.AgentAutoPolicy
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err := tx.FindAgent(ctx, agentStaffID); err != nil {
			return err
		}
		got, err := tx.ListAgentAutoPolicies(ctx, agentStaffID)
		if err != nil {
			return err
		}
		have := map[string]repository.AgentAutoPolicy{}
		for _, p := range got {
			have[p.Kind] = p
		}
		for _, k := range []string{ProposalKindInventoryAdjust, ProposalKindFlashPrice, ProposalKindCoupon, ProposalKindProductCopy} {
			if p, ok := have[k]; ok {
				out = append(out, p)
			} else {
				out = append(out, repository.AgentAutoPolicy{AgentStaffID: agentStaffID, Kind: k, MinDiscountRate: 1000})
			}
		}
		return nil
	})
	if errors.Is(err, repository.ErrAgentNotFound) {
		return nil, fmt.Errorf("%w: staff_id=%d", ErrAgentNotFound, agentStaffID)
	}
	return out, err
}

// PutAutoPolicy 实现 PUT /admin/agents/{staff_id}/auto-policies/{kind}。只有本店管理员能放手。
func (s *AgentProposalService) PutAutoPolicy(ctx context.Context, p repository.AgentAutoPolicy) (repository.AgentAutoPolicy, error) {
	id, err := requireShopAdmin(ctx)
	if err != nil {
		return repository.AgentAutoPolicy{}, err
	}
	if !autoKinds[p.Kind] {
		return repository.AgentAutoPolicy{}, fmt.Errorf("%w: kind 取 inventory_adjust / flash_price / coupon / product_copy"+
			"（售后审核不许自动执行）", ErrAutoPolicyBadRequest)
	}
	switch {
	case p.MaxUnits < 0 || p.MaxUnits > proposalMaxDelta:
		return repository.AgentAutoPolicy{}, fmt.Errorf("%w: max_units 取 0–%d", ErrAutoPolicyBadRequest, proposalMaxDelta)
	case p.MinDiscountRate < flashMinRate || p.MinDiscountRate > 1000:
		return repository.AgentAutoPolicy{}, fmt.Errorf("%w: min_discount_rate 取 %d–1000", ErrAutoPolicyBadRequest, flashMinRate)
	case p.MaxDiscountCents < 0 || p.MaxDiscountCents > couponMaxDiscount:
		return repository.AgentAutoPolicy{}, fmt.Errorf("%w: max_discount_cents 取 0–%d", ErrAutoPolicyBadRequest, couponMaxDiscount)
	case p.DailyLimit < 0 || p.DailyLimit > 100:
		return repository.AgentAutoPolicy{}, fmt.Errorf("%w: daily_limit 取 0–100", ErrAutoPolicyBadRequest)
	}
	by := id.StaffID
	p.UpdatedBy = &by
	var out repository.AgentAutoPolicy
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err := tx.FindAgent(ctx, p.AgentStaffID); err != nil {
			return err
		}
		if err := tx.UpsertAgentAutoPolicy(ctx, p); err != nil {
			return err
		}
		var e error
		out, e = tx.FindAgentAutoPolicy(ctx, p.AgentStaffID, p.Kind)
		return e
	})
	if errors.Is(err, repository.ErrAgentNotFound) {
		return repository.AgentAutoPolicy{}, fmt.Errorf("%w: staff_id=%d", ErrAgentNotFound, p.AgentStaffID)
	}
	return out, err
}

// withinPolicy：这条提案在不在策略的单笔上限内。
func withinPolicy(pol repository.AgentAutoPolicy, p repository.AgentProposal) bool {
	switch p.Kind {
	case ProposalKindInventoryAdjust:
		var pl InventoryAdjustPayload
		return json.Unmarshal(p.Payload, &pl) == nil && pl.Delta <= pol.MaxUnits
	case ProposalKindFlashPrice:
		var pl FlashPricePayload
		if json.Unmarshal(p.Payload, &pl) != nil {
			return false
		}
		for _, it := range pl.Items {
			if it.DiscountRate < pol.MinDiscountRate {
				return false
			}
		}
		return true
	case ProposalKindCoupon:
		var pl CouponPayload
		if json.Unmarshal(p.Payload, &pl) != nil {
			return false
		}
		face := pl.DiscountCents
		if pl.CouponType == 2 {
			face = pl.MaxDiscountCents
		}
		return face <= pol.MaxDiscountCents
	case ProposalKindProductCopy:
		return true
	}
	return false
}

// afterPropose：提案写入之后，看策略要不要当场执行。策略不命中、超限、今天的额度用完都原样返回（进待处理）；
// 命中则认领并执行，返回执行后的提案（状态 20 / 40）。任何一步出错都不影响「提案已经提了」这件事：只记日志。
func (s *AgentProposalService) afterPropose(ctx context.Context, p repository.AgentProposal,
	err error) (repository.AgentProposal, error) {
	if err != nil || !autoKinds[p.Kind] {
		return p, err
	}
	agent, e := requireAgent(ctx)
	if e != nil {
		return p, nil
	}
	claimed := false
	e = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		pol, err := tx.FindAgentAutoPolicy(ctx, p.AgentStaffID, p.Kind)
		if errors.Is(err, repository.ErrAutoPolicyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !pol.Enabled || pol.DailyLimit <= 0 || !withinPolicy(pol, p) {
			return nil
		}
		n, err := tx.CountAutoApprovedSince(ctx, p.AgentStaffID, p.Kind, time.Now().Add(-24*time.Hour))
		if err != nil {
			return err
		}
		if n >= int64(pol.DailyLimit) {
			return nil
		}
		if err := tx.ClaimAgentProposalAuto(ctx, p.ID); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	if e != nil {
		s.log.ErrorContext(ctx, "自动执行策略判定出错，提案留在待处理", "proposal_id", p.ID, "err", e)
		return p, nil
	}
	if !claimed {
		return p, nil
	}
	out, e := s.runClaimed(ctx, p, agent, agent.Status == auth.StaffStatusActive)
	if e != nil {
		// 基础设施错误：提案停在 15，人可以在后台再点批准（同一个幂等键）重试。
		s.log.ErrorContext(ctx, "自动执行出错，提案停在执行中", "proposal_id", p.ID, "err", e)
		return p, nil
	}
	return out, nil
}
