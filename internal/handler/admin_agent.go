package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// AI 员工与接入密钥（AI 经营 M9 任务 1，docs/AI经营-M9设计.md §2）。
//
//	GET    /admin/agents                         列表
//	POST   /admin/agents                         新建
//	GET    /admin/agents/{staff_id}              详情（含密钥元数据）
//	PATCH  /admin/agents/{staff_id}              改名 / 角色 / 范围 / 停用
//	POST   /admin/agents/{staff_id}/keys         发密钥（明文只回这一次）
//	DELETE /admin/agents/{staff_id}/keys/{key_id} 吊销
//	GET    /agent/whoami                         AI 员工凭密钥自检（挂 auth.AgentBearer）
//
// 判权（本店管理员）在 service 里；这里只做形状与错误映射。

// AgentHandler 挂在 StaffService 上：AI 员工就是一种员工。
type AgentHandler struct {
	svc *service.StaffService
}

func NewAgentHandler(svc *service.StaffService) *AgentHandler { return &AgentHandler{svc: svc} }

func apiAgentKey(k repository.AgentKey) api.AgentKey {
	return api.AgentKey{Id: k.ID, Name: k.Name, Prefix: k.Prefix, ExpiresAt: k.ExpiresAt,
		RevokedAt: k.RevokedAt, LastUsedAt: k.LastUsedAt, CreatedAt: k.CreatedAt}
}

func apiAgent(a repository.Agent, sc *repository.StaffScopes, keys []repository.AgentKey) api.AdminAgent {
	out := api.AdminAgent{Id: a.ID, Name: a.Name, Role: api.AdminAgentRole(a.Role), Status: api.AdminAgentStatus(a.Status),
		LiveKeys: int(a.LiveKeys), LastUsedAt: a.LastUsedAt, CreatedAt: a.CreatedAt,
		RegionIds: []int64{}, StoreIds: []int64{}}
	if sc != nil {
		out.RegionIds = nonNilIDs(sc.RegionIDs)
		out.StoreIds = nonNilIDs(sc.StoreIDs)
	}
	if keys != nil {
		ks := make([]api.AgentKey, 0, len(keys))
		live := 0
		for _, k := range keys {
			ks = append(ks, apiAgentKey(k))
			if k.RevokedAt == nil && (k.ExpiresAt == nil || k.ExpiresAt.After(time.Now())) {
				live++
			}
		}
		out.Keys = &ks
		out.LiveKeys = live
	}
	return out
}

func apiAgentView(v service.AgentView) api.AdminAgent {
	return apiAgent(v.Agent, &v.Scopes, v.Keys)
}

func writeAgentError(c *gin.Context, err error) {
	if writePermissionError(c, err) {
		return
	}
	switch {
	case errors.Is(err, service.ErrStaffBadRequest):
		detail := err.Error()
		problem.WriteValue(c, http.StatusUnprocessableEntity, api.Problem{Type: problem.TypeInvalidRequest,
			Title: "请求参数不合法", Status: http.StatusUnprocessableEntity, Detail: &detail})
	case errors.Is(err, service.ErrAgentNotFound):
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "不是本店的 AI 员工")
	case errors.Is(err, service.ErrAgentKeyNotFound):
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "接入密钥不存在、已吊销或不属于这名 AI 员工")
	case errors.Is(err, service.ErrIdempotencyKeyMissing):
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "缺少必填的 Idempotency-Key 请求头")
	default:
		// 幂等键被别的请求体用过 / 正在处理中，以及兜底 500，与其它后台写接口同一个出口。
		writeOrderError(c, err)
	}
}

