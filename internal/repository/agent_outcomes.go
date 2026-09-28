package repository

import (
	"context"
	"time"

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
