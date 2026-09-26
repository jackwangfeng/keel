package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/keel/keel/internal/repository/internal/db"
)

// skuCodeIndex 是 uk_skus_code 这条**部分**唯一索引的名字（00018 起带
// WHERE deleted_at IS NULL）。
//
// 靠索引名而不是「凡是 23505 都当货号撞车」：后者会把别的唯一冲突也翻成
// ErrSKUCodeDuplicated，而那条错误会引导商家去改一个根本没问题的货号。
// 这条路子与 payment.go 里按 channelTxnConstraint 分辨 23505 是同一个。
const skuCodeIndex = "uk_skus_code"

// AdminSKUTx 是商家写路径上 SKU 与库存这一面。
type AdminSKUTx interface {
	// AdminListProductSKUs 取一件商品的全部**未软删** SKU（含停售），
	// 带上库存水位与预警线。
	AdminListProductSKUs(ctx context.Context, productID int64) ([]AdminSKU, error)

	// AdminFindSKU 取一个未软删的 SKU。查不到返回 ErrCatalogNotFound。
	AdminFindSKU(ctx context.Context, id int64) (AdminSKU, error)

	// CreateSKU 建一个 SKU，**并在同一个事务里建出它的 inventories 行**。
	// 货号撞车返回 ErrSKUCodeDuplicated。
	CreateSKU(ctx context.Context, n NewSKU) (AdminSKU, error)

	// UpdateSKU 改 SKU（含改价）。**没有 AvailableQty** —— 库存走 SetInventory。
	UpdateSKU(ctx context.Context, id int64, p SKUPatch) (AdminSKU, error)

	// SoftDeleteSKU 置 deleted_at，并返回它所属的 product_id。
	// 两支：ErrCatalogNotFound 与 ErrSKULastOfPublishedProduct。
	SoftDeleteSKU(ctx context.Context, id int64) (int64, error)

	// SetInventory 是比较并设置。三条出路，形状各不相同：
	//
	//	err == nil                                 写成功，返回写后的那一行
	//	errors.Is(err, ErrSKUNotInTenant)          这个 SKU 在本租户不可见 → 404
	//	errors.Is(err, ErrInventoryPrecondition)   CAS 对不上 → 409，
	//	                                           errors.As 取 *InventoryConflict
	//	                                           拿当前真实值
	SetInventory(ctx context.Context, storeID, skuID int64, expected, want int32, warning *int32) (Inventory, error)
}

// NewSKU 是建 SKU 的入参。AvailableQty 在这里是**允许的**，而在 SKUPatch 里
// 不允许：建行与改行是两件事 —— 建的时候没有并发对手（这一行还不存在），
// 改的时候有（下单 SAGA 正在扣它）。
type NewSKU struct {
	ProductID    int64
	SKUCode      string
	SpecValues   []byte
	PriceCents   int64
	CostCents    int64
	WeightGram   int32
	ImageURL     *string
	Status       int16
	AvailableQty int32
	WarningQty   int32
}

// SKUPatch 是改 SKU 的入参，每个字段 nil 表示「不动」。
// ImageURL 的开关同 ProductPatch.BrandID：「清空小图」与「不动」是两件事。
type SKUPatch struct {
	SKUCode    *string
	SpecValues []byte
	PriceCents *int64
	CostCents  *int64
	WeightGram *int32
	Status     *int16

	SetImageURL bool
	ImageURL    *string
}

