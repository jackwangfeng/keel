package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/service"
)

// Scorecard 实现 GET /api/v1/admin/agents/{staff_id}/scorecard（00122）。
func (h *AgentProposalHandler) Scorecard(c *gin.Context) {
	id, ok := pathID(c, "staff_id")
	if !ok {
		return
	}
	sc, err := h.svc.Scorecard(c.Request.Context(), id)
	if err != nil {
		writeAgentError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiScorecard(sc))
}

func apiScorecard(sc service.Scorecard) api.AgentScorecard {
	out := api.AgentScorecard{AgentStaffId: sc.AgentStaffID, Since: sc.Since,
		Kinds: make([]api.AgentScorecardKind, 0, len(sc.Kinds)), Recent: make([]api.AgentScorecardEntry, 0, len(sc.Recent))}
	for _, k := range sc.Kinds {
		out.Kinds = append(out.Kinds, api.AgentScorecardKind{Kind: k.Kind, Proposed: k.Proposed, Approved: k.Approved,
			Executed: k.Executed, Failed: k.Failed, Rejected: k.Rejected, Expired: k.Expired, Open: k.Open,
			Positive: k.Positive, Neutral: k.Neutral, Negative: k.Negative})
	}
	for _, r := range sc.Recent {
		o := map[string]any{}
		if m := jsonObject(r.Outcome); m != nil {
			o = *m
		}
		out.Recent = append(out.Recent, api.AgentScorecardEntry{ProposalId: r.ID, Kind: r.Kind, Title: r.Title,
			Outcome: o, OutcomeAt: r.OutcomeAt})
	}
	return out
}
