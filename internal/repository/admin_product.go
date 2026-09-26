package repository

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// AdminProductTx 是商家写路径上商品与商品图这一面。
type AdminProductTx interface {
	// AdminListProducts 返回当前租户的商品。不传筛选条件时含草稿与已下架，
	// 但**不含软删** —— 那要 Filter.IncludeDeleted 显式打开。
	AdminListProducts(ctx context.Context, f ProductFilter) ([]AdminProduct, error)

	// AdminCountProducts 用同一组筛选条件数总数，填契约里必填的 total。
	AdminCountProducts(ctx context.Context, f ProductFilter) (int64, error)

	// AdminFindProduct 取一件商品，**含草稿、已下架与已软删**。
	// 查不到返回 ErrCatalogNotFound。
	AdminFindProduct(ctx context.Context, id int64) (AdminProduct, error)

	// CreateProduct 建一件商品，落地即草稿（status = 0，published_at 为空）。
	CreateProduct(ctx context.Context, n NewProduct) (AdminProduct, error)

	// UpdateProduct 只改文案与归属。改不动时的两支分开回：
	// ErrCatalogNotFound（不存在 / 不是本租户的）与 ErrProductDeleted（已软删）。
	UpdateProduct(ctx context.Context, id int64, p ProductPatch) (AdminProduct, error)

	// SoftDeleteProduct 置 deleted_at。两支：ErrCatalogNotFound（不存在 /
	// 已经删过了）与 ErrProductStillPublished（还在架，先下架）。
	SoftDeleteProduct(ctx context.Context, id int64) error

	// SetProductPublication 上架 / 下架。publish = true 时会挡住「一个 SKU 都
	// 没有」（ErrProductHasNoSKU）。重复调用是幂等的。
	SetProductPublication(ctx context.Context, id int64, publish bool) (AdminProduct, error)

	// ReplaceProductImages **整组替换**商品图，数组顺序即展示顺序，第 0 个是主图。
	// 传空切片即清空。它在同一个事务里把这些 upload 的 referenced 置为 TRUE。
	ReplaceProductImages(ctx context.Context, productID int64, uploadIDs []int64) ([]ProductImage, error)

	// ListProductImages 按展示顺序取商品图。
	ListProductImages(ctx context.Context, productID int64) ([]ProductImage, error)
}

// ProductFilter 是后台列表的三个可选筛选条件。
//
// 三个都是「省略即不筛」，没有一个有默认值 —— 契约刻意不给 status 默认值，
// 理由写在端点上：默认值被代入会静默改变「返回哪些行」。
type ProductFilter struct {
	Status         *int16
	CategoryID     *int64
	IncludeDeleted bool
	Limit, Offset  int64
}

// NewProduct 是建商品的入参。**没有 Status，也没有 MerchantID**：
// 前者因为创建与发布是两个动作，后者因为租户由列默认值 current_merchant() 填 ——
// 这个结构体里根本没有那个字段可以传错。
type NewProduct struct {
	CategoryID  int64
	BrandID     *int64
	Title       string
	Subtitle    *string
	Description *string
}

// ProductPatch 是改商品的入参，每个字段 nil 表示「不动」。
//
// BrandID 需要一个单独的 SetBrandID 开关：契约里它是 integer | null，
// 「传 null 表示清空品牌」与「没传这个字段」是两件事，而一个 *int64 表达不了
// 三种状态。写成两个字段之后，「清空」必须被显式地写出来。
type ProductPatch struct {
	Title       *string
	Subtitle    *string
	Description *string
	CategoryID  *int64

	SetBrandID bool
	BrandID    *int64
}

// ProductAggregates 是 §3 的三个冗余字段重算之后的值。
type ProductAggregates struct {
	MinPriceCents int64
	MaxPriceCents int64
	TotalStock    int32
}

func clampPage(limit, offset int64) (int32, int32, error) {
	// 到这里还越界只可能是上游的钳制没生效。报错而不是截断：截断会把
	// 「第 1 亿页」悄悄变成某一页真实数据，一个错误的结果比一个错误更难发现。
	if limit < 0 || limit > math.MaxInt32 {
		return 0, 0, fmt.Errorf("limit %d 超出范围 [0, %d]", limit, math.MaxInt32)
	}
	if offset < 0 || offset > math.MaxInt32 {
		return 0, 0, fmt.Errorf("offset %d 超出范围 [0, %d]", offset, math.MaxInt32)
	}
	return int32(limit), int32(offset), nil
}

