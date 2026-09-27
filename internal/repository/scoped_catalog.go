package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 门店 / 大区两个作用域下的可见性、三层定价与按门店的库存
// （契约 Store + Catalog 两个 tag 交叉的那 8 条）。数据模型 §4。

// ErrCatalogBadReference：请求体里指名的某个 id 不存在或不属于当前租户
// （契约 422）。
//
// 与 ErrCatalogNotFound 分开，判据是数据模型 §4 那条分界线：
// **路径里指名的资源不存在 → 404；请求体里指名的东西不存在或不可用 → 422。**
// 合成一个的话，POST /admin/stores 里一个错的 region_id 会让整条端点回 404，
// 而客户端会以为 /admin/stores 这个 URL 不存在。
var ErrCatalogBadReference = errors.New("请求体里引用的对象不存在或不属于当前租户")

// pgMessage 取 PostgreSQL 自己那句话，用来填 Problem 的 detail。
//
// 只在围栏那一条路径上用，而且只在**校验**失败时用：那句话
// （Self-intersection at or near point 0.5 0.5）是运营唯一能拿来定位自己画错
// 在哪儿的东西。别处不要用它 —— 把数据库的错误消息转述给调用方，
// 一般是在泄露 schema。
func pgMessage(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Message
	}
	return err.Error()
}

// ScopedListing 是一件商品在某个作用域（门店或大区）下的可见性与生效价。
type ScopedListing struct {
	ProductID       int64
	Title           string
	Status          int16
	Listed          bool
	EffectiveListed bool
	MinPriceCents   int64
	MaxPriceCents   int64
	PriceSource     int32
}

// ScopedPrice 是一个 SKU 在某个作用域下的价格覆盖与生效价。
//
// OverridePriceCents 为 nil 表示**这一层没有覆盖**，沿用上一层 —— 那正是两张
// 覆盖表「缺一行即继承」的语义。它与 EffectivePriceCents 刻意分开：
// 后台要同时显示「这一层填了什么」与「最终是多少」。
type ScopedPrice struct {
	SKUID               int64
	SKUCode             string
	BasePriceCents      int64
	OverridePriceCents  *int64
	EffectivePriceCents int64
	PriceSource         int32
}

// StoreInventory 是「这家店这个 SKU 的水位」。
type StoreInventory struct {
	SKUID        int64
	StoreID      int64
	SKUCode      string
	AvailableQty int32
	WarningQty   int32
	UpdatedAt    time.Time
}

// StoreInventorySKU 是门店库存清单里 SKU 那一半：水位由库存服务补。
// UpdatedAt 是 SKU 自己的更新时间 —— 这家店缺行时清单回落到它（拆分前的 COALESCE）。
type StoreInventorySKU struct {
	SKUID     int64
	SKUCode   string
	UpdatedAt time.Time
}

// ScopedCatalogTx 是这一块的接口面。
type ScopedCatalogTx interface {
	ListStoreProducts(ctx context.Context, storeID, regionID int64, listed *bool, limit, offset int32) ([]ScopedListing, int64, error)
	ListRegionProducts(ctx context.Context, regionID int64, listed *bool, limit, offset int32) ([]ScopedListing, int64, error)
	SetStoreProductListing(ctx context.Context, storeID, regionID, productID int64, listed bool, staffID *int64) (ScopedListing, error)
	SetRegionProductListing(ctx context.Context, regionID, productID int64, listed bool, staffID *int64) (ScopedListing, error)

	SetStorePrice(ctx context.Context, storeID, skuID, priceCents int64) (ScopedPrice, error)
	ClearStorePrice(ctx context.Context, storeID, skuID int64) error
	SetRegionPrice(ctx context.Context, regionID, skuID, priceCents int64) (ScopedPrice, error)
	ClearRegionPrice(ctx context.Context, regionID, skuID int64) error

	// 门店库存的 core 那一半（微服务拆分阶段 1a）。水位的读写归库存服务
	// （inventory.Service），这里只剩要 JOIN skus / stores / 覆盖表才答得出的两件事。
	//
	// ListStoreInventorySKUs 是门店库存清单的 SKU 一页（未软删、按 id 升序），
	// excludeSKUIDs 里的不算（low_stock_only 时是这家店水位高于预警线的那批）。
	ListStoreInventorySKUs(ctx context.Context, excludeSKUIDs []int64, limit, offset int32) ([]StoreInventorySKU, int64, error)
	// SKUSellableInStore 是改库存之前的可售判定：SKU 与门店都可见且未软删、
	// 这家店与它所在大区都没有下架这件商品。
	SKUSellableInStore(ctx context.Context, storeID, skuID int64) (bool, error)

	// SoleStore 返回本租户唯一那家未软删门店的 id。
	// 有零家或多家时返回 ErrStoreAmbiguous —— 契约把那条不带门店的库存路径
	// 的语义写死成「恰好一家才可用」，不是「猜一家」。
	SoleStore(ctx context.Context) (int64, error)
}

