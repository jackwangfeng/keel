package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 单独一个文件，不塞进 product.go：contract_test.go 的参数对账按**文件**解析
// c.Query，同一个文件里的路由会被当成共用一组 query 参数。GET /categories 在契约
// 里一个参数都没有，和 GET /products 放在一起，闸门会认为它也读 page / store_id /
// category_id。

// Categories 实现 GET /categories：当前店启用中的类目树。
//
// 契约里 security: []，不需要登录；租户照常由 Host 定、由 RLS 挡。
// 叶子节点也给 children: []，不省略：客户端拿到一个恒定的形状，
// 不必在每一层都判一次「这个字段在不在」。
func (h *ProductHandler) Categories(c *gin.Context) {
	tree, err := h.svc.Categories(c.Request.Context())
	if err != nil {
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "读取类目失败")
		return
	}
	c.JSON(http.StatusOK, categoriesOut(tree))
}

func categoriesOut(in []service.Category) []api.Category {
	out := make([]api.Category, 0, len(in))
	for _, c := range in {
		children := categoriesOut(c.Children)
		out = append(out, api.Category{
			Id: c.ID, Name: c.Name, Level: c.Level, Children: &children,
		})
	}
	return out
}