func (t tenantTx) AdminListProducts(ctx context.Context, f ProductFilter) ([]AdminProduct, error) {
	limit, offset, err := clampPage(f.Limit, f.Offset)
	if err != nil {
		return nil, err
	}
	rows, err := t.q.AdminListProducts(ctx, db.AdminListProductsParams{
		Status:         f.Status,
		CategoryID:     f.CategoryID,
		IncludeDeleted: f.IncludeDeleted,
		RowLimit:       limit,
		RowOffset:      offset,
	})
	if err != nil {
		return nil, err
	}
	out := make([]AdminProduct, 0, len(rows))
	for _, r := range rows {
		out = append(out, AdminProduct{
			ID: r.ID, CategoryID: r.CategoryID, BrandID: r.BrandID,
			Title: r.Title, Subtitle: r.Subtitle, Description: r.Description,
			MinPriceCents: r.MinPriceCents, MaxPriceCents: r.MaxPriceCents,
			TotalStock: r.TotalStock, SalesCount: r.SalesCount, Status: r.Status,
			PublishedAt: optTime(r.PublishedAt), DeletedAt: optTime(r.DeletedAt),
			CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
		})
	}
	return out, nil
}

func (t tenantTx) AdminCountProducts(ctx context.Context, f ProductFilter) (int64, error) {
	return t.q.AdminCountProducts(ctx, db.AdminCountProductsParams{
		Status:         f.Status,
		CategoryID:     f.CategoryID,
		IncludeDeleted: f.IncludeDeleted,
	})
}

func (t tenantTx) AdminFindProduct(ctx context.Context, id int64) (AdminProduct, error) {
	r, err := t.q.AdminGetProduct(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminProduct{}, fmt.Errorf("product %d: %w", id, ErrCatalogNotFound)
	}
	if err != nil {
		return AdminProduct{}, err
	}
	return AdminProduct{
		ID: r.ID, CategoryID: r.CategoryID, BrandID: r.BrandID,
		Title: r.Title, Subtitle: r.Subtitle, Description: r.Description,
		MinPriceCents: r.MinPriceCents, MaxPriceCents: r.MaxPriceCents,
		TotalStock: r.TotalStock, SalesCount: r.SalesCount, Status: r.Status,
		PublishedAt: optTime(r.PublishedAt), DeletedAt: optTime(r.DeletedAt),
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}, nil
}

func (t tenantTx) CreateProduct(ctx context.Context, n NewProduct) (AdminProduct, error) {
	r, err := t.q.CreateProduct(ctx, db.CreateProductParams{
		CategoryID:  n.CategoryID,
		BrandID:     n.BrandID,
		Title:       n.Title,
		Subtitle:    n.Subtitle,
		Description: n.Description,
	})
	if err != nil {
		// category_id 不属于当前租户时，复合外键
		// products_category_id_merchant_id_fkey 直接拒绝（23503）。
		// 契约把这一条定成 404 —— 与「这个类目不存在」同一个响应，
		// 不给探测器留下区分两者的口子。
		if isForeignKeyViolation(err) {
			return AdminProduct{}, fmt.Errorf("category %d: %w", n.CategoryID, ErrCatalogNotFound)
		}
		return AdminProduct{}, err
	}
	return AdminProduct{
		ID: r.ID, CategoryID: r.CategoryID, BrandID: r.BrandID,
		Title: r.Title, Subtitle: r.Subtitle, Description: r.Description,
		MinPriceCents: r.MinPriceCents, MaxPriceCents: r.MaxPriceCents,
		TotalStock: r.TotalStock, SalesCount: r.SalesCount, Status: r.Status,
		PublishedAt: optTime(r.PublishedAt), DeletedAt: optTime(r.DeletedAt),
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}, nil
}

