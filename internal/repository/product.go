package repository

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// Product 是商品在 repository 边界上的形状 —— 领域类型，不是 sqlc 的产物。
//
// 为什么不直接把 db.ListProductsRow 用类型别名导出去（那样少写一个结构体和
// 一段拷贝）：别名会把生成代码的形状焊进 service 的函数签名。之后每一次改
// db/queries/*.sql —— 多 SELECT 一列、把一列改名 —— 都会直接改掉 service 与
// handler 能看到的类型，而 sqlc 的重生成是一条不经人眼的自动化路径。
// 这里多写的这几行，换的是「查询怎么写」与「业务看到什么」之间的一道缝。
//
// 字段类型也在这里收口：sqlc 按 PostgreSQL 的 int4/int8 出 int32/int64，
// 那是列的宽度，不是业务的语义。
type Product struct {
	ID            int64
	Title         string
	Subtitle      *string
	MinPriceCents int64
	MaxPriceCents int64
	TotalStock    int32
	SalesCount    int32
	Status        int16
}

// Tx 是一次租户事务里能做的全部事情，也是 service 能拿到的全部。
//
// 这个接口存在的理由是 *db.Queries 上有一个 WithTx(pgx.Tx) 方法。它是生成代码
// 的导出方法，所以只要 service 手里握着 *db.Queries，它就能自己 Begin 一个事务
// 再 WithTx 过去 —— 那条路上没有 set_config('app.merchant_id')，查询会直接撞上
// current_merchant() 抛的 42501，症状是线上偶发的 insufficient_privilege，
// 而不是任何指向「少设了租户」的信息。
//
// 交出接口而不是具体类型，那个方法在 service 那一侧就不存在了：不是「不该调」，
// 是编译器不认得。fn 收到的 Tx 背后永远是本包在 WithTenant 里建出来的实现，
// 而 WithTenant 是唯一能建出它的地方。
//
// 新查询加进来时，这个接口跟着长 —— 长的过程本身就是一次复核：
// 这个能力确实要交给业务层吗？
//
// 它由若干个按主题拆开的接口嵌套而成，而不是一张平铺的方法表。理由是并发：
// 同一时期有好几条任务在往这一层加能力，平铺意味着他们改的是同一处声明的
// 同一批行。拆开之后每条任务加自己那个文件里的方法，这里只多一行嵌入。
type Tx interface {
	ProductTx
	SagaTx
	UserTx
	OrderTx
	OrderQueryTx
	SweepTx
	PaymentTx
	IndexTx
	JobTx
	SearchTx
	StaffTx
	AdminCatalogTx
	StoreTx
	ScopedCatalogTx
	CouponTx
	AddressBookTx
	ProfileTx
	CartTx
	FulfillmentTx
	RefundTx
	AdminOrderTx
	ProductImportTx
}

// StoreScope 是「本次请求按哪家门店算」——门店 id 与它所属的大区 id。
//
// 两个值一起传而不是只传 store_id 再让每条查询自己去 JOIN 一次 stores：
// 三层定价与两层可见性排除各要用到其中一个，而它们出现在同一条 SQL 的
// 四个不同位置（视图的 store_id、两条 NOT EXISTS 的 region_id / store_id）。
// 让查询自己去推的话，那次 JOIN 会在每一条读路径上各写一遍。
//
// 它由 service 层的门店解析产出（坐标 → 围栏 → 门店 → 它的大区），
// 那一次解析同时给出这两个值 —— 大区没有自己的几何，正是为了这一点
// （数据模型 §4）。
type StoreScope struct {
	StoreID  int64
	RegionID int64
}

