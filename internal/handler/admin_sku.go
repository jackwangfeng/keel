package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// SKU 与库存那 4 条写接口。
//
// **这个文件里一个 c.Query 都不许出现**（理由见 admin_product.go 的文件头）。

// CreateSKU 实现 POST /api/v1/admin/products/{product_id}/skus。
//
// 「同一个事务里还会建出这个 SKU 的 inventories 行」这件事**不在这里做**：
// 它收在 repository.CreateSKU 里，调用方没有机会只调其中一条 INSERT。
// 理由值得在这一层也写一遍，因为它是这条接口最贵的一个错误：
// inventories.sku_id 是主键，下单 SAGA 的正向分支是
// `UPDATE ... WHERE sku_id = $1 AND available_qty >= $2`，没有那一行时
// rows_affected = 0，而 SAGA 把 0 判成**库存不足** —— 于是一个漏建库存行的
// SKU 表现为「这件商品永远缺货」，排查方向从第一步就是错的。
func (h *AdminCatalogHandler) CreateSKU(c *gin.Context) {
	productID, ok := pathID(c, "product_id")
	if !ok {
		return
	}
	// 请求体用生成类型：SkuCreateRequest 里**有** available_qty 而
	// SkuUpdateRequest 里没有，契约的描述写着这个不对称是刻意的
	// （建行的时候没有并发对手，改行的时候有）。手写两个结构体的话，
	// 那条不对称迟早被抹平。
	var req api.SkuCreateRequest
	if !bindJSON(c, &req) {
		return
	}
	in := service.NewSKUInput{
		SKUCode:       req.SkuCode,
		PriceCents:    int64(req.PriceCents),
		ImageUploadID: req.ImageUploadId,
	}
	if req.SpecValues != nil {
		in.SpecValues = *req.SpecValues
	}
	if req.CostCents != nil {
		in.CostCents = int64(*req.CostCents)
	}
	if req.WeightGram != nil {
		in.WeightGram = int32(*req.WeightGram)
	}
	// available_qty / warning_qty 省略即 0（契约原话）。
	if req.AvailableQty != nil {
		in.AvailableQty = int32(*req.AvailableQty)
	}
	if req.WarningQty != nil {
		in.WarningQty = int32(*req.WarningQty)
	}

	sku, replayed, err := h.svc.CreateSKU(c.Request.Context(), productID, in, idemKeyOf(c))
	if err != nil {
		writeCatalogError(c, err)
		return
	}
	markReplayed(c, replayed)
	c.JSON(http.StatusCreated, apiAdminSKU(sku))
}

// adminSKUPatchRequest 是 PATCH /admin/skus/{sku_id} 的请求体。
//
// image_upload_id 收 *json.RawMessage，理由与 adminProductPatchRequest 的
// brand_id 一字不差：契约里它是 `integer | null`，「清空这一格小图」与
// 「没传这个字段」是两件事，而生成类型上两者都是 nil。
type adminSKUPatchRequest struct {
	SKUCode       *string            `json:"sku_code"`
	SpecValues    *map[string]string `json:"spec_values"`
	PriceCents    *int64             `json:"price_cents"`
	CostCents     *int64             `json:"cost_cents"`
	WeightGram    *int32             `json:"weight_gram"`
	Status        *int16             `json:"status"`
	ImageUploadID *json.RawMessage   `json:"image_upload_id"`
}

// UpdateSKU 实现 PATCH /api/v1/admin/skus/{sku_id}。
//
// **请求体里没有 available_qty，这是刻意的**（契约原话）：库存走
// PUT .../inventory，因为它要表达「基于我看到的值改」，而一个什么都能改的
// PATCH 表达不了乐观并发 —— 把库存混进来，一次改价就能把并发下单扣掉的量抹掉。
// 上面那个结构体里没有那个字段，也就没有一行代码写得出这件事。
func (h *AdminCatalogHandler) UpdateSKU(c *gin.Context) {
	id, ok := pathID(c, "sku_id")
	if !ok {
		return
	}
	var req adminSKUPatchRequest
	if !bindJSON(c, &req) {
		return
	}
	in := service.SKUPatchInput{
		SKUCode:    req.SKUCode,
		SpecValues: req.SpecValues,
		PriceCents: req.PriceCents,
		CostCents:  req.CostCents,
		WeightGram: req.WeightGram,
		Status:     req.Status,
	}
	if req.ImageUploadID != nil {
		in.SetImageUploadID = true
		var uid *int64
		if err := json.Unmarshal(*req.ImageUploadID, &uid); err != nil {
			problem.Write(c, http.StatusUnprocessableEntity,
				problem.TypeInvalidRequest, "image_upload_id 必须是整数或 null")
			return
		}
		in.ImageUploadID = uid
	}
	sku, err := h.svc.UpdateSKU(c.Request.Context(), id, in)
	if err != nil {
		writeCatalogError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminSKU(sku))
}

