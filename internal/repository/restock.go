package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 补货计算（AI 经营 M9 任务 3，service/restock.go）的 core 一半。

// RestockCandidate 是一个在售 SKU 与它的人读标签。
type RestockCandidate struct {
	SKUID        int64
	ProductID    int64
	ProductTitle string
	SKUCode      string
	SpecValues   string // JSON 文本，如 {"颜色":"黑"}
	CreatedAt    time.Time
}

// RestockTx 是补货计算那一面。
type RestockTx interface {
	RestockCandidates(ctx context.Context) ([]RestockCandidate, error)
	// StoreSKUSales 是一家门店自 since 以来已付款订单的件数（键 sku_id）。
	StoreSKUSales(ctx context.Context, storeID int64, since time.Time) (map[int64]int64, error)
}

func (t tenantTx) RestockCandidates(ctx context.Context) ([]RestockCandidate, error) {
	rows, err := t.q.RestockCandidates(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RestockCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, RestockCandidate{SKUID: r.SkuID, ProductID: r.ProductID, ProductTitle: r.ProductTitle,
			SKUCode: r.SkuCode, SpecValues: r.SpecValues, CreatedAt: r.CreatedAt.Time})
	}
	return out, nil
}

func (t tenantTx) StoreSKUSales(ctx context.Context, storeID int64, since time.Time) (map[int64]int64, error) {
	rows, err := t.q.StoreSKUSales(ctx, db.StoreSKUSalesParams{StoreID: storeID,
		Since: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		return nil, err
	}
	out := make(map[int64]int64, len(rows))
	for _, r := range rows {
		out[r.SkuID] = r.Qty
	}
	return out, nil
}
