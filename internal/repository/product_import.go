package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 商品批量导入的仓储那一面（db/queries/product_imports.sql，迁移 00054）。
//
// 建商品、建 SKU 不在这里：它们走 AdminCatalogTx 里已有的 CreateProduct / CreateSKU，
// 与后台逐个建商品是**同一段代码** —— 导入只是在一个事务里把它们调很多次。
// 另写一套「批量插入」会让「建一个 SKU 必须同时建库存行」这类不变量有第二份实现
// （repository.CreateSKU 的注释里写着那一条为什么不能漏）。

// ProductImportTx 是批量导入要的仓储能力。
type ProductImportTx interface {
	// ClaimProductImport 占住「这份文件在本店导入过」。ok 为 false 表示同一份文件
	// 已经导入过（调用方转去 FindProductImportBySHA 取那一次的回执）。
	ClaimProductImport(ctx context.Context, n NewProductImport) (id int64, createdAt time.Time, ok bool, err error)

	// FinishProductImport 写回执与计数，与 Claim 在同一个事务里。
	FinishProductImport(ctx context.Context, id int64, f ProductImportTotals, result []byte) error

	// FindProductImportBySHA 取这份文件在本店的导入记录；没有时返回 ErrProductImportNotFound。
	FindProductImportBySHA(ctx context.Context, sha256 string) (ProductImport, error)

	// ExistingSKUCodes 返回 codes 里在本店已经被占用的那些（含已软删的 SKU）。
	ExistingSKUCodes(ctx context.Context, codes []string) ([]string, error)
}

// ErrProductImportNotFound：这份文件在本店没有导入记录。
var ErrProductImportNotFound = errors.New("这份文件没有导入记录")

// NewProductImport 是占位那一行的入参。
type NewProductImport struct {
	FileSHA256 string
	FileName   string
	FileFormat string // xlsx / csv
	StaffID    int64
	TotalRows  int32
}

// ProductImportTotals 是回执里的三个计数。
type ProductImportTotals struct {
	CreatedProducts int32
	CreatedSKUs     int32
	FailedRows      int32
}

// ProductImport 是一条导入记录。Result 是回执的 JSON 原样字节 —— 这一层不解释它。
type ProductImport struct {
	ID         int64
	FileSHA256 string
	TotalRows  int32
	ProductImportTotals
	Result    []byte
	CreatedAt time.Time
}

func (t tenantTx) ClaimProductImport(ctx context.Context, n NewProductImport) (int64, time.Time, bool, error) {
	r, err := t.q.ClaimProductImport(ctx, db.ClaimProductImportParams{
		FileSha256: n.FileSHA256,
		FileName:   n.FileName,
		FileFormat: n.FileFormat,
		StaffID:    n.StaffID,
		TotalRows:  n.TotalRows,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, err
	}
	return r.ID, r.CreatedAt.Time, true, nil
}

func (t tenantTx) FinishProductImport(ctx context.Context, id int64, f ProductImportTotals, result []byte) error {
	if len(result) == 0 {
		// result 是 NOT NULL 的 JSONB；送空字节会以 22P02 失败，而那条错误里
		// 没有任何东西指向「回执没序列化出来」。
		return fmt.Errorf("导入记录 %d：回执是空的", id)
	}
	return t.q.FinishProductImport(ctx, db.FinishProductImportParams{
		CreatedProducts: f.CreatedProducts,
		CreatedSkus:     f.CreatedSKUs,
		FailedRows:      f.FailedRows,
		Result:          result,
		ID:              id,
	})
}

func (t tenantTx) FindProductImportBySHA(ctx context.Context, sha256 string) (ProductImport, error) {
	r, err := t.q.FindProductImportBySHA(ctx, sha256)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProductImport{}, ErrProductImportNotFound
	}
	if err != nil {
		return ProductImport{}, err
	}
	return ProductImport{
		ID: r.ID, FileSHA256: r.FileSha256, TotalRows: r.TotalRows,
		ProductImportTotals: ProductImportTotals{
			CreatedProducts: r.CreatedProducts, CreatedSKUs: r.CreatedSkus, FailedRows: r.FailedRows,
		},
		Result: r.Result, CreatedAt: r.CreatedAt.Time,
	}, nil
}

func (t tenantTx) ExistingSKUCodes(ctx context.Context, codes []string) ([]string, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	return t.q.ExistingSKUCodes(ctx, codes)
}
