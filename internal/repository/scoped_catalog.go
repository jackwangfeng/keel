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

	ListStoreInventories(ctx context.Context, storeID int64, lowStockOnly bool, limit, offset int32) ([]StoreInventory, int64, error)
	SetStoreInventory(ctx context.Context, storeID, skuID int64, in InventorySet) (StoreInventory, error)
	// AdjustStoreInventory 是按门店的相对调整（available_qty += delta，结果不得为负），
	// 并在同一个事务里写一行 biz_type = 5 的流水。三条出路见实现上的注释。
	AdjustStoreInventory(ctx context.Context, storeID, skuID int64, in InventoryAdjust) (StoreInventory, error)

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
}

// InventoryAdjust 是一次相对调整的全部输入。
//
// BizID 由 service 拼好递下来（「adj:<staff_id>:<Idempotency-Key>」），不在这一层现编：
// 这一层不认识员工，也不认识幂等键，而流水的 biz_id 要能顺着它找回那一次请求。
type InventoryAdjust struct {
	Delta  int32
	Reason *string
	BizID  string
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

func (t tenantTx) ListStoreInventories(ctx context.Context, storeID int64, lowStockOnly bool,
	limit, offset int32) ([]StoreInventory, int64, error) {
	rows, err := t.q.AdminListStoreInventories(ctx, db.AdminListStoreInventoriesParams{
		StoreID: storeID, LowStockOnly: lowStockOnly, PageLimit: limit, PageOffset: offset,
	})
	if err != nil {
		return nil, 0, err
	}
	total, err := t.q.AdminCountStoreInventories(ctx, db.AdminCountStoreInventoriesParams{
		StoreID: storeID, LowStockOnly: lowStockOnly,
	})
	if err != nil {
		return nil, 0, err
	}
	out := make([]StoreInventory, 0, len(rows))
	for _, r := range rows {
		out = append(out, StoreInventory{
			SKUID: r.SkuID, StoreID: storeID, SKUCode: r.SkuCode,
			AvailableQty: r.AvailableQty, WarningQty: r.WarningQty,
			UpdatedAt: r.UpdatedAt.Time,
		})
	}
	return out, total, nil
}

// SetStoreInventory 是按门店的比较并设置。三条出路：
//
//	err == nil                                成功
//	errors.Is(err, ErrCatalogNotFound)        门店 / SKU 不可见，或这家店不卖它
//	*InventoryConflict（errors.As）            CAS 对不上，带当前真实值
//
// 三种 404 合成一个 sentinel，因为契约把它们合成了一个响应：两个 id 都在
// 路径里，「这个 URI 下没有这个资源」是 404 的本义。而 409 必须分得开 ——
// 把「不是你的 SKU」报成 409 会让调用方以为重读一次再试就能成功，
// 而那个循环永远不会结束。
func (t tenantTx) SetStoreInventory(ctx context.Context, storeID, skuID int64,
	in InventorySet) (StoreInventory, error) {
	row, err := t.q.SetStoreInventoryByCAS(ctx, db.SetStoreInventoryByCASParams{
		StoreID: storeID, SkuID: skuID,
		AvailableQty:         in.AvailableQty,
		ExpectedAvailableQty: in.ExpectedAvailableQty,
		WarningQty:           in.WarningQty,
	})
	if err != nil {
		return StoreInventory{}, err
	}
	if row.SellableRows == 0 {
		return StoreInventory{}, fmt.Errorf(
			"sku %d 在门店 %d 不可见或已下架: %w", skuID, storeID, ErrCatalogNotFound)
	}
	if row.WrittenRows == 0 {
		// CAS 对不上。当前值一起回传 —— 契约把它定成必填（InventoryConflict），
		// 拿不到的话后台只能自己再查一次，而那一跳正是这个字段要省掉的。
		//
		// current_rows = 0 时（这家店根本没这一行，而 expected 不是 0）
		// 当前值就是「可售 0」：缺行 ≡ 可售 0，这里把那条语义兑现成一个
		// 调用方能直接照着重试的数。
		cur := StoreInventory{SKUID: skuID, StoreID: storeID}
		if row.CurrentAvailableQty != nil {
			cur.AvailableQty = *row.CurrentAvailableQty
		}
		if row.CurrentWarningQty != nil {
			cur.WarningQty = *row.CurrentWarningQty
		}
		if row.CurrentUpdatedAt.Valid {
			cur.UpdatedAt = row.CurrentUpdatedAt.Time
		}
		return StoreInventory{}, &StoreInventoryConflict{Current: cur}
	}
	out := StoreInventory{SKUID: skuID, StoreID: storeID}
	if row.NewAvailableQty != nil {
		out.AvailableQty = *row.NewAvailableQty
	}
	if row.NewWarningQty != nil {
		out.WarningQty = *row.NewWarningQty
	}
	if row.NewUpdatedAt.Valid {
		out.UpdatedAt = row.NewUpdatedAt.Time
	}
	return out, nil
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

// AdjustStoreInventory 是按门店的相对调整。三条出路：
//
//	err == nil                                 成功（流水已在同一个事务里写好）
//	errors.Is(err, ErrCatalogNotFound)         门店 / SKU 不可见，或这家店不卖它
//	*InventoryInsufficient（errors.As）         扣完会变负，带当前水位
//
// 404 与 409 的分辨发生在 SQL 里（AdjustStoreInventory 的 sellable / wrote 两个 CTE，
// 同一个 MVCC 快照），这一层只把两个计数翻成两种错误 —— 与 SetStoreInventory 同构。
//
// **流水排在写库存之后、同一个事务里**，这一层不开事务：调用方（service）已经在
// WithTenant 里，幂等存档也在那同一个事务里。流水写失败，整个事务回滚，
// 库存那一笔随之消失 —— 不会出现「库存加了、流水没有」的对不平。
func (t tenantTx) AdjustStoreInventory(ctx context.Context, storeID, skuID int64,
	in InventoryAdjust) (StoreInventory, error) {
	if storeID <= 0 {
		// 漏传 store_id 的症状是「每一个 SKU 都 404」，一个看起来像鉴权问题的 bug
		// （SetInventory 上那段注释的同一条）。
		return StoreInventory{}, fmt.Errorf("sku %d 的相对调整没有门店上下文", skuID)
	}
	if in.Delta == 0 {
		// service 已经按契约拒过（422）。这里再挡一道是因为 0 会在流水里留一行
		// before = after 的噪声，而这一层看不见调用方是谁。
		return StoreInventory{}, fmt.Errorf("sku %d 的相对调整 delta 为 0", skuID)
	}
	if in.BizID == "" {
		return StoreInventory{}, fmt.Errorf("sku %d 的相对调整没有 biz_id —— 流水会追不回那一次请求", skuID)
	}
	row, err := t.q.AdjustStoreInventory(ctx, db.AdjustStoreInventoryParams{
		StoreID: storeID, SkuID: skuID, Delta: in.Delta,
	})
	if err != nil {
		return StoreInventory{}, err
	}
	if row.SellableRows == 0 {
		return StoreInventory{}, fmt.Errorf(
			"sku %d 在门店 %d 不可见或已下架: %w", skuID, storeID, ErrCatalogNotFound)
	}
	if row.WrittenRows == 0 {
		// 扣完会变负。缺行（current 全为 NULL）时当前值就是「可售 0」——
		// 缺行 ≡ 可售 0，这里把那条语义兑现成一个调用方能直接看懂的数。
		cur := StoreInventory{SKUID: skuID, StoreID: storeID}
		if row.CurrentAvailableQty != nil {
			cur.AvailableQty = *row.CurrentAvailableQty
		}
		if row.CurrentWarningQty != nil {
			cur.WarningQty = *row.CurrentWarningQty
		}
		if row.CurrentUpdatedAt.Valid {
			cur.UpdatedAt = row.CurrentUpdatedAt.Time
		}
		return StoreInventory{}, &InventoryInsufficient{Delta: in.Delta, Current: cur}
	}
	if row.NewAvailableQty == nil || row.NewWarningQty == nil {
		// 写成功却没回传水位，只可能是那条 SQL 被改坏了。不要静默返回 0：
		// 0 是一个合法的库存水位，而且它会被拿去算流水的 before。
		return StoreInventory{}, fmt.Errorf(
			"sku %d 调整成功但没有回传水位——AdjustStoreInventory 的 SQL 被改坏了", skuID)
	}
	after := *row.NewAvailableQty
	// before = after - delta 是精确的：那条语句写的就是 「+ delta」，
	// 插入那一支（缺行）也是 0 + delta。不再读一次 —— 多读一次就是多一个快照。
	if err := t.q.AppendManualInventoryLog(ctx, db.AppendManualInventoryLogParams{
		SkuID: skuID, StoreID: storeID, ChangeQty: in.Delta, BizID: in.BizID,
		BeforeAvailable: after - in.Delta, AfterAvailable: after, Reason: in.Reason,
	}); err != nil {
		return StoreInventory{}, err
	}
	out := StoreInventory{SKUID: skuID, StoreID: storeID,
		AvailableQty: after, WarningQty: *row.NewWarningQty}
	if row.NewUpdatedAt.Valid {
		out.UpdatedAt = row.NewUpdatedAt.Time
	}
	return out, nil
}

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
