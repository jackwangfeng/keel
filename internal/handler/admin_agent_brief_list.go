package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
)

// GET /admin/agent-briefs（AI 经营 M9 任务 5）。单独一个文件：它读分页 query 参数（contract_test.go 按文件对账 c.Query）。

// List 实现 GET /admin/agent-briefs。
func (h *AgentBriefHandler) List(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	out, err := h.svc.List(c.Request.Context(), page, pageSize)
	if err != nil {
		writeBriefError(c, err)
		return
	}
	items := make([]api.AgentBrief, 0, len(out.Items))
	for _, b := range out.Items {
		items = append(items, apiAgentBrief(b))
	}
	c.JSON(http.StatusOK, struct {
		api.PageMeta
		Items []api.AgentBrief `json:"items"`
	}{PageMeta: api.PageMeta{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total)}, Items: items})
}
