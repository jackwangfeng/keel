package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// AdminAuthHandler 实现 /admin/auth/* 三条与 /admin/me、/admin/staff* 三条。
//
// 一个 handler 而不是两个：它们共用同一个 service、同一张错误映射表，
// 而那张表是这个文件最要紧的东西 —— 分成两份之后，两边对同一个业务错误
// 给出不同状态码的那天不会有任何东西变红。
type AdminAuthHandler struct{ svc *service.StaffService }

func NewAdminAuthHandler(s *service.StaffService) *AdminAuthHandler {
	return &AdminAuthHandler{svc: s}
}

type adminBootstrapRequest struct {
	Token string `json:"token"`
	Email string `json:"email"`
}

type adminEmailLinkRequest struct {
	Email string `json:"email"`
}

type adminSessionRequest struct {
	Token string `json:"token"`
}

// adminStaffPatchRequest 是 PATCH /admin/staff/{staff_id} 的请求体。
//
// 两个字段都是指针：契约里它是 minProperties: 1 的部分更新，
// 而「没给」与「给了 0」在这条接口上是两件完全不同的事 ——
// status 给 0 是一个不存在的状态（枚举只有 1/2），没给是「别动它」。
// 用值类型的话两者都是 0，于是每一次只改 role 的 PATCH 都会顺手把 status
// 判成非法，或者（更糟）把它当成一个有效值写进去。
//
// region_ids / store_ids 用 *[]int64：「没给」是不动范围，「给了 []」是清空，
// 两者在这条接口上是两件事（契约：给了就是整体替换）。
type adminStaffPatchRequest struct {
	Role      *int16   `json:"role"`
	Status    *int16   `json:"status"`
	RegionIDs *[]int64 `json:"region_ids"`
	StoreIDs  *[]int64 `json:"store_ids"`
}

// Bootstrap 实现 POST /api/v1/admin/auth/bootstrap。
//
// 未认证接口（契约里 security: []）。它凭什么能被调用，写在
// service.StaffService.EnsureBootstrapAdmin 的注释里 —— 一句话：
// 它不建账号，只拿一串在进程启动时打进 stdout 的一次性 token 换会话。
func (h *AdminAuthHandler) Bootstrap(c *gin.Context) {
	var req adminBootstrapRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		problem.WriteBindError(c, err)
		return
	}
	sess, err := h.svc.Bootstrap(c.Request.Context(), req.Token, req.Email)
	if err != nil {
		writeStaffError(c, err)
		return
	}
	c.JSON(http.StatusOK, staffSessionResponse(sess))
}

// EmailLink 实现 POST /api/v1/admin/auth/email-link。
//
// 本轮它回 501，理由完整写在 service.StaffService.RequestEmailLink 上：
// 没有邮件服务，而 202「已受理」是一句在邮件服务上线之前都不会被纠正的假话。
// 这笔账挂在 contract_test.go 的 NotYetImplementedBody 里，
// 由 admin_auth_test.go 的 TestAdminEmailLinkSaysItIsNotImplemented 两个方向都锁。
func (h *AdminAuthHandler) EmailLink(c *gin.Context) {
	var req adminEmailLinkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		problem.WriteBindError(c, err)
		return
	}
	if err := h.svc.RequestEmailLink(c.Request.Context(), req.Email); err != nil {
		writeStaffError(c, err)
		return
	}
	c.Status(http.StatusAccepted)
}

// Session 实现 POST /api/v1/admin/auth/session。
func (h *AdminAuthHandler) Session(c *gin.Context) {
	var req adminSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		problem.WriteBindError(c, err)
		return
	}
	sess, err := h.svc.ExchangeEmailLink(c.Request.Context(), req.Token)
	if err != nil {
		writeStaffError(c, err)
		return
	}
	c.JSON(http.StatusOK, staffSessionResponse(sess))
}

// Me 实现 GET /api/v1/admin/me。
func (h *AdminAuthHandler) Me(c *gin.Context) {
	st, err := h.svc.Me(c.Request.Context())
	if err != nil {
		writeStaffError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiStaff(st))
}

// CreateStaff 实现 POST /api/v1/admin/staff。
func (h *AdminAuthHandler) CreateStaff(c *gin.Context) {
	var req api.StaffCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		problem.WriteBindError(c, err)
		return
	}
	// 用契约生成的类型收请求体：**它根本没有 merchant_id 这个字段**
	// （StaffCreateRequest 的描述逐字写着「没有 merchant_id 字段，这是刻意的」）。
	// 手写一个结构体的话，哪天有人顺手加上那一列，编译器不会有任何意见。
	name := ""
	if req.Name != nil {
		name = *req.Name
	}
	var scopes repository.StaffScopes
	if req.RegionIds != nil {
		scopes.RegionIDs = *req.RegionIds
	}
	if req.StoreIds != nil {
		scopes.StoreIDs = *req.StoreIds
	}
	out, replayed, err := h.svc.CreateStaff(c.Request.Context(),
		string(req.Email), name, int16(req.Role), scopes, idemKeyOf(c))
	if err != nil {
		writeStaffError(c, err)
		return
	}
	if replayed {
		// 幂等重放：本次没有建人，也没有签登录链接（存档里只有 Staff，
		// 一次性凭据的明文不进数据库）。第一次那串已经进过日志。
		markReplayed(c, true)
		c.JSON(http.StatusCreated, apiStaff(out.Staff))
		return
	}

	// 一次性登录链接的明文**不进响应体**：契约里 201 的 schema 是 Staff，
	// 没有这个字段；而且把它回给创建者等于让任何一个管理员都能冒充他刚建的
	// 那个人。本轮没有邮件服务，所以它进进程日志 —— 与引导 token 同一个办法。
	//
	// 用 c.Error 而不是直接 log：这个包里所有需要出现在日志里的东西都走
	// logHandlerErrors 那条统一出口（internal/app/errorlog.go）。
	_ = c.Error(&staffLoginLinkNotice{StaffID: out.Staff.ID, Token: out.LoginToken})

	c.JSON(http.StatusCreated, apiStaff(out.Staff))
}

