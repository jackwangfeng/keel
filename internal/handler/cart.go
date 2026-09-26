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

// 购物车里返回 Cart 的 5 条（契约 Cart tag）：
//
//	GET   /cart                       购物车
//	POST  /cart/items                 加入购物车（Idempotency-Key 必填）
//	PUT   /cart/selection             批量勾选
//	POST  /cart/items/batch-delete    批量删除（Idempotency-Key 必填）
//	PATCH /cart/items/{item_id}       改数量 / 勾选态
//
// 五条都读 store_id（契约 CartStoreId）：车里的价与可买状态按门店算。
// 另外两条（DELETE /cart、DELETE /cart/items/{item_id}）回 204、不读任何 query 参数，
// 放在 cart_delete.go —— contract_test.go 的 query 参数对账按 HandlerFile 解析整份源码，
// 同一个文件里读了 store_id，那两条的 NoQueryParams 登记就会红
// （与 admin_staff_list.go 单独成文件同一个理由）。

type CartHandler struct{ svc *service.CartService }

func NewCartHandler(s *service.CartService) *CartHandler { return &CartHandler{svc: s} }

// cartStoreID 读 store_id。解析规则与 GET /products 逐字一致：解析不出正整数就按
// 没传处理（走回落链）；真传了一个不存在的门店由 service 报 ErrStoreNotFound（422）。
func cartStoreID(c *gin.Context) *int64 {
	if raw := c.Query("store_id"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v > 0 {
			return &v
		}
	}
	return nil
}

// Get 实现 GET /api/v1/cart。
func (h *CartHandler) Get(c *gin.Context) {
	out, err := h.svc.Get(c.Request.Context(), cartStoreID(c))
	if err != nil {
		writeCartError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiCart(out))
}

// AddItem 实现 POST /api/v1/cart/items。
func (h *CartHandler) AddItem(c *gin.Context) {
	var raw api.PostCartItemsJSONRequestBody
	if !bindJSON(c, &raw) {
		return
	}
	if raw.Quantity < 1 || raw.Quantity > 999 {
		// 在 int → int32 之前挡：一个超出 int32 的数量截断之后可能落回合法区间。
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "quantity 必须在 [1, 999] 内")
		return
	}
	out, replayed, err := h.svc.Add(c.Request.Context(), service.AddRequest{
		StoreID: cartStoreID(c), SKUID: raw.SkuId, Quantity: int32(raw.Quantity),
	}, idemKeyOf(c))
	if err != nil {
		writeCartError(c, err)
		return
	}
	markReplayed(c, replayed)
	c.JSON(http.StatusOK, apiCart(out))
}

// Select 实现 PUT /api/v1/cart/selection。
func (h *CartHandler) Select(c *gin.Context) {
	body, err := c.GetRawData()
	if err != nil {
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "请求体读不出来")
		return
	}
	var raw api.PutCartSelectionJSONRequestBody
	var present map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil || json.Unmarshal(body, &present) != nil {
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "请求体不是合法的 JSON")
		return
	}
	if _, ok := present["selected"]; !ok {
		// 契约 required: [selected]。生成类型里它是一个非指针 bool，缺席会被读成 false ——
		// 一个漏写了字段的「全选」请求会变成「全不选」。所以这里单独看它在不在。
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "selected 是必填字段")
		return
	}
	out, err := h.svc.Select(c.Request.Context(), cartStoreID(c), raw.Selected, raw.ItemIds)
	if err != nil {
		writeCartError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiCart(out))
}

// BatchDelete 实现 POST /api/v1/cart/items/batch-delete。
func (h *CartHandler) BatchDelete(c *gin.Context) {
	var raw api.PostCartItemsBatchDeleteJSONRequestBody
	if !bindJSON(c, &raw) {
		return
	}
	out, replayed, err := h.svc.BatchDelete(c.Request.Context(), service.BatchDeleteRequest{
		StoreID: cartStoreID(c), ItemIDs: raw.ItemIds, Selected: raw.Selected,
	}, idemKeyOf(c))
	if err != nil {
		writeCartError(c, err)
		return
	}
	markReplayed(c, replayed)
	c.JSON(http.StatusOK, apiCart(out))
}

