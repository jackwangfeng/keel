package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 自动执行策略（AI 经营 M11，00130）：GET /admin/agents/{staff_id}/auto-policies、PUT …/auto-policies/{kind}。

func writeAutoPolicyError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrAutoPolicyBadRequest) {
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "自动执行策略不成立", err)
		return
	}
	writeAgentError(c, err)
}

// ListAutoPolicies 实现 GET /api/v1/admin/agents/{staff_id}/auto-policies。
func (h *AgentProposalHandler) ListAutoPolicies(c *gin.Context) {
	id, ok := pathID(c, "staff_id")
	if !ok {
		return
	}
	ps, err := h.svc.ListAutoPolicies(c.Request.Context(), id)
	if err != nil {
		writeAutoPolicyError(c, err)
		return
	}
	items := make([]api.AgentAutoPolicy, 0, len(ps))
	for _, p := range ps {
		items = append(items, apiAutoPolicy(p))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// PutAutoPolicy 实现 PUT /api/v1/admin/agents/{staff_id}/auto-policies/{kind}。
func (h *AgentProposalHandler) PutAutoPolicy(c *gin.Context) {
	id, ok := pathID(c, "staff_id")
	if !ok {
		return
	}
	var req api.AgentAutoPolicyInput
	if !bindJSON(c, &req) {
		return
	}
	p, err := h.svc.PutAutoPolicy(c.Request.Context(), repository.AgentAutoPolicy{AgentStaffID: id, Kind: c.Param("kind"),
		Enabled: req.Enabled, MaxUnits: req.MaxUnits, MinDiscountRate: int16(req.MinDiscountRate),
		MaxDiscountCents: req.MaxDiscountCents, DailyLimit: req.DailyLimit})
	if err != nil {
		writeAutoPolicyError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAutoPolicy(p))
}

func apiAutoPolicy(p repository.AgentAutoPolicy) api.AgentAutoPolicy {
	return api.AgentAutoPolicy{Kind: api.AgentAutoPolicyKind(p.Kind), Enabled: p.Enabled, MaxUnits: p.MaxUnits,
		MinDiscountRate: int32(p.MinDiscountRate), MaxDiscountCents: p.MaxDiscountCents, DailyLimit: p.DailyLimit,
		UpdatedBy: p.UpdatedBy, UpdatedAt: p.UpdatedAt}
}
