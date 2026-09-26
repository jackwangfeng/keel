package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// GET /uploads/{upload_id} —— 读文件（契约 Upload tag）。M4 收尾。
//
// ===========================================================================
// 它是**买家侧**接口，所以这个文件里一道后台鉴权都没有
// ===========================================================================
//
// 路径上没有 /admin/ 前缀，契约里也没给它 security —— 商品图本就公开。
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

// Redirect 实现 GET /api/v1/uploads/{upload_id}。
func (h *UploadHandler) Redirect(c *gin.Context) {
	id, ok := pathID(c, "upload_id")
	if !ok {
		return
	}
	target, err := h.svc.RedirectTarget(c.Request.Context(), id)
	if err != nil {
		writeUploadError(c, err)
		return
	}
	// 302 而不是 307/308：契约逐字写的就是 302，而且这条路上没有方法或请求体
	// 需要被保留（它只可能是 GET）。
	//
	// 跳转目标是**相对路径**。绝对 URL 要这个进程知道自己对外是什么 origin，
	// 而它不知道（反代、自定义域名、路径路由三种形态各不一样）——
	// 猜一个的结果是在自定义域名的店上把用户跳到别的域名去。
	// 同一条推理写在 service/payment_intent.go 的 settle.url 上。
	c.Header("Cache-Control", "no-store")
	c.Redirect(http.StatusFound, target)
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