// InventorySet 是按门店改库存的入参。ExpectedAvailableQty 是必填的
// 「我看到的那个值」，不是可选的乐观锁开关。
type InventorySet struct {
	AvailableQty         int32
	ExpectedAvailableQty int32
	WarningQty           *int32
	// BizID 是这次覆盖在流水里的 biz_id（「set:<staff_id>:<随机串>」，service 拼好）。
	// 数量真的变了才用得上 —— 流水由库存服务写（inventory.Local.Set）。
	BizID string
}

// ---------------------------------------------------------------------------
// 可见性
// ---------------------------------------------------------------------------

func (t tenantTx) ListStoreProducts(ctx context.Context, storeID, regionID int64,
	listed *bool, limit, offset int32) ([]ScopedListing, int64, error) {
	rows, err := t.q.ScopedListStoreProducts(ctx, db.ScopedListStoreProductsParams{
		StoreID: storeID, RegionID: regionID, Listed: listed,
		PageLimit: limit, PageOffset: offset,
	})
	if err != nil {
		return nil, 0, err
	}
	total, err := t.q.ScopedCountStoreProducts(ctx, db.ScopedCountStoreProductsParams{
		StoreID: storeID, Listed: listed,
	})
	if err != nil {
		return nil, 0, err
	}
	out := make([]ScopedListing, 0, len(rows))
	for _, r := range rows {
		out = append(out, ScopedListing{
			ProductID: r.ProductID, Title: r.Title, Status: r.Status,
			Listed: r.Listed, EffectiveListed: r.EffectiveListed,
			MinPriceCents: r.MinPriceCents, MaxPriceCents: r.MaxPriceCents,
			PriceSource: r.PriceSource,
		})
	}
	return out, total, nil
}

func (t tenantTx) ListRegionProducts(ctx context.Context, regionID int64,
	listed *bool, limit, offset int32) ([]ScopedListing, int64, error) {
	rows, err := t.q.ScopedListRegionProducts(ctx, db.ScopedListRegionProductsParams{
		RegionID: regionID, Listed: listed, PageLimit: limit, PageOffset: offset,
	})
	if err != nil {
		return nil, 0, err
	}
	total, err := t.q.ScopedCountRegionProducts(ctx, db.ScopedCountRegionProductsParams{
		RegionID: regionID, Listed: listed,
	})
	if err != nil {
		return nil, 0, err
	}
	out := make([]ScopedListing, 0, len(rows))
	for _, r := range rows {
		out = append(out, ScopedListing{
			ProductID: r.ProductID, Title: r.Title, Status: r.Status,
			Listed: r.Listed, EffectiveListed: r.EffectiveListed,
			MinPriceCents: r.MinPriceCents, MaxPriceCents: r.MaxPriceCents,
			PriceSource: r.PriceSource,
		})
	}
	return out, total, nil
}

