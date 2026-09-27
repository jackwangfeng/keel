// Package service 承载业务规则与事务编排入口。这里不拼 SQL。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/keel/keel/internal/inventory"
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

	// ImageURL 是主图的对外地址（契约 ProductSummary.image_url：「取的就是
	// images[0]」），形如 /api/v1/uploads/{id}。这件商品一张图都没有时为 nil，
	// handler 据此让字段**缺席**：空串会让客户端去请求一个空地址、渲染一张
	// 「加载失败」，而缺席让它走自己的占位封面（app/src/api/view.uts 的 coverOf）。
	ImageURL *string

	// PromotionTags 是这件商品在这家店此刻生效的活动标签（00058，promotion_tags.go）。
	// MinPriceCents 仍是门店价：活动价看标签与 SKU.PromoPriceCents。
	PromotionTags []ProductPromotionTag

	// InStock：这家店里任意一个在售 SKU 水位 > 0（与详情页、检索同一个判据）。
	// nil 表示这一次不知道（拆分部署下库存服务不在）—— handler 让字段缺席，客户端不敢说它没货。
	// 2026-09-27 之前列表根本不读库存，这个字段恒缺席，于是买家端列表上无货的商品照样挂着「＋」。
	InStock *bool
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

type ProductService struct {
	repo ProductRepository
	// inv 是库存服务（微服务拆分阶段 1a）：详情页的 SKU 水位与 in_stock 经它。
	inv inventory.Service
}

func NewProductService(r ProductRepository, inv inventory.Service) *ProductService {
	return &ProductService{repo: r, inv: inv}
}

// List 返回当前租户的在架商品。
//
// page / pageSize 在这里钳制，不在 handler：分页规则是业务规则，
// 换一个 handler（比如将来的 gRPC）不该重写一遍。
// storeID 非 nil 表示客户端显式指名了一家门店；nil 走回落链（数据模型 §4：
// 「没有位置」与「位置不在任何围栏内」是同一条路径）。
//
// categoryID 非 nil 时按类目筛，含子孙。它和 storeID 一样只是被透传 ——
// 「含子孙」「不存在的类目给空列表」这两条规则在 SQL 里（products.sql 文件头），
// 这里重复一遍就是两份会分叉的实现。
func (s *ProductService) List(ctx context.Context, storeID, categoryID *int64, page, pageSize int) (ProductList, error) {
	page, pageSize = clampPaging(page, pageSize)

	out := ProductList{Items: []ProductSummary{}, Page: page, PageSize: pageSize}
	var promo promoTagMaterial
	var onSale map[int64][]int64
	var stockStore int64
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
		total, err := q.CountProducts(ctx, sc, categoryID)
		if err != nil {
			return err
		}
		out.Total = total

		rows, err := q.ListProducts(ctx, sc, categoryID, int64(pageSize), offsetOf(page, pageSize))
		if err != nil {
			return err
		}
		ids := make([]int64, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		if promo, err = loadPromotionTags(ctx, q, sc, ids, time.Now()); err != nil {
			return err
		}
		if onSale, err = q.OnSaleSKUsOfProducts(ctx, ids); err != nil {
			return err
		}
		stockStore = sc.StoreID
		for _, r := range rows {
			out.Items = append(out.Items, ProductSummary{
				ID:            r.ID,
				Title:         r.Title,
				Subtitle:      r.Subtitle,
				MinPriceCents: r.MinPriceCents,
				MaxPriceCents: r.MaxPriceCents,
				SalesCount:    r.SalesCount,
				Status:        r.Status,
				ImageURL:      imageURLOf(r.MainImageUploadID),
			})
		}
		return nil
	})
	if err != nil {
		return ProductList{}, err
	}
	// 活动标签在事务之后算（秒杀配额要问库存服务，promotion_tags.go）。列表不读库存，
	// 库存服务不在时照常返回（标签里没有单价类活动），与 in_stock 缺席同一个降级口径。
	tags, _, err := promo.finish(ctx, s.inv, true)
	if err != nil {
		return ProductList{}, err
	}
	for i := range out.Items {
		out.Items[i].PromotionTags = tags[out.Items[i].ID]
	}
	s.fillInStock(ctx, stockStore, onSale, out.Items)
	return out, nil
}