func (t tenantTx) UpdateProduct(ctx context.Context, id int64, p ProductPatch) (AdminProduct, error) {
	r, err := t.q.UpdateProduct(ctx, db.UpdateProductParams{
		ID:          id,
		Title:       p.Title,
		Subtitle:    p.Subtitle,
		Description: p.Description,
		CategoryID:  p.CategoryID,
		SetBrandID:  p.SetBrandID,
		BrandID:     p.BrandID,
	})
	if err != nil {
		if isForeignKeyViolation(err) {
			return AdminProduct{}, fmt.Errorf("product %d 的新 category: %w", id, ErrCatalogNotFound)
		}
		return AdminProduct{}, err
	}
	// 两种 0 行，分开回。顺序要紧：先问「看得见吗」，再问「改成了吗」——
	// 反过来的话，一条不存在的商品会被报成「已软删」。
	if r.VisibleRows == 0 {
		return AdminProduct{}, fmt.Errorf("product %d: %w", id, ErrCatalogNotFound)
	}
	if r.UpdatedRows == 0 {
		return AdminProduct{}, fmt.Errorf("product %d: %w", id, ErrProductDeleted)
	}
	if r.ID == nil {
		// 改成功了却没回传行，只可能是那条 SQL 被改坏了。不要静默返回零值：
		// 一个全零的 AdminProduct 会被服务层当成一件真实商品序列化出去。
		return AdminProduct{}, fmt.Errorf("product %d 改成功但没有回传行——UpdateProduct 的 SQL 被改坏了", id)
	}
	return AdminProduct{
		ID: *r.ID, CategoryID: *r.CategoryID, BrandID: r.BrandID,
		Title: *r.Title, Subtitle: r.Subtitle, Description: r.Description,
		MinPriceCents: *r.MinPriceCents, MaxPriceCents: *r.MaxPriceCents,
		TotalStock: *r.TotalStock, SalesCount: *r.SalesCount, Status: *r.Status,
		PublishedAt: optTime(r.PublishedAt), DeletedAt: optTime(r.DeletedAt),
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}, nil
}

func (t tenantTx) SoftDeleteProduct(ctx context.Context, id int64) error {
	r, err := t.q.SoftDeleteProduct(ctx, id)
	if err != nil {
		return err
	}
	if r.VisibleRows == 0 {
		return fmt.Errorf("product %d: %w", id, ErrCatalogNotFound)
	}
	if r.DeletedRows == 0 {
		// 看得见但没删成，只有两个原因，而契约给了两个不同的响应码。
		// 已经删过了 → 404（契约：「商品不存在、不属于当前租户，或已被软删」）。
		if r.CurrentDeletedAt.Valid {
			return fmt.Errorf("product %d 已经软删过: %w", id, ErrCatalogNotFound)
		}
		// 剩下的只可能是仍在架 → 409。断言一下而不是直接返回：
		// 哪天 SQL 的 WHERE 多长出一个条件，这里会说出真话而不是撒一个谎。
		if r.CurrentStatus != nil && *r.CurrentStatus == 1 {
			return fmt.Errorf("product %d: %w", id, ErrProductStillPublished)
		}
		return fmt.Errorf("product %d 可见、未软删、不在架，却没删成——SoftDeleteProduct 的 SQL 被改坏了", id)
	}
	return nil
}

func (t tenantTx) SetProductPublication(ctx context.Context, id int64, publish bool) (AdminProduct, error) {
	if publish {
		r, err := t.q.PublishProduct(ctx, id)
		if err != nil {
			return AdminProduct{}, err
		}
		if r.VisibleRows == 0 {
			return AdminProduct{}, fmt.Errorf("product %d: %w", id, ErrCatalogNotFound)
		}
		if r.SkuRows == 0 {
			return AdminProduct{}, fmt.Errorf("product %d: %w", id, ErrProductHasNoSKU)
		}
		if r.UpdatedRows == 0 {
			return AdminProduct{}, fmt.Errorf("product %d 可见、有 SKU，却没上架成——PublishProduct 的 SQL 被改坏了", id)
		}
	} else {
		r, err := t.q.UnpublishProduct(ctx, id)
		if err != nil {
			return AdminProduct{}, err
		}
		if r.VisibleRows == 0 {
			return AdminProduct{}, fmt.Errorf("product %d: %w", id, ErrCatalogNotFound)
		}
		if r.UpdatedRows == 0 {
			return AdminProduct{}, fmt.Errorf("product %d 可见却没下架成——UnpublishProduct 的 SQL 被改坏了", id)
		}
	}
	// 回读整行：契约的 200 响应是完整的 AdminProduct，而上面那两条语句
	// 只回传了判定所需的几个数。同一个事务里的回读，读到的就是刚写的值。
	return t.AdminFindProduct(ctx, id)
}