func isUniqueViolation(err error, index string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == index
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func (t tenantTx) AdminListProductSKUs(ctx context.Context, productID int64) ([]AdminSKU, error) {
	rows, err := t.q.AdminListProductSKUs(ctx, productID)
	if err != nil {
		return nil, err
	}
	out := make([]AdminSKU, 0, len(rows))
	for _, r := range rows {
		out = append(out, AdminSKU{
			ID: r.ID, ProductID: r.ProductID, SKUCode: r.SkuCode,
			SpecValues: r.SpecValues, PriceCents: r.PriceCents, CostCents: r.CostCents,
			WeightGram: r.WeightGram, ImageURL: r.ImageUrl, Status: r.Status,
			AvailableQty: r.AvailableQty, WarningQty: r.WarningQty,
			CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
		})
	}
	return out, nil
}

func (t tenantTx) AdminFindSKU(ctx context.Context, id int64) (AdminSKU, error) {
	r, err := t.q.AdminGetSKU(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminSKU{}, fmt.Errorf("sku %d: %w", id, ErrCatalogNotFound)
	}
	if err != nil {
		return AdminSKU{}, err
	}
	return AdminSKU{
		ID: r.ID, ProductID: r.ProductID, SKUCode: r.SkuCode,
		SpecValues: r.SpecValues, PriceCents: r.PriceCents, CostCents: r.CostCents,
		WeightGram: r.WeightGram, ImageURL: r.ImageUrl, Status: r.Status,
		AvailableQty: r.AvailableQty, WarningQty: r.WarningQty,
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}, nil
}

// CreateSKU 建 SKU 与它的库存行。
//
// ===========================================================================
// 那一行 inventories 不是顺手做的
// ===========================================================================
//
// inventories.sku_id 是主键，而下单 SAGA 的正向分支是
// 「UPDATE inventories SET available_qty = available_qty - $2
//
//	WHERE sku_id = $1 AND available_qty >= $2」。
//
// 没有那一行时 rows_affected = 0，而 SAGA 把 0 判成**库存不足**（数据模型 §4）。
// 于是一个漏建库存行的 SKU 表现为「这件商品永远缺货」——而排查方向
// （去查库存水位、查扣减逻辑、查 SAGA 补偿）从第一步就是错的，
// 因为那条记录压根不存在，所有「查水位」的动作都会返回空。
//
// 所以两条 INSERT 收在这一个方法里，调用方**没有机会**只调其中一条：
// CreateInventoryRow 不在 Tx 接口上。
//
// 为什么不压进一条 SQL（WITH ins AS (INSERT INTO skus ... RETURNING)
// INSERT INTO inventories SELECT ... FROM ins）：RLS 会拒绝。inventories 是
// parent-scoped 表，它的 WITH CHECK 是对 skus 的 EXISTS 子查询，而同一条语句里
// 数据修改 CTE 的结果对语句的其余部分不可见 —— 那个 EXISTS 看不到刚插出来的
// SKU，每一次建 SKU 都会以 42501 失败。本轮实测确认过。
func (t tenantTx) CreateSKU(ctx context.Context, n NewSKU) (AdminSKU, error) {
	if n.AvailableQty < 0 || n.WarningQty < 0 {
		// chk_qty_nonneg 也会拦住负的 available_qty，但它给出的是 23514，
		// 而 warning_qty 没有 CHECK 兜着。挡在这里，两者的失败形状才一样。
		return AdminSKU{}, fmt.Errorf("初始库存 %d / 预警线 %d 不能为负",
			n.AvailableQty, n.WarningQty)
	}
	specValues := n.SpecValues
	if len(specValues) == 0 {
		// spec_values 是 NOT NULL DEFAULT '{}' 的 JSONB，而这条 INSERT 显式给它
		// 赋值，于是默认值用不上。送一个空字节串进去会以 22P02 失败，
		// 而那条错误里没有任何东西指向「规格没填」。
		specValues = []byte("{}")
	}

	s, err := t.q.CreateSKU(ctx, db.CreateSKUParams{
		ProductID:  n.ProductID,
		SkuCode:    n.SKUCode,
		SpecValues: specValues,
		PriceCents: n.PriceCents,
		CostCents:  n.CostCents,
		WeightGram: n.WeightGram,
		ImageUrl:   n.ImageURL,
		Status:     n.Status,
	})
	if err != nil {
		if isUniqueViolation(err, skuCodeIndex) {
			return AdminSKU{}, fmt.Errorf("sku_code %q: %w", n.SKUCode, ErrSKUCodeDuplicated)
		}
		if isForeignKeyViolation(err) {
			// product_id 不属于当前租户（或根本不存在）→ 复合外键
			// skus_product_id_merchant_id_fkey 拒绝。契约定成 404。
			return AdminSKU{}, fmt.Errorf("product %d: %w", n.ProductID, ErrCatalogNotFound)
		}
		return AdminSKU{}, err
	}

	_, err = t.q.CreateInventoryRow(ctx, db.CreateInventoryRowParams{
		SkuID:        s.ID,
		AvailableQty: n.AvailableQty,
		WarningQty:   n.WarningQty,
	})
	if err != nil {
		return AdminSKU{}, fmt.Errorf("sku %d 建出来了但库存行没建成（这件商品会表现为永远缺货）: %w", s.ID, err)
	}

	// 这里**没有**「重算商品冗余价格」那一步了（00019 把那两列删了，
	// RecalcProductAggregates 也一起删了）。价格区间与总库存现在由读路径上的
	// LEFT JOIN LATERAL 现算 —— 建完 SKU 立刻去读商品，读到的就是含这个新
	// SKU 的区间，不需要任何人记得调一个同步函数。
	return AdminSKU{
		ID: s.ID, ProductID: s.ProductID, SKUCode: s.SkuCode,
		SpecValues: s.SpecValues, PriceCents: s.PriceCents, CostCents: s.CostCents,
		WeightGram: s.WeightGram, ImageURL: s.ImageUrl, Status: s.Status,
		// 库存取入参而不是取那条 INSERT 的回显：本轮它变成了 :execrows
		// （只给默认门店建行，没有默认门店时一行都不建，见那条查询的注释），
		// 于是没有行可回。回显入参在两种情况下都是对的：建成了就是这个数，
		// 没建成的话「这个 SKU 现在可售 0」也确实是 n.AvailableQty 之外
		// 唯一诚实的答案 —— 而 rows 会告诉调用方到底是哪一种。
		AvailableQty: n.AvailableQty, WarningQty: n.WarningQty,
		CreatedAt: s.CreatedAt.Time, UpdatedAt: s.UpdatedAt.Time,
	}, nil
}

func (t tenantTx) UpdateSKU(ctx context.Context, id int64, p SKUPatch) (AdminSKU, error) {
	r, err := t.q.UpdateSKU(ctx, db.UpdateSKUParams{
		ID:          id,
		SkuCode:     p.SKUCode,
		SpecValues:  p.SpecValues,
		PriceCents:  p.PriceCents,
		CostCents:   p.CostCents,
		WeightGram:  p.WeightGram,
		Status:      p.Status,
		SetImageUrl: p.SetImageURL,
		ImageUrl:    p.ImageURL,
	})
	if err != nil {
		if isUniqueViolation(err, skuCodeIndex) {
			return AdminSKU{}, fmt.Errorf("sku %d 改货号: %w", id, ErrSKUCodeDuplicated)
		}
		return AdminSKU{}, err
	}
	if r.VisibleRows == 0 {
		return AdminSKU{}, fmt.Errorf("sku %d: %w", id, ErrCatalogNotFound)
	}
	if r.UpdatedRows == 0 || r.ID == nil {
		return AdminSKU{}, fmt.Errorf("sku %d 可见却没改成——UpdateSKU 的 SQL 被改坏了", id)
	}

	// 改价之后**不需要**重算商品的价格区间：00019 起那两列不在表上了，
	// 区间在读商品的时候从 skus 现算。契约里那句「改价后服务端重算
	// products.min_price_cents / max_price_cents」因此变成了「下一次读就是新的」——
	// 对调用方可观察的行为一模一样，少掉的是「有人忘了调一个函数」这种 bug，
	// 而它的症状（改了价但列表页还是旧价）看起来像缓存问题。

	// 回读整行。这条 UPDATE 不碰 inventories，所以它回传不了 available_qty /
	// warning_qty，而契约的 AdminSku 里这两个字段是有的。
	//
	// 多一次往返，换的是「不返回一个除了库存之外都对的结构体」——
	// 那两个字段的零值和真实的 0 长得一模一样，而 0 是一个合法的库存水位，
	// 客户端会照着它把一个有货的规格渲染成售罄。
	//
	// 同一个事务里的回读，读到的就是刚写的值。
	return t.AdminFindSKU(ctx, id)
}

func (t tenantTx) SoftDeleteSKU(ctx context.Context, id int64) (int64, error) {
	r, err := t.q.SoftDeleteSKU(ctx, id)
	if err != nil {
		return 0, err
	}
	if r.VisibleRows == 0 {
		return 0, fmt.Errorf("sku %d: %w", id, ErrCatalogNotFound)
	}
	if r.DeletedRows == 0 {
		// 看得见却没删成，在这条语句里只有一个原因：它是某个在架商品的最后
		// 一个 SKU。断言而不是直接返回 —— 哪天 WHERE 多长出一个条件，
		// 这里会说出真话而不是撒一个谎。
		if r.ProductStatus != nil && *r.ProductStatus == 1 && r.SiblingRows <= 1 {
			return 0, fmt.Errorf("sku %d: %w", id, ErrSKULastOfPublishedProduct)
		}
		return 0, fmt.Errorf("sku %d 可见却没删成——SoftDeleteSKU 的 SQL 被改坏了", id)
	}
	if r.ProductID == nil {
		return 0, fmt.Errorf("sku %d 删成功但没有回传 product_id——SoftDeleteSKU 的 SQL 被改坏了", id)
	}
	// 软删掉一个规格会改变价格区间与总库存，而那两件事现在**不需要在这里做**：
	// 00019 之后它们是读的时候现算的，而现算的取值范围正是
	// 「deleted_at IS NULL 的 SKU」—— 这一行刚被排除出去。
	return *r.ProductID, nil
}

// SetInventory 比较并设置。
//
// ===========================================================================
// rows_affected = 0 的两个来源，以及为什么必须分开
// ===========================================================================
//
//	① CAS 不匹配 —— 库存在你读到它之后被改过（多半是并发下单扣减）。
//	   契约定成 409，并把**当前真实值**放进 Problem 的 current 里一起返回，
//	   调用方刷新那一格再试一次就能成功。
//	② 这个 SKU 不在本租户 / 不存在 / 已软删 —— 契约定成 404。
//	   重试**永远**不会成功。
//
// 契约在这条端点上明写了混掉的代价：把「不是你的 SKU」也报成 409，会让调用方
// 以为重读一次再试就能成功，而那个循环永远不会结束。这与 inventory.go 里
// ErrInsufficientStock / ErrSKUNotInTenant 那一对是同一件事的另一面。
//
// 两者的分辨发生在 **SQL 里**，不在这里：db/queries/admin_skus.sql 的
// SetInventoryByCAS 用两个 CTE 在同一个 MVCC 快照下分别回传 visible_rows 与
// updated_rows。这一层只是把两个数翻成两个 sentinel —— 如果分辨发生在这里
// （先 SELECT 再 UPDATE），那就是两次快照，中间的窗口正是这条接口要防的东西。
// **storeID 由调用方定，这一层不猜。** 00020 之后 inventories 的主键是
// (sku_id, store_id)，一条只给 sku_id 的 CAS 没有唯一的目标行。契约把这条
// 路径的语义写死成「本租户恰好一家未软删门店时它就是那一家，否则 409
// store-ambiguous」，而那一步判断在 service 里用 SoleStore 做 —— 放在这里
// 或放进 SQL，都等于在最热的写路径上默认一个猜测，而猜错的后果是把另一家店
// 的水位覆盖掉，没有任何东西会响。
func (t tenantTx) SetInventory(ctx context.Context, storeID, skuID int64, expected, want int32, warning *int32) (Inventory, error) {
	if storeID <= 0 {
		// 漏传 store_id 的症状最难查：params 里那个字段会取零值，
		// 而 store_id = 0 匹配不上任何一行，于是 visible_rows = 0，
		// 这条接口对**每一个** SKU 都回 404 —— 一个看起来像鉴权问题的 bug。
		return Inventory{}, fmt.Errorf("sku %d 的库存 CAS 没有门店上下文", skuID)
	}
	if want < 0 || expected < 0 {
		// chk_qty_nonneg 也会兜住负的新值，但它兜不住负的 expected ——
		// 而一个负的 expected 永远匹配不上，症状是「怎么改都 409」。
		return Inventory{}, fmt.Errorf("库存 %d / 期望值 %d 不能为负", want, expected)
	}
	if warning != nil && *warning < 0 {
		return Inventory{}, fmt.Errorf("预警线 %d 不能为负", *warning)
	}

	r, err := t.q.SetInventoryByCAS(ctx, db.SetInventoryByCASParams{
		SkuID:                skuID,
		StoreID:              storeID,
		AvailableQty:         want,
		WarningQty:           warning,
		ExpectedAvailableQty: expected,
	})
	if err != nil {
		return Inventory{}, err
	}

	// 顺序要紧：先问「看得见吗」。反过来的话，一个别家店的 sku_id 会被报成
	// 「CAS 对不上」，而 current 字段里是一个 NULL —— 调用方拿着它去刷新页面，
	// 刷出一个空格子，然后无限重试。
	if r.VisibleRows == 0 {
		return Inventory{}, fmt.Errorf("sku %d: %w", skuID, ErrSKUNotInTenant)
	}
	if r.UpdatedRows == 0 {
		if r.CurrentAvailableQty == nil || r.CurrentWarningQty == nil {
			return Inventory{}, fmt.Errorf(
				"sku %d 可见却没回传当前值——SetInventoryByCAS 的 SQL 被改坏了", skuID)
		}
		return Inventory{}, &InventoryConflict{
			SKUID:    skuID,
			Expected: expected,
			Current: Inventory{
				SKUID:        skuID,
				StoreID:      storeID,
				AvailableQty: *r.CurrentAvailableQty,
				WarningQty:   *r.CurrentWarningQty,
				UpdatedAt:    r.CurrentUpdatedAt.Time,
			},
		}
	}
	if r.NewAvailableQty == nil || r.NewWarningQty == nil {
		// 写成功却没回传水位，只可能是那条 SQL 被改坏了。不要静默返回 0：
		// 0 是一个合法的库存水位，它会一路写进后台页面。
		return Inventory{}, fmt.Errorf(
			"sku %d 写成功但没有回传水位——SetInventoryByCAS 的 SQL 被改坏了", skuID)
	}
	return Inventory{
		SKUID:        skuID,
		StoreID:      storeID,
		AvailableQty: *r.NewAvailableQty,
		WarningQty:   *r.NewWarningQty,
		UpdatedAt:    r.NewUpdatedAt.Time,
	}, nil
}
