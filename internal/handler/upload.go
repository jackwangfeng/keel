package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// GET /uploads/{upload_id} —— 读文件（契约 Upload tag）。M4 收尾。
//
// ===========================================================================
// 这个文件里的四条接口
// ===========================================================================
//
//	GET  /uploads/{upload_id}         读文件，买家侧，鉴权可选（auth.OptionalBearer）
//	POST /uploads                     买家上传头像 / 退款凭证（auth.Bearer）
//	GET  /admin/uploads/{upload_id}   后台客服读文件（staffAuth）
//	（GET /uploads/{upload_id}/blob 在 upload_blob.go，理由见下）
//
// 四条都没有契约里的 query 参数，所以可以同处一个文件（contract_test.go 按文件对账）。
// 鉴权全在路由上（internal/app/app.go），这个文件里一行判权都没有 —— 判权在 service。
//
// ===========================================================================
// GET /uploads/{upload_id} 是**买家侧**接口，鉴权是可选的
// ===========================================================================
//
// 路径上没有 /admin/ 前缀，契约里给它的 security 是「匿名或买家令牌」——
// 商品图与头像本就公开；退款凭证只有上传者本人（带自己的令牌）能读。
// 「一个租户的人不能读到另一个租户的文件」不是靠鉴权给出的，是靠租户中间件
// 解出来的那个 merchant_id 加 uploads 上那条 RLS 策略：别家店的那一行在这条
// 连接上根本读不出来，于是它与「这个 id 不存在」是同一个 404。
//
// ===========================================================================
// 两条路由，第二条刻意不在契约里
// ===========================================================================
//
//	GET /uploads/:upload_id        契约里那一条。判归属，302 到下面那个地址。
//	GET /uploads/:upload_id/blob   限时地址本身。只认签名与过期时间。
//
// 第二条在 **upload_blob.go**，而那不是随手分的文件：contract_test.go 的
// routes 表在这条接口上登记着 NoQueryParams，而那条对账按 HandlerFile
// 解析**整份源码**里的 c.Query 调用 —— 第二跳要读 exp 与 sig 两个 query
// 参数（它自己签出来的，不是契约里的），写在同一个文件里会让这条登记
// 当场红。同一条纪律让 GET /admin/products 单独占了 admin_product_list.go。
//
// 第二条也不在契约里（nonContractRoutes 里挂着账）：契约描述的是「跳到
// driver 生成的限时地址」，而那个地址的形状随 driver 变 —— S3 driver 跳的
// 是别人家的域名，写进契约等于把本地磁盘这一种形态钉死。
//
// 两跳各自挡什么，写在 service/upload.go 的文件头。

// UploadHandler 实现那两跳。
type UploadHandler struct{ svc *service.UploadService }

func NewUploadHandler(s *service.UploadService) *UploadHandler {
	return &UploadHandler{svc: s}
}

