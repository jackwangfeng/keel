package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/problem"
)

// GET /uploads/{upload_id} —— 第一跳。它单独占这个文件，因为它读 query 参数 w（缩略图宽度，
// 2026-09-28），而 contract_test.go 按 HandlerFile 解析整份源码里的 c.Query：与 POST /uploads、
// GET /admin/uploads/{id} 同文件的话，那两条「没有 query 参数」的登记会当场红。
// 与 upload_blob.go、admin_product_list.go 单独成文件是同一条纪律。

// Redirect 实现 GET /api/v1/uploads/{upload_id}。
func (h *UploadHandler) Redirect(c *gin.Context) {
	id, ok := pathID(c, "upload_id")
	if !ok {
		return
	}
	// ?w= 缩略图宽度（契约：160 / 320 / 480 / 640 四档，其它值向上取档；不传即原图）。
	w := 0
	if raw := c.Query("w"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest,
				"w 必须是正整数（缩略图宽度，按 160 / 320 / 480 / 640 取档）")
			return
		}
		w = n
	}
	target, err := h.svc.RedirectTargetWidth(c.Request.Context(), id, w)
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
