package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/keel/keel/internal/repository"
)

// AI 员工写的经营简报（AI 经营 M9 任务 5，docs/AI经营-M9设计.md §6）。
//
// 只有 AI 员工能写（MCP 工具 post_brief）；只有全店范围的人（管理员 / 操作员）能看 —— 简报是全店口径的经营信息，
// 一名门店管理员不该从里面读到别的门店的销售额。

var (
	ErrBriefBadRequest = errors.New("简报参数不合法")
	ErrBriefNotFound   = errors.New("简报不存在")
)

const (
	briefMaxTitle = 100
	briefMaxBody  = 8192
)

// AgentBriefService 实现简报。
type AgentBriefService struct {
	repo tenantRunner
}

func NewAgentBriefService(repo tenantRunner) *AgentBriefService { return &AgentBriefService{repo: repo} }

// Post 是 MCP 工具 post_brief。period_* 是 YYYY-MM-DD（店铺时区的日期，简报说的是哪几天）。
func (s *AgentBriefService) Post(ctx context.Context, title, body, periodStart, periodEnd string) (repository.AgentBrief, error) {
	id, err := requireStaff(ctx)
	if err != nil {
		return repository.AgentBrief{}, err
	}
	if !id.IsAgent() {
		return repository.AgentBrief{}, fmt.Errorf("%w: 简报只能由 AI 员工写", ErrRoleForbidden)
	}
	title, body = strings.TrimSpace(title), strings.TrimSpace(body)
	if n := len([]rune(title)); n < 1 || n > briefMaxTitle {
		return repository.AgentBrief{}, fmt.Errorf("%w: title 1–%d 字", ErrBriefBadRequest, briefMaxTitle)
	}
	if len(body) < 1 || len(body) > briefMaxBody {
		return repository.AgentBrief{}, fmt.Errorf("%w: body 1–%d 字节", ErrBriefBadRequest, briefMaxBody)
	}
	start, err1 := time.Parse("2006-01-02", periodStart)
	end, err2 := time.Parse("2006-01-02", periodEnd)
	if err1 != nil || err2 != nil || end.Before(start) || end.Sub(start) > 92*24*time.Hour {
		return repository.AgentBrief{}, fmt.Errorf("%w: period_start / period_end 是 YYYY-MM-DD，起不晚于止，跨度不超过 92 天",
			ErrBriefBadRequest)
	}
	var out repository.AgentBrief
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		bid, err := tx.InsertAgentBrief(ctx, id.StaffID, title, body, start, end)
		if err != nil {
			return err
		}
		out, err = tx.FindAgentBrief(ctx, bid)
		return err
	})
	return out, err
}

// BriefPage 是一页简报。
type BriefPage struct {
	Items    []repository.AgentBrief
	Total    int64
	Page     int
	PageSize int
}

func requireMerchantWideReader(ctx context.Context) error {
	id, err := requireStaff(ctx)
	if err != nil {
		return err
	}
	if !id.MerchantWide() {
		return fmt.Errorf("%w: 简报是全店口径的经营信息，只给管理员与操作员看", ErrRoleForbidden)
	}
	return nil
}

// List 实现 GET /admin/agent-briefs。
func (s *AgentBriefService) List(ctx context.Context, page, pageSize int) (BriefPage, error) {
	if err := requireMerchantWideReader(ctx); err != nil {
		return BriefPage{}, err
	}
	page, pageSize = clampPaging(page, pageSize)
	out := BriefPage{Page: page, PageSize: pageSize}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out.Items, out.Total, e = tx.ListAgentBriefs(ctx, int32(pageSize), int32(offsetOf(page, pageSize)))
		return e
	})
	return out, err
}

// Get 实现 GET /admin/agent-briefs/{brief_id}。
func (s *AgentBriefService) Get(ctx context.Context, briefID int64) (repository.AgentBrief, error) {
	if err := requireMerchantWideReader(ctx); err != nil {
		return repository.AgentBrief{}, err
	}
	var out repository.AgentBrief
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out, e = tx.FindAgentBrief(ctx, briefID)
		return e
	})
	if errors.Is(err, repository.ErrBriefNotFound) {
		return repository.AgentBrief{}, fmt.Errorf("%w: %v", ErrBriefNotFound, err)
	}
	return out, err
}