// fillInStock 按这家店一次批量问库存服务，给这一页每件商品填 InStock。
// 在事务之外问（拆分部署下是一次网络往返）；问不到就整页不填 —— 列表照常返回，
// in_stock 缺席即「这次不知道」，与检索同一个降级口径（search.go 的 applyStock）。
func (s *ProductService) fillInStock(ctx context.Context, storeID int64,
	onSale map[int64][]int64, items []ProductSummary) {
	if s.inv == nil || storeID == 0 || len(items) == 0 {
		return
	}
	var all []int64
	for _, list := range onSale {
		all = append(all, list...)
	}
	levels := map[int64]inventory.Level{}
	if len(all) > 0 {
		var err error
		levels, err = s.inv.StoreStock(ctx, storeID, all)
		if err != nil {
			slog.WarnContext(ctx, "商品列表问不到库存，这一页 in_stock 缺席", "store_id", storeID, "err", err)
			return
		}
	}
	for i := range items {
		in := false
		for _, id := range onSale[items[i].ID] {
			if levels[id].Available > 0 {
				in = true
				break
			}
		}
		items[i].InStock = &in
	}
}

// imageURLOf 把主图的 upload id 拼成对外地址；没有图（nil）时回 nil。
//
// 列表与检索共用这一个：两边拼法不同的话，同一件商品在列表与搜索结果里
// 会顶着两个不同的 image_url，而客户端按 URL 做图片缓存。
func imageURLOf(uploadID *int64) *string {
	if uploadID == nil {
		return nil
	}
	u := UploadURL(*uploadID)
	return &u
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

	// PromoPriceCents / PromotionID：这家店此刻的活动价与给出它的活动（00058）。
	// 没有单价类活动、或特价不低于门店价时为 nil。
	PromoPriceCents *int64
	PromotionID     *int64
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

	// Images 是这件商品的全部商品图地址，按 sort_order（再按 id）排好，
	// 第 0 张即主图，与 ProductSummary.ImageURL 是同一张。
	//
	// 形状是地址串的数组而不是 ProductImage 对象：契约的 ProductDetail.images
	// 就是 `items: { type: string }`。upload_id / sort_order 是后台管图要的东西，
	// 买家端拿地址直接喂 <image src>，顺序即数组下标。
	//
	// 一张图都没有时为 nil（handler 让字段缺席），理由同 ImageURL。
	Images []string

	// Store 同 ProductList.Store：SKU 的 price_cents 与 available_qty 都是
	// **这家门店**的值，不写明是哪一家，它们就是三个不知道属于谁的数。
	Store StoreContext

	// InStock 是「这件商品现在还买得到吗」：任意一个在售 SKU 水位 > 0。
	//
	// 它是**算出来的**，不读任何汇总列（products.total_stock 没人维护，已停用，见 00062）：
	// 详情页正下方就列着每个 SKU 的真实水位 —— 汇总与它对不上时，
	// 用户看到的是「有货」配一排全是 0 的规格。
	InStock bool
}

