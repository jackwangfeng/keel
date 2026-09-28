// Package handler 把 HTTP 请求翻译成 service 调用，再把结果翻译成契约里的响应。
//
// 这里不准出现 SQL 和事务（CONTRIBUTING 的硬规矩一）。业务规则在 service，
// 数据访问在 repository —— 包括分页的钳制：它是业务规则，不是解析细节。
//
// 响应体用 internal/api 里由契约生成的类型，不手写结构体。手写的那版能编译、
// 能通过测试，却和契约之间只有人的注意力在维系：契约里把 min_price_cents 改个
// 名字、把 status 从 required 里挪走，构建照样绿，直到线上客户端解析失败。
// 用生成的类型之后，同一个改动会让 go build 当场失败 —— 那才是硬规矩二
// 「OpenAPI 是唯一真相源」的字面意思。
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

type ProductHandler struct{ svc *service.ProductService }

func NewProductHandler(s *service.ProductService) *ProductHandler {
	return &ProductHandler{svc: s}
}

// listResponse 是契约里 `allOf: [PageMeta, {items}]` 的 Go 形状。
//
// 内嵌 api.PageMeta 而不是重打一遍 page / page_size / total 三个 tag：
// 内嵌会把它们平铺进同一层 JSON，正是 allOf 的意思，而且它们的名字与类型
// 由生成代码说了算。
type listResponse struct {
	api.PageMeta
	Items []api.ProductSummary `json:"items"`

	// Store 在契约里是**必返**的（required: [items, store]）：
	// 没有它，客户端拿到的 in_stock 与价格是不知道属于谁的。
	Store api.StoreContext `json:"store"`
}

// apiStoreContext 把 service 的解析结果拍成契约类型。
//
// 一个函数而不是在三条读路径上各拼一遍：拼岔一处的症状是某一条接口的
// match_type 与它实际用的门店对不上，而那看上去完全正常。
func apiStoreContext(sc service.StoreContext) api.StoreContext {
	return api.StoreContext{
		MatchType: api.StoreMatchType(sc.MatchType),
		StoreId:   sc.StoreID,
		RegionId:  sc.RegionID,
	}
}

// List 实现 GET /api/v1/products。
func (h *ProductHandler) List(c *gin.Context) {
	// 解析失败就当没传：page / page_size 是可选参数，`?page=abc` 与不带 page
	// 对客户端是同一件事。越界值不在这里判 —— 钳制规则在 service。
	//
	// 契约里这个接口还有 sort / min_price_cents / max_price_cents 三个可选参数，
	// 眼下**没有实现**，传了会被忽略。它们不在这里读，也不该在这里回 400 ——
	// 对一份冻结的契约把 optional 参数判成错误是违约。（category_id 原本也在这张
	// 单子上，买家端要做类目浏览，本轮接上了，见下面。）
	// contract_test.go 里那份 notYetImplemented 清单钉着这笔账：契约新增参数、
	// 或者某个参数实现了却忘了从清单里划掉，那条测试都会红。
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))

	// store_id：按哪家门店算「卖不卖 / 多少钱 / 有没有货」。
	//
	// **不传不是「全租户并集」，是走回落链**（契约原话）：service 按
	// GET /stores/resolve 的同一段代码解析出默认门店。所以这里只把
	// 「传了没有」传下去，回落规则一个字都不在 handler 里。
	//
	// 解析不出正整数就按没传处理，与 page 一致：`?store_id=abc` 是一次
	// 客户端的笔误，而契约在这条接口上没有 422。真传了一个不存在的门店 id
	// 则是另一回事 —— 那条由 service 报 ErrStoreNotFound，见下面 422 那一支。
	var storeID *int64
	if raw := c.Query("store_id"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v > 0 {
			storeID = &v
		}
	}

	// category_id：按类目筛，**含子孙**。解析规则与 store_id 一致 ——
	// 解析不出正整数按没传处理（契约在这条接口上没有 422）。传了一个不存在的
	// 类目则是空列表而不是全部商品，那条规则在 SQL 里（products.sql 文件头）。
	var categoryID *int64
	if raw := c.Query("category_id"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v > 0 {
			categoryID = &v
		}
	}

	list, err := h.svc.List(c.Request.Context(), storeID, categoryID, page, pageSize)
	switch {
	case err == nil:
	case errors.Is(err, service.ErrStoreNotFound):
		// 显式指名了一家不存在 / 不属于本租户的门店。422 而不是静默回落：
		// 静默回落会让客户端以为自己看的是 A 店的价，而实际上是 B 店的。
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "store_id 指向的门店不存在或不属于当前店铺")
		return
	default:
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError,
			problem.TypeInternal, "服务内部错误")
		return
	}

	items := make([]api.ProductSummary, 0, len(list.Items))
	for _, it := range list.Items {
		max := api.Money(it.MaxPriceCents)
		sales := int(it.SalesCount)
		items = append(items, api.ProductSummary{
			Id:       it.ID,
			Title:    it.Title,
			Subtitle: it.Subtitle,
			// 主图地址；没有图时 nil，字段缺席（不是空串）。
			ImageUrl:      it.ImageURL,
			MinPriceCents: api.Money(it.MinPriceCents),
			MaxPriceCents: &max,
			SalesCount:    &sales,
			Status:        api.ProductSummaryStatus(it.Status),
			// 这家店此刻生效的活动标签（00058）。没有活动时是空数组。
			PromotionTags: ptrTags(it.PromotionTags),
			// 最低活动价，只在比门店最低价低时有；否则字段缺席。
			PromoMinPriceCents: moneyPtrOf(it.PromoMinPriceCents),
			// 这家店有没有货；库存服务不在时 nil，字段缺席。
			InStock: it.InStock,
		})
	}
	c.JSON(http.StatusOK, listResponse{
		PageMeta: api.PageMeta{
			Page:     list.Page,
			PageSize: list.PageSize,
			Total:    int(list.Total),
		},
		Items: items,
		Store: apiStoreContext(list.Store),
	})
}