// ProductTx 是商品读取这一面。
//
// **本轮（00020）每一条都多了一个 StoreScope。** 「这件商品多少钱、有没有货、
// 卖不卖」在多门店之后都取决于哪一家店服务这次请求，而一个不带门店的读路径
// 只能回一个租户级的答案 —— 那个答案对任何一个具体的买家都是错的。
type ProductTx interface {
	// ListProducts 返回当前租户在这家门店可见的在架商品，按上架时间倒序。
	//
	// limit / offset 的钳制是业务规则，在 service 里做。这里只负责把它们安全地
	// 送进 int32 的参数位 —— 越界的值到这一层还是要挡，因为 int32 溢出的后果是
	// 一个负数 OFFSET，Postgres 会报错，而错误里没有任何东西指向「页码太大」。
	//
	// categoryID 为 nil 表示不按类目筛；非 nil 时**含子孙**（db/queries/products.sql
	// 文件头那一段）。不存在或已软删的类目返回空列表，不是错误。
	ListProducts(ctx context.Context, sc StoreScope, categoryID *int64, limit, offset int64) ([]Product, error)

	// CountProducts 返回当前租户在架商品的总数，用于填契约里必填的 total。
	// categoryID 必须与同一页 ListProducts 传的是同一个 —— 两边条件不一致，
	// total 数的就不是列表实际会分出来的那批行。
	CountProducts(ctx context.Context, sc StoreScope, categoryID *int64) (int64, error)

	// ListVisibleCategories 返回启用且未软删的类目，扁平，父节点先于子节点。
	// 拼成树是 service 的事（那里有「父节点停用则整棵子树不显示」这条规则）。
	ListVisibleCategories(ctx context.Context) ([]CategoryNode, error)

	// FindProduct 取一件**可见**商品（在架且未软删）。查不到返回 ErrProductNotFound。
	FindProduct(ctx context.Context, sc StoreScope, id int64) (ProductDetail, error)

	// ListProductSKUs 取一件商品的全部在售 SKU，带上当前可售水位。
	ListProductSKUs(ctx context.Context, sc StoreScope, productID int64) ([]SKU, error)

	// 库存的两个方法搬去了 SagaTx（saga.go）：它们本来就是为 SAGA 分支存在的
	// —— 正向扣减、补偿回补、超时关单释放。它们的三条出路是这一层唯一一处
	// 「用返回值的形状去挡一类误用」的设计，注释仍在 inventory.go。
}

// tenantTx 是 Tx 的唯一实现：一层薄薄的转换，把 sqlc 的行变成领域类型。
//
// scope 是这个事务的作用域租户：WithTenant 里是 ctx 里那一个，
// WithPlatform 里是 nil（平台级，不属于任何一家店 —— 数据模型 §14）。
//
// 它在这里而不是从行里 SELECT 出来，理由与 User 上那句「没有 MerchantID」
// 一字不差：每一次读写都发生在一个设好作用域的事务里，所以查出来的行必然
// 属于当前作用域，再从行里读一遍只会制造第二个可能与它对不上的真相。
// 眼下只有 staff 用得上它（那张表的租户列可空，所以它是响应的一部分），
// 别的领域类型仍然完全不带租户。
type tenantTx struct {
	q     *db.Queries
	scope *int64
}

