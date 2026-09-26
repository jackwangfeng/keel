package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// GET /uploads/{upload_id}/blob —— 第一跳跳过去的那个限时地址。
//
// ===========================================================================
// 它为什么自己一个文件
// ===========================================================================
//
// 因为它要读 query 参数，而**第一跳登记着「这条接口一个 query 参数都没有」**。
// contract_test.go 那条对账按 HandlerFile 解析整份源码里的 c.Query 调用，
// 所以两跳同文件的话，那条登记会被判成「handler 读了一个契约里没有的参数」。
// 同一条纪律让 GET /admin/products 单独占了 admin_product_list.go
// （理由写在那个文件头上）。
//
// 这条路由**不在契约里**，在 contract_test.go 的 nonContractRoutes 里挂着账。
// 它与第一跳的分工写在 service/upload.go 的文件头：第一跳判归属、发一个
// 5 分钟后失效的地址；这一跳只认签名与过期时间，不判归属 —— 于是被转发
// 出去的地址自带寿命，而契约里那个固定形状永远是要过归属校验的那一个。

// Blob 实现 GET /api/v1/uploads/{upload_id}/blob（**不在契约里**，见文件头）。
func (h *UploadHandler) Blob(c *gin.Context) {
	id, ok := pathID(c, "upload_id")
	if !ok {
		return
	}
	// exp 与 sig 是**上一跳自己签出来的**参数，不是契约里的 query 参数
	// —— 它们整份契约里都不存在，这条路由也不在契约里。
	// 这正是这个文件单独存在的全部原因，见文件头。
	//
	// 两个都不做任何形状校验就交给 service：解不开 exp、签名对不上、
	// 过期了，在这条路上是同一件事（地址不能用），而分辨它们只会造出一个
	// 「这个签名是不是我们签的」的预言机。service.BlobFor 把三者合成一个
	// ErrUploadLinkInvalid。
	blob, err := h.svc.BlobFor(c.Request.Context(), id, c.Query("exp"), c.Query("sig"))
	if err != nil {
		writeUploadError(c, err)
		return
	}
	defer func() { _ = blob.Body.Close() }()

	// nosniff 不是可选的：这里吐的是**用户上传的字节**，而 content_type 是
	// 上传时客户端在 multipart part 头上声明的那个（admin_upload.go 刻意
	// 不嗅探内容）。也就是说「声明 image/png、内容是一段 HTML」是做得到的，
	// 而没有这个头时某些浏览器会按内容嗅探并把它当页面渲染 —— 那就是一个
	// 挂在本站域名下的存储型 XSS。
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Disposition", "inline")
	// 地址本身已经是限时的，所以可以让浏览器缓存到它过期为止；
	// 但**必须是 private**：这些字节属于某一家店，不该进任何共享缓存。
	c.Header("Cache-Control", "private, max-age=300")
	c.DataFromReader(http.StatusOK, blob.SizeBytes, blob.ContentType, blob.Body, nil)
}
