package repository

import (
	"context"
	"errors"
	"fmt"

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
	SalesCount    int32
	Status        int16

	// MainImageUploadID 是主图（product_images 里 sort_order 最小的那一张）的
	// upload id；这件商品一张图都没有时为 nil。这一层只交 id 不交 URL：
	// URL 的形状（/api/v1/uploads/{id}）是 service.UploadURL 的事，理由同
	// handler/admin_catalog.go 里 ProductImage.url 那一段 —— repository 认得的是列。
	MainImageUploadID *int64
}

// mainImageOf 把查询里的「0 = 没有图」翻回 nil。
//
// 0 是 SQL 那边的权宜（sqlc 推不出 LEFT JOIN LATERAL 那一侧可空，
// 见 db/queries/products.sql 的 ListProducts），翻译只在这里做一次：
// 让 0 漏到 service，下游就会拼出一个 /api/v1/uploads/0，
// 买家端渲染的是一张 404 的图，而不是「这件商品没有图」的占位。
func mainImageOf(uploadID int64) *int64 {
	if uploadID <= 0 {
		return nil
	}
	return &uploadID
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
	PromotionTx
	ProductImportTx
	ReportTx
	NotificationTx
	FreightTx
	ShopPreferencesTx
	ReconcileTx
	StockFlagTx
	RestockTx
	AgentProposalTx
	AgentBriefTx
	AgentEventTx
	AgentComputeTx
	PaymentReturnTx
	MsgTx
	SearchEvalTx
	SearchJudgmentTx
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
	// ListProducts 返回当前租户在这家门店可见的在架商品，有货在前、再按上架时间倒序。
	//
	// limit / offset 的钳制是业务规则，在 service 里做。这里只负责把它们安全地
	// 送进 int32 的参数位 —— 越界的值到这一层还是要挡，因为 int32 溢出的后果是
	// 一个负数 OFFSET，Postgres 会报错，而错误里没有任何东西指向「页码太大」。
	//
	// f.Categories 为 nil 表示不按类目筛；非 nil 时是已经解析好的子树（CategorySubtreeIDs），
	// 空切片（类目不存在或已软删）返回空列表，不是错误。
	ListProducts(ctx context.Context, sc StoreScope, f ListingFilter, limit, offset int64) ([]Product, error)

	// CountProducts 返回当前租户在架商品的总数，用于填契约里必填的 total。
	// f 必须与同一页 ListProducts 传的是同一个 —— 两边条件不一致，
	// total 数的就不是列表实际会分出来的那批行。它是 O(全店) 的一次计数，service 缓存它的结果。
	CountProducts(ctx context.Context, sc StoreScope, f ListingFilter) (int64, error)

	// ListProductsInStock / CountProductsInStock 是 GET /products?in_stock_only=true：只取「有货」那一段
	// （判据是有货排序标记，缺行按无货，db/queries/products.sql 文件头），段内次序与 ListProducts 相同。
	ListProductsInStock(ctx context.Context, sc StoreScope, f ListingFilter, limit, offset int64) ([]Product, error)
	CountProductsInStock(ctx context.Context, sc StoreScope, f ListingFilter) (int64, error)

	// CategorySubtreeIDs 返回一个类目连同全部子孙的 id（未软删的），买家列表按类目筛之前先取一次，
	// 结果放进 ListingFilter.Categories。类目不存在或已软删时返回**非 nil 的空切片** ——
	// nil 在 ListingFilter 里的意思是「不筛类目」，两者混了就是「过期的类目链接显示全部商品」。
	CategorySubtreeIDs(ctx context.Context, categoryID int64) ([]int64, error)

	// ListVisibleCategories 返回启用且未软删的类目，扁平，父节点先于子节点。
	// 拼成树是 service 的事（那里有「父节点停用则整棵子树不显示」这条规则）。
	ListVisibleCategories(ctx context.Context) ([]CategoryNode, error)

	// FindProduct 取一件**可见**商品（在架且未软删）。查不到返回 ErrProductNotFound。
	FindProduct(ctx context.Context, sc StoreScope, id int64) (ProductDetail, error)

	// ListProductSKUs 取一件商品的全部在售 SKU，带上当前可售水位。
	ListProductSKUs(ctx context.Context, sc StoreScope, productID int64) ([]SKU, error)

	// 库存的扣减与回补不在 core 的 Tx 上（微服务拆分阶段 1b）：它们归库存服务，
	// 仓储是 inventory_svc.go 的 InventoryStore（库存池、只碰库存的表）。
}

