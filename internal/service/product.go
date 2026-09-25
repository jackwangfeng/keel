// Package service 承载业务规则与事务编排入口。这里不拼 SQL。
package service

import (
	"context"

	"github.com/keel/keel/internal/repository"
)

const (
	// DefaultPageSize / MaxPageSize 与契约里 PageSize 参数的 default / maximum 一致。
	// 契约是唯一真相源，这两个常量是它在 Go 这一侧的落点 —— 改契约时要一起改。
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// ProductSummary 是列表里的一件商品。
//
// 它与 repository.Product 眼下几乎一样，但不是同一个东西：repository 那个
// 描述「库里存着什么」，这个描述「接口答应给什么」（契约的 ProductSummary）。
// 之后它会长出库里没有的字段（in_stock 要问库存服务，image_url 要拼 CDN 前缀），
// 而那时不该回头去改 repository 的类型。
type ProductSummary struct {
	ID            int64
	Title         string
	Subtitle      *string
	MinPriceCents int64
	MaxPriceCents int64
	SalesCount    int32
	Status        int16
}

// ProductList 是一页商品，带上生效后的分页参数。
//
// Page / PageSize 返回的是**钳制之后**的值，不是调用方传进来的值。这不是为了
// 好看：契约的 200 响应里 page / page_size 是必填的，而客户端要拿它们算总页数、
// 决定还翻不翻得动。回显原始输入的话，传 page_size=100000 的客户端会以为自己
// 拿到了十万件，然后在第二页上永远卡住。
//
// 它同时让「钳制发生在 service 而不是 handler」这件事可被测试观察到 ——
// 否则钳制的效果只体现在 SQL 的 LIMIT 上，从响应里看不出任何差别。
type ProductList struct {
	Items    []ProductSummary
	Page     int
	PageSize int
	Total    int64
}

// ProductRepository 是本服务需要的仓储能力。
//
// 收接口而不是 *repository.Repo，是为了让「business 只经 WithTenant 访问数据库」
// 这句话在类型上就是全部事实：这个接口上没有池、没有 Begin、没有别的出口。
type ProductRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
}

type ProductService struct{ repo ProductRepository }

func NewProductService(r ProductRepository) *ProductService { return &ProductService{repo: r} }

// List 返回当前租户的在架商品。
//
// page / pageSize 在这里钳制，不在 handler：分页规则是业务规则，
// 换一个 handler（比如将来的 gRPC）不该重写一遍。
func (s *ProductService) List(ctx context.Context, page, pageSize int) (ProductList, error) {
	page, pageSize = clampPaging(page, pageSize)

	out := ProductList{Items: []ProductSummary{}, Page: page, PageSize: pageSize}
	err := s.repo.WithTenant(ctx, func(q repository.Tx) error {
		// 计数与取页在同一个事务里，所以 total 和 items 看到的是同一个快照。
		// 分开两次访问的话，两者之间的一次上下架会让「total=21 但第二页是空的」
		// 这种自相矛盾的响应偶发出现。
		total, err := q.CountProducts(ctx)
		if err != nil {
			return err
		}
		out.Total = total

		rows, err := q.ListProducts(ctx, int64(pageSize), offsetOf(page, pageSize))
		if err != nil {
			return err
		}
		for _, r := range rows {
			out.Items = append(out.Items, ProductSummary{
				ID:            r.ID,
				Title:         r.Title,
				Subtitle:      r.Subtitle,
				MinPriceCents: r.MinPriceCents,
				MaxPriceCents: r.MaxPriceCents,
				SalesCount:    r.SalesCount,
				Status:        r.Status,
			})
		}
		return nil
	})
	if err != nil {
		return ProductList{}, err
	}
	return out, nil
}

// clampPaging 把越界的分页参数收进契约允许的范围。
//
// 钳制而不是报 400：page / page_size 是可选参数，客户端漏传、传 0、传个巨大的
// page_size 想「一次拿完」都是常见写法，把这些变成错误只会让接口难用。
// 但它们绝不能原样进 SQL —— page_size=100000 是一次全表扫描外加一个十万行的
// 响应体，谁都可以匿名发起（这个接口 security: []）。
func clampPaging(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = DefaultPageSize
	}
	if pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}
	return page, pageSize
}

// maxOffset 是 OFFSET 的上限。它等于 sqlc 参数的 int32 宽度 —— 那是 db/queries
// 里 $2 的实际类型，不是随手挑的一个大数。
const maxOffset = int64(1)<<31 - 1

// offsetOf 算偏移量，并且不让它溢出。
//
// 必须用 int64 算：page 是客户端给的，(page-1)*pageSize 在 int32 里早就绕回负数了，
// 而负的 OFFSET 会让 Postgres 报错 —— 一个 500，错误信息里只字不提「页码太大」。
// 超出上限时钳到上限：那之后的页本来就是空的，返回空页比返回一个错误更诚实。
func offsetOf(page, pageSize int) int64 {
	// 先把 page 本身收住，再乘。不先收的话，`page=9000000000000000000` 这种输入
	// 会让乘法在 int64 里绕回来 —— 绕回来的结果可以是任意值，包括一个看着很正常的
	// 小正数，于是下面那个 `off < 0` 的检查不响，客户端拿到的是随机的一页。
	if int64(page) > maxOffset {
		return maxOffset
	}
	off := int64(page-1) * int64(pageSize)
	if off < 0 || off > maxOffset {
		return maxOffset
	}
	return off
}
