package repository

import (
	"context"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 上新整理（service/catalog_issues.go）在 repository 边界上的那一面：两条只读查询，
// 见 db/queries/catalog_issues.sql。

// CatalogProduct 是体检用的一件商品。
type CatalogProduct struct {
	ID           int64
	Title        string
	Subtitle     string
	Description  string
	Status       int16
	CategoryID   int64
	CategoryName string
	ImageCount   int64
}

// CatalogSKUPrice 是一个 SKU 的价格与它所属商品的类目。
type CatalogSKUPrice struct {
	SKUID      int64
	ProductID  int64
	PriceCents int64
	CategoryID int64
}

// CatalogIssuesTx 是 catalog_issues 要的查询。
type CatalogIssuesTx interface {
	CatalogIssueProducts(ctx context.Context, statuses []int16, limit int32) ([]CatalogProduct, error)
	CatalogSKUPrices(ctx context.Context) ([]CatalogSKUPrice, error)
	CatalogLeafCategories(ctx context.Context) ([]CatalogCategory, error)
}

// CatalogCategory 是一个可以挪进去的叶子类目。
type CatalogCategory struct {
	ID       int64  `json:"id"`
	PathName string `json:"path_name"`
}

func (t tenantTx) CatalogLeafCategories(ctx context.Context) ([]CatalogCategory, error) {
	rows, err := t.q.CatalogLeafCategories(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CatalogCategory, 0, len(rows))
	for _, r := range rows {
		out = append(out, CatalogCategory{ID: r.ID, PathName: r.PathName})
	}
	return out, nil
}

func (t tenantTx) CatalogIssueProducts(ctx context.Context, statuses []int16, limit int32) ([]CatalogProduct, error) {
	rows, err := t.q.CatalogIssueProducts(ctx, db.CatalogIssueProductsParams{Statuses: statuses, RowLimit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]CatalogProduct, 0, len(rows))
	for _, r := range rows {
		p := CatalogProduct{ID: r.ID, Title: r.Title, Status: r.Status, CategoryID: r.CategoryID,
			CategoryName: r.CategoryName, ImageCount: r.ImageCount}
		if r.Subtitle != nil {
			p.Subtitle = *r.Subtitle
		}
		if r.Description != nil {
			p.Description = *r.Description
		}
		out = append(out, p)
	}
	return out, nil
}

func (t tenantTx) CatalogSKUPrices(ctx context.Context) ([]CatalogSKUPrice, error) {
	rows, err := t.q.CatalogIssueSKUs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CatalogSKUPrice, 0, len(rows))
	for _, r := range rows {
		out = append(out, CatalogSKUPrice{SKUID: r.ID, ProductID: r.ProductID, PriceCents: r.PriceCents, CategoryID: r.CategoryID})
	}
	return out, nil
}
