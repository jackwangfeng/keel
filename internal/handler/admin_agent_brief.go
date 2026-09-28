package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// AI 员工写的经营简报：后台详情（AI 经营 M9 任务 5）。列表读分页 query 参数，单独在 admin_agent_brief_list.go。
//
//	GET /admin/agent-briefs/{brief_id}

type AgentBriefHandler struct {
	svc *service.AgentBriefService
}

func NewAgentBriefHandler(svc *service.AgentBriefService) *AgentBriefHandler {
	return &AgentBriefHandler{svc: svc}
}

func apiAgentBrief(b repository.AgentBrief) api.AgentBrief {
	return api.AgentBrief{Id: b.ID, AgentStaffId: b.AgentStaffID, AgentName: b.AgentName, Title: b.Title, Body: b.Body,
		PeriodStart: openapi_types.Date{Time: b.PeriodStart}, PeriodEnd: openapi_types.Date{Time: b.PeriodEnd},
		CreatedAt: b.CreatedAt, CorrectsBriefId: b.CorrectsID, CorrectedByBriefId: b.CorrectedByID}
}

func writeBriefError(c *gin.Context, err error) {
	if writePermissionError(c, err) {
		return
	}
	switch {
	case errors.Is(err, service.ErrBriefNotFound):
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "本店没有这份简报")
	case errors.Is(err, service.ErrBriefBadRequest):
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "请求参数不合法", err)
	default:
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "服务内部错误")
	}
}

// Get 实现 GET /admin/agent-briefs/{brief_id}。
func (h *AgentBriefHandler) Get(c *gin.Context) {
	id, ok := pathID(c, "brief_id")
	if !ok {
		return
	}
	b, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		writeBriefError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAgentBrief(b))
}
