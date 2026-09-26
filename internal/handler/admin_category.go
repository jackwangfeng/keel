package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 类目那 4 条接口。
//
// **这个文件里一个 c.Query 都不许出现**（理由见 admin_product.go 的文件头）。
// 后台分类列表在契约里一个 query 参数都没有 —— 它返回**整棵树拍平之后的
// 全部节点**，没有分页。这一点与 /admin/products 相反，是契约定的：
// 一家店的类目是几十个量级，而客户端要拿 path/level 自己拼树，
// 分页拼不出一棵完整的树。

// ListCategories 实现 GET /api/v1/admin/categories。
func (h *AdminCatalogHandler) ListCategories(c *gin.Context) {
	rows, err := h.svc.ListCategories(c.Request.Context())
	if err != nil {
		writeCatalogError(c, err)
		return
	}
	out := make([]api.AdminCategory, 0, len(rows))
	for _, r := range rows {
		out = append(out, apiAdminCategory(r))
	}
	// 契约里这条的 200 是一个**裸数组**，不是 {items}。回 nil 的话会序列化成
	// null，而 null 与 [] 对客户端是两件事（「出错了」与「这家店还没建类目」）。
	c.JSON(http.StatusOK, out)
}

// CreateCategory 实现 POST /api/v1/admin/categories。
//
// path 与 level **不在请求体里**（生成类型 CategoryCreateRequest 上就没有
// 这两个字段），它们由 repository.CreateCategory 从 parent_id 算出来。
// 让客户端传 path 等于让前端去维护一个索引的内容 —— 算错一次，
// 前台目录树就会长出一整个错位的子树，而数据库不会拒绝。
func (h *AdminCatalogHandler) CreateCategory(c *gin.Context) {
	var req api.CategoryCreateRequest
	if !bindJSON(c, &req) {
		return
	}
	n := repository.NewCategory{Name: req.Name, ParentID: req.ParentId}
	if req.SortOrder != nil {
		n.SortOrder = int32(*req.SortOrder)
	}
	cat, err := h.svc.CreateCategory(c.Request.Context(), n)
	if err != nil {
		writeCatalogError(c, err)
		return
	}
	c.JSON(http.StatusCreated, apiAdminCategory(cat))
}

// adminCategoryPatchRequest 是 PATCH /admin/categories/{category_id} 的请求体。
//
// parent_id 收 *json.RawMessage，而契约在这一条上把理由写得最直白：
// 「显式传 null 表示移到根（level 变成 1）。不传这个字段则不动层级 ——
// null 与『没传』在这里是两件事。」生成类型上两者都是 nil，
// 用它的话每一次改名都会顺手把这个类目连同它整棵子树挪到根下。
type adminCategoryPatchRequest struct {
	Name      *string          `json:"name"`
	SortOrder *int32           `json:"sort_order"`
	Status    *int16           `json:"status"`
	ParentID  *json.RawMessage `json:"parent_id"`
}

// UpdateCategory 实现 PATCH /api/v1/admin/categories/{category_id}。
//
// 传了 parent_id 就是**移动子树**：改 parent_id、重写自己以及全部后代的
// path 与 level、拒绝成环，三件事在 repository.MoveCategory 的同一个事务里。
// 判环那一条不在这里实现 —— 它用的判据是 `目标.path 以 自己.path 打头`，
// 而 path 只有那一层认得。
func (h *AdminCatalogHandler) UpdateCategory(c *gin.Context) {
	id, ok := pathID(c, "category_id")
	if !ok {
		return
	}
	var req adminCategoryPatchRequest
	if !bindJSON(c, &req) {
		return
	}
	in := service.CategoryPatchInput{
		Name: req.Name, SortOrder: req.SortOrder, Status: req.Status,
	}
	if req.ParentID != nil {
		in.SetParentID = true
		var pid *int64
		if err := json.Unmarshal(*req.ParentID, &pid); err != nil {
			problem.Write(c, http.StatusUnprocessableEntity,
				problem.TypeInvalidRequest, "parent_id 必须是整数或 null")
			return
		}
		in.ParentID = pid
	}
	cat, err := h.svc.UpdateCategory(c.Request.Context(), id, in)
	if err != nil {
		writeCatalogError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminCategory(cat))
}

// DeleteCategory 实现 DELETE /api/v1/admin/categories/{category_id}（软删）。
//
// 两条 409 闸门（还有子分类 / 还有商品）在 repository.SoftDeleteCategory 里。
// 它们**不能**靠数据库兜底：软删不删行，所以复合外键在数据库看来一直是
// 完整的 —— 这两件事只有那一层挡得住，而它给了两个不同的 sentinel，
// 因为契约给了两个不同的 Problem type。
func (h *AdminCatalogHandler) DeleteCategory(c *gin.Context) {
	id, ok := pathID(c, "category_id")
	if !ok {
		return
	}
	if err := h.svc.DeleteCategory(c.Request.Context(), id); err != nil {
		writeCatalogError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
