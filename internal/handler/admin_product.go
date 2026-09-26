package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
)

// 商品域那 6 条写接口（列表在 admin_product_list.go，理由见那个文件的头）。
//
// **这个文件里一个 c.Query 都不许出现**：它实现的 6 条接口在 contract_test.go
// 的 routes 表里全登记着 NoQueryParams，而那条对账两个方向都锁。

// adminProductPatchRequest 是 PATCH /admin/products/{product_id} 的请求体。
//
// 不用生成的 api.ProductUpdateRequest，理由只有一个、但它是硬的：
// **brand_id 在契约里是 `integer | null`，而生成类型给的是 `*int64`** ——
// 「显式传 null 表示清空品牌」与「没传这个字段」在那上面是同一个 nil。
// 用它的话，每一次只改标题的 PATCH 都会顺手把品牌清空。
//
// 所以 brand_id 收 *json.RawMessage：非 nil 即「这个键出现过」，
// 再解一次拿它是 null 还是一个数。其余字段没有这个问题（契约里它们没有
// null 取值），照常用指针表示「没传」。
//
// 这个形状与 admin_auth.go 的 adminStaffPatchRequest 同源，也是同一条纪律：
// 手写请求体结构体要写清楚**为什么不用生成的那个**。
type adminProductPatchRequest struct {
	Title       *string          `json:"title"`
	Subtitle    *string          `json:"subtitle"`
	Description *string          `json:"description"`
	CategoryID  *int64           `json:"category_id"`
	BrandID     *json.RawMessage `json:"brand_id"`
}

// Create 实现 POST /api/v1/admin/products。
func (h *AdminCatalogHandler) Create(c *gin.Context) {
	// 请求体用生成类型：ProductCreateRequest 里**没有 status 也没有
	// merchant_id**（契约的描述逐字写着这是刻意的）。手写一个的话，
	// 哪天有人顺手加上那两个字段，编译器不会有任何意见 ——
	// 而 status 那一个是一条绕过 publication 端点的路。
	var req api.ProductCreateRequest
	if !bindJSON(c, &req) {
		return
	}
	p, err := h.svc.CreateProduct(c.Request.Context(), repository.NewProduct{
		CategoryID:  req.CategoryId,
		BrandID:     req.BrandId,
		Title:       req.Title,
		Subtitle:    req.Subtitle,
		Description: req.Description,
	})
	if err != nil {
		writeCatalogError(c, err)
		return
	}
	c.JSON(http.StatusCreated, apiAdminProduct(p))
}

// Detail 实现 GET /api/v1/admin/products/{product_id}。
func (h *AdminCatalogHandler) Detail(c *gin.Context) {
	id, ok := pathID(c, "product_id")
	if !ok {
		return
	}
	d, err := h.svc.FindProduct(c.Request.Context(), id)
	if err != nil {
		writeCatalogError(c, err)
		return
	}
	// AdminProductDetail 在契约里是 allOf(AdminProduct, {skus, images})，
	// 而生成器把它展平成了一个独立的结构体 —— 所以这里逐字段填，
	// 不能内嵌 api.AdminProduct。
	base := apiAdminProduct(d.Product)
	skus := make([]api.AdminSku, 0, len(d.SKUs))
	for _, s := range d.SKUs {
		skus = append(skus, apiAdminSKU(s))
	}
	c.JSON(http.StatusOK, api.AdminProductDetail{
		Id: base.Id, CategoryId: base.CategoryId, BrandId: base.BrandId,
		Title: base.Title, Subtitle: base.Subtitle, Description: base.Description,
		MinPriceCents: base.MinPriceCents, MaxPriceCents: base.MaxPriceCents,
		TotalStock: base.TotalStock, SalesCount: base.SalesCount,
		Status:      api.AdminProductDetailStatus(base.Status),
		PublishedAt: base.PublishedAt, DeletedAt: base.DeletedAt,
		CreatedAt: base.CreatedAt, UpdatedAt: base.UpdatedAt,
		Skus:   skus,
		Images: apiProductImages(d.Images),
	})
}

