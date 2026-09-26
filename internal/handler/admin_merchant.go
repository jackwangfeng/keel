package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
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

// OpenShop 实现 POST /api/v1/admin/merchants。
func (h *AdminAuthHandler) OpenShop(c *gin.Context) {
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
		writeStaffError(c, err)
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
func apiMerchant(m repository.Merchant) api.Merchant {
	return api.Merchant{
		Id:        m.ID,
		Code:      m.Code,
		Name:      m.Name,
		Status:    api.MerchantStatus(m.Status),
		CreatedAt: m.CreatedAt,
	}
}
