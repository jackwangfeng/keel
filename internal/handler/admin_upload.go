package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// POST /admin/uploads —— 后台上传商品图。
//
// ===========================================================================
// 这一轮做的是**完整的那一半还是元数据那一半**：完整的
// ===========================================================================
//
// 任务书允许两条路：真的把文件落盘，或者只接元数据那一半并在 contract_test.go
// 里显式挂账。选了前者，理由三条：
//
//	① **元数据那一半绕不开读整个文件。** size_bytes 与 sha256 都是 NOT NULL
//	   的列，也就是说服务端反正要把请求体从头读到尾。把 io.Copy 的目的地从
//	   io.Discard 换成一个真实文件是 30 行的事。
//	② **只登记元数据会在库里留下一条无法与正常记录区分的假记录。**
//	   uploads.storage_key 的语义是「driver 内部路径」；那条路径后面什么都没有
//	   这件事，表上没有任何一列说得出来。而 §13 建这张表的三个理由
//	   （归属校验、孤儿回收、迁移能力）全都建立在「这份清单是真的」之上。
//	③ **本地磁盘 driver 在 §13 里已经设计完了**，而且「不是新增组件，
//	   就是容器的一个卷」——它不引入 README 第一段那句「只有一个数据库」
//	   之外的任何东西。
//
// ===========================================================================
// 响应里那个 url 指向哪儿
// ===========================================================================
//
// 指向 `GET /uploads/{upload_id}`，而那条 **M4 收尾这一轮实现了**
// （handler/upload.go 与 handler/upload_blob.go）。它是买家侧接口（没有
// /admin/ 前缀），判完归属之后 302 到一个带签名与过期时间的站内地址。
//
// 这一段原先写的是「那条路由还没有实现，所以 url 打过去是 404」。留着一句
// 过期的「还没实现」，下一个人会照着它去找一个已经存在的东西 —— 这个仓库
// 为同一件事在 notYetRouted 上写过一模一样的话。
//
// 当时那句话里真正要紧的那一半仍然成立，记在这里：**字节是真的在磁盘上**，
// 所以补上读路由那天，此前登记的每一条记录都有用；而「只登记元数据」那条路
// 会让存量数据在补路由的那天全部作废。
//
// 另外两笔账仍然在，它们都是**设计上接受**的，不是遗漏：
//
//	· `referenced` 是单向布尔（§13 原话），被替换下来的旧图会永远停在 TRUE，
//	  不会被回收。一期接受这个磁盘增长。
//	· 孤儿回收本身（§12 的 jobs）还没有执行者，24 小时那条只写在设计里。

// maxUploadFormMemory 是 multipart 解析时留在内存里的上限。
//
// 取得比 10 MB 的文件上限小：超出的部分由 net/http 落到临时文件，
// 也就是说一个 10 MB 的上传不会在内存里占 10 MB。
// 真正挡住「请求体有多大」的是下面那道 MaxBytesReader，不是这个数。
const maxUploadFormMemory = 1 << 20

// CreateUpload 实现 POST /api/v1/admin/uploads。
func (h *AdminCatalogHandler) CreateUpload(c *gin.Context) {
	// 先限大小、再解析 —— 与 webhook 和 /search 那两处同一个顺序。
	//
	// 反过来的话，一个声称自己是 2 GB 的 multipart 会在「判断它超没超」
	// 之前就被 net/http 落进临时文件（或吃进内存）。
	// 余量给 1 MB：multipart 的边界、part 头、表单其余字段都在请求体里，
	// 而契约那个 10 MB 说的是**文件本身**。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body,
		service.MaxUploadBytes+(1<<20))

	if err := c.Request.ParseMultipartForm(maxUploadFormMemory); err != nil {
		// 请求体超了 MaxBytesReader 也会走这一支。两者都回 413 而不是 422：
		// 客户端要做的事是同一件（换一张小点的图），而契约在这条端点上
		// 给了 413。
		problem.Write(c, http.StatusRequestEntityTooLarge,
			problem.TypeUploadTooLarge,
			"请求体解析失败或超过上限（单文件不超过 10 MB）")
		return
	}

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		// 契约：requestBody 的 required 里只有 file。
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求体里没有 file 这一项")
		return
	}
	defer func() { _ = file.Close() }()

	// content_type 取 **part 头上声明的那个**，不从文件名后缀猜，也不嗅探内容。
	//
	// 不猜后缀：文件名是客户端随便写的。不嗅探：契约把 415 定成
	// 「content_type 不在允许列表内」——判据是声明值，而按嗅探结果判会让
	// 一个声明 image/png、内容却是 JPEG 的上传变成 201，
	// 与契约说的不是同一件事。
	contentType := header.Header.Get("Content-Type")

	up, replayed, err := h.svc.CreateUpload(c.Request.Context(), contentType, file, idemKeyOf(c))
	if err != nil {
		// MaxBytesReader 触发时 io.Copy 会带回一个 *http.MaxBytesError，
		// 而不是 service.ErrUploadTooLarge —— 两条路都要落到 413，
		// 否则一个超大文件会被报成 500。
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			problem.Write(c, http.StatusRequestEntityTooLarge,
				problem.TypeUploadTooLarge, "文件超过 10 MB")
			return
		}
		writeCatalogError(c, err)
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
