package handler

import (
	"encoding/json"
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
		problem.WriteBindError(c, err)
		return
	}

	out, replayed, err := h.svc.OpenShop(c.Request.Context(), req.Code, req.Name,
		string(req.AdminEmail), idemKeyOf(c))
	if err != nil {
		writeMerchantError(c, err, "只有平台级管理员能开店")
		return
	}
	if replayed {
		// 幂等重放：店早就开好了，本次什么都没建，也没有新的登录链接可给。
		//
		// 响应里那三个凭据字段**必须缺席**，而不是回放第一次那一串：存档落在
		// idempotency_keys.response_body，而一次性凭据的明文不进数据库 ——
		// 存的是 repository.Merchant，压根不含 token。所以这里想回放也没有。
		markReplayed(c, true)
		c.JSON(http.StatusCreated, apiMerchantOpened(out.Merchant))
		return
	}

	// 新店第一个管理员的一次性登录凭据**进响应体**（契约 MerchantOpened）。
	// 这条与 POST /admin/staff 相反，差别是刻意的，理由写在契约那段：平台会话
	// 看不见商家的员工，重签那条对商家级 staff 回 404，而这家新店除了他没有别人。
	_ = c.Error(&shopAdminLinkNotice{Code: out.Merchant.Code, StaffID: out.Admin.ID})

	opened := apiMerchantOpened(out.Merchant)
	opened.AdminStaffId = &out.Admin.ID
	opened.AdminLoginToken = &out.LoginToken
	opened.AdminLoginTokenExpireAt = &out.LoginTokenExpireAt
	c.JSON(http.StatusCreated, opened)
}

// shopAdminLinkNotice 与 staffLoginLinkNotice 是同一件事的两处，
// 分开是因为运维要分得清「某家店加了个员工」和「开了一家新店」——
// 后者在日志里应当是显眼的。
//
// **它只打事实，不打 token 明文。** 这一轮之前这条日志是唯一的取用渠道
// （「没有邮件服务，只能打进日志」），所以它带着明文；现在凭据在 201 的响应体里，
// 日志留在这里的目的是「谁在什么时候开了哪家店、给谁签过登录凭据」，
// 那件事不需要明文就能说清。日志会被采集、会进索引、会复制到不知多少地方，
// 而一串 15 分钟的凭据落进任何一处索引里就是一个可被人翻出来的登录入口。
// 引导 token 是唯一的例外，它没有第二条渠道，见 service/staff.go 的 EnsureBootstrapAdmin。
type shopAdminLinkNotice struct {
	Code    string
	StaffID int64
}

func (n *shopAdminLinkNotice) Error() string {
	return "开店成功。已为它第一个管理员签出一次性登录链接 token，" +
		"明文在本次的 201 响应体里（admin_login_token），不进日志也不进数据库：" +
		"merchant_code=" + n.Code + " staff_id=" + strconv.FormatInt(n.StaffID, 10) +
		"（15 分钟有效，用掉即失效；重放这一次响应没有那三个字段）"
}

// apiMerchant 把库里那一行装成契约的 Merchant。
//
// 开店那条路的 **Domain 恒为 nil**，而这不是没实现：开店刻意不登记域名
// （00021 文件头「为什么不连 shop_settings 一起给」；域名那条写路径是 00340 之后
// 才有的，而它只开在 PATCH 这一条上）。契约里 domain 是可选
// 字段，缺席的含义正是「这家店还没绑自定义域名」—— 它走
// {code}.KEEL_BASE_DOMAIN。填一个空串是另一回事：那意味着
// 「绑了一个空域名」。
//
// 商家目录的读接口会填 Domain（从 merchant_domains 读）与 UpdatedAt（最新一行修订）。
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

