package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
)

// GET /admin/agent-proposals（AI 经营 M9 任务 4）。单独一个文件：它读 query 参数，而 contract_test.go 按
// HandlerFile 解析整份源码里的 c.Query —— 与详情 / 批准 / 驳回同文件会让那几条「没有 query 参数」的登记红掉。

// List 实现 GET /admin/agent-proposals。
func (h *AgentProposalHandler) List(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	out, err := h.svc.List(c.Request.Context(), adminListInt64(c.Query("status")),
		adminListInt64(c.Query("agent_staff_id")), page, pageSize)
	if err != nil {
		writeProposalError(c, err)
		return
	}
	items := make([]api.AgentProposal, 0, len(out.Items))
	for _, p := range out.Items {
		items = append(items, apiAgentProposal(p))
	}
	c.JSON(http.StatusOK, struct {
		api.PageMeta
		Items []api.AgentProposal `json:"items"`
	}{PageMeta: api.PageMeta{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total)}, Items: items})
}
