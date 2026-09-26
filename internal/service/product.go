// Package service 承载业务规则与事务编排入口。这里不拼 SQL。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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

	// Store 是「本次结果按哪家门店算的」，契约里**必返**。
	//
	// 没有它，客户端拿到的 in_stock 与价格是不知道属于谁的 —— 按门店分之后，
	// 同一件商品对不同的人有不同的答案。MatchType = none 时 Items 是空数组，
	// 那是「你不在服务范围」，不是「这家店没有商品」。
	Store StoreContext
}

// StoreContext 是每一条受门店影响的读接口都要回显的那一小块（契约的
// StoreContext）。StoreID / RegionID 在 MatchNone 时为 nil。
type StoreContext struct {
	MatchType MatchType
	StoreID   *int64
	RegionID  *int64
}

// storeContextOf 把一次解析的结果拍成 StoreContext。
//
// 一个函数而不是在四条读路径上各拼一遍：拼岔一处的症状是某一条接口的
// match_type 与它实际用的门店对不上，而那看上去完全正常。
func storeContextOf(sc repository.StoreScope, mt MatchType) StoreContext {
	out := StoreContext{MatchType: mt}
	if mt == MatchNone {
		return out
	}
	storeID, regionID := sc.StoreID, sc.RegionID
	out.StoreID, out.RegionID = &storeID, &regionID
	return out
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
// storeID 非 nil 表示客户端显式指名了一家门店；nil 走回落链（数据模型 §4：
// 「没有位置」与「位置不在任何围栏内」是同一条路径）。
func (s *ProductService) List(ctx context.Context, storeID *int64, page, pageSize int) (ProductList, error) {
	page, pageSize = clampPaging(page, pageSize)

	out := ProductList{Items: []ProductSummary{}, Page: page, PageSize: pageSize}
	err := s.repo.WithTenant(ctx, func(q repository.Tx) error {
		// 门店解析与后面两条查询在**同一个事务**里：分成两次的话，
		// 两者之间的一次门店软删会让「解析到了 A 店」与「按 A 店读商品」
		// 看到不同的世界。
		sc, mt, err := scopeIn(ctx, q, storeID)
		if errors.Is(err, ErrOutOfServiceArea) {
			// 不在服务范围：空列表 + match_type = none，**HTTP 200**。
			// 那不是「这家店没有商品」，客户端要渲染的是完全不同的页面。
			out.Store = StoreContext{MatchType: MatchNone}
			return nil
		}
		if err != nil {
			return err
		}
		out.Store = storeContextOf(sc, mt)

		// 计数与取页在同一个事务里，所以 total 和 items 看到的是同一个快照。
		// 分开两次访问的话，两者之间的一次上下架会让「total=21 但第二页是空的」
		// 这种自相矛盾的响应偶发出现。
		total, err := q.CountProducts(ctx, sc)
		if err != nil {
			return err
		}
		out.Total = total

		rows, err := q.ListProducts(ctx, sc, int64(pageSize), offsetOf(page, pageSize))
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

// ErrProductNotFound：这个 product_id 在本租户查不到，或者它不可见
// （草稿 / 已下架 / 已软删）。handler 把它映射成契约里的 404。
//
// 三种成因合成一个，理由写在 repository.ErrProductNotFound 上：商品 id 是自增的，
// 而这条接口是 security: []。
var ErrProductNotFound = errors.New("商品不存在")

// SKU 是详情里的一个规格（契约的 Sku）。
//
// SpecValues 在这一层已经是 map 了，不是 JSONB 的字节：repository 认得列的类型，
// service 认得它的语义。解不开就报错而不是给个空 map —— 空 map 会让详情页显示
// 一件没有任何规格的商品，而用户点下去才发现选不了。
type SKU struct {
	ID           int64
	SKUCode      string
	SpecValues   map[string]string
	PriceCents   int64
	ImageURL     *string
	AvailableQty int32
}

// ProductDetail 是一件商品的详情（契约的 ProductDetail = ProductSummary + 四个字段）。
//
// 内嵌 ProductSummary 而不是把七个字段再抄一遍：契约里它就是 allOf 的第一项，
// 而抄一遍意味着列表与详情可以对同一件商品给出不同的 status / 价格区间。
type ProductDetail struct {
	ProductSummary

	CategoryID  int64
	Description *string
	SKUs        []SKU

	// Store 同 ProductList.Store：SKU 的 price_cents 与 available_qty 都是
	// **这家门店**的值，不写明是哪一家，它们就是三个不知道属于谁的数。
	Store StoreContext

	// InStock 是「这件商品现在还买得到吗」：任意一个在售 SKU 水位 > 0。
	//
	// 它是**算出来的**，不是 products.total_stock 那一列。那一列是冗余的汇总，
	// 由别处维护，而详情页正下方就列着每个 SKU 的真实水位 —— 两个数对不上时，
	// 用户看到的是「有货」配一排全是 0 的规格。
	InStock bool
}

// Detail 返回一件在架商品的详情，含全部在售 SKU 与它们的可售水位。
//
// 商品与 SKU 在**同一个事务**里读，理由与 List 里那对计数/取页一样：
// 分两次访问的话，中间的一次下架会让「商品在架，但一个 SKU 都没有」这种
// 自相矛盾的响应偶发出现。
func (s *ProductService) Detail(ctx context.Context, storeID *int64, id int64) (ProductDetail, error) {
	var out ProductDetail
	err := s.repo.WithTenant(ctx, func(q repository.Tx) error {
		sc, mt, err := scopeIn(ctx, q, storeID)
		if errors.Is(err, ErrOutOfServiceArea) {
			// 不在服务范围时详情是 404，不是一个「空的详情页」：
			// 这条路径要么给出一件能买的商品，要么说没有。
			// 与列表那边的 200 + 空数组不同，因为列表的语义是「有哪些」，
			// 详情的语义是「这一件」—— 而「这一件」在这里确实不存在。
			return fmt.Errorf("%w: product_id=%d（不在服务范围）", ErrProductNotFound, id)
		}
		if err != nil {
			return err
		}
		p, err := q.FindProduct(ctx, sc, id)
		if errors.Is(err, repository.ErrProductNotFound) {
			return fmt.Errorf("%w: product_id=%d", ErrProductNotFound, id)
		}
		if err != nil {
			return err
		}
		out.Store = storeContextOf(sc, mt)
		rows, err := q.ListProductSKUs(ctx, sc, p.ID)
		if err != nil {
			return err
		}

		skus := make([]SKU, 0, len(rows))
		inStock := false
		for _, r := range rows {
			spec, err := DecodeSpecValues(r.SpecValues)
			if err != nil {
				return fmt.Errorf("sku %d 的 spec_values 解不开: %w", r.ID, err)
			}
			if r.AvailableQty > 0 {
				inStock = true
			}
			skus = append(skus, SKU{
				ID:           r.ID,
				SKUCode:      r.SKUCode,
				SpecValues:   spec,
				PriceCents:   r.PriceCents,
				ImageURL:     r.ImageURL,
				AvailableQty: r.AvailableQty,
			})
		}
		out = ProductDetail{
			ProductSummary: ProductSummary{
				ID:            p.ID,
				Title:         p.Title,
				Subtitle:      p.Subtitle,
				MinPriceCents: p.MinPriceCents,
				MaxPriceCents: p.MaxPriceCents,
				SalesCount:    p.SalesCount,
				Status:        p.Status,
			},
			CategoryID:  p.CategoryID,
			Description: p.Description,
			SKUs:        skus,
			InStock:     inStock,
		}
		return nil
	})
	if err != nil {
		return ProductDetail{}, err
	}
	return out, nil
}

// DecodeSpecValues 把 JSONB 的字节解成 map[string]string（契约的
// `additionalProperties: {type: string}`）。
//
// 导出是因为 handler 那边读 order_items.spec_snapshot 要用同一套解法。
// 那里对**失败**的处置与这里相反（历史快照解不开不该让订单打不开），
// 但「怎么解」必须是同一份：两份解法意味着同一块 JSONB 在商品页和订单页上
// 可以显示出不同的规格。
//
// 空字节按空 map 处理：列的 DEFAULT 是 '{}'，但历史行里可能是 SQL NULL，
// 而 nil 与 []byte("{}") 在这里是同一个意思 —— 这件 SKU 没有规格维度。
// 值不是字符串时报错而不是丢掉那一项：静默丢掉会让「颜色」这一维凭空消失，
// 而用户在下单时并不知道自己少选了一个维度。
func DecodeSpecValues(raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return map[string]string{}, nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return map[string]string{}, nil
	}
	return m, nil
}
