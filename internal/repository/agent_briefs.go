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

// AI 员工写的经营简报（00092，AI 经营 M9 任务 5）。

// ErrBriefNotFound：本店没有这份简报。
var ErrBriefNotFound = errors.New("简报不存在")

// AgentBrief 是一份简报。
type AgentBrief struct {
	ID           int64
	AgentStaffID int64
	AgentName    string
	Title        string
	Body         string
	PeriodStart  time.Time
	PeriodEnd    time.Time
	CreatedAt    time.Time
}

// AgentBriefTx 是简报那一面。
type AgentBriefTx interface {
	InsertAgentBrief(ctx context.Context, agentStaffID int64, title, body string, start, end time.Time) (int64, error)
	FindAgentBrief(ctx context.Context, id int64) (AgentBrief, error)
	ListAgentBriefs(ctx context.Context, limit, offset int32) ([]AgentBrief, int64, error)
}

func pgDate(t time.Time) pgtype.Date { return pgtype.Date{Time: t, Valid: true} }

func (t tenantTx) InsertAgentBrief(ctx context.Context, agentStaffID int64, title, body string, start, end time.Time) (int64, error) {
	return t.q.InsertAgentBrief(ctx, db.InsertAgentBriefParams{AgentStaffID: agentStaffID, Title: title, Body: body,
		PeriodStart: pgDate(start), PeriodEnd: pgDate(end)})
}

func (t tenantTx) FindAgentBrief(ctx context.Context, id int64) (AgentBrief, error) {
	r, err := t.q.GetAgentBrief(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentBrief{}, fmt.Errorf("brief %d: %w", id, ErrBriefNotFound)
	}
	if err != nil {
		return AgentBrief{}, err
	}
	return AgentBrief{ID: r.ID, AgentStaffID: r.AgentStaffID, AgentName: r.AgentName, Title: r.Title, Body: r.Body,
		PeriodStart: r.PeriodStart.Time, PeriodEnd: r.PeriodEnd.Time, CreatedAt: r.CreatedAt.Time}, nil
}

func (t tenantTx) ListAgentBriefs(ctx context.Context, limit, offset int32) ([]AgentBrief, int64, error) {
	rows, err := t.q.ListAgentBriefs(ctx, db.ListAgentBriefsParams{PageLimit: limit, PageOffset: offset})
	if err != nil {
		return nil, 0, err
	}
	total, err := t.q.CountAgentBriefs(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]AgentBrief, 0, len(rows))
	for _, r := range rows {
		out = append(out, AgentBrief{ID: r.ID, AgentStaffID: r.AgentStaffID, AgentName: r.AgentName, Title: r.Title,
			Body: r.Body, PeriodStart: r.PeriodStart.Time, PeriodEnd: r.PeriodEnd.Time, CreatedAt: r.CreatedAt.Time})
	}
	return out, total, nil
}