// UpdateStaff 实现 PATCH /api/v1/admin/staff/{staff_id}。
func (h *AdminAuthHandler) UpdateStaff(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("staff_id"), 10, 64)
	if err != nil || id <= 0 {
		// 契约里 staff_id 是 int64。一个非数字的路径参数不是「没找到」，
		// 是请求本身不合法 —— 但这条接口的响应集合里没有 422 之外的选择，
		// 而 404 会让「/admin/staff/abc」看起来像一个存在过的资源。
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "staff_id 必须是正整数")
		return
	}
	var req adminStaffPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		problem.WriteBindError(c, err)
		return
	}
	st, err := h.svc.UpdateStaff(c.Request.Context(), id, req.Role, req.Status,
		req.RegionIDs, req.StoreIDs)
	if err != nil {
		writeStaffError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiStaff(st))
}

// ReissueLoginToken 实现 POST /api/v1/admin/staff/{staff_id}/login-token。
//
// 与 CreateStaff 的差别只有一处：token 明文**进响应体**（契约 StaffLoginToken）。
// 理由写在契约的描述里 —— 没有邮件服务时，管理员拿到它才能交给本人；签发人
// 本来就能改这个人的角色与状态（权限判据与 PATCH 同一个）。日志照旧打一份，
// 让「谁在什么时候给谁签过」查得出来。
func (h *AdminAuthHandler) ReissueLoginToken(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("staff_id"), 10, 64)
	if err != nil || id <= 0 {
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "staff_id 必须是正整数")
		return
	}
	out, err := h.svc.ReissueLoginToken(c.Request.Context(), id)
	if err != nil {
		writeStaffError(c, err)
		return
	}
	_ = c.Error(&staffLoginLinkNotice{StaffID: out.StaffID, Token: out.Token, Reissued: true})
	c.JSON(http.StatusCreated, api.StaffLoginToken{
		StaffId: out.StaffID, Token: out.Token, ExpireAt: out.ExpireAt,
	})
}

// staffLoginLinkNotice 是「这串一次性登录链接 token 本轮只能进日志」这件事的
// 载体。做成一个类型而不是一句 fmt.Sprintf，是为了让它在日志里认得出来 ——
// 运维要能一眼找到它，而且要知道它为什么在那里。
type staffLoginLinkNotice struct {
	StaffID int64
	Token   string
	// Reissued 为 true 表示这是给已有员工重签的（POST /admin/staff/{id}/login-token），
	// 不是新建时那一串。运维追查时这两件事要分得开。
	Reissued bool
}

func (n *staffLoginLinkNotice) Error() string {
	who := "新员工"
	if n.Reissued {
		who = "重签给已有员工"
	}
	return "本项目没有接邮件服务，" + who + "的一次性登录链接 token 只能打进日志：" +
		"staff_id=" + strconv.FormatInt(n.StaffID, 10) + " token=" + n.Token +
		"（15 分钟有效，用掉即失效；接上 SMTP 之后这条就该消失）"
}

// apiStaff 把库里那一行装成契约的 Staff。
//
// 用生成类型而不是手写结构体（同 apiUser 的理由）：契约改个字段名，
// 这里当场编译失败，而不是等线上客户端解析失败。
func apiStaff(st repository.Staff) api.Staff {
	out := api.Staff{
		Id:        st.ID,
		Email:     openapi_types.Email(st.Email),
		Role:      api.StaffRole(st.Role),
		Status:    api.StaffStatus(st.Status),
		CreatedAt: st.CreatedAt,
		// MerchantId 在契约里是 type: [integer, 'null'] 且**必填**：
		// 为 null 表示平台级操作员。所以这里原样传指针 —— nil 会序列化成
		// `"merchant_id": null`，而不是让这个字段消失。
		// 两者对客户端是两件事：缺席意味着「这个字段还没实现」，
		// null 意味着「这个人不属于任何一家店」。
		MerchantId: st.MerchantID,
		// 契约里两者都是必返的数组：没有范围是 []，不是 null 也不是缺席。
		RegionIds: nonNilIDs(st.RegionIDs),
		StoreIds:  nonNilIDs(st.StoreIDs),
	}
	if st.Name != "" {
		name := st.Name
		out.Name = &name
	}
	if st.LastLoginAt != nil {
		out.LastLoginAt = st.LastLoginAt
	}
	return out
}

