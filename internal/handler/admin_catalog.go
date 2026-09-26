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

// 商家自助发布那 16 条写接口共用的东西：一个 handler 类型、**一张**错误映射表，
// 以及几个把领域类型装成契约类型的函数。M4 Task 3。
//
// ===========================================================================
// 为什么错误映射只能有一张表
// ===========================================================================
//
// 与 admin_auth.go 那一处是同一条理由的加强版：这 16 条接口共用同一批业务
// 错误（ErrCatalogNotFound 出现在其中 11 条上），而契约对它们的状态码要求
// 完全一致。分成几份之后，两边对同一个业务错误给出不同状态码的那天不会有
// 任何东西变红 —— 而这里有两处混掉会直接造出无限重试：
//
//	ErrSKUNotInTenant       → 404，重试**永远**不会成功
//	ErrInventoryPrecondition→ 409 + current，刷新那一格再试**会**成功
//
// repository 已经在一条 SQL 语句里把这两种 rows_affected = 0 分开回传了
// （db/queries/admin_skus.sql 的 SetInventoryByCAS，两个 CTE 一个快照）。
// 那件事做对了，而这张表做错的话前面全白做。
//
// ===========================================================================
// 文件怎么分：由 contract_test.go 的参数对账决定，不由行数决定
// ===========================================================================
//
// contract_test.go 的 queryParamsReadByHandler 按 route.HandlerFile 解析**整份
// 源码**里的 c.Query 调用，而这 16 条里只有 GET /admin/products 有 query 参数，
// 另外 15 条在 routes 表里登记着 NoQueryParams。把它们放进同一个文件的话，
// 那 15 条会被判成「handler 读了一个契约里没有的参数」——
// 而那条断言正是用来抓真实漂移的，不该为文件划分让路（理由与
// admin_staff_list.go 单独成文件一字不差）。
//
// 于是：GET /admin/products 在 admin_product_list.go 里，别的按域分三个文件，
// 这里放共用的部分。

// AdminCatalogHandler 实现契约 Admin + Catalog 两个 tag 的 16 条写接口。
type AdminCatalogHandler struct{ svc *service.AdminCatalogService }

func NewAdminCatalogHandler(s *service.AdminCatalogService) *AdminCatalogHandler {
	return &AdminCatalogHandler{svc: s}
}

// pathID 取一个路径参数并要求它是正整数。
//
// 非数字的路径参数回 422 而不是 404，理由与 admin_auth.go 的 UpdateStaff
// 一字不差：`/admin/skus/abc` 不是「没找到」，而 404 会让它看起来像一个
// 存在过的资源。契约在这几条上都有 422。
func pathID(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, name+" 必须是正整数")
		return 0, false
	}
	return id, true
}

// idemKeyOf 取那个必填的请求头。空串交给 service 去拒 —— 判据是业务规则
// （哪些接口要幂等），而 handler 不该有第二份那张清单。
func idemKeyOf(c *gin.Context) string { return c.GetHeader(idempotencyKeyHeader) }

// markReplayed 在这是一次幂等重放时给响应加上契约那个头。
//
// 一个函数而不是五处 if：契约明写它影响埋点与提示文案，而漏掉其中一处的症状
// 是「这条接口的重放被客户端记成一次新建」—— 没有任何东西会红。
func markReplayed(c *gin.Context, replayed bool) {
	if replayed {
		c.Header(idempotencyReplayedHeader, "true")
	}
}

// bindJSON 收请求体。解不开一律 422（契约里这几条都有 422）。
func bindJSON(c *gin.Context, dst any) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求体不是合法的 JSON")
		return false
	}
	return true
}

