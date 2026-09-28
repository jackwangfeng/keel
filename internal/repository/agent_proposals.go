package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// AI 员工的提案（00091，AI 经营 M9 任务 4）。状态机与判权在 service/agent_proposal.go。

const (
	ProposalPending   int16 = 10
	ProposalExecuting int16 = 15
	ProposalExecuted  int16 = 20
	ProposalRejected  int16 = 30
	ProposalFailed    int16 = 40
	ProposalExpired   int16 = 50
)

var (
	// ErrProposalNotFound：本店没有这条提案。
	ErrProposalNotFound = errors.New("提案不存在")
	// ErrProposalNotOpen：提案不在能批准 / 驳回的状态（已处理、已过期，或被别人抢先处理了）。
	ErrProposalNotOpen = errors.New("提案已经处理过或已过期")
	// ErrProposalDuplicate：同一个（kind，门店，SKU）已有一条待处理 / 执行中的提案。
	ErrProposalDuplicate = errors.New("已有一条同样的待处理提案")
)

// AgentProposal 是一条提案（含 AI 员工、门店、批准人的名字，给后台列表直接用）。
type AgentProposal struct {
	ID             int64
	AgentStaffID   int64
	AgentName      string
	Kind           string
	StoreID        *int64 // 全店类提案（营销、商品）为 nil（00120）
	StoreName      *string
	SKUID          *int64
	Payload        []byte
	Title          string
	Evidence       string
	ExpectedImpact string
	Status         int16
	DecidedBy      *int64
	DecidedByName  *string
	DecidedAt      *time.Time
	RejectReason   *string
	Result         []byte
	ExpiresAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// 执行后复盘（00122）。
	ExecutedAt *time.Time
	Outcome    []byte
	OutcomeAt  *time.Time
	// AutoApproved：按自动执行策略当场执行（00130），DecidedBy 为空。
	AutoApproved bool
}

// NewAgentProposal 是一条新提案。
type NewAgentProposal struct {
	AgentStaffID   int64
	Kind           string
	StoreID        *int64
	SKUID          *int64
	TargetKey      string // 作用对象（去重键，00120）
	Payload        []byte
	Title          string
	Evidence       string
	ExpectedImpact string
	ExpiresAt      time.Time
}

// ProposalFilter 是后台列表的筛选。StoreIDs 为 nil 即不收窄（全店范围的人）。
type ProposalFilter struct {
	Status       *int16
	AgentStaffID *int64
	StoreIDs     []int64
	Kind         *string
}

// AgentProposalTx 是提案那一面。
type AgentProposalTx interface {
	AgentOutcomeTx
	AgentAutoPolicyTx
	AgentSQLTx

	InsertAgentProposal(ctx context.Context, p NewAgentProposal) (int64, error)
	FindAgentProposal(ctx context.Context, id int64) (AgentProposal, error)
	OpenAgentProposalFor(ctx context.Context, kind, targetKey string) (int64, error)
	ListAgentProposals(ctx context.Context, f ProposalFilter, limit, offset int32) ([]AgentProposal, int64, error)
	ClaimAgentProposal(ctx context.Context, id, decidedBy int64) error
	FinishAgentProposal(ctx context.Context, id int64, status int16, result []byte) error
	RejectAgentProposal(ctx context.Context, id, decidedBy int64, reason string) error
	ExpireAgentProposals(ctx context.Context) (int64, error)
}

const proposalOpenIndex = "uk_agent_proposals_open"

func (t tenantTx) InsertAgentProposal(ctx context.Context, p NewAgentProposal) (int64, error) {
	id, err := t.q.InsertAgentProposal(ctx, db.InsertAgentProposalParams{AgentStaffID: p.AgentStaffID, Kind: p.Kind,
		StoreID: p.StoreID, SkuID: p.SKUID, TargetKey: p.TargetKey, Payload: p.Payload, Title: p.Title, Evidence: p.Evidence,
		ExpectedImpact: p.ExpectedImpact, ExpiresAt: pgtype.Timestamptz{Time: p.ExpiresAt, Valid: true}})
	if isUniqueViolation(err, proposalOpenIndex) {
		return 0, ErrProposalDuplicate
	}
	return id, err
}