// SetStoreProductListing 幂等：listed = false 写一行排除表，true 删掉那一行。
//
// **大区排掉的，这里捞不回来。** 所以回显里的 effective_listed 会是 false
// 而 listed 是 true —— 两个字段刻意分开，否则后台会显示「已上架」而买家看不到。
func (t tenantTx) SetStoreProductListing(ctx context.Context, storeID, regionID, productID int64,
	listed bool, staffID *int64) (ScopedListing, error) {
	if listed {
		if err := t.q.RelistProductInStore(ctx, db.RelistProductInStoreParams{
			StoreID: storeID, ProductID: productID,
		}); err != nil {
			return ScopedListing{}, err
		}
	} else {
		err := t.q.DelistProductInStore(ctx, db.DelistProductInStoreParams{
			StoreID: storeID, ProductID: productID, UpdatedBy: staffID,
		})
		if isForeignKeyViolation(err) {
			// product_id 不存在、不属于本租户、或门店不属于本租户。
			// 两个 id 都在路径里，所以 404（契约）。
			return ScopedListing{}, fmt.Errorf("product %d: %w", productID, ErrCatalogNotFound)
		}
		if err != nil {
			return ScopedListing{}, err
		}
	}
	return t.storeListing(ctx, storeID, regionID, productID)
}

