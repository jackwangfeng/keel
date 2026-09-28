package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 公开的 AI 经营日志（AI 经营 M11，00132）：GET /api/v1/ai-log（免登录）与 GET / PUT /api/v1/admin/ai-log/settings。

// PublicAILogHandler 是公开日志与它的开关。
type PublicAILogHandler struct {
	svc *service.PublicAILogService
}

func NewPublicAILogHandler(svc *service.PublicAILogService) *PublicAILogHandler {
	return &PublicAILogHandler{svc: svc}
}

// Get 实现 GET /api/v1/ai-log。
func (h *PublicAILogHandler) Get(c *gin.Context) {
	l, err := h.svc.Get(c.Request.Context())
	if errors.Is(err, service.ErrAILogDisabled) {
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "这家店没有公开 AI 经营日志")
		return
	}
	if err != nil {
		writeCatalogError(c, err)
		return
	}
	out := api.PublicAILog{Briefs: make([]api.PublicAILogBrief, 0, len(l.Briefs)),
		Proposals: make([]api.PublicAILogProposal, 0, len(l.Proposals)),
		Summary: api.PublicAILogSummary{Proposed: l.Summary.Proposed, Executed: l.Summary.Executed,
			AutoExecuted: l.Summary.AutoExecuted, Rejected: l.Summary.Rejected, Positive: l.Summary.Positive,
			Negative: l.Summary.Negative}}
	for _, b := range l.Briefs {
		out.Briefs = append(out.Briefs, api.PublicAILogBrief{Id: b.ID, Title: b.Title, Excerpt: b.Excerpt,
			AgentName: b.AgentName, PeriodStart: openapi_types.Date{Time: b.PeriodStart},
			PeriodEnd: openapi_types.Date{Time: b.PeriodEnd}, CreatedAt: b.CreatedAt,
			CorrectsBriefId: b.CorrectsID, CorrectedByBriefId: b.CorrectedByID})
	}
	for _, p := range l.Proposals {
		ap := api.PublicAILogProposal{Id: p.ID, Kind: p.Kind, Title: p.Title, Status: int(p.Status),
			AutoApproved: p.AutoApproved, AgentName: p.AgentName, CreatedAt: p.CreatedAt, DecidedAt: p.DecidedAt}
		if p.Verdict != "" {
			v := api.PublicAILogProposalVerdict(p.Verdict)
			ap.Verdict = &v
		}
		out.Proposals = append(out.Proposals, ap)
	}
	c.JSON(http.StatusOK, out)
}

// Settings 实现 GET /api/v1/admin/ai-log/settings。
func (h *PublicAILogHandler) Settings(c *gin.Context) {
	on, err := h.svc.Enabled(c.Request.Context())
	if err != nil {
		writeAgentError(c, err)
		return
	}
	c.JSON(http.StatusOK, api.AILogSettings{Enabled: on})
}

// PutSettings 实现 PUT /api/v1/admin/ai-log/settings。
func (h *PublicAILogHandler) PutSettings(c *gin.Context) {
	var req api.AILogSettings
	if !bindJSON(c, &req) {
		return
	}
	on, err := h.svc.SetEnabled(c.Request.Context(), req.Enabled)
	if err != nil {
		writeAgentError(c, err)
		return
	}
	c.JSON(http.StatusOK, api.AILogSettings{Enabled: on})
}