func staffSessionResponse(s service.StaffSessionResult) api.StaffSession {
	return api.StaffSession{
		Token:    s.Token,
		ExpireAt: s.ExpireAt,
		Staff:    apiStaff(s.Staff),
	}
}

// writeStaffError 把 service 的业务错误翻成契约里那几种响应。
//
// 每一条都对应契约里明写的一个状态码；没对上的一律 500 —— 兜底分支不该猜一个
// 4xx，那会把服务端的 bug 报成客户端的错，而客户端会照着这个错重试。
func writeStaffError(c *gin.Context, err error) {
	if writePermissionError(c, err) || writeAdminIdempotencyError(c, err) {
		return
	}
	switch {
	case errors.Is(err, service.ErrStaffBadRequest):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求参数不合法")
	case errors.Is(err, service.ErrStaffTokenInvalid):
		// 契约：bootstrap 与 session 两条的 401「token 无效、已过期或已被使用」。
		// 三种成因共用一条消息是刻意的：分辨它们只会造出一个探针，
		// 而对调用方它们是同一件事 —— 重新拿一串 token。
		problem.Write(c, http.StatusUnauthorized,
			problem.TypeUnauthorized, "token 无效、已过期或已被使用")
	case errors.Is(err, service.ErrBootstrapClosed):
		// 契约：409「已经有 staff 了，引导通道已关闭」。
		problem.Write(c, http.StatusConflict,
			problem.TypeBootstrapClosed, "引导通道已关闭：这个部署已经有平台级管理员了")
	case errors.Is(err, service.ErrStaffForbidden):
		// 契约：403「调用者不是管理员」。
		problem.Write(c, http.StatusForbidden,
			problem.TypeStaffForbidden, "需要管理员权限")
	case errors.Is(err, service.ErrStaffNotFound):
		// 契约 /admin/ 那一段的约定 3：查不到当前租户名下的那一个一律 404，
		// 不是 403 —— 「这个 id 存在但不是你的」本身就是一条不该泄露的信息。
		problem.Write(c, http.StatusNotFound,
			problem.TypeNotFound, "操作员不存在")
	case errors.Is(err, service.ErrStaffEmailTaken):
		problem.Write(c, http.StatusConflict,
			problem.TypeStaffEmailTaken, "该邮箱在本租户内已经存在")
	case errors.Is(err, service.ErrPlatformOnly):
		// 契约 POST /admin/merchants：403「调用者不是平台级管理员」。
		// 与上面那条 staff-forbidden 分开报，理由写在 problem.TypePlatformOnly 上。
		problem.Write(c, http.StatusForbidden,
			problem.TypePlatformOnly, "只有平台级管理员能开店")
	case errors.Is(err, repository.ErrMerchantCodeTaken):
		// 契约 POST /admin/merchants：409「code 已被占用」。
		//
		// 从 repository 的 sentinel 直接翻，不在 service 那一层再定义一个 ——
		// 理由与 admin_catalog.go 头上那段一字不差：两张会分叉的映射表，
		// 分叉的那天某一条 409 会变成 404，而没有任何东西会红。
		problem.Write(c, http.StatusConflict,
			problem.TypeMerchantCodeTaken, "这个 code 已经有店在用了")
	case errors.Is(err, service.ErrStaffDisabled):
		// 契约 POST /admin/staff/{id}/login-token：409 staff-disabled。先启用再签。
		problem.Write(c, http.StatusConflict,
			problem.TypeStaffDisabled, "这个员工已停用，先启用再重签登录 token")
	case errors.Is(err, service.ErrLastAdmin):
		// 契约：409「会导致该租户没有在职管理员」。数据模型 §14 那条
		// 进不了数据库的约束，只能在业务逻辑里拦。
		problem.Write(c, http.StatusConflict,
			problem.TypeLastAdmin, "不能把最后一个在职管理员降级或停用")
	case errors.Is(err, service.ErrEmailServiceUnavailable):
		// 501 而不是 202：这条路服务端还没通。回 202 的话操作员会去收件箱
		// 等一封永远不来的信，而服务端这一侧没有任何东西显示出问题。
		//
		// 契约允许这个状态码：/admin/auth/email-link 的响应集合是 202 /
		// 429 / default，而 default 收的正是「其余一切，体是 Problem」。
		// 这笔账同时挂在 contract_test.go 的 NotYetImplementedBody 里。
		_ = c.Error(err)
		problem.Write(c, http.StatusNotImplemented, problem.TypeNotImplemented,
			"邮箱登录链接尚未实现：本项目还没有接邮件服务。"+
				"引导 token 在进程启动日志里，新员工的登录链接在建号时打进日志")
	default:
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError,
			problem.TypeInternal, "服务内部错误")
	}
}

// nonNilIDs 让 nil 切片序列化成 []：encoding/json 把 nil 切片写成 null，
// 而契约把 region_ids / store_ids 定成必返的数组。
func nonNilIDs(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}
