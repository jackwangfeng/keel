package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 提案复盘与成绩单（00122）。

// DueProposalOutcome 是一条到点该量效果的提案。
type DueProposalOutcome struct {
	ID         int64
	Kind       string
	StoreID    *int64
	SKUID      *int64
	Payload    []byte
	ExecutedAt time.Time
}

// ScorecardKind 是成绩单上一种提案的计数。
type ScorecardKind struct {
	Kind                                                    string
	Proposed, Approved, Executed, Failed, Rejected, Expired int64
	Open, Positive, Neutral, Negative                       int64
}

// RecentOutcome 是成绩单明细的一行。
type RecentOutcome struct {
	ID        int64
	Kind      string
	Title     string
	Outcome   []byte
	OutcomeAt time.Time
}

// AgentOutcomeTx 是复盘与成绩单这一面。
type AgentOutcomeTx interface {
	// SetAgentProposalExecuted 在执行成功（status 20）后记下执行时间与何时量效果；outcome 非空即当场写好（不量效果的种类）。
	SetAgentProposalExecuted(ctx context.Context, id int64, dueAt *time.Time, outcome []byte) error
	DueAgentProposalOutcomes(ctx context.Context) ([]DueProposalOutcome, error)
	SaveAgentProposalOutcome(ctx context.Context, id int64, outcome []byte) error
	// DeferAgentProposalOutcome 把一条到点的复盘推迟到 dueAt（统计窗口还没走完）。
	DeferAgentProposalOutcome(ctx context.Context, id int64, dueAt time.Time) error
	SKUUnitsSoldBetween(ctx context.Context, skuIDs []int64, storeID *int64, from, to time.Time) (qty, amountCents int64, err error)
	ProductUnitsSoldBetween(ctx context.Context, productID int64, from, to time.Time) (int64, error)
	CouponTemplateUsage(ctx context.Context, templateID int64) (claimed, used int64, err error)
	AgentScorecard(ctx context.Context, agentStaffID int64, since time.Time) ([]ScorecardKind, []RecentOutcome, error)
}

func tsArg(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func (t tenantTx) SetAgentProposalExecuted(ctx context.Context, id int64, dueAt *time.Time, outcome []byte) error {
	p := db.SetAgentProposalExecutedParams{ID: id, Outcome: outcome}
	if dueAt != nil {
		p.OutcomeDueAt = tsArg(*dueAt)
	}
	return t.q.SetAgentProposalExecuted(ctx, p)
}

func (t tenantTx) DueAgentProposalOutcomes(ctx context.Context) ([]DueProposalOutcome, error) {
	rows, err := t.q.DueAgentProposalOutcomes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]DueProposalOutcome, 0, len(rows))
	for _, r := range rows {
		out = append(out, DueProposalOutcome{ID: r.ID, Kind: r.Kind, StoreID: r.StoreID, SKUID: r.SkuID,
			Payload: r.Payload, ExecutedAt: r.ExecutedAt.Time})
	}
	return out, nil
}

func (t tenantTx) SaveAgentProposalOutcome(ctx context.Context, id int64, outcome []byte) error {
	return t.q.SaveAgentProposalOutcome(ctx, db.SaveAgentProposalOutcomeParams{ID: id, Outcome: outcome})
}

func (t tenantTx) DeferAgentProposalOutcome(ctx context.Context, id int64, dueAt time.Time) error {
	return t.q.DeferAgentProposalOutcome(ctx, db.DeferAgentProposalOutcomeParams{ID: id, DueAt: tsArg(dueAt)})
}

func (t tenantTx) SKUUnitsSoldBetween(ctx context.Context, skuIDs []int64, storeID *int64, from, to time.Time) (int64, int64, error) {
	r, err := t.q.SKUUnitsSoldBetween(ctx, db.SKUUnitsSoldBetweenParams{SkuIds: skuIDs, StoreID: storeID,
		FromAt: tsArg(from), ToAt: tsArg(to)})
	return r.Qty, r.AmountCents, err
}

func (t tenantTx) ProductUnitsSoldBetween(ctx context.Context, productID int64, from, to time.Time) (int64, error) {
	return t.q.ProductUnitsSoldBetween(ctx, db.ProductUnitsSoldBetweenParams{ProductID: productID,
		FromAt: tsArg(from), ToAt: tsArg(to)})
}

