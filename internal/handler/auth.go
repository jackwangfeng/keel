package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

type AuthHandler struct{ svc *service.AuthService }

func NewAuthHandler(s *service.AuthService) *AuthHandler { return &AuthHandler{svc: s} }

// loginRequest 是契约里 /auth/login 的请求体。
//
// 三个字段全是 string 而不是 *string：契约里 phone 必填、code 与 password
// 是 oneOf，而「给了空串」与「没给」在这条接口上是同一件事（都不能用来登录）。
// 用指针的话，每个分支都要先判 nil 再判空串，而漏掉后者的那一支会拿着
// 一个空口令去比对。
type loginRequest struct {
	Phone    string `json:"phone"`
	Code     string `json:"code"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// Login 实现 POST /api/v1/auth/login。
func (h *AuthHandler) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求体不是合法的 JSON")
		return
	}

	sess, err := h.svc.Login(c.Request.Context(), service.LoginRequest{
		Phone:    req.Phone,
		Code:     req.Code,
		Password: req.Password,
	})
	if err != nil {
		writeAuthError(c, err)
		return
	}
	c.JSON(http.StatusOK, loginResponse(sess))
}

// Refresh 实现 POST /api/v1/auth/refresh。
func (h *AuthHandler) Refresh(c *gin.Context) {
	var req refreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求体不是合法的 JSON")
		return
	}
	sess, err := h.svc.Refresh(c.Request.Context(), req.RefreshToken)
	if err != nil {
		writeAuthError(c, err)
		return
	}
	c.JSON(http.StatusOK, loginResponse(sess))
}

// Logout 实现 POST /api/v1/auth/logout。
//
// 它挂在 auth.Bearer 后面（契约里这条接口没有 security: []，继承全局的
// bearerAuth）。当前是哪个会话由令牌里的 sid 说了算，请求体里什么都不需要 ——
// 让客户端把 refresh_token 再传一遍的话，一个拿着 access_token 的人就吊销不了
// 自己的会话，而那正是「在别人的手机上退出登录」要做的事。
func (h *AuthHandler) Logout(c *gin.Context) {
	if err := h.svc.Logout(c.Request.Context()); err != nil {
		writeAuthError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// loginResponse 把 service 的结果装成契约里的 LoginResponse。
//
// 用生成类型而不是手写结构体（同 product.go 的理由）：契约改个字段名，
// 这里当场编译失败，而不是等线上客户端解析失败。
func loginResponse(s service.Session) api.LoginResponse {
	refresh := s.RefreshToken
	return api.LoginResponse{
		AccessToken:  s.AccessToken,
		RefreshToken: &refresh,
		TokenType:    "Bearer",
		ExpiresIn:    s.ExpiresIn,
		User:         apiUser(s.User),
		// is_new_user 刻意不填：密码登录**不会**注册新账号（契约里首登即注册
		// 只属于验证码那条路）。填一个 false 与不填对客户端是同一件事，
		// 而不填让「这条路径不产生新用户」在代码里看得见。
	}
}

// apiUser 把库里那一行装成契约的 User。
func apiUser(u repository.User) api.User {
	out := api.User{
		Id:       u.ID,
		Nickname: u.Nickname,
	}
	if u.Phone != nil {
		masked := maskPhone(*u.Phone)
		out.Phone = &masked
	}
	if u.AvatarURL != nil {
		out.AvatarUrl = u.AvatarURL
	}
	gender := api.UserGender(u.Gender)
	out.Gender = &gender
	hasPassword := u.PasswordHash != nil
	out.HasPassword = &hasPassword
	if u.LastLoginAt != nil {
		out.LastLoginAt = u.LastLoginAt
	}
	created := u.CreatedAt
	out.CreatedAt = &created
	return out
}

// maskPhone 按契约的例子脱敏：138****8000。
//
// 为什么响应里不给完整号码：这个对象在登录之后会被存进本地缓存、被日志采集、
// 被前端的状态管理落盘。完整手机号是最值钱的一类个人信息，而前端拿它没有用处
// —— 契约的注释写着「完整号码只在绑定流程中由用户自己输入」。
//
// 短号码整串打码而不是照样露出前三后四：一个 7 位的号码脱敏之后还剩 7 位里的
// 7 位，等于没脱。这条路径在国内手机号上不会走到，但海外号码、测试数据会。
func maskPhone(phone string) string {
	const head, tail = 3, 4
	r := []rune(phone)
	if len(r) < head+tail+1 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:head]) + "****" + string(r[len(r)-tail:])
}

// writeAuthError 把 service 的业务错误翻成契约里那几种响应。
//
// 每一条都对应契约里明写的一个状态码；没对上的一律 500 —— 兜底分支不该
// 猜一个 4xx，那会把服务端的 bug 报成客户端的错，而客户端会照着这个错重试。
func writeAuthError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrBadRequest):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求参数不合法")
	case errors.Is(err, service.ErrInvalidCredentials):
		// 契约：401「验证码错误 / 密码错误 / 该账号未设置密码」。
		// 三者共用一条消息是刻意的，理由见 service.ErrInvalidCredentials。
		problem.Write(c, http.StatusUnauthorized,
			problem.TypeUnauthorized, "手机号或密码不正确")
	case errors.Is(err, service.ErrAccountDisabled):
		problem.Write(c, http.StatusForbidden,
			problem.TypeAccountDisabled, "账号已封禁或已注销")
	case errors.Is(err, service.ErrTokenTenantMismatch):
		// 与下面那条 401 分开：这串令牌本身是有效的，只是不属于这家店。
		// 合并的话，一次跨店尝试在日志与监控里会与一次普通的过期失效同形。
		problem.Write(c, http.StatusUnauthorized,
			problem.TypeTokenTenantMismatch, "这串令牌不属于本店")
	case errors.Is(err, service.ErrInvalidToken):
		problem.Write(c, http.StatusUnauthorized,
			problem.TypeUnauthorized, "令牌无效或已失效，请重新登录")
	case errors.Is(err, service.ErrSMSLoginUnavailable):
		// 501 而不是 401：这条路服务端还没通，不是用户输错了。
		// 回 401 的话客户端会让用户一遍遍重输验证码，而那串验证码根本没人发过。
		//
		// 契约允许这个状态码：/auth/login 的响应集合是 200 / 401 / 403 /
		// default，而 default 收的正是「其余一切，体是 Problem」。
		// 这笔账同时挂在 contract_test.go 的 NotYetImplementedBody 里。
		_ = c.Error(err)
		problem.Write(c, http.StatusNotImplemented, problem.TypeNotImplemented,
			"验证码登录尚未实现：本项目还没有接短信服务。请用密码登录")
	default:
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError,
			problem.TypeInternal, "服务内部错误")
	}
}
