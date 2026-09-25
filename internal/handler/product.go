// Package handler 把 HTTP 请求翻译成 service 调用，再把结果翻译成契约里的响应。
//
// 这里不准出现 SQL 和事务（CONTRIBUTING 的硬规矩一）。业务规则在 service，
// 数据访问在 repository —— 包括分页的钳制：它是业务规则，不是解析细节。
package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/service"
)

type ProductHandler struct{ svc *service.ProductService }

func NewProductHandler(s *service.ProductService) *ProductHandler {
	return &ProductHandler{svc: s}
}

// productSummary 是契约 ProductSummary 在响应里的形状。
//
// 手写而不是直接 JSON 化 service 的类型：字段名（min_price_cents 而不是
// MinPriceCents）和「哪些字段可以缺席」都是契约的规定，得有一个地方对着契约写。
// 可选字段用指针 + omitempty，必填字段（id / title / min_price_cents / status）
// 不带 omitempty —— 否则 status=0 的商品会让 status 整个消失，而 0 在契约里
// 是「草稿」这个有意义的取值。
type productSummary struct {
	ID            int64   `json:"id"`
	Title         string  `json:"title"`
	Subtitle      *string `json:"subtitle,omitempty"`
	MinPriceCents int64   `json:"min_price_cents"`
	MaxPriceCents int64   `json:"max_price_cents"`
	SalesCount    int32   `json:"sales_count"`
	Status        int16   `json:"status"`
}

// listResponse 是契约里 PageMeta + items 的那个 allOf。
type listResponse struct {
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
	Total    int64            `json:"total"`
	Items    []productSummary `json:"items"`
}

// List 实现 GET /api/v1/products。
func (h *ProductHandler) List(c *gin.Context) {
	// 解析失败就当没传：page / page_size 是可选参数，`?page=abc` 与不带 page
	// 对客户端是同一件事。越界值不在这里判 —— 钳制规则在 service。
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))

	list, err := h.svc.List(c.Request.Context(), page, pageSize)
	if err != nil {
		_ = c.Error(err)
		problem(c, http.StatusInternalServerError,
			"https://keel.dev/problems/internal", "服务内部错误")
		return
	}

	items := make([]productSummary, 0, len(list.Items))
	for _, it := range list.Items {
		items = append(items, productSummary{
			ID:            it.ID,
			Title:         it.Title,
			Subtitle:      it.Subtitle,
			MinPriceCents: it.MinPriceCents,
			MaxPriceCents: it.MaxPriceCents,
			SalesCount:    it.SalesCount,
			Status:        it.Status,
		})
	}
	c.JSON(http.StatusOK, listResponse{
		Page: list.Page, PageSize: list.PageSize, Total: list.Total, Items: items,
	})
}

// problem 按 RFC 9457 回错误，不使用 {code,message,data} 信封。
//
// 刻意不把 err 的内容放进响应：这个接口是匿名可访问的，而数据库错误里
// 常常带着表名、列名和参数值。
func problem(c *gin.Context, status int, kind, title string) {
	c.Header("Content-Type", "application/problem+json")
	c.JSON(status, gin.H{"type": kind, "title": title, "status": status})
}