func (t tenantTx) CouponTemplateUsage(ctx context.Context, templateID int64) (int64, int64, error) {
	r, err := t.q.CouponTemplateUsage(ctx, templateID)
	return r.Claimed, r.Used, err
}

func (t tenantTx) AgentScorecard(ctx context.Context, agentStaffID int64, since time.Time) ([]ScorecardKind, []RecentOutcome, error) {
	kinds, err := t.q.AgentScorecardByKind(ctx, db.AgentScorecardByKindParams{AgentStaffID: agentStaffID, Since: tsArg(since)})
	if err != nil {
		return nil, nil, err
	}
	recent, err := t.q.AgentRecentOutcomes(ctx, db.AgentRecentOutcomesParams{AgentStaffID: agentStaffID, Since: tsArg(since)})
	if err != nil {
		return nil, nil, err
	}
	ks := make([]ScorecardKind, 0, len(kinds))
	for _, k := range kinds {
		ks = append(ks, ScorecardKind{Kind: k.Kind, Proposed: k.Proposed, Approved: k.Approved, Executed: k.Executed,
			Failed: k.Failed, Rejected: k.Rejected, Expired: k.Expired, Open: k.Open, Positive: k.Positive,
			Neutral: k.Neutral, Negative: k.Negative})
	}
	rs := make([]RecentOutcome, 0, len(recent))
	for _, r := range recent {
		rs = append(rs, RecentOutcome{ID: r.ID, Kind: r.Kind, Title: r.Title, Outcome: r.Outcome, OutcomeAt: r.OutcomeAt.Time})
	}
	return ks, rs, nil
}

// AgentAutoPolicy 是一名 AI 员工对一种提案的自动执行策略（00130）。没配过 = 不自动执行。
type AgentAutoPolicy struct {
	AgentStaffID     int64
	Kind             string
	Enabled          bool
	MaxUnits         int32
	MinDiscountRate  int16
	MaxDiscountCents int64
	DailyLimit       int32
	UpdatedBy        *int64
	UpdatedAt        *time.Time
}

// ErrAutoPolicyNotFound：这名 AI 员工对这种提案没有策略。
var ErrAutoPolicyNotFound = errors.New("没有自动执行策略")

// AgentAutoPolicyTx 是自动执行策略这一面。
type AgentAutoPolicyTx interface {
	ListAgentAutoPolicies(ctx context.Context, agentStaffID int64) ([]AgentAutoPolicy, error)
	FindAgentAutoPolicy(ctx context.Context, agentStaffID int64, kind string) (AgentAutoPolicy, error)
	UpsertAgentAutoPolicy(ctx context.Context, p AgentAutoPolicy) error
	CountAutoApprovedSince(ctx context.Context, agentStaffID int64, kind string, since time.Time) (int64, error)
	// ClaimAgentProposalAuto：10 → 15（自动执行）。没认领到（已被处理 / 过期）返回 ErrProposalNotOpen。
	ClaimAgentProposalAuto(ctx context.Context, id int64) error
}

func (t tenantTx) ListAgentAutoPolicies(ctx context.Context, agentStaffID int64) ([]AgentAutoPolicy, error) {
	rows, err := t.q.ListAgentAutoPolicies(ctx, agentStaffID)
	if err != nil {
		return nil, err
	}
	out := make([]AgentAutoPolicy, 0, len(rows))
	for _, r := range rows {
		out = append(out, AgentAutoPolicy{AgentStaffID: r.AgentStaffID, Kind: r.Kind, Enabled: r.Enabled,
			MaxUnits: r.MaxUnits, MinDiscountRate: r.MinDiscountRate, MaxDiscountCents: r.MaxDiscountCents,
			DailyLimit: r.DailyLimit, UpdatedBy: r.UpdatedBy, UpdatedAt: tsPtr(r.UpdatedAt)})
	}
	return out, nil
}

