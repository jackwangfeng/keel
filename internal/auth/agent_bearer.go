package auth

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/problem"
)

// AgentBearer 是 AI 员工的鉴权中间件（AI 经营 M9，docs/AI经营-M9设计.md §2–§3）。
//
// Authorization: Bearer kagt_… → 在本请求的租户（已由 Host 解析）里查这把密钥 → 拼出 StaffIdentity 放进 ctx。
// 之后的判权与后台接口逐字相同：AI 员工就是一个角色 / 范围受限的员工。
//
// 与 StaffBearer 的区别只在「令牌是什么」：人的是签名的会话令牌，AI 的是查库的接入密钥。
// 失败口径同 StaffBearer：密钥不对 / 吊销 / 过期一律 401（不区分，免得成为探测密钥状态的预言机）；
// 员工停用 403 account-disabled。
type AgentKeyLoader interface {
	LoadAgentIdentity(ctx context.Context, raw string) (StaffIdentity, error)
}

func AgentBearer(loader AgentKeyLoader, isRejected func(error) bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			problem.Write(c, http.StatusUnauthorized, problem.TypeUnauthorized, "需要接入密钥（Authorization: Bearer kagt_…）")
			return
		}
		id, err := loader.LoadAgentIdentity(c.Request.Context(), raw)
		if err != nil {
			if isRejected(err) {
				problem.Write(c, http.StatusUnauthorized, problem.TypeUnauthorized, "接入密钥无效、已吊销或已过期")
				return
			}
			_ = c.Error(err)
			problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "服务内部错误")
			return
		}
		if id.Status != StaffStatusActive {
			problem.Write(c, http.StatusForbidden, problem.TypeAccountDisabled, "这名 AI 员工已停用")
			return
		}
		c.Request = c.Request.WithContext(NewStaffContext(c.Request.Context(), id))
		c.Next()
	}
}
