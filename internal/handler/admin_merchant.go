package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// POST /admin/merchants —— 开店（契约 Admin tag）。M4 Task 4。
//
// ===========================================================================
// 为什么它自己一个文件
// ===========================================================================
//
// contract_test.go 那条 query 参数对账按 HandlerFile 解析**整份源码**里的
// c.Query 调用，所以「这条接口一个 query 参数都没有」这句登记，是对整个文件
// 说的。它今天和 admin_auth.go 里那几条一样是 NoQueryParams，合在一起也不会
// 红；分开是因为它们是两个不同的东西：那个文件是后台身份（谁在操作），
// 这一条是平台运营（开一家新店），而它带着这个仓库里唯一一次
// 「在指定租户里开事务」的调用。
//
// ===========================================================================
// 这个文件里**一个 c.Query 都不许出现**
// ===========================================================================
//
// 理由同 admin_product.go 头上那句：routes 表里这条登记着 NoQueryParams，
// 而那条对账两个方向都锁。

// AdminMerchantHandler 是商家管理那一组（开店、详情、改名 / 停用 / 启用；
// 列表在 admin_merchant_list.go —— 它读 query 参数，按闸门要求自己一个文件）。
//
// 开店原先挂在 AdminAuthHandler 上、直接调 StaffService.OpenShop。现在它先过
// MerchantAdminService 那道单商家闸门：单商家部署里开出第二家店，
// 下一次重启就起不来（service/merchant_admin.go 的 ErrSingleMerchantMode）。
type AdminMerchantHandler struct{ svc *service.MerchantAdminService }

func NewAdminMerchantHandler(s *service.MerchantAdminService) *AdminMerchantHandler {
	return &AdminMerchantHandler{svc: s}
}

// OpenShop 实现 POST /api/v1/admin/merchants。
func (h *AdminMerchantHandler) OpenShop(c *gin.Context) {
	// 用契约生成的类型收请求体：MerchantCreateRequest 里只有 code / name /
	// admin_email 三个字段，**没有 status、没有 domain**。手写一个结构体的话，
	// 哪天有人顺手加上 status，编译器不会有任何意见 —— 而那是一条能直接开出
	// 一家「已审核通过」的店的路（审核流程本轮还不存在，将来补时没人会想起这里）。
	var req api.MerchantCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求体不是合法的 JSON")
		return
	}

	out, err := h.svc.OpenShop(c.Request.Context(), req.Code, req.Name, string(req.AdminEmail))
	if err != nil {
		writeMerchantError(c, err, "只有平台级管理员能开店")
		return
	}

	// 新店第一个管理员的一次性登录链接明文**不进响应体**：契约里 201 的
	// schema 是 Merchant，连 Staff 都没有。本轮没有邮件服务，所以它进日志，
	// 与 CreateStaff 走同一条出口（logHandlerErrors）。
	_ = c.Error(&shopAdminLinkNotice{
		Code: out.Merchant.Code, StaffID: out.Admin.ID, Token: out.LoginToken,
	})

	c.JSON(http.StatusCreated, apiMerchant(out.Merchant))
}

// shopAdminLinkNotice 与 staffLoginLinkNotice 是同一件事的两处，
// 分开是因为运维要分得清「某家店加了个员工」和「开了一家新店」——
// 后者在日志里应当是显眼的。
type shopAdminLinkNotice struct {
	Code    string
	StaffID int64
	Token   string
}

func (n *shopAdminLinkNotice) Error() string {
	return "开店成功。本项目没有接邮件服务，新店第一个管理员的一次性登录链接 token " +
		"只能打进日志：merchant_code=" + n.Code +
		" staff_id=" + strconv.FormatInt(n.StaffID, 10) + " token=" + n.Token +
		"（15 分钟有效，用掉即失效；接上 SMTP 之后这条就该消失）"
}

// apiMerchant 把库里那一行装成契约的 Merchant。
//
// **Domain 恒为 nil**，而这不是没实现：开店这条路刻意不写 shop_settings
// （00021 文件头「为什么不连 shop_settings 一起给」）。契约里 domain 是可选
// 字段，缺席的含义正是「这家店还没绑自定义域名」—— 它走
// {code}.KEEL_BASE_DOMAIN 或 /s/{code}。填一个空串是另一回事：那意味着
// 「绑了一个空域名」。
//
// 商家目录的读接口会填 Domain（从 shop_settings 读）与 UpdatedAt（最新一行修订）。
func apiMerchant(m repository.Merchant) api.Merchant {
	return api.Merchant{
		Id:        m.ID,
		Code:      m.Code,
		Name:      m.Name,
		Status:    api.MerchantStatus(m.Status),
		CreatedAt: m.CreatedAt,
		Domain:    m.Domain,
		UpdatedAt: m.RevisedAt,
	}
}

// GetMerchant 实现 GET /api/v1/admin/merchants/{merchant_id}。
func (h *AdminMerchantHandler) GetMerchant(c *gin.Context) {
	id, ok := merchantIDParam(c)
	if !ok {
		return
	}
	m, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		writeMerchantError(c, err, "只有平台级操作员能看商家目录")
		return
	}
	c.JSON(http.StatusOK, apiMerchant(m))
}

// UpdateMerchant 实现 PATCH /api/v1/admin/merchants/{merchant_id}。
func (h *AdminMerchantHandler) UpdateMerchant(c *gin.Context) {
	id, ok := merchantIDParam(c)
	if !ok {
		return
	}
	// 契约生成的类型：只有 name 与 status。没有 code（改 code 等于改这家店的域名，
	// 本轮不开这条路），也没有 domain。
	var req api.MerchantUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求体不是合法的 JSON")
		return
	}
	var status *int16
	if req.Status != nil {
		v := int16(*req.Status)
		status = &v
	}
	m, err := h.svc.Update(c.Request.Context(), id, req.Name, status)
	if err != nil {
		writeMerchantError(c, err, "只有平台级管理员能改商家")
		return
	}
	c.JSON(http.StatusOK, apiMerchant(m))
}

func merchantIDParam(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("merchant_id"), 10, 64)
	if err != nil || id <= 0 {
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "merchant_id 必须是正整数")
		return 0, false
	}
	return id, true
}

// writeMerchantError 在 writeStaffError 之前挑出商家管理自己的两种错误。
// platformOnlyTitle 按接口给：同一个 403 在开店与看列表时该说的话不一样。
func writeMerchantError(c *gin.Context, err error, platformOnlyTitle string) {
	switch {
	case errors.Is(err, service.ErrSingleMerchantMode):
		problem.Write(c, http.StatusConflict, problem.TypeSingleMerchantMode,
			"这是一套单商家部署（配了 KEEL_DEFAULT_MERCHANT），不能有第二家活跃商家："+
				"新开或启用的店谁也访问不到，而且下一次重启会因为启动自检失败而起不来。"+
				"要开多家店，请切到多商家部署：清空 KEEL_DEFAULT_MERCHANT、配置 KEEL_BASE_DOMAIN")
	case errors.Is(err, service.ErrMerchantNotFound):
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "商家不存在")
	case errors.Is(err, service.ErrPlatformOnly):
		problem.Write(c, http.StatusForbidden, problem.TypePlatformOnly, platformOnlyTitle)
	default:
		writeStaffError(c, err)
	}
}