// DeleteSKU 实现 DELETE /api/v1/admin/skus/{sku_id}（软删）。
//
// 「在架商品的最后一个 SKU 删不得」那条 409 在 repository 的 SoftDeleteSKU 里：
// 闸门写进 UPDATE 的 WHERE，不是先查后改 —— 两次快照之间，另一个会话可以把
// 兄弟 SKU 删掉，于是「删的时候还有两个」在提交时变成了「删完一个不剩」。
func (h *AdminCatalogHandler) DeleteSKU(c *gin.Context) {
	id, ok := pathID(c, "sku_id")
	if !ok {
		return
	}
	if err := h.svc.DeleteSKU(c.Request.Context(), id); err != nil {
		writeCatalogError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// SetInventory 实现 PUT /api/v1/admin/skus/{sku_id}/inventory（比较并设置）。
//
// **这条接口和下单 SAGA 抢同一行。** expected_available_qty 是必填的，
// 它是「我看到的那个值」；服务端执行的是一条带 `available_qty = $expected`
// 条件的 UPDATE。写成无条件覆盖的话，「商家看到 10、页面停了三分钟、
// 期间卖掉 4 件、商家把它改成 20」的结果是 20 而正确答案是 16，
// 而且没有任何东西会响。
//
// 两种 rows_affected = 0 的映射在 writeCatalogError 里，而它们的分辨发生在
// SQL 里（同一个 MVCC 快照下的两个 CTE）。这里什么也不判。
func (h *AdminCatalogHandler) SetInventory(c *gin.Context) {
	id, ok := pathID(c, "sku_id")
	if !ok {
		return
	}
	// 用生成类型：两个数量在契约里**都是必填**，缺一不可 ——
	// 只给 available_qty 就退化成无条件覆盖，那正是这条接口要防的东西。
	var req api.InventorySetRequest
	if !bindJSON(c, &req) {
		return
	}
	var warn *int32
	if req.WarningQty != nil {
		w := int32(*req.WarningQty)
		warn = &w
	}
	inv, err := h.svc.SetInventory(c.Request.Context(), id,
		int32(req.ExpectedAvailableQty), int32(req.AvailableQty), warn)
	if err != nil {
		writeCatalogError(c, err)
		return
	}
	// store_id 必返，即使这条路径的 URL 里没有它：它是服务端推出来的
	// （本租户恰好一家门店），而调用方要知道自己刚改的是哪一家 ——
	// 它哪天开第二家店时，同一个请求会 409 store-ambiguous 而不是猜一家。
	c.JSON(http.StatusOK, apiAdminInventory(inv))
}

// AdjustInventory 实现 POST /api/v1/admin/skus/{sku_id}/inventory/adjustments
// （相对调整的单店捷径，Idempotency-Key 必填）。
//
// 门店由服务端推出（本租户恰好一家），推不出来是 409 store-ambiguous ——
// 与 PUT 那条捷径同一个语义。错误映射与按门店那条共用 writeInventoryAdjustError。
func (h *AdminCatalogHandler) AdjustInventory(c *gin.Context) {
	id, ok := pathID(c, "sku_id")
	if !ok {
		return
	}
	var req api.InventoryAdjustRequest
	if !bindJSON(c, &req) {
		return
	}
	inv, replayed, err := h.svc.AdjustInventory(c.Request.Context(), id,
		inventoryAdjustInput(req), idemKeyOf(c))
	if err != nil {
		writeInventoryAdjustError(c, err)
		return
	}
	markReplayed(c, replayed)
	// store_id 必返：它是服务端推出来的，调用方要知道自己刚调的是哪一家。
	c.JSON(http.StatusOK, apiStoreInventory(inv))
}