// Update 实现 PATCH /api/v1/admin/products/{product_id}。
func (h *AdminCatalogHandler) Update(c *gin.Context) {
	id, ok := pathID(c, "product_id")
	if !ok {
		return
	}
	var req adminProductPatchRequest
	if !bindJSON(c, &req) {
		return
	}
	p := repository.ProductPatch{
		Title:       req.Title,
		Subtitle:    req.Subtitle,
		Description: req.Description,
		CategoryID:  req.CategoryID,
	}
	if req.BrandID != nil {
		// 这个键出现过。再解一次：null → 清空，一个数 → 改成它。
		p.SetBrandID = true
		var bid *int64
		if err := json.Unmarshal(*req.BrandID, &bid); err != nil {
			problem.Write(c, http.StatusUnprocessableEntity,
				problem.TypeInvalidRequest, "brand_id 必须是整数或 null")
			return
		}
		p.BrandID = bid
	}
	out, err := h.svc.UpdateProduct(c.Request.Context(), id, p)
	if err != nil {
		writeCatalogError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminProduct(out))
}

// Delete 实现 DELETE /api/v1/admin/products/{product_id}（软删）。
//
// 「在架商品不能删」那条 409 闸门在 repository 的 SoftDeleteProduct 里 ——
// 它与「已经删过了」（404）在同一条 SQL 语句、同一个快照里分开回传。
// 在这里先查一遍 status 再删是两次快照，而且两份实现一定会分叉。
func (h *AdminCatalogHandler) Delete(c *gin.Context) {
	id, ok := pathID(c, "product_id")
	if !ok {
		return
	}
	if err := h.svc.DeleteProduct(c.Request.Context(), id); err != nil {
		writeCatalogError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// adminPublicationRequest 是 POST .../publication 的请求体。
//
// 这里可以用生成的 api.ProductPublicationRequest（它只有一个必填的枚举串），
// 而之所以还是手写：生成类型把 action 定成了 ProductPublicationRequestAction
// 这个字符串别名，收进来之后仍然要自己比对两个取值 —— 一个未知取值
// （"delete"）在两种写法下都要被判成 422，而手写这一版让那个判断就在眼前。
type adminPublicationRequest struct {
	Action string `json:"action"`
}

// Publication 实现 POST /api/v1/admin/products/{product_id}/publication。
func (h *AdminCatalogHandler) Publication(c *gin.Context) {
	id, ok := pathID(c, "product_id")
	if !ok {
		return
	}
	var req adminPublicationRequest
	if !bindJSON(c, &req) {
		return
	}
	var publish bool
	switch req.Action {
	case string(api.Publish):
		publish = true
	case string(api.Unpublish):
		publish = false
	default:
		// 契约里 action 的枚举只有这两个，**没有回到 0 草稿的取值**
		// （端点描述：草稿的含义是「从未对外出现过」，一旦发布过就不再为真）。
		// 未知取值回 422 而不是默默当成下架 —— 后者会让一次拼错的请求
		// 把商品从前台撤下来，而调用方收到的是 200。
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, `action 只能是 "publish" 或 "unpublish"`)
		return
	}
	p, err := h.svc.SetPublication(c.Request.Context(), id, publish)
	if err != nil {
		writeCatalogError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminProduct(p))
}

// ReplaceImages 实现 PUT /api/v1/admin/products/{product_id}/images。
func (h *AdminCatalogHandler) ReplaceImages(c *gin.Context) {
	id, ok := pathID(c, "product_id")
	if !ok {
		return
	}
	var req api.ProductImagesReplaceRequest
	if !bindJSON(c, &req) {
		return
	}
	// images 在契约里是必填的数组，**传空数组即清空全部图片**。
	// 所以这里不能把 nil 和 [] 当成两回事去区分 —— JSON 里 `{"images": []}`
	// 解出来就是一个长度 0 的切片，而它的含义是「清空」，不是「没传」。
	ids := make([]int64, 0, len(req.Images))
	for _, im := range req.Images {
		ids = append(ids, im.UploadId)
	}
	imgs, err := h.svc.ReplaceImages(c.Request.Context(), id, ids)
	if err != nil {
		writeCatalogError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiProductImages(imgs))
}