// ListingFilter 是买家商品列表（GET /products）的筛选条件。
type ListingFilter struct {
	// Categories 为 nil：不按类目筛。非 nil：只要这些类目里的商品 —— 已经解析好的子树
	// （CategorySubtreeIDs，含子孙）。空切片表示类目不存在或已软删：列表为空、计数为 0，不发 SQL。
	Categories []int64

	// Rows / StoreRows 只用来在两种按类目取页的写法之间挑一个（perCategoryCheaper），不影响结果：
	// Rows 是这批类目里大约有多少件，StoreRows 是不筛类目时全店大约多少件，与这一页同一个口径
	// （全部 / 只看有货）。service 拿缓存里的 total 填，晚几十秒无妨。0 表示不知道。
	Rows, StoreRows int64
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
	// raw 是底下那个事务本身：只读 SQL 工具（agent_sql.go）用它 —— 它要在同一个事务里 SET ROLE、跑一条
	// 不经 sqlc 的查询再复核租户；读渠道回调密钥（payment.go）也用它 —— shop_settings 没有 RLS、那条 SQL
	// 走裸 SQL，而事务里的调用方不能再去池上拿第二条连接。别的方法一律走 q。
	// 没有它的构造路径（平台作用域、开店）调那两个方法会报错。
	raw pgx.Tx
}

// jitOffWindow：offset+limit 超过它时，这个事务里先关掉 JIT 再取页。
//
// 规划器按估算代价决定要不要 JIT 编译（jit_above_cost 默认 10 万），而列表的估算代价随 offset 线性涨：
// 有货判据是一个标量子查询，规划器对它的选择率只能猜，深页、按类目筛的写法估得尤其高
// （10 万商品、类目占一成、OFFSET 980 估到 58 万）。一旦越线，每次执行先花 ≈200 ms 编译，
// 实际执行只要 12 ms（2026-10-01 在 10 万商品的压测库上实测，EXPLAIN 用 COSTS OFF 时看不到 JIT 那一段）。
// 浅页的估算代价离线还远（第一页 1 万上下），不为它们多发一条语句。
const jitOffWindow = 100

// beforeListing 在取页之前对这个事务做的准备：深页关 JIT（见 jitOffWindow）。
// set_config(..., true) 是事务内的，事务结束即失效，不会跟着连接回到池里。
func (t tenantTx) beforeListing(ctx context.Context, limit, offset int64) error {
	if offset+limit <= jitOffWindow || t.raw == nil {
		return nil
	}
	_, err := t.raw.Exec(ctx, `SELECT set_config('jit', 'off', true)`)
	return err
}

// wideScanRatio 是 perCategoryCheaper 里两种代价的折算：沿上架时间索引扫过一行、只判一次类目，
// 大约是「对一件候选商品做完两条 NOT EXISTS 与有货标记的点查」的十分之一（2026-10-01 压测库实测：
// 后者每行 ≈5 µs，前者 ≈0.5 µs）。
const wideScanRatio = 10

