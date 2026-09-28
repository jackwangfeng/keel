package service

import (
	"context"
	"errors"
	"time"

	"github.com/keel/keel/internal/repository"
)

// 公开的 AI 经营日志（AI 经营 M11，docs/AI经营-M10M11设计.md §8，00132）。
//
//	GET /api/v1/ai-log                 买家侧、免登录：店铺开了开关才有，否则 404
//	GET / PUT /admin/ai-log/settings   开关，只有本店管理员
//
// 公开的只有：简报的标题、覆盖日期与开头一段；提案的种类、标题、状态、是否自动执行、复盘结论与时间。
// 证据全文、执行参数、驳回理由（可能写着内部考虑）一律不公开。

// ErrAILogDisabled：这家店没开公开日志。契约 404（「没有这个页面」，与没有这家店的其它资源同一个说法）。
var ErrAILogDisabled = errors.New("这家店没有公开 AI 经营日志")

const aiLogExcerptRunes = 300

// PublicAILogBrief 是公开日志上的一份简报。
type PublicAILogBrief struct {
	ID          int64
	Title       string
	Excerpt     string
	AgentName   string
	PeriodStart time.Time
	PeriodEnd   time.Time
	CreatedAt   time.Time
}

// PublicAILog 是公开日志的全部。
type PublicAILog struct {
	Briefs    []PublicAILogBrief
	Proposals []repository.PublicAILogEntry
	Summary   repository.PublicAILogSummary
}

// PublicAILogService 实现公开日志与它的开关。
type PublicAILogService struct {
	repo tenantRunner
}

func NewPublicAILogService(repo tenantRunner) *PublicAILogService { return &PublicAILogService{repo: repo} }

// Get 实现 GET /api/v1/ai-log。
func (s *PublicAILogService) Get(ctx context.Context) (PublicAILog, error) {
	var out PublicAILog
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		on, err := tx.PublicAILogEnabled(ctx)
		if err != nil {
			return err
		}
		if !on {
			return ErrAILogDisabled
		}
		briefs, _, err := tx.ListAgentBriefs(ctx, 10, 0)
		if err != nil {
			return err
		}
		for _, b := range briefs {
			out.Briefs = append(out.Briefs, PublicAILogBrief{ID: b.ID, Title: b.Title, Excerpt: truncRunes(b.Body, aiLogExcerptRunes),
				AgentName: b.AgentName, PeriodStart: b.PeriodStart, PeriodEnd: b.PeriodEnd, CreatedAt: b.CreatedAt})
		}
		out.Proposals, out.Summary, err = tx.PublicAILog(ctx)
		return err
	})
	return out, err
}

// Enabled 实现 GET /admin/ai-log/settings。
func (s *PublicAILogService) Enabled(ctx context.Context) (bool, error) {
	if _, err := requireShopAdmin(ctx); err != nil {
		return false, err
	}
	var on bool
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		on, e = tx.PublicAILogEnabled(ctx)
		return e
	})
	return on, err
}

// SetEnabled 实现 PUT /admin/ai-log/settings。
func (s *PublicAILogService) SetEnabled(ctx context.Context, on bool) (bool, error) {
	if _, err := requireShopAdmin(ctx); err != nil {
		return false, err
	}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error { return tx.SetPublicAILog(ctx, on) })
	return on, err
}