// Detail 返回一件在架商品的详情，含全部在售 SKU 与它们的可售水位。
//
// 商品与 SKU 在**同一个事务**里读，理由与 List 里那对计数/取页一样：
// 分两次访问的话，中间的一次下架会让「商品在架，但一个 SKU 都没有」这种
// 自相矛盾的响应偶发出现。
//
// **水位在事务之后向库存服务批量问一次**（微服务拆分阶段 1a），按这家门店、这批 SKU，
// 没问到的记可售 0（缺行 ≡ 可售 0）。在事务之外问，是为了不在攥着一条业务连接时
// 等下游（单体形态下库存池就是业务池，那是整池互等；见 inventory_admin.go 的文件头）。
//
// 库存服务不可用（只在拆分形态出现）时整个详情回 503，而不是给一排 0：
// Sku.available_qty 在契约里是必填的，编一个 0 会把每个规格渲染成售罄，
// 而详情页存在的全部理由就是让用户挑一个有货的规格去买。列表与检索不受影响
// （它们的 in_stock 是可选字段，照常返回）。
func (s *ProductService) Detail(ctx context.Context, storeID *int64, id int64) (ProductDetail, error) {
	var (
		out     ProductDetail
		stockAt int64
		promo   promoTagMaterial
	)
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
		stockAt = sc.StoreID
		rows, err := q.ListProductSKUs(ctx, sc, p.ID)
		if err != nil {
			return err
		}
		if promo, err = loadPromotionTags(ctx, q, sc, []int64{p.ID}, time.Now()); err != nil {
			return err
		}

		// 商品图。借用后台那条 ListProductImages（同一条 SQL，同一个排序键），
		// 而不是再写一条买家版：两条写岔的话，后台排好的第一张与买家看到的主图
		// 会是两张图。它本身不判可见性 —— 用不着：走到这里 FindProduct 已经
		// 确认过这件商品在架、未软删、这家店卖；租户照例由 RLS 挡。
		imgs, err := q.ListProductImages(ctx, p.ID)
		if err != nil {
			return err
		}
		var images []string
		for _, im := range imgs {
			images = append(images, UploadURL(im.UploadID))
		}
		var mainImage *string
		if len(images) > 0 {
			mainImage = &images[0]
		}

		skus := make([]SKU, 0, len(rows))
		for _, r := range rows {
			spec, err := DecodeSpecValues(r.SpecValues)
			if err != nil {
				return fmt.Errorf("sku %d 的 spec_values 解不开: %w", r.ID, err)
			}
			sku := SKU{
				ID:         r.ID,
				SKUCode:    r.SKUCode,
				SpecValues: spec,
				PriceCents: r.PriceCents,
				ImageURL:   r.ImageURL,
			}
			skus = append(skus, sku)
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
				ImageURL:      mainImage,
			},
			CategoryID:  p.CategoryID,
			Description: p.Description,
			SKUs:        skus,
			Images:      images,
		}
		return nil
	})
	if err != nil {
		return ProductDetail{}, err
	}

	ids := make([]int64, 0, len(out.SKUs))
	for _, sk := range out.SKUs {
		ids = append(ids, sk.ID)
	}
	levels, err := s.inv.StoreStock(ctx, stockAt, ids)
	if err != nil {
		return ProductDetail{}, err
	}
	// 活动标签与各 SKU 的活动价：秒杀配额在库存服务，事务之后问（promotion_tags.go）。
	// 问不到时与水位同一个口径：整页 503（degrade = false）。
	tags, promoPrices, err := promo.finish(ctx, s.inv, false)
	if err != nil {
		return ProductDetail{}, err
	}
	out.PromotionTags = tags[out.ID]
	for i := range out.SKUs {
		out.SKUs[i].AvailableQty = levels[out.SKUs[i].ID].Available
		if out.SKUs[i].AvailableQty > 0 {
			out.InStock = true
		}
		if pp, ok := promoPrices[out.SKUs[i].ID]; ok {
			price, pid := pp.PriceCents, pp.PromotionID
			out.SKUs[i].PromoPriceCents, out.SKUs[i].PromotionID = &price, &pid
		}
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

// Category 是买家侧类目树上的一个节点（契约 Category）。
type Category struct {
	ID       int64
	Name     string
	Level    int
	Children []Category
}

// Categories 返回当前租户启用中的类目树（契约 GET /categories）。
//
// ## 父节点停用了，子节点怎么办
//
// **整棵子树都不显示。** 商家停用一个类目，意思是把它连同下面的东西从导航里
// 收起来；把孤儿子类目提到顶层，等于「停用」只停了一半，而且买家会在首页看到
// 一个莫名其妙冒出来的二级类目。
//
// 实现就是「只挂到在场的父节点上」：查询只返回启用的，父节点不在结果里的那个
// 子节点就挂不上去，它的子孙也跟着挂不上去 —— 规则是传递的，不需要单独写。
// 软删同理（查询本来就排除了软删）。
//
// 这条规则只管**导航**。停用类目下的在架商品照样能在列表里看到，见
// products.sql 文件头关于 category_id 那一段。
func (s *ProductService) Categories(ctx context.Context) ([]Category, error) {
	var nodes []repository.CategoryNode
	err := s.repo.WithTenant(ctx, func(q repository.Tx) error {
		var e error
		nodes, e = q.ListVisibleCategories(ctx)
		return e
	})
	if err != nil {
		return nil, err
	}
	return buildCategoryTree(nodes), nil
}

// buildCategoryTree 把扁平的节点拼成树。输入按 level 升序（SQL 保证），
// 所以一趟就能挂完：处理到某个节点时，它的父节点要么已经在树上，要么永远不会来。
//
// 用 id → 路径下标 的方式挂，而不是 id → *Category：Children 是值切片，
// append 会搬家，先拿到的指针会指向旧底层数组，后挂的子节点就丢了。
func buildCategoryTree(nodes []repository.CategoryNode) []Category {
	roots := []Category{}
	// 每个已挂上的节点，从根到它的下标路径。
	where := map[int64][]int{}
	for _, n := range nodes {
		c := Category{ID: n.ID, Name: n.Name, Level: int(n.Level), Children: []Category{}}
		if n.ParentID == nil {
			roots = append(roots, c)
			where[n.ID] = []int{len(roots) - 1}
			continue
		}
		path, ok := where[*n.ParentID]
		if !ok {
			// 父节点停用、软删，或者本来就不属于这棵树：整支不显示（见上）。
			continue
		}
		parent := &roots[path[0]]
		for _, i := range path[1:] {
			parent = &parent.Children[i]
		}
		parent.Children = append(parent.Children, c)
		child := make([]int, len(path)+1)
		copy(child, path)
		child[len(path)] = len(parent.Children) - 1
		where[n.ID] = child
	}
	return roots
}
