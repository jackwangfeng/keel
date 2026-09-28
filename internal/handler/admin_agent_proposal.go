package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// AI 员工的提案：后台的详情 / 批准 / 驳回（AI 经营 M9 任务 4）。列表读 query 参数，单独在 admin_agent_proposal_list.go。
//
//	GET  /admin/agent-proposals/{proposal_id}
//	POST /admin/agent-proposals/{proposal_id}/approve
//	POST /admin/agent-proposals/{proposal_id}/reject

type AgentProposalHandler struct {
	svc *service.AgentProposalService
}

func NewAgentProposalHandler(svc *service.AgentProposalService) *AgentProposalHandler {
	return &AgentProposalHandler{svc: svc}
}

func jsonObject(raw []byte) *map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return &m
}

func apiAgentProposal(p repository.AgentProposal) api.AgentProposal {
	payload := map[string]any{}
	if m := jsonObject(p.Payload); m != nil {
		payload = *m
	}
	return api.AgentProposal{Id: p.ID, AgentStaffId: p.AgentStaffID, AgentName: p.AgentName,
		Kind: api.AgentProposalKind(p.Kind), StoreId: p.StoreID, StoreName: p.StoreName, SkuId: p.SKUID,
		Payload: payload, Title: p.Title, Evidence: p.Evidence, ExpectedImpact: p.ExpectedImpact,
		Status: api.AgentProposalStatus(p.Status), DecidedBy: p.DecidedBy, DecidedByName: p.DecidedByName,
		DecidedAt: p.DecidedAt, RejectReason: p.RejectReason, Result: jsonObject(p.Result),
		ExpiresAt: p.ExpiresAt, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
}

func writeProposalError(c *gin.Context, err error) {
	if writePermissionError(c, err) || writeInventoryUnavailable(c, err) {
		return
	}
	switch {
	case errors.Is(err, service.ErrProposalNotFound):
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "本店没有这条提案")
	case errors.Is(err, service.ErrProposalNotOpen):
		problem.Write(c, http.StatusConflict, problem.TypeProposalNotOpen, "提案已经处理过或已过期")
	case errors.Is(err, service.ErrProposalDuplicate):
		writeProblemDetail(c, http.StatusConflict, problem.TypeInvalidRequest, "已有一条同样的待处理提案", err)
	case errors.Is(err, service.ErrProposalBadRequest), errors.Is(err, service.ErrAdminListBadRequest):
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "请求参数不合法", err)
	default:
		// 提案里的门店 / SKU 校验失败（不存在、这家店不卖）与其它后台写接口同一个出口。
		writeCatalogError(c, err)
	}
}

// Get 实现 GET /admin/agent-proposals/{proposal_id}。
func (h *AgentProposalHandler) Get(c *gin.Context) {
	id, ok := pathID(c, "proposal_id")
	if !ok {
		return
	}
	p, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		writeProposalError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAgentProposal(p))
}

// Approve 实现 POST /admin/agent-proposals/{proposal_id}/approve。
func (h *AgentProposalHandler) Approve(c *gin.Context) {
	id, ok := pathID(c, "proposal_id")
	if !ok {
		return
	}
	p, err := h.svc.Approve(c.Request.Context(), id)
	if err != nil {
		writeProposalError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAgentProposal(p))
}

// Reject 实现 POST /admin/agent-proposals/{proposal_id}/reject。
func (h *AgentProposalHandler) Reject(c *gin.Context) {
	id, ok := pathID(c, "proposal_id")
	if !ok {
		return
	}
	var req api.AgentProposalRejectRequest
	if !bindJSON(c, &req) {
		return
	}
	p, err := h.svc.Reject(c.Request.Context(), id, req.Reason)
	if err != nil {
		writeProposalError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAgentProposal(p))
}