// apiMerchantOpened 装开店那一条的 201（契约 MerchantOpened）。
//
// 它不是 Merchant 加三个字段的一个包装类型：契约用 allOf 把两者摊平成**一个扁平对象**
// （老客户端读 id / code / name 的代码不用改），而摊平之后生成的是一个独立结构体，
// 连 status 都是另一个枚举类型（MerchantOpenedStatus）。
//
// 所以这里从 apiMerchant 起建，而不是重抄一遍那七个字段：Domain 的「nil 就缺席」、
// UpdatedAt 的「没改过就不给」这两条规矩只有一份实现。抄第二份的那天，
// 这两处里的一处会和 GET / PATCH 分叉 —— 而三家店的 domain 在列表里有、在开店响应里没有，
// 客户端看不出哪个是对的。
//
// 那三个凭据字段由调用点往上加，且**只有真正建店那一次加**：重放时这里给的是空结构体，
// 因为存档里压根没有 token（一次性凭据的明文不进数据库）。
func apiMerchantOpened(m repository.Merchant) api.MerchantOpened {
	base := apiMerchant(m)
	return api.MerchantOpened{
		Id:        base.Id,
		Code:      base.Code,
		Name:      base.Name,
		Status:    api.MerchantOpenedStatus(base.Status),
		Domain:    base.Domain,
		CreatedAt: base.CreatedAt,
		UpdatedAt: base.UpdatedAt,
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

// adminMerchantPatchRequest 是 PATCH /admin/merchants/{merchant_id} 的请求体。
//
// domain 的「三个态」由 bindPatchBody 返回的键集合判，不是由这个结构体判：
// 契约在这里是三件事——**不传 = 不动**、**显式 null = 摘掉这条登记**、
// 字符串 = 登记成这家店的域名。而 `*json.RawMessage` 里的指针在 JSON null 时
// 被 encoding/json 置 nil（它压根不调 RawMessage 的 UnmarshalJSON），
// 于是前两者又塌回同一个 nil —— 用它的话每一次改名都会顺手把这家店的入口摘掉，
// 而摘掉的后果是它立刻对全部买家 404，响应却回 200。
// 同一个坑与同一个解法见 admin_product.go 的 brand_id、admin_catalog.go 的
// bindPatchBody。
//
// name / status 继续用普通指针：那两个字段「不传」与「传 null」在契约里是同一件事
// （null 不是合法值，两种写法都不动）。
type adminMerchantPatchRequest struct {
	Name   *string          `json:"name"`
	Status *int16           `json:"status"`
	Domain *json.RawMessage `json:"domain"`
}

// UpdateMerchant 实现 PATCH /api/v1/admin/merchants/{merchant_id}。
func (h *AdminMerchantHandler) UpdateMerchant(c *gin.Context) {
	id, ok := merchantIDParam(c)
	if !ok {
		return
	}
	var req adminMerchantPatchRequest
	present, bound := bindPatchBody(c, &req)
	if !bound {
		return
	}
	if v, ok := present["domain"]; ok {
		req.Domain = &v
	}
	in := repository.MerchantEdit{Name: req.Name, Status: req.Status}
	if req.Domain != nil {
		var d *string
		if err := json.Unmarshal(*req.Domain, &d); err != nil {
			problem.Write(c, http.StatusUnprocessableEntity,
				problem.TypeInvalidRequest, "domain 必须是字符串或 null")
			return
		}
		if d == nil {
			in.ClearDomain = true
		} else {
			in.Domain = d
		}
	}
	m, err := h.svc.Update(c.Request.Context(), id, in)
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
			"这是一套单商家部署（配了 KEEL_DEFAULT_MERCHANT）：不能有第二家活跃商家，"+
				"也不接受自有域名登记（单商家模式完全不解析 Host，登记了永远不会生效）。"+
				"要开多家店或绑域名，请切到多商家部署：清空 KEEL_DEFAULT_MERCHANT、配置 KEEL_BASE_DOMAIN")
	case errors.Is(err, service.ErrMerchantDomainTaken):
		problem.Write(c, http.StatusConflict, problem.TypeMerchantDomainTaken,
			"这个域名已经被另一家店登记了：一个域名只能是一家店的入口。"+
				"换一家店重试没有用，要改的是域名")
	case errors.Is(err, service.ErrMerchantNotFound):
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "商家不存在")
	case errors.Is(err, service.ErrPlatformOnly):
		problem.Write(c, http.StatusForbidden, problem.TypePlatformOnly, platformOnlyTitle)
	default:
		writeStaffError(c, err)
	}
}