// List 实现 GET /admin/agents。
func (h *AgentHandler) List(c *gin.Context) {
	agents, err := h.svc.ListAgents(c.Request.Context())
	if err != nil {
		writeAgentError(c, err)
		return
	}
	items := make([]api.AdminAgent, 0, len(agents))
	for _, v := range agents {
		items = append(items, apiAgent(v.Agent, &v.Scopes, nil))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// Create 实现 POST /admin/agents。
func (h *AgentHandler) Create(c *gin.Context) {
	var req api.AgentCreateRequest
	if !bindJSON(c, &req) {
		return
	}
	sc := repository.StaffScopes{}
	if req.RegionIds != nil {
		sc.RegionIDs = *req.RegionIds
	}
	if req.StoreIds != nil {
		sc.StoreIDs = *req.StoreIds
	}
	v, replayed, err := h.svc.CreateAgent(c.Request.Context(), req.Name, int16(req.Role), sc, idemKeyOf(c))
	if err != nil {
		writeAgentError(c, err)
		return
	}
	markReplayed(c, replayed)
	c.JSON(http.StatusCreated, apiAgentView(v))
}

// Get 实现 GET /admin/agents/{staff_id}。
func (h *AgentHandler) Get(c *gin.Context) {
	id, ok := pathID(c, "staff_id")
	if !ok {
		return
	}
	v, err := h.svc.GetAgent(c.Request.Context(), id)
	if err != nil {
		writeAgentError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAgentView(v))
}

// Update 实现 PATCH /admin/agents/{staff_id}。
func (h *AgentHandler) Update(c *gin.Context) {
	id, ok := pathID(c, "staff_id")
	if !ok {
		return
	}
	var req api.AgentUpdateRequest
	if !bindJSON(c, &req) {
		return
	}
	var role, status *int16
	if req.Role != nil {
		r := int16(*req.Role)
		role = &r
	}
	if req.Status != nil {
		s := int16(*req.Status)
		status = &s
	}
	v, err := h.svc.UpdateAgent(c.Request.Context(), id, req.Name, role, status, req.RegionIds, req.StoreIds)
	if err != nil {
		writeAgentError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAgentView(v))
}

// CreateKey 实现 POST /admin/agents/{staff_id}/keys。
func (h *AgentHandler) CreateKey(c *gin.Context) {
	id, ok := pathID(c, "staff_id")
	if !ok {
		return
	}
	var req api.AgentKeyCreateRequest
	if !bindJSON(c, &req) {
		return
	}
	days := 0
	if req.ExpiresInDays != nil {
		days = *req.ExpiresInDays
	}
	k, err := h.svc.CreateAgentKey(c.Request.Context(), id, req.Name, days)
	if err != nil {
		writeAgentError(c, err)
		return
	}
	// 明文只在这里出现一次：不进任何缓存。
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, api.AgentKeyCreated{Id: k.Key.ID, Name: k.Key.Name, Prefix: k.Key.Prefix,
		ExpiresAt: k.Key.ExpiresAt, RevokedAt: k.Key.RevokedAt, LastUsedAt: k.Key.LastUsedAt,
		CreatedAt: k.Key.CreatedAt, Secret: k.Secret})
}

// RevokeKey 实现 DELETE /admin/agents/{staff_id}/keys/{key_id}。
func (h *AgentHandler) RevokeKey(c *gin.Context) {
	id, ok := pathID(c, "staff_id")
	if !ok {
		return
	}
	keyID, ok := pathID(c, "key_id")
	if !ok {
		return
	}
	if err := h.svc.RevokeAgentKey(c.Request.Context(), id, keyID); err != nil {
		writeAgentError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// WhoAmI 实现 GET /agent/whoami（挂 auth.AgentBearer）。
func (h *AgentHandler) WhoAmI(c *gin.Context) {
	id, err := auth.StaffFromContext(c.Request.Context())
	if err != nil || !id.IsAgent() {
		problem.Write(c, http.StatusUnauthorized, problem.TypeUnauthorized, "需要接入密钥")
		return
	}
	v, err := h.svc.AgentSelf(c.Request.Context())
	if err != nil {
		writeAgentError(c, err)
		return
	}
	c.JSON(http.StatusOK, api.AgentWhoAmI{StaffId: id.StaffID, Name: v.Name, Role: api.AgentWhoAmIRole(id.Role),
		RegionIds: nonNilIDs(id.RegionIDs), StoreIds: nonNilIDs(id.StoreIDs), KeyId: id.AgentKeyID})
}