// perCategoryCheaper 决定按类目筛的一段用哪条语句取（db/queries/products.sql 的「类目筛选」一节）：
//
//   - per-category（ListProductsByStockInCategories）：每个类目沿 idx_products_listing_category 各取前
//     offset+limit 行再合并，每一行都要做完点查 —— 代价 ≈ min(类目里的件数, 类目数 × (offset+limit))。
//   - wide（ListProductsByStockInWideCategories）：沿 idx_products_listing_published 按序扫、逐行判类目，
//     凑满 offset+limit 行要扫 (offset+limit) / 占比 行，只有命中的那些做点查 ——
//     代价 ≈ (offset+limit) × (1 + 全店件数 / (wideScanRatio × 类目里的件数))。
//
// 只有一个类目（叶子）时 per-category 恒不比 wide 贵，不必知道件数；件数不知道时也选它 ——
// 它的代价有类目里的件数兜底，wide 在小类目上却是扫全店（原来的样子）。
// 选错不影响结果，只影响快慢：两条语句的筛选与次序逐字一致（rls_index_plans_test.go 逐行对照）。
func perCategoryCheaper(f ListingFilter, limit, offset int64) bool {
	n := int64(len(f.Categories))
	if n <= 1 || f.Rows <= 0 || f.StoreRows <= 0 {
		return true
	}
	window := offset + limit
	perCategory := min(f.Rows, n*window)
	wide := window + window*f.StoreRows/(wideScanRatio*f.Rows)
	return perCategory <= wide
}

// listSegment 取「有货」或「无货」那一段里的一页，按 f 挑语句。
func (t tenantTx) listSegment(ctx context.Context, sc StoreScope, f ListingFilter, inStock bool,
	limit, offset int64) ([]db.ListProductsByStockRow, error) {
	if f.Categories == nil {
		return t.q.ListProductsByStock(ctx, db.ListProductsByStockParams{
			StoreID: sc.StoreID, RegionID: sc.RegionID,
			InStock: inStock, PageLimit: int32(limit), PageOffset: int32(offset),
		})
	}
	if len(f.Categories) == 0 {
		return nil, nil
	}
	// 三条语句的列逐字一致，sqlc 生成的三个 Row 类型字段相同，可以直接转换。
	if perCategoryCheaper(f, limit, offset) {
		rows, err := t.q.ListProductsByStockInCategories(ctx, db.ListProductsByStockInCategoriesParams{
			CategoryIds: f.Categories, StoreID: sc.StoreID, RegionID: sc.RegionID,
			InStock: inStock, PageLimit: int32(limit), PageOffset: int32(offset),
		})
		out := make([]db.ListProductsByStockRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, db.ListProductsByStockRow(r))
		}
		return out, err
	}
	rows, err := t.q.ListProductsByStockInWideCategories(ctx, db.ListProductsByStockInWideCategoriesParams{
		CategoryIds: f.Categories, StoreID: sc.StoreID, RegionID: sc.RegionID,
		InStock: inStock, PageLimit: int32(limit), PageOffset: int32(offset),
	})
	out := make([]db.ListProductsByStockRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, db.ListProductsByStockRow(r))
	}
	return out, err
}

func (t tenantTx) ListProducts(ctx context.Context, sc StoreScope, f ListingFilter, limit, offset int64) ([]Product, error) {
	if err := checkPaging(limit, offset); err != nil {
		return nil, err
	}
	if f.Categories != nil && len(f.Categories) == 0 {
		return []Product{}, nil
	}
	if err := t.beforeListing(ctx, limit, offset); err != nil {
		return nil, err
	}

	// 「有货在前」分两段取（db/queries/products.sql 的 ListProductsByStock 说明了为什么）：
	// 先在有货段里取这一页；取满即返回。没取满说明这一页跨过了有货段的末尾，从无货段补。
	// 无货段的起点：有货段这一页取到了几行，就说明有货段恰好在 offset + 那几行处结束，
	// 无货段从 0 开始；一行都没取到，才要数一次有货段有多长。
	//
	// 那一次计数要**精确**，不走 service 的总数缓存：它决定无货段从第几行开始，差一行就是
	// 这一页与上一页重一件或漏一件。它只在整页都落在无货段时才发生（翻到列表尾部）。
	//
	// 两段是同一个事务里的两条语句（READ COMMITTED，各自一个快照）：两次之间有货标记被刷新，
	// 这一页的边界可能差一两行 —— 与跨两次请求翻页本来就有的漂移同一个量级。起点算成负数
	// 只可能是这种竞争，钳到 0。
	rows, err := t.listSegment(ctx, sc, f, true, limit, offset)
	if err != nil {
		return nil, err
	}
	if int64(len(rows)) < limit {
		off2 := int64(0)
		if len(rows) == 0 {
			inStock, err := t.CountProductsInStock(ctx, sc, f)
			if err != nil {
				return nil, err
			}
			off2 = max(offset-inStock, 0)
		}
		more, err := t.listSegment(ctx, sc, f, false, limit-int64(len(rows)), off2)
		if err != nil {
			return nil, err
		}
		rows = append(rows, more...)
	}
	return productsOf(rows), nil
}

