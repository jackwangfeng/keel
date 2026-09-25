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
}

// List 实现 GET /api/v1/products。
func (h *ProductHandler) List(c *gin.Context) {
	// 解析失败就当没传：page / page_size 是可选参数，`?page=abc` 与不带 page
	// 对客户端是同一件事。越界值不在这里判 —— 钳制规则在 service。
	//
	// 契约里这个接口还有 category_id / sort / min_price_cents / max_price_cents
	// 四个可选参数，眼下**没有实现**，传了会被忽略。它们不在这里读，也不该在这里
	// 回 400 —— 对一份冻结的契约把 optional 参数判成错误是违约。
	// contract_test.go 里那份 notYetImplemented 清单钉着这笔账：契约新增参数、
	// 或者某个参数实现了却忘了从清单里划掉，那条测试都会红。
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))

	list, err := h.svc.List(c.Request.Context(), page, pageSize)
	if err != nil {
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
			Id:            it.ID,
			Title:         it.Title,
			Subtitle:      it.Subtitle,
			MinPriceCents: api.Money(it.MinPriceCents),
			MaxPriceCents: &max,
			SalesCount:    &sales,
			Status:        api.ProductSummaryStatus(it.Status),
		})
	}
	c.JSON(http.StatusOK, listResponse{
		PageMeta: api.PageMeta{
			Page:     list.Page,
			PageSize: list.PageSize,
			Total:    int(list.Total),
		},
		Items: items,
	})
}
