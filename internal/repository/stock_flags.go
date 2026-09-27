package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// StockFlagTx 是 product_store_stock（00087）那一面：门店 × 商品有没有货的冗余标记，
// 只给商品列表排序用。谁在什么时候写它见 service/stock_flags.go。
type StockFlagTx interface {
	// UpsertProductStoreStock 整批写一家店的一批商品；值没变的行不写。
	UpsertProductStoreStock(ctx context.Context, storeID int64, productIDs []int64, inStock []bool) error
	// AllOnSaleSKUs 返回全部在架商品各自的在售 SKU（键是 product_id）。
	AllOnSaleSKUs(ctx context.Context) (map[int64][]int64, error)
	// StoreIDsForStockFlags 是要刷新的门店（未软删的全部）。
	StoreIDsForStockFlags(ctx context.Context) ([]int64, error)
	// ProductOfSKU 查 SKU 属于哪件商品。SKU 不存在返回 ErrCatalogNotFound。
	ProductOfSKU(ctx context.Context, skuID int64) (int64, error)
}

func (t tenantTx) UpsertProductStoreStock(ctx context.Context, storeID int64, productIDs []int64, inStock []bool) error {
	if len(productIDs) != len(inStock) {
		return fmt.Errorf("UpsertProductStoreStock: %d 个商品配了 %d 个标记", len(productIDs), len(inStock))
	}
	if len(productIDs) == 0 {
		return nil
	}
	return t.q.UpsertProductStoreStock(ctx, db.UpsertProductStoreStockParams{
		StoreID: storeID, ProductIds: productIDs, InStocks: inStock,
	})
}

func (t tenantTx) AllOnSaleSKUs(ctx context.Context) (map[int64][]int64, error) {
	rows, err := t.q.ListOnSaleSKUsForStockFlags(ctx)
	if err != nil {
		return nil, err
	}
	out := map[int64][]int64{}
	for _, r := range rows {
		out[r.ProductID] = append(out[r.ProductID], r.ID)
	}
	return out, nil
}

func (t tenantTx) StoreIDsForStockFlags(ctx context.Context) ([]int64, error) {
	return t.q.ListStoreIDsForStockFlags(ctx)
}

func (t tenantTx) ProductOfSKU(ctx context.Context, skuID int64) (int64, error) {
	id, err := t.q.ProductOfSKU(ctx, skuID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("sku %d: %w", skuID, ErrCatalogNotFound)
	}
	return id, err
}