// writeCatalogError 把业务错误翻成契约里那几种响应。
//
// 每一条都对应契约里明写的一个状态码 + Problem type；没对上的一律 500 ——
// 兜底分支不该猜一个 4xx，那会把服务端的 bug 报成客户端的错，
// 而客户端会照着这个错重试。
//
// **顺序要紧的只有一处**：*repository.InventoryConflict 的 errors.As 要排在
// errors.Is(ErrInventoryPrecondition) 前面 —— 后者是前者的 Unwrap 目标，
// 反过来写的话 409 仍然是 409，但响应体里那个 current 会消失，
// 而契约把它定成必填（InventoryConflict schema）。客户端拿不到当前值，
// 就只能自己再查一次 —— 那一跳正是这个字段存在要省掉的东西。
func writeCatalogError(c *gin.Context, err error) {
	var conflict *repository.InventoryConflict
	switch {
	// —— 幂等那一组（M4 收尾）。契约给后台那 5 条 POST 声明的就是这三种。
	case errors.Is(err, service.ErrIdempotencyKeyMissing):
		// 契约把 Idempotency-Key 定成 required。422 而不是 400：
		// 这几条接口的错误集合里有 422 没有 400，而「必填的东西没给」
		// 正是 422 说的那件事。
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "缺少必填的 Idempotency-Key 请求头")

	case errors.Is(err, service.ErrIdempotencyInFlight):
		// 契约：409 + Retry-After，客户端应退避重试，**不要当成业务失败弹窗**。
		// Retry-After 在 problem.Write 之前设：那个函数会 Abort。
		c.Header("Retry-After", strconv.Itoa(retryAfterSeconds))
		problem.Write(c, http.StatusConflict, problem.TypeIdempotencyKeyInFlight,
			"这个 Idempotency-Key 正在处理中，请稍后重试")

	case errors.Is(err, service.ErrIdempotencyKeyReused):
		// 契约那个 422。§12 原话：这类失败必须显式，不能被当成重放静默吞掉。
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeIdempotencyKeyReused,
			"同一个 Idempotency-Key 配了不同的请求体")

	case errors.Is(err, service.ErrCatalogBadRequest):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求参数不合法")

	case errors.Is(err, service.ErrUploadTooLarge):
		problem.Write(c, http.StatusRequestEntityTooLarge,
			problem.TypeUploadTooLarge, "文件超过 10 MB")

	case errors.Is(err, service.ErrUploadMediaType):
		problem.Write(c, http.StatusUnsupportedMediaType,
			problem.TypeUploadUnsupportedMedia,
			"只接受 image/jpeg、image/png、image/webp")

	// —— 404 这一组。契约 /admin/ 段头的约定 3：查不到当前租户名下的那一个
	// 一律 404 而不是 403 —— 403 会让自增 id 空间变成一个跨租户的存在性探针。
	case errors.Is(err, repository.ErrCatalogNotFound):
		problem.Write(c, http.StatusNotFound,
			problem.TypeNotFound, "目标不存在或不属于当前租户")

	case errors.Is(err, repository.ErrSKUNotInTenant):
		// **它与下面那条 409 是这个文件最要紧的一对。**
		// 把「不是你的 SKU」报成 409，调用方会以为重读一次再试就能成功，
		// 而那个循环永远不会结束（契约在那条端点上明写了这一条）。
		problem.Write(c, http.StatusNotFound,
			problem.TypeNotFound, "SKU 不存在、不属于当前租户，或已被软删")

	// —— 409 这一组，按 Problem type 区分，状态码分不开它们。
	case errors.As(err, &conflict):
		// CAS 对不上。响应体是 InventoryConflict（Problem + 必填的 current），
		// 用生成类型而不是手拼一个 map：契约改字段名时这里当场编译失败。
		problem.WriteValue(c, http.StatusConflict, api.InventoryConflict{
			Type:   problem.TypeInventoryPrecondition,
			Title:  "库存的 expected_available_qty 与当前值不符",
			Status: http.StatusConflict,
			Current: api.AdminInventory{
				SkuId:        conflict.Current.SKUID,
				AvailableQty: int(conflict.Current.AvailableQty),
				WarningQty:   int(conflict.Current.WarningQty),
				UpdatedAt:    conflict.Current.UpdatedAt,
			},
		})

	case errors.Is(err, repository.ErrProductDeleted):
		problem.Write(c, http.StatusConflict,
			problem.TypeProductDeleted, "商品已软删，不接受修改")

	case errors.Is(err, repository.ErrProductStillPublished):
		problem.Write(c, http.StatusConflict,
			problem.TypeProductStillPublished, "商品仍在架，请先下架再删")

	case errors.Is(err, repository.ErrProductHasNoSKU):
		problem.Write(c, http.StatusConflict,
			problem.TypeProductHasNoSKU, "商品一个 SKU 都没有，不能上架")

	case errors.Is(err, repository.ErrSKUCodeDuplicated):
		problem.Write(c, http.StatusConflict,
			problem.TypeSKUCodeDuplicated, "该租户内已有同一个货号")

	case errors.Is(err, repository.ErrSKULastOfPublishedProduct):
		problem.Write(c, http.StatusConflict,
			problem.TypeSKULastOfPublished, "这是某个在架商品的最后一个 SKU")

	case errors.Is(err, repository.ErrCategoryHasChildren):
		problem.Write(c, http.StatusConflict,
			problem.TypeCategoryHasChildren, "分类下还有未软删的子分类")

	case errors.Is(err, repository.ErrCategoryHasProducts):
		problem.Write(c, http.StatusConflict,
			problem.TypeCategoryHasProducts, "分类下还有未软删的商品")

	case errors.Is(err, repository.ErrCategoryCycle):
		problem.Write(c, http.StatusConflict,
			problem.TypeCategoryCycle, "目标父节点是自己或自己的后代，移动会形成环")

	// —— 422 这一组（商品图那三条，契约按 Problem type 区分）。
	case errors.Is(err, repository.ErrUploadNotFound):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeUploadNotFound, "upload 不存在或不属于当前租户")

	case errors.Is(err, repository.ErrUploadWrongPurpose):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeUploadWrongPurpose, "upload 的用途不是商品图")

	case errors.Is(err, repository.ErrProductImageDuplicated):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeProductImageDuplicated, "同一个 upload_id 出现了两次")

	default:
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError,
			problem.TypeInternal, "服务内部错误")
	}
}