func (t tenantTx) storeListing(ctx context.Context, storeID, regionID, productID int64) (ScopedListing, error) {
	r, err := t.q.GetStoreProductListing(ctx, db.GetStoreProductListingParams{
		StoreID: storeID, RegionID: regionID, ProductID: productID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ScopedListing{}, fmt.Errorf("product %d: %w", productID, ErrCatalogNotFound)
	}
	if err != nil {
		return ScopedListing{}, err
	}
	return ScopedListing{
		ProductID: r.ProductID, Title: r.Title, Status: r.Status,
		Listed: r.Listed, EffectiveListed: r.EffectiveListed,
		MinPriceCents: r.MinPriceCents, MaxPriceCents: r.MaxPriceCents,
		PriceSource: r.PriceSource,
	}, nil
}

func (t tenantTx) SetRegionProductListing(ctx context.Context, regionID, productID int64,
	listed bool, staffID *int64) (ScopedListing, error) {
	if listed {
		if err := t.q.RelistProductInRegion(ctx, db.RelistProductInRegionParams{
			RegionID: regionID, ProductID: productID,
		}); err != nil {
			return ScopedListing{}, err
		}
	} else {
		err := t.q.DelistProductInRegion(ctx, db.DelistProductInRegionParams{
			RegionID: regionID, ProductID: productID, UpdatedBy: staffID,
		})
		if isForeignKeyViolation(err) {
			return ScopedListing{}, fmt.Errorf("product %d: %w", productID, ErrCatalogNotFound)
		}
		if err != nil {
			return ScopedListing{}, err
		}
	}
	r, err := t.q.GetRegionProductListing(ctx, db.GetRegionProductListingParams{
		RegionID: regionID, ProductID: productID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ScopedListing{}, fmt.Errorf("product %d: %w", productID, ErrCatalogNotFound)
	}
	if err != nil {
		return ScopedListing{}, err
	}
	return ScopedListing{
		ProductID: r.ProductID, Title: r.Title, Status: r.Status,
		Listed: r.Listed, EffectiveListed: r.EffectiveListed,
		MinPriceCents: r.MinPriceCents, MaxPriceCents: r.MaxPriceCents,
		PriceSource: r.PriceSource,
	}, nil
}

// ---------------------------------------------------------------------------
// 三层定价的写面
// ---------------------------------------------------------------------------

func (t tenantTx) SetStorePrice(ctx context.Context, storeID, skuID, priceCents int64) (ScopedPrice, error) {
	_, err := t.q.UpsertStoreSkuPrice(ctx, db.UpsertStoreSkuPriceParams{
		StoreID: storeID, SkuID: skuID, PriceCents: priceCents,
	})
	if isForeignKeyViolation(err) {
		return ScopedPrice{}, fmt.Errorf("store %d / sku %d: %w", storeID, skuID, ErrCatalogNotFound)
	}
	if err != nil {
		return ScopedPrice{}, err
	}
	return t.storePrice(ctx, storeID, skuID)
}

func (t tenantTx) ClearStorePrice(ctx context.Context, storeID, skuID int64) error {
	// **本来就没有那一行时也成功**：调用方的意图是「这家店不要自己的价」，
	// 那个意图在两种情况下都已经达成。404 留给「门店或 SKU 根本不存在」，
	// 而那一条由 service 在这之前单独确认。
	return t.q.DeleteStoreSkuPrice(ctx, db.DeleteStoreSkuPriceParams{
		StoreID: storeID, SkuID: skuID,
	})
}

func (t tenantTx) SetRegionPrice(ctx context.Context, regionID, skuID, priceCents int64) (ScopedPrice, error) {
	_, err := t.q.UpsertRegionSkuPrice(ctx, db.UpsertRegionSkuPriceParams{
		RegionID: regionID, SkuID: skuID, PriceCents: priceCents,
	})
	if isForeignKeyViolation(err) {
		return ScopedPrice{}, fmt.Errorf("region %d / sku %d: %w", regionID, skuID, ErrCatalogNotFound)
	}
	if err != nil {
		return ScopedPrice{}, err
	}
	return t.regionPrice(ctx, regionID, skuID)
}

func (t tenantTx) ClearRegionPrice(ctx context.Context, regionID, skuID int64) error {
	return t.q.DeleteRegionSkuPrice(ctx, db.DeleteRegionSkuPriceParams{
		RegionID: regionID, SkuID: skuID,
	})
}

// storePrice 拼一份 ScopedSkuPrice：生效价与来源走视图（那是唯一一处
// COALESCE(门店价, 大区价, 基准价)），覆盖值与基准价各读一次自己的表。
//
// 三次查询而不是一条大 JOIN：那条大 JOIN 会把「就近生效」的公式在 db/queries
// 里再写一遍，而那正是 scripts/check_query_tenancy.py 本轮新加的闸门要挡的。
// 代价是两次额外的主键点查，发生在一条后台写接口的回显路径上。
func (t tenantTx) storePrice(ctx context.Context, storeID, skuID int64) (ScopedPrice, error) {
	sku, err := t.AdminFindSKU(ctx, skuID)
	if err != nil {
		return ScopedPrice{}, err
	}
	eff, err := t.q.GetStoreEffectivePrice(ctx, db.GetStoreEffectivePriceParams{
		StoreID: storeID, SkuID: skuID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ScopedPrice{}, fmt.Errorf("store %d / sku %d: %w", storeID, skuID, ErrCatalogNotFound)
	}
	if err != nil {
		return ScopedPrice{}, err
	}
	out := ScopedPrice{
		SKUID: skuID, SKUCode: sku.SKUCode, BasePriceCents: sku.PriceCents,
		EffectivePriceCents: eff.PriceCents, PriceSource: eff.PriceSource,
	}
	ov, err := t.q.GetStoreSkuPriceOverride(ctx, db.GetStoreSkuPriceOverrideParams{
		StoreID: storeID, SkuID: skuID,
	})
	if err == nil {
		v := ov
		out.OverridePriceCents = &v
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ScopedPrice{}, err
	}
	return out, nil
}

func (t tenantTx) regionPrice(ctx context.Context, regionID, skuID int64) (ScopedPrice, error) {
	sku, err := t.AdminFindSKU(ctx, skuID)
	if err != nil {
		return ScopedPrice{}, err
	}
	eff, err := t.q.GetRegionEffectivePrice(ctx, db.GetRegionEffectivePriceParams{
		RegionID: regionID, SkuID: skuID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ScopedPrice{}, fmt.Errorf("region %d / sku %d: %w", regionID, skuID, ErrCatalogNotFound)
	}
	if err != nil {
		return ScopedPrice{}, err
	}
	out := ScopedPrice{
		SKUID: skuID, SKUCode: sku.SKUCode, BasePriceCents: sku.PriceCents,
		EffectivePriceCents: eff.PriceCents, PriceSource: eff.PriceSource,
	}
	ov, err := t.q.GetRegionSkuPriceOverride(ctx, db.GetRegionSkuPriceOverrideParams{
		RegionID: regionID, SkuID: skuID,
	})
	if err == nil {
		v := ov
		out.OverridePriceCents = &v
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ScopedPrice{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 按门店的库存
// ---------------------------------------------------------------------------

func (t tenantTx) ListStoreInventorySKUs(ctx context.Context, excludeSKUIDs []int64,
	limit, offset int32) ([]StoreInventorySKU, int64, error) {
	if excludeSKUIDs == nil {
		// nil 会被编码成 SQL NULL，而 NOT (x = ANY(NULL)) 是 NULL —— 每一行都被滤掉，
		// 症状是「这家店一个 SKU 都没有」。
		excludeSKUIDs = []int64{}
	}
	rows, err := t.q.AdminListStoreInventorySKUs(ctx, db.AdminListStoreInventorySKUsParams{
		ExcludeSkuIds: excludeSKUIDs, PageLimit: limit, PageOffset: offset,
	})
	if err != nil {
		return nil, 0, err
	}
	total, err := t.q.AdminCountStoreInventorySKUs(ctx, excludeSKUIDs)
	if err != nil {
		return nil, 0, err
	}
	out := make([]StoreInventorySKU, 0, len(rows))
	for _, r := range rows {
		out = append(out, StoreInventorySKU{SKUID: r.SkuID, SKUCode: r.SkuCode, UpdatedAt: r.UpdatedAt.Time})
	}
	return out, total, nil
}

func (t tenantTx) SKUSellableInStore(ctx context.Context, storeID, skuID int64) (bool, error) {
	if storeID <= 0 {
		// 漏传 store_id 的症状是「每一个 SKU 都 404」，一个看起来像鉴权问题的 bug。
		return false, fmt.Errorf("sku %d 的可售判定没有门店上下文", skuID)
	}
	return t.q.SKUSellableInStore(ctx, db.SKUSellableInStoreParams{StoreID: storeID, SkuID: skuID})
}

// StoreInventoryConflict 是按门店那条 CAS 的 409，带当前真实值。
//
// 与既有的 InventoryConflict 平行而不是复用它：那一个的 Current 里没有
// store_id，而契约把 AdminInventory.store_id 定成**必返**——
// 一个不写明属于谁的水位在多门店之后没有意义。
type StoreInventoryConflict struct{ Current StoreInventory }

func (e *StoreInventoryConflict) Error() string {
	return fmt.Sprintf("门店 %d 的 sku %d：expected_available_qty 与当前值 %d 不符",
		e.Current.StoreID, e.Current.SKUID, e.Current.AvailableQty)
}

// Unwrap 让 errors.Is(err, ErrInventoryPrecondition) 成立 —— handler 那张错误
// 映射表按 sentinel 分支，而这个类型要落在同一个 409 上。
func (e *StoreInventoryConflict) Unwrap() error { return ErrInventoryPrecondition }

// ErrInventoryInsufficient：相对调整扣完会变负（契约 409 inventory-insufficient）。
//
// 与 ErrInventoryPrecondition 分成两个 sentinel，因为客户端对它们的处置相反：
// CAS 对不上是「重读一次再试**会**成功」，这一个原样重试**不会** —— 要么改小扣减量，
// 要么先补货。共用一个 type 的话，客户端会把一次「库存不够扣」写成无限重试。
// 与下单那条的 ErrInsufficientStock 也分开：那一个是买家语境（422 / 409 各有出处），
// 响应体里没有 current。
var ErrInventoryInsufficient = errors.New("扣完之后库存会变负")

// InventoryInsufficient 是相对调整的 409，带当前水位（缺行时为 0）。
type InventoryInsufficient struct {
	Delta   int32
	Current StoreInventory
}

func (e *InventoryInsufficient) Error() string {
	return fmt.Sprintf("门店 %d 的 sku %d：当前可售 %d，调整 %d 之后会变负",
		e.Current.StoreID, e.Current.SKUID, e.Current.AvailableQty, e.Delta)
}

// Unwrap 让 errors.Is(err, ErrInventoryInsufficient) 成立。
func (e *InventoryInsufficient) Unwrap() error { return ErrInventoryInsufficient }

func (t tenantTx) SoleStore(ctx context.Context) (int64, error) {
	r, err := t.q.CountStoresForTenant(ctx)
	if err != nil {
		return 0, err
	}
	if r.StoreCount != 1 {
		return 0, fmt.Errorf("本租户有 %d 家门店: %w", r.StoreCount, ErrStoreAmbiguous)
	}
	return r.OnlyStoreID, nil
}