func (t tenantTx) ListProductsInStock(ctx context.Context, sc StoreScope, f ListingFilter, limit, offset int64) ([]Product, error) {
	if err := checkPaging(limit, offset); err != nil {
		return nil, err
	}
	if f.Categories != nil && len(f.Categories) == 0 {
		return []Product{}, nil
	}
	if err := t.beforeListing(ctx, limit, offset); err != nil {
		return nil, err
	}
	rows, err := t.listSegment(ctx, sc, f, true, limit, offset)
	if err != nil {
		return nil, err
	}
	return productsOf(rows), nil
}

func (t tenantTx) CountProductsInStock(ctx context.Context, sc StoreScope, f ListingFilter) (int64, error) {
	if f.Categories == nil {
		return t.q.CountProductsInStock(ctx, db.CountProductsInStockParams{
			StoreID: sc.StoreID, RegionID: sc.RegionID,
		})
	}
	if len(f.Categories) == 0 {
		return 0, nil
	}
	return t.q.CountProductsInStockInCategories(ctx, db.CountProductsInStockInCategoriesParams{
		CategoryIds: f.Categories, StoreID: sc.StoreID, RegionID: sc.RegionID,
	})
}

func productsOf(rows []db.ListProductsByStockRow) []Product {
	out := make([]Product, 0, len(rows))
	for _, r := range rows {
		out = append(out, Product{
			ID:            r.ID,
			Title:         r.Title,
			Subtitle:      r.Subtitle,
			MinPriceCents: r.MinPriceCents,
			MaxPriceCents: r.MaxPriceCents,
			SalesCount:    r.SalesCount,
			Status:        r.Status,

			MainImageUploadID: mainImageOf(r.MainImageUploadID),
		})
	}
	return out
}

func (t tenantTx) CountProducts(ctx context.Context, sc StoreScope, f ListingFilter) (int64, error) {
	if f.Categories == nil {
		return t.q.CountProducts(ctx, db.CountProductsParams{
			StoreID: sc.StoreID, RegionID: sc.RegionID,
		})
	}
	if len(f.Categories) == 0 {
		return 0, nil
	}
	return t.q.CountProductsInCategories(ctx, db.CountProductsInCategoriesParams{
		CategoryIds: f.Categories, StoreID: sc.StoreID, RegionID: sc.RegionID,
	})
}

func (t tenantTx) CategorySubtreeIDs(ctx context.Context, categoryID int64) ([]int64, error) {
	ids, err := t.q.CategorySubtreeIDs(ctx, categoryID)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []int64{}
	}
	return ids, nil
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
	ID         int64
	SKUCode    string
	SpecValues []byte
	PriceCents int64
	ImageURL   *string
	// AvailableQty 这家门店的可售量。**ListProductSKUs 不填它**（微服务拆分阶段 1a）：
	// 库存归库存服务，service/product.go 批量问一次再填；这一层返回时恒为 0。
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
			ID:         r.ID,
			SKUCode:    r.SkuCode,
			SpecValues: r.SpecValues,
			PriceCents: r.PriceCents,
			ImageURL:   r.ImageUrl,
		})
	}
	return out, nil
}
