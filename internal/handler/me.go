package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 个人信息（契约 User tag）：
//
//	GET    /me                        当前用户资料
//	PATCH  /me                        修改资料
//	GET    /me/identities             已绑定的第三方身份
//	POST   /me/identities/wechat      绑定微信 —— 501，本项目没接微信开放平台
//	DELETE /me/identities/{provider}  解绑第三方身份
//	POST   /me/phone                  绑定 / 换绑手机号 —— 501，本项目没接短信服务
//
// 哪些 501、为什么，写在 service/profile.go 的文件头。两条 501 在
// contract_test.go 的 NotYetImplementedBody 里挂着账。

type MeHandler struct{ svc *service.ProfileService }

func NewMeHandler(s *service.ProfileService) *MeHandler { return &MeHandler{svc: s} }

// Get 实现 GET /api/v1/me。
func (h *MeHandler) Get(c *gin.Context) {
	u, err := h.svc.Me(c.Request.Context())
	if err != nil {
		writeMeError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiUser(u))
}

// Patch 实现 PATCH /api/v1/me。
func (h *MeHandler) Patch(c *gin.Context) {
	var raw api.PatchMeJSONRequestBody
	if !bindJSON(c, &raw) {
		return
	}
	in := service.ProfilePatch{Nickname: raw.Nickname, AvatarURL: raw.AvatarUrl}
	if raw.Gender != nil {
		g := int(*raw.Gender)
		in.Gender = &g
	}
	u, err := h.svc.Update(c.Request.Context(), in)
	if err != nil {
		writeMeError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiUser(u))
}

// Identities 实现 GET /api/v1/me/identities。
func (h *MeHandler) Identities(c *gin.Context) {
	list, err := h.svc.Identities(c.Request.Context())
	if err != nil {
		writeMeError(c, err)
		return
	}
	out := make([]api.UserIdentity, 0, len(list))
	for _, it := range list {
		has := it.HasUnionID
		out = append(out, api.UserIdentity{
			Id:         it.ID,
			Provider:   api.IdentityProvider(it.Provider),
			HasUnionId: &has,
			CreatedAt:  it.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, out)
}

// Unbind 实现 DELETE /api/v1/me/identities/{provider}。
func (h *MeHandler) Unbind(c *gin.Context) {
	p, err := strconv.Atoi(c.Param("provider"))
	if err != nil {
		// 路径段不是数字：它不可能指向任何一条绑定。与「没有这个绑定」同一个 404。
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "没有绑定这个第三方身份")
		return
	}
	if err := h.svc.Unbind(c.Request.Context(), p); err != nil {
		writeMeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// BindWechat 实现 POST /api/v1/me/identities/wechat —— 本期 501。
//
// 请求体不解析：服务端这条路一步都走不下去，校验一个不会被用到的 code
// 只会让客户端以为「格式对了就能成」。
func (h *MeHandler) BindWechat(c *gin.Context) {
	writeMeError(c, h.svc.BindWechat(c.Request.Context()))
}

// BindPhone 实现 POST /api/v1/me/phone —— 本期 501。理由同 BindWechat。
func (h *MeHandler) BindPhone(c *gin.Context) {
	writeMeError(c, h.svc.BindPhone(c.Request.Context()))
}

// writeMeError 把 /me 那一组的业务错误翻成契约里的响应。
func writeMeError(c *gin.Context, err error) {
	var fields *service.InvalidFieldsError
	switch {
	case errors.As(err, &fields):
		writeFieldErrors(c, fields)
	case errors.Is(err, service.ErrIdentityNotFound):
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "没有绑定这个第三方身份")
	case errors.Is(err, service.ErrLastCredential):
		problem.Write(c, http.StatusConflict, problem.TypeLastCredential,
			"这是账号最后一个可登录的凭据，请先设置密码或绑定其他身份再解绑")
	case errors.Is(err, service.ErrWechatBindUnavailable):
		// 501 而不是 4xx：不是客户端的错，也不是重试能好的事 —— 这条路服务端还没通。
		problem.Write(c, http.StatusNotImplemented, problem.TypeNotImplemented,
			"绑定微信尚未开通：服务端没有接入微信开放平台")
	case errors.Is(err, service.ErrPhoneBindUnavailable):
		problem.Write(c, http.StatusNotImplemented, problem.TypeNotImplemented,
			"绑定手机号尚未开通：服务端没有接入短信服务")
	case writeBuyerAccountError(c, err):
	default:
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "服务内部错误")
	}
}