// PatchItem 实现 PATCH /api/v1/cart/items/{item_id}。
func (h *CartHandler) PatchItem(c *gin.Context) {
	itemID, ok := cartItemPathID(c)
	if !ok {
		return
	}
	var raw api.PatchCartItemsItemIdJSONRequestBody
	if !bindJSON(c, &raw) {
		return
	}
	req := service.PatchRequest{StoreID: cartStoreID(c), ItemID: itemID, Selected: raw.Selected}
	if raw.Quantity != nil {
		if *raw.Quantity < 1 || *raw.Quantity > 999 {
			problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "quantity 必须在 [1, 999] 内")
			return
		}
		q := int32(*raw.Quantity)
		req.Quantity = &q
	}
	out, err := h.svc.Patch(c.Request.Context(), req)
	if err != nil {
		writeCartError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiCart(out))
}

// cartItemPathID 取路径里的 item_id。非正整数回 404：它不可能是任何一个条目，
// 与「不在你的车里」同一个答案（同 addressPathID）。
func cartItemPathID(c *gin.Context) (int64, bool) {
	id, ok := parsePositiveID(c.Param("item_id"))
	if !ok {
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "购物车条目不存在")
		return 0, false
	}
	return id, true
}

func apiCart(v service.CartView) api.Cart {
	items := make([]api.CartItem, 0, len(v.Lines))
	for _, ln := range v.Lines {
		productID := ln.ProductID
		title := ln.Title
		it := api.CartItem{
			Id:         ln.ID,
			SkuId:      ln.SKUID,
			ProductId:  &productID,
			Title:      &title,
			ImageUrl:   ln.ImageURL,
			PriceCents: ln.PriceCents,
			// 命中限时折扣 / 秒杀时 PriceCents 是活动价，门店价给出来划线（00044）。
			ListPriceCents:   ln.ListPriceCents,
			PricePromotionId: ln.PricePromotionID,
			Quantity:         int(ln.Quantity),
			Selected:         ln.Selected,
			Available:        ln.Status == service.CartLineAvailable,
			Status:           api.CartItemStatus(ln.Status),
		}
		// 规格解不开不让整辆车 500：展示素材坏了是数据问题，不该挡住用户看到自己的车。
		// 字段整个不出现，而不是给一个空对象 —— 空对象会被渲染成「无规格」。
		if spec, err := service.DecodeSpecValues(ln.SpecValues); err == nil && spec != nil {
			it.SpecValues = &spec
		}
		items = append(items, it)
	}
	return api.Cart{
		Items:              items,
		TotalCents:         api.Money(v.TotalCents),
		SelectedTotalCents: api.Money(v.SelectedTotalCents),
		// 已勾选、可买的行上满减满折的结果（与试算同一份计算、同一段渲染）。
		PromotionDiscountCents: api.Money(v.PromotionDiscountCents),
		Promotions:             apiPromotionHits(v.Promotions),
		Store:                  apiStoreContext(v.Store),
	}
}

// writeCartError 把购物车的业务错误翻成契约里的响应。cart_delete.go 共用。
func writeCartError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrCartItemNotFound):
		// 别人车里的条目与不存在的条目同一个 404（契约：不是 403）。
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "购物车条目不存在")
	case errors.Is(err, service.ErrCartQuantityExceeded):
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeCartQuantityExceeded,
			"购物车里这件商品最多 999 件", err)
	case errors.Is(err, service.ErrCartFull):
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest,
			"购物车已满，请先清理一些商品", err)
	case errors.Is(err, service.ErrOutOfServiceArea):
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest,
			"当前不在任何门店的服务范围，无法判断这件商品卖不卖")
	case errors.Is(err, service.ErrInsufficientStock):
		writeProblemDetail(c, http.StatusConflict, problem.TypeInsufficientStock, "库存不足", err)
	case errors.Is(err, service.ErrSKUUnavailable):
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest,
			"这件商品不存在或已下架", err)
	case errors.Is(err, repository.ErrSKUNotSoldInStore):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeSKUNotSoldInStore, "这家门店不卖这件商品，请换一家门店")
	case errors.Is(err, service.ErrBadRequest):
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "请求参数不合法", err)
	case errors.Is(err, service.ErrIdempotencyKeyMissing):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "缺少必填的 Idempotency-Key 请求头")
	case writeBuyerAccountError(c, err):
	default:
		// ErrStoreNotFound（422）、幂等的两种、兜底 500 都在那里，同一个出口。
		writeOrderError(c, err)
	}
}