// ---------------------------------------------------------------------------
// 领域类型 → 契约类型
// ---------------------------------------------------------------------------
//
// 一律用 internal/api 里生成的类型（CONTRIBUTING 硬规矩二 + product.go 的
// 包注释）：契约里把某个字段改名或挪出 required，这里当场编译失败，
// 而不是等线上客户端解析失败。

func apiAdminProduct(p repository.AdminProduct) api.AdminProduct {
	out := api.AdminProduct{
		Id:            p.ID,
		CategoryId:    p.CategoryID,
		BrandId:       p.BrandID,
		Title:         p.Title,
		Subtitle:      p.Subtitle,
		Description:   p.Description,
		MinPriceCents: api.Money(p.MinPriceCents),
		MaxPriceCents: api.Money(p.MaxPriceCents),
		TotalStock:    int(p.TotalStock),
		SalesCount:    int(p.SalesCount),
		Status:        api.AdminProductStatus(p.Status),
		// published_at / deleted_at 在契约里是 [string, 'null']：
		// 原样传指针，nil 序列化成缺席。**这与 Staff.merchant_id 那一处不同**
		// （那个是必填的可空字段，缺席与 null 是两件事），这两个不在 required
		// 里，生成类型带着 omitempty —— 也就是说契约允许它们缺席。
		PublishedAt: p.PublishedAt,
		DeletedAt:   p.DeletedAt,
		CreatedAt:   p.CreatedAt,
	}
	if !p.UpdatedAt.IsZero() {
		u := p.UpdatedAt
		out.UpdatedAt = &u
	}
	return out
}

func apiAdminSKU(s repository.AdminSKU) api.AdminSku {
	cost := api.Money(s.CostCents)
	weight := int(s.WeightGram)
	warn := int(s.WarningQty)
	out := api.AdminSku{
		Id:           s.ID,
		ProductId:    s.ProductID,
		SkuCode:      s.SKUCode,
		PriceCents:   api.Money(s.PriceCents),
		CostCents:    &cost,
		WeightGram:   &weight,
		ImageUrl:     s.ImageURL,
		Status:       api.AdminSkuStatus(s.Status),
		AvailableQty: int(s.AvailableQty),
		WarningQty:   &warn,
	}
	// spec_values 解不开就报错而不是给一个空 map：空 map 会让后台的规格矩阵
	// 显示成「这件 SKU 没有任何规格维度」，而那与「这一格 JSONB 坏了」
	// 是两件事。用与前台详情同一个解法（service.DecodeSpecValues），
	// 两份解法意味着同一块 JSONB 在商品页和后台页上可以显示出不同的规格。
	if spec, err := service.DecodeSpecValues(s.SpecValues); err == nil {
		out.SpecValues = &spec
	}
	if !s.CreatedAt.IsZero() {
		t := s.CreatedAt
		out.CreatedAt = &t
	}
	if !s.UpdatedAt.IsZero() {
		t := s.UpdatedAt
		out.UpdatedAt = &t
	}
	return out
}

func apiAdminCategory(c repository.AdminCategory) api.AdminCategory {
	out := api.AdminCategory{
		Id:        c.ID,
		ParentId:  c.ParentID,
		Name:      c.Name,
		Path:      c.Path,
		Level:     int(c.Level),
		SortOrder: int(c.SortOrder),
		Status:    api.AdminCategoryStatus(c.Status),
		DeletedAt: c.DeletedAt,
		CreatedAt: c.CreatedAt,
	}
	if !c.UpdatedAt.IsZero() {
		u := c.UpdatedAt
		out.UpdatedAt = &u
	}
	return out
}

// apiProductImages 把库里的关联行装成契约的 ProductImage。
//
// url 在这一层拼（service.UploadURL），不在 repository：那一层认得的是列，
// 不是对外路由。返回**空切片而不是 nil**：契约里 images 是必填数组，
// nil 会序列化成 null，而 null 与 [] 对客户端是两件事。
func apiProductImages(imgs []repository.ProductImage) []api.ProductImage {
	out := make([]api.ProductImage, 0, len(imgs))
	for _, im := range imgs {
		out = append(out, api.ProductImage{
			UploadId:  im.UploadID,
			Url:       service.UploadURL(im.UploadID),
			SortOrder: int(im.SortOrder),
		})
	}
	return out
}
