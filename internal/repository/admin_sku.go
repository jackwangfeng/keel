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
	// AdminListProductSKUs 取一件商品的全部**未软删** SKU（含停售）。
	//
	// **AvailableQty / WarningQty 是零值**（微服务拆分阶段 1a）：库存归库存服务，
	// service 拿这批 id 向它要跨门店合计再填上。这一层不读库存表。
	AdminListProductSKUs(ctx context.Context, productID int64) ([]AdminSKU, error)

	// AdminFindSKU 取一个未软删的 SKU。查不到返回 ErrCatalogNotFound。
	// 库存两列同上，是零值。
	AdminFindSKU(ctx context.Context, id int64) (AdminSKU, error)

	// CreateSKU 建一个 SKU。**库存行不在这里建**（阶段 1a 起归库存服务）：
	// 返回值里的 AvailableQty / WarningQty 是入参原样回显，service 在事务提交之后
	// 调库存服务建行（门店取 DefaultStoreForNewSKU）。货号撞车返回 ErrSKUCodeDuplicated。
	CreateSKU(ctx context.Context, n NewSKU) (AdminSKU, error)

	// DefaultStoreForNewSKU 返回新 SKU 的第一行库存该建在哪家店（默认门店，未软删）。
	// 没有默认门店时 ok 为 false —— 那不是失败：缺行 ≡ 可售 0。
	DefaultStoreForNewSKU(ctx context.Context) (storeID int64, ok bool, err error)

	// UpdateSKU 改 SKU（含改价）。**没有 AvailableQty** —— 库存走 SetInventory。
	UpdateSKU(ctx context.Context, id int64, p SKUPatch) (AdminSKU, error)

	// SoftDeleteSKU 置 deleted_at，并返回它所属的 product_id。
	// 两支：ErrCatalogNotFound 与 ErrSKULastOfPublishedProduct。
	SoftDeleteSKU(ctx context.Context, id int64) (int64, error)
}

// NewSKU}

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

	// 这里**没有**「重算商品冗余价格」那一步了（00019 把那两列删了，
	// RecalcProductAggregates 也一起删了）。价格区间与总库存现在由读路径上的
	// LEFT JOIN LATERAL 现算 —— 建完 SKU 立刻去读商品，读到的就是含这个新
	// SKU 的区间，不需要任何人记得调一个同步函数。
	return AdminSKU{
		ID: s.ID, ProductID: s.ProductID, SKUCode: s.SkuCode,
		SpecValues: s.SpecValues, PriceCents: s.PriceCents, CostCents: s.CostCents,
		WeightGram: s.WeightGram, ImageURL: s.ImageUrl, Status: s.Status,
		// 库存取入参：库存行由 service 在提交之后经库存服务建（阶段 1a），
		// 这里没有行可回。回显入参与拆分前同一个口径（契约的 AdminSku 回的是「建成的样子」）。
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

func (t tenantTx) DefaultStoreForNewSKU(ctx context.Context) (int64, bool, error) {
	ids, err := t.q.DefaultStoreForNewSKU(ctx)
	if err != nil {
		return 0, false, err
	}
	if len(ids) == 0 {
		return 0, false, nil
	}
	// uk_stores_default 保证至多一行。
	return ids[0], true, nil
}