func (t tenantTx) FindAgentAutoPolicy(ctx context.Context, agentStaffID int64, kind string) (AgentAutoPolicy, error) {
	r, err := t.q.GetAgentAutoPolicy(ctx, db.GetAgentAutoPolicyParams{AgentStaffID: agentStaffID, Kind: kind})
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentAutoPolicy{}, ErrAutoPolicyNotFound
	}
	if err != nil {
		return AgentAutoPolicy{}, err
	}
	return AgentAutoPolicy{AgentStaffID: r.AgentStaffID, Kind: r.Kind, Enabled: r.Enabled, MaxUnits: r.MaxUnits,
		MinDiscountRate: r.MinDiscountRate, MaxDiscountCents: r.MaxDiscountCents, DailyLimit: r.DailyLimit,
		UpdatedBy: r.UpdatedBy, UpdatedAt: tsPtr(r.UpdatedAt)}, nil
}

func (t tenantTx) UpsertAgentAutoPolicy(ctx context.Context, p AgentAutoPolicy) error {
	return t.q.UpsertAgentAutoPolicy(ctx, db.UpsertAgentAutoPolicyParams{AgentStaffID: p.AgentStaffID, Kind: p.Kind,
		Enabled: p.Enabled, MaxUnits: p.MaxUnits, MinDiscountRate: p.MinDiscountRate,
		MaxDiscountCents: p.MaxDiscountCents, DailyLimit: p.DailyLimit, UpdatedBy: p.UpdatedBy})
}

func (t tenantTx) CountAutoApprovedSince(ctx context.Context, agentStaffID int64, kind string, since time.Time) (int64, error) {
	return t.q.CountAutoApprovedSince(ctx, db.CountAutoApprovedSinceParams{AgentStaffID: agentStaffID, Kind: kind,
		DecidedAt: tsArg(since)})
}

func (t tenantTx) ClaimAgentProposalAuto(ctx context.Context, id int64) error {
	_, err := t.q.ClaimAgentProposalAuto(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrProposalNotOpen
	}
	return err
}

// PublicAILogEntry 是公开 AI 经营日志上的一条提案（00132）。Verdict 为空 = 还没复盘。
type PublicAILogEntry struct {
	ID           int64
	Kind         string
	Title        string
	Status       int16
	AutoApproved bool
	Verdict      string
	AgentName    string
	CreatedAt    time.Time
	DecidedAt    *time.Time
}

// PublicAILogSummary 是近 30 天的总数。
type PublicAILogSummary struct {
	Proposed, Executed, AutoExecuted, Rejected, Positive, Negative int64
}

// PublicAILogTx 是公开 AI 经营日志这一面。
type PublicAILogTx interface {
	// PublicAILogEnabled：店铺设置里的开关；没有 shop_preferences 那一行即关。
	PublicAILogEnabled(ctx context.Context) (bool, error)
	SetPublicAILog(ctx context.Context, enabled bool) error
	PublicAILog(ctx context.Context) ([]PublicAILogEntry, PublicAILogSummary, error)
}

func (t tenantTx) PublicAILogEnabled(ctx context.Context) (bool, error) {
	on, err := t.q.GetPublicAILog(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return on, err
}

func (t tenantTx) SetPublicAILog(ctx context.Context, enabled bool) error {
	return t.q.SetPublicAILog(ctx, enabled)
}

func (t tenantTx) PublicAILog(ctx context.Context) ([]PublicAILogEntry, PublicAILogSummary, error) {
	rows, err := t.q.PublicAILogProposals(ctx)
	if err != nil {
		return nil, PublicAILogSummary{}, err
	}
	out := make([]PublicAILogEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, PublicAILogEntry{ID: r.ID, Kind: r.Kind, Title: r.Title, Status: r.Status,
			AutoApproved: r.AutoApproved, Verdict: r.Verdict, AgentName: r.AgentName, CreatedAt: r.CreatedAt.Time,
			DecidedAt: tsPtr(r.DecidedAt)})
	}
	s, err := t.q.PublicAILogSummary(ctx)
	if err != nil {
		return nil, PublicAILogSummary{}, err
	}
	return out, PublicAILogSummary{Proposed: s.Proposed, Executed: s.Executed, AutoExecuted: s.AutoExecuted,
		Rejected: s.Rejected, Positive: s.Positive, Negative: s.Negative}, nil
}