func (t tenantTx) ListProducts(ctx context.Context, sc StoreScope, categoryID *int64, limit, offset int64) ([]Product, error) {
	// 到这里还越界只可能是上游的钳制没生效。报错而不是截断：截断会把
	// 「第 1 亿页」悄悄变成某一页真实数据，一个错误的结果比一个错误更难发现。
	if limit < 0 || limit > math.MaxInt32 {
		return nil, fmt.Errorf("limit %d 超出范围 [0, %d]", limit, math.MaxInt32)
	}
	if offset < 0 || offset > math.MaxInt32 {
		return nil, fmt.Errorf("offset %d 超出范围 [0, %d]", offset, math.MaxInt32)
	}

	rows, err := t.q.ListProducts(ctx, db.ListProductsParams{
		StoreID:    sc.StoreID,
		RegionID:   sc.RegionID,
		CategoryID: categoryID,
		PageLimit:  int32(limit),
		PageOffset: int32(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Product, 0, len(rows))
	for _, r := range rows {
		out = append(out, Product{
			ID:            r.ID,
			Title:         r.Title,
			Subtitle:      r.Subtitle,
			MinPriceCents: r.MinPriceCents,
			MaxPriceCents: r.MaxPriceCents,
			TotalStock:    r.TotalStock,
			SalesCount:    r.SalesCount,
			Status:        r.Status,
		})
	}
	return out, nil
}

func (t tenantTx) CountProducts(ctx context.Context, sc StoreScope, categoryID *int64) (int64, error) {
	return t.q.CountProducts(ctx, db.CountProductsParams{
		StoreID: sc.StoreID, RegionID: sc.RegionID, CategoryID: categoryID,
	})
}

// CategoryNode 是一个启用中的类目，扁平形态。树由 service 拼。
type CategoryNode struct {
	ID        int64
	ParentID  *int64
	Name      string
	Level     int16
	SortOrder int32
}

func (t tenantTx) ListVisibleCategories(ctx context.Context) ([]CategoryNode, error) {
	rows, err := t.q.ListVisibleCategories(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CategoryNode, 0, len(rows))
	for _, r := range rows {
		out = append(out, CategoryNode{
			ID: r.ID, ParentID: r.ParentID, Name: r.Name,
			Level: r.Level, SortOrder: r.SortOrder,
		})
	}
	return out, nil
}

// ProductDetail 是商品详情在 repository 边界上的形状（契约的 ProductDetail）。
//
// 与 Product 分开而不是给它加几个字段：列表一页要搬 20 行，而 description
// 是一整段富文本。两者共用一个结构体的话，列表要么白搬这些字节，要么把它们
// 留成零值 —— 而「零值」与「这件商品没有描述」在 *string 上长得一模一样。
type ProductDetail struct {
	ID            int64
	CategoryID    int64
	Title         string
	Subtitle      *string
	Description   *string
	MinPriceCents int64
	MaxPriceCents int64
	SalesCount    int32
	Status        int16
}

// SKU 是详情页里的一个规格，带上当前可售水位。
//
// AvailableQty 来自 inventories 的 LEFT JOIN，没有库存行时是 0。
// SpecValues 是 JSONB 原样的字节：这一层不解释它，解释放在 service ——
// repository 认得的是列的类型，不是它的语义。
type SKU struct {
	ID           int64
	SKUCode      string
	SpecValues   []byte
	PriceCents   int64
	ImageURL     *string
	AvailableQty int32
}

// ErrProductNotFound：这个 product_id 在本租户查不到，或者它不可见
// （草稿 / 已下架 / 已软删）。
//
// 三种成因刻意合成一个：商品 id 是自增的，所以「这个 id 存在但你看不到」
// 与「这个 id 不存在」一旦分开报，这个接口就成了一个能数出别家店有多少商品的
// 探测器 —— 而它是 security: [] 的，谁都打得到。
var ErrProductNotFound = errors.New("商品不存在或不可见")

func (t tenantTx) FindProduct(ctx context.Context, sc StoreScope, id int64) (ProductDetail, error) {
	r, err := t.q.GetProduct(ctx, db.GetProductParams{
		StoreID: sc.StoreID, RegionID: sc.RegionID, ID: id,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ProductDetail{}, fmt.Errorf("product %d: %w", id, ErrProductNotFound)
	}
	if err != nil {
		return ProductDetail{}, err
	}
	return ProductDetail{
		ID:            r.ID,
		CategoryID:    r.CategoryID,
		Title:         r.Title,
		Subtitle:      r.Subtitle,
		Description:   r.Description,
		MinPriceCents: r.MinPriceCents,
		MaxPriceCents: r.MaxPriceCents,
		SalesCount:    r.SalesCount,
		Status:        r.Status,
	}, nil
}

func (t tenantTx) ListProductSKUs(ctx context.Context, sc StoreScope, productID int64) ([]SKU, error) {
	rows, err := t.q.ListProductSKUs(ctx, db.ListProductSKUsParams{
		StoreID: sc.StoreID, ProductID: productID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]SKU, 0, len(rows))
	for _, r := range rows {
		out = append(out, SKU{
			ID:           r.ID,
			SKUCode:      r.SkuCode,
			SpecValues:   r.SpecValues,
			PriceCents:   r.PriceCents,
			ImageURL:     r.ImageUrl,
			AvailableQty: r.AvailableQty,
		})
	}
	return out, nil
}