func (t tenantTx) ReplaceProductImages(ctx context.Context, productID int64, uploadIDs []int64) ([]ProductImage, error) {
	// ① 商品必须存在且未软删。契约：这三种情形一律 404。
	//
	// 这一步不能省。省掉的话，「给别家店的商品挂图」会被复合外键
	// product_images_product_id_merchant_id_fkey 拦下来（那是对的），
	// 但错误是一条 23503，服务层分不清它指的是 product 还是 upload。
	p, err := t.AdminFindProduct(ctx, productID)
	if err != nil {
		return nil, err
	}
	if p.DeletedAt != nil {
		return nil, fmt.Errorf("product %d 已软删: %w", productID, ErrCatalogNotFound)
	}

	// ② 输入自身的合法性：同一个 upload_id 不许出现两次。
	//
	// 在动任何一行之前查。UNIQUE (merchant_id, product_id, upload_id) 也会拒绝，
	// 但那时 ClearProductImages 已经执行过了 —— 事务会回滚，数据是对的，
	// 可「先毁掉再发现输入不合法」这个形状不该留在代码里。
	seen := make(map[int64]struct{}, len(uploadIDs))
	for _, uid := range uploadIDs {
		if _, dup := seen[uid]; dup {
			return nil, fmt.Errorf("upload %d: %w", uid, ErrProductImageDuplicated)
		}
		seen[uid] = struct{}{}
	}

	// ③ 每个 upload 都要在本租户视野内、且用途确实是商品图。
	//
	// 两道闸门挡的不是同一件事，缺一不可：
	//   · 跨租户由 RLS（这里查不到）与复合外键 (upload_id, merchant_id)
	//     （写不进去）两层挡，数据库自己就能兜住；
	//   · 「是自己的文件，但用途不对」**数据库表达不了**，只有这里挡得住。
	for _, uid := range uploadIDs {
		u, err := t.FindUpload(ctx, uid)
		if err != nil {
			return nil, err
		}
		if u.Purpose != UploadPurposeProductImage {
			return nil, fmt.Errorf("upload %d 的 purpose 是 %d: %w",
				uid, u.Purpose, ErrUploadWrongPurpose)
		}
	}

	// ④ 整组替换：先清空再按下标重建。
	//
	// 顺序只有这一个写入点，所以 PUT 天然幂等：重发同一个请求不会多出一张图，
	// 也不会让顺序漂移。增量接口做不到这一点 —— 「把第 3 张挪到第 1 位」与
	// 「删掉第 2 张」两个并发请求会互相踩出一个有空洞的顺序。
	if _, err := t.q.ClearProductImages(ctx, productID); err != nil {
		return nil, err
	}
	for i, uid := range uploadIDs {
		if _, err := t.q.InsertProductImage(ctx, db.InsertProductImageParams{
			ProductID: productID,
			UploadID:  uid,
			// sort_order **就是数组下标**：0 即主图。这一行是「顺序」这个概念
			// 在库里的全部落点，没有第二处 —— 也就没有第二处会与它不一致。
			SortOrder: int32(i),
		}); err != nil {
			return nil, err
		}
		// ⑤ referenced 必须与引用它的业务对象在**同一个事务**里置位
		//（数据模型 §13 明写）。否则存在这样的窗口：图刚提交、清理任务恰好扫到、
		// 文件被删，而商品详情页上那张图已经是 404。
		//
		// 这句在事务内，而整个 ReplaceProductImages 跑在 WithTenant 的事务里，
		// 所以「同一个事务」这件事由结构保证，不靠调用方记得。
		if _, err := t.q.MarkUploadReferenced(ctx, uid); err != nil {
			return nil, err
		}
	}
	return t.ListProductImages(ctx, productID)
}

func (t tenantTx) ListProductImages(ctx context.Context, productID int64) ([]ProductImage, error) {
	rows, err := t.q.ListProductImages(ctx, productID)
	if err != nil {
		return nil, err
	}
	out := make([]ProductImage, 0, len(rows))
	for _, r := range rows {
		out = append(out, ProductImage{
			ID: r.ID, ProductID: r.ProductID, UploadID: r.UploadID, SortOrder: r.SortOrder,
		})
	}
	return out, nil
}

// recalcProductAggregates 重算 §3 的三个冗余字段。
//
// 它**不在 Tx 接口上**：调用方没有理由单独调它。SKU 的增 / 改 / 删各自在自己
// 那个方法的末尾调它，于是「改了 SKU 却忘了同步冗余字段」这件事在这一层根本
// 写不出来。冗余字段不同步的症状是前台列表上的价格区间与详情页对不上，
// 而那看起来像缓存问题。
func (t tenantTx) recalcProductAggregates(ctx context.Context, productID int64) (ProductAggregates, error) {
	r, err := t.q.RecalcProductAggregates(ctx, productID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProductAggregates{}, fmt.Errorf("product %d: %w", productID, ErrCatalogNotFound)
	}
	if err != nil {
		return ProductAggregates{}, err
	}
	return ProductAggregates{
		MinPriceCents: r.MinPriceCents,
		MaxPriceCents: r.MaxPriceCents,
		TotalStock:    r.TotalStock,
	}, nil
}