// AdminRedirect 实现 GET /api/v1/admin/uploads/{upload_id}（后台客服读文件）。
// 响应形状与 Redirect 相同：302 到同一种限时地址。判权在 service.AdminRedirectTarget。
func (h *UploadHandler) AdminRedirect(c *gin.Context) {
	id, ok := pathID(c, "upload_id")
	if !ok {
		return
	}
	target, err := h.svc.AdminRedirectTarget(c.Request.Context(), id)
	if err != nil {
		if writePermissionError(c, err) {
			return
		}
		writeUploadError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Redirect(http.StatusFound, target)
}

// Create 实现 POST /api/v1/uploads（买家上传头像或退款凭证）。
//
// 请求体的解析与 POST /admin/uploads 逐条相同（先限大小再解析、content_type 取 part 头
// 上声明的那个、MaxBytesError 也落到 413），理由写在 admin_upload.go 上，这里不重复。
// 多出来的只有 purpose 这一个表单字段。
func (h *UploadHandler) Create(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body,
		service.MaxUploadBytes+(1<<20))
	if err := c.Request.ParseMultipartForm(maxUploadFormMemory); err != nil {
		problem.Write(c, http.StatusRequestEntityTooLarge,
			problem.TypeUploadTooLarge,
			"请求体解析失败或超过上限（单文件不超过 10 MB）")
		return
	}
	purpose, err := strconv.ParseInt(strings.TrimSpace(c.Request.FormValue("purpose")), 10, 16)
	if err != nil {
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "purpose 必填，取值 2 头像或 3 退款凭证")
		return
	}
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求体里没有 file 这一项")
		return
	}
	defer func() { _ = file.Close() }()

	up, replayed, err := h.svc.CreateBuyerUpload(c.Request.Context(), int16(purpose),
		header.Header.Get("Content-Type"), file, idemKeyOf(c))
	if err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig), errors.Is(err, service.ErrUploadTooLarge):
			problem.Write(c, http.StatusRequestEntityTooLarge,
				problem.TypeUploadTooLarge, "文件超过 10 MB")
		case errors.Is(err, service.ErrUploadMediaType):
			problem.Write(c, http.StatusUnsupportedMediaType,
				problem.TypeUploadUnsupportedMedia,
				"只接受 image/jpeg、image/png、image/webp")
		case writeAdminIdempotencyError(c, err):
		case errors.Is(err, service.ErrCatalogBadRequest):
			writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest,
				"请求参数不合法", err)
		default:
			_ = c.Error(err)
			problem.Write(c, http.StatusInternalServerError,
				problem.TypeInternal, "服务内部错误")
		}
		return
	}
	markReplayed(c, replayed)
	c.JSON(http.StatusCreated, api.Upload{
		Id:          up.ID,
		Url:         service.UploadURL(up.ID),
		ContentType: up.ContentType,
		SizeBytes:   up.SizeBytes,
		CreatedAt:   up.CreatedAt,
	})
}

// writeUploadError 把读文件那两跳的失败翻成契约里那几种响应。
//
// 只有三种，而它们的分界线是这条接口最容易写错的地方：
//
//	404  「不存在」与「是别家店的」合成同一个 —— upload id 是全局自增的，
//	      两者一旦分开报，这个接口就成了一个能数出别家店传了多少文件的探测器。
//	403  用途不允许（典型：别人的退款凭证）。契约明写这里**不能**用 404 掩盖：
//	      「文件 id 是自增的，用 404 掩盖存在性并不能阻止枚举，反而让合法用户
//	      分不清『没权限』和『传错了 id』」。
//	404  限时地址无效 / 过期 / 被改过。回 404 而不是 403 —— 那条地址不在契约里，
//	      对客户端唯一正确的动作是回到 /uploads/{id} 重新要一个，
//	      而 403 会让它以为是权限问题去重新登录。
func writeUploadError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, repository.ErrUploadNotFound):
		problem.Write(c, http.StatusNotFound,
			problem.TypeUploadNotFound, "文件不存在或已被清理")

	case errors.Is(err, service.ErrUploadForbidden):
		problem.Write(c, http.StatusForbidden,
			problem.TypeUploadForbidden, "无权读取该文件")

	case errors.Is(err, service.ErrUploadLinkInvalid):
		problem.Write(c, http.StatusNotFound,
			problem.TypeNotFound, "这个文件地址无效或已过期，请重新获取")

	case errors.Is(err, service.ErrUploadBlobMissing):
		// 元数据在、字节不在：卷没挂上、有人手工删过文件、或者换 driver 时
		// 存量没搬完。**这是一次运维事故，不是一次正常的 404**，所以它进
		// c.Error（日志看得见）并回 500 —— 报成 404 的话，一次挂错卷的部署
		// 会长得和「用户传的图过期被回收了」一模一样。
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError,
			problem.TypeInternal, "服务内部错误")

	case errors.Is(err, service.ErrCatalogBadRequest):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求参数不合法")

	default:
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError,
			problem.TypeInternal, "服务内部错误")
	}
}