func proposalOf(id, agentID int64, agentName, kind string, storeID *int64, storeName *string, skuID *int64,
	payload []byte, title, evidence, impact string, status int16, decidedBy *int64, decidedByName *string,
	decidedAt pgtype.Timestamptz, reject *string, result []byte, expires, created, updated pgtype.Timestamptz,
	executed pgtype.Timestamptz, outcome []byte, outcomeAt pgtype.Timestamptz, auto bool) AgentProposal {
	return AgentProposal{ID: id, AgentStaffID: agentID, AgentName: agentName, Kind: kind, StoreID: storeID,
		StoreName: storeName, SKUID: skuID, Payload: payload, Title: title, Evidence: evidence, ExpectedImpact: impact,
		Status: status, DecidedBy: decidedBy, DecidedByName: decidedByName, DecidedAt: tsPtr(decidedAt),
		RejectReason: reject, Result: result, ExpiresAt: expires.Time, CreatedAt: created.Time, UpdatedAt: updated.Time,
		ExecutedAt: tsPtr(executed), Outcome: outcome, OutcomeAt: tsPtr(outcomeAt), AutoApproved: auto}
}

func (t tenantTx) FindAgentProposal(ctx context.Context, id int64) (AgentProposal, error) {
	r, err := t.q.GetAgentProposal(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentProposal{}, fmt.Errorf("proposal %d: %w", id, ErrProposalNotFound)
	}
	if err != nil {
		return AgentProposal{}, err
	}
	return proposalOf(r.ID, r.AgentStaffID, r.AgentName, r.Kind, r.StoreID, r.StoreName, r.SkuID, r.Payload, r.Title,
		r.Evidence, r.ExpectedImpact, r.Status, r.DecidedBy, r.DecidedByName, r.DecidedAt, r.RejectReason, r.Result,
		r.ExpiresAt, r.CreatedAt, r.UpdatedAt, r.ExecutedAt, r.Outcome, r.OutcomeAt, r.AutoApproved), nil
}

func (t tenantTx) OpenAgentProposalFor(ctx context.Context, kind, targetKey string) (int64, error) {
	id, err := t.q.OpenAgentProposalFor(ctx, db.OpenAgentProposalForParams{Kind: kind, TargetKey: targetKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrProposalNotFound
	}
	return id, err
}

func (t tenantTx) ListAgentProposals(ctx context.Context, f ProposalFilter, limit, offset int32) ([]AgentProposal, int64, error) {
	rows, err := t.q.ListAgentProposals(ctx, db.ListAgentProposalsParams{Status: f.Status, AgentStaffID: f.AgentStaffID,
		StoreIds: f.StoreIDs, Kind: f.Kind, PageLimit: limit, PageOffset: offset})
	if err != nil {
		return nil, 0, err
	}
	total, err := t.q.CountAgentProposals(ctx, db.CountAgentProposalsParams{Status: f.Status,
		AgentStaffID: f.AgentStaffID, StoreIds: f.StoreIDs, Kind: f.Kind})
	if err != nil {
		return nil, 0, err
	}
	out := make([]AgentProposal, 0, len(rows))
	for _, r := range rows {
		out = append(out, proposalOf(r.ID, r.AgentStaffID, r.AgentName, r.Kind, r.StoreID, r.StoreName, r.SkuID,
			r.Payload, r.Title, r.Evidence, r.ExpectedImpact, r.Status, r.DecidedBy, r.DecidedByName, r.DecidedAt,
			r.RejectReason, r.Result, r.ExpiresAt, r.CreatedAt, r.UpdatedAt, r.ExecutedAt, r.Outcome, r.OutcomeAt, r.AutoApproved))
	}
	return out, total, nil
}

func (t tenantTx) ClaimAgentProposal(ctx context.Context, id, decidedBy int64) error {
	_, err := t.q.ClaimAgentProposal(ctx, db.ClaimAgentProposalParams{ID: id, DecidedBy: &decidedBy})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("proposal %d: %w", id, ErrProposalNotOpen)
	}
	return err
}

func (t tenantTx) FinishAgentProposal(ctx context.Context, id int64, status int16, result []byte) error {
	n, err := t.q.FinishAgentProposal(ctx, db.FinishAgentProposalParams{ID: id, Status: status, Result: result})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("proposal %d: %w", id, ErrProposalNotOpen)
	}
	return nil
}

func (t tenantTx) RejectAgentProposal(ctx context.Context, id, decidedBy int64, reason string) error {
	n, err := t.q.RejectAgentProposal(ctx, db.RejectAgentProposalParams{ID: id, DecidedBy: &decidedBy, Reason: &reason})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("proposal %d: %w", id, ErrProposalNotOpen)
	}
	return nil
}

func (t tenantTx) ExpireAgentProposals(ctx context.Context) (int64, error) {
	return t.q.ExpireAgentProposals(ctx)
}
