package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 商品详情：GET /api/v1/products/{product_id}。
//
// # 它为什么单独一个文件
//
// contract_test.go 的参数对账测试用 AST 读 handler 源文件里所有 c.Query 调用，
// 再与契约声明的 query 参数双向核对。那个核对是**按文件**做的，所以一个
// 「契约里一个 query 参数都没有」的接口不能和 GET /products 挤在 product.go 里
// —— 它会看见列表那两个 c.Query("page") 并报「handler 读了一个契约里没有的参数」。
//
// 换句话说这不是审美上的拆分：把两条接口放进同一个文件会让那条对账测试
// 失去区分力（要么红，要么得为它开一个口子，而口子一开，「路径改名之后测试
// 恒绿」那类失效就回来了）。
//
// **本轮它自己也有了一个 query 参数**（store_id），于是上面那段话的前提
// 「契约里一个 query 参数都没有」不再成立 —— 但拆分仍然要留着，
// 因为 GET /products 声明的是五个参数，两条接口的参数集合不相同，
// 而那条对账是按文件做的。
func (h *ProductHandler) Detail(c *gin.Context) {
	// 解析失败按 404 处理，不是 422。
	//
	// 契约里这条接口的响应集合只有 200 / 404 / default，而 `/products/abc`
	// 在客户端看来与 `/products/999999` 是同一件事：这个地址上没有商品。
	// 回 422 会让按契约生成的客户端去找一个它不该处理的分支，
	// 而回 404 说的正是实情。
	id, err := strconv.ParseInt(c.Param("product_id"), 10, 64)
	if err != nil || id <= 0 {
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "商品不存在")
		return
	}

	// store_id：按哪家门店算这件商品的价格与库存。解析与 GET /products 逐字
	// 一致（契约原话），所以这里的形状与那边一模一样 —— 两处写岔会让
	// 「列表里 129 元，点进去 159 元」，而那看起来像缓存问题。
	var storeID *int64
	if raw := c.Query("store_id"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v > 0 {
			storeID = &v
		}
	}

	d, err := h.svc.Detail(c.Request.Context(), storeID, id)
	switch {
	case err == nil:
	case errors.Is(err, service.ErrStoreNotFound):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "store_id 指向的门店不存在或不属于当前店铺")
		return
	case errors.Is(err, service.ErrProductNotFound):
		// 「不存在」「是草稿」「已下架」「已软删」「是别家店的」都走这一支，
		// 理由写在 repository.ErrProductNotFound 上：这条接口是 security: []，
		// 分开报等于送出一个能数出别家店有多少商品的探测器。
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "商品不存在")
		return
	default:
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "服务内部错误")
		return
	}

	skus := make([]api.Sku, 0, len(d.SKUs))
	for _, s := range d.SKUs {
		spec := s.SpecValues
		skus = append(skus, api.Sku{
			Id:       s.ID,
			SkuCode:  s.SKUCode,
			ImageUrl: s.ImageURL,

			// AvailableQty 是契约里的必填字段，而且它是**这一刻的水位**，
			// 不是一个「够不够」的布尔。客户端要拿它做数量步进器的上限 ——
			// 只给 in_stock 的话，用户能选 99 件再被 409 打回来。
			AvailableQty: int(s.AvailableQty),
			PriceCents:   api.Money(s.PriceCents),
			SpecValues:   &spec,
		})
	}

	max := api.Money(d.MaxPriceCents)
	sales := int(d.SalesCount)
	category := d.CategoryID
	inStock := d.InStock
	c.JSON(http.StatusOK, api.ProductDetail{
		Id:            d.ID,
		Title:         d.Title,
		Subtitle:      d.Subtitle,
		Description:   d.Description,
		CategoryId:    &category,
		MinPriceCents: api.Money(d.MinPriceCents),
		MaxPriceCents: &max,
		SalesCount:    &sales,
		Status:        api.ProductDetailStatus(d.Status),
		Skus:          skus,

		// InStock 在这里是**算出来的**（任意在售 SKU 水位 > 0），不是留空。
		// 列表那条接口上它至今没填，挂在 contract_test.go 的
		// NotYetImplementedResponse 里；详情这条既然已经把每个 SKU 的水位
		// 都查出来了，再留空就只是懒 —— 而客户端拿不到它就只能自己遍历 skus，
		// 那等于把「什么叫有货」这条规则复制到每一个客户端里。
		InStock: &inStock,

		// ImageUrl 与 Images 刻意缺席：products 表上没有图片列，商品图在
		// 数据模型里还没有落地（uploads 那张表存的是上传件，没有与商品的关联）。
		// 这两笔挂在 contract_test.go 的 NotYetImplementedResponse 里。
		// 回空字符串或空数组会让客户端渲染一个「加载失败」的占位图，
		// 而缺席说的是实话：这个字段还没有数据来源。
	})
}
