package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// AI 员工的事件 webhook（AI 经营 M10 §3「推」）。
//
//	GET    /admin/agents/{staff_id}/webhook   看配置（不含密钥）与最近 20 次投递
//	PUT    /admin/agents/{staff_id}/webhook   建 / 改（新建或 rotate_secret 时回一次密钥）
//	DELETE /admin/agents/{staff_id}/webhook   删
//
// 只有本店管理员（与 /admin/agents 的其它接口同一个判据，在 service 里）。

// AgentWebhookHandler 挂在 AgentEventService 上。
type AgentWebhookHandler struct {
	svc *service.AgentEventService
}

func NewAgentWebhookHandler(svc *service.AgentEventService) *AgentWebhookHandler {
	return &AgentWebhookHandler{svc: svc}
}

func apiAgentWebhook(v service.AgentWebhookView, withDeliveries bool) api.AgentWebhook {
	w := v.Webhook
	out := api.AgentWebhook{Id: w.ID, Url: w.URL, Enabled: w.Enabled, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}
	if v.Secret != "" {
		s := v.Secret
		out.Secret = &s
	}
	if withDeliveries {
		ds := make([]api.AgentWebhookDelivery, 0, len(v.Deliveries))
		for _, d := range v.Deliveries {
			x := api.AgentWebhookDelivery{Id: d.ID, EventId: d.EventID, Attempt: int(d.Attempt), Error: d.Error,
				DeliveredAt: d.DeliveredAt}
			if d.StatusCode != nil {
				c := int(*d.StatusCode)
				x.StatusCode = &c
			}
			ds = append(ds, x)
		}
		out.RecentDeliveries = &ds
	}
	return out
}

func writeAgentWebhookError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrAgentWebhookNotFound) {
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "这名 AI 员工没有配 webhook")
		return
	}
	writeAgentError(c, err)
}

// Get 实现 GET /admin/agents/{staff_id}/webhook。
func (h *AgentWebhookHandler) Get(c *gin.Context) {
	id, ok := pathID(c, "staff_id")
	if !ok {
		return
	}
	v, err := h.svc.GetWebhook(c.Request.Context(), id)
	if err != nil {
		writeAgentWebhookError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAgentWebhook(v, true))
}

// Put 实现 PUT /admin/agents/{staff_id}/webhook。
func (h *AgentWebhookHandler) Put(c *gin.Context) {
	id, ok := pathID(c, "staff_id")
	if !ok {
		return
	}
	var req api.AgentWebhookPutRequest
	if !bindJSON(c, &req) {
		return
	}
	in := service.AgentWebhookInput{URL: req.Url, Enabled: req.Enabled}
	if req.RotateSecret != nil {
		in.RotateSecret = *req.RotateSecret
	}
	v, err := h.svc.PutWebhook(c.Request.Context(), id, in)
	if err != nil {
		writeAgentWebhookError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAgentWebhook(v, false))
}

// Delete 实现 DELETE /admin/agents/{staff_id}/webhook。
func (h *AgentWebhookHandler) Delete(c *gin.Context) {
	id, ok := pathID(c, "staff_id")
	if !ok {
		return
	}
	if err := h.svc.DeleteWebhook(c.Request.Context(), id); err != nil {
		writeAgentWebhookError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
