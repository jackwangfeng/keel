package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// SearchJudgmentTx 是检索相关度预判（00230）的读写。query 一律是归一化后的（search.NormQuery），
// 归一化是调用方的事：这一层不替它做，免得写与读两边各归一化一次、规则漂移。
type SearchJudgmentTx interface {
	// SearchJudgments 返回这条查询对这批商品的预判（product_id → relevance）；没判过的不在结果里。
	SearchJudgments(ctx context.Context, query string, productIDs []int64) (map[int64]float64, error)
	// ProductsNeedingJudgment 返回这批商品里还要（重新）判的：没判过、judge 不同、判过后商品又改过、或早于 freshAfter。
	ProductsNeedingJudgment(ctx context.Context, query string, productIDs []int64, judge string, freshAfter time.Time) ([]int64, error)
	// UpsertSearchJudgments 写一条查询的一批判断（按 query + product_id 覆盖）。两个切片必须等长。
	UpsertSearchJudgments(ctx context.Context, query string, productIDs []int64, relevances []float32, judge string) error
}

func (t tenantTx) SearchJudgments(ctx context.Context, query string, productIDs []int64) (map[int64]float64, error) {
	out := make(map[int64]float64, len(productIDs))
	if len(productIDs) == 0 {
		return out, nil
	}
	rows, err := t.q.GetSearchJudgments(ctx, db.GetSearchJudgmentsParams{Query: query, ProductIds: productIDs})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ProductID] = r.Relevance
	}
	return out, nil
}

func (t tenantTx) ProductsNeedingJudgment(ctx context.Context, query string, productIDs []int64, judge string,
	freshAfter time.Time) ([]int64, error) {
	if len(productIDs) == 0 {
		return nil, nil
	}
	return t.q.ListProductsNeedingJudgment(ctx, db.ListProductsNeedingJudgmentParams{
		Query: query, ProductIds: productIDs, Judge: judge,
		FreshAfter: pgtype.Timestamptz{Time: freshAfter, Valid: true},
	})
}

func (t tenantTx) UpsertSearchJudgments(ctx context.Context, query string, productIDs []int64, relevances []float32,
	judge string) error {
	if len(productIDs) != len(relevances) {
		return fmt.Errorf("预判写入：%d 个商品配 %d 个相关度，按位置配对会张冠李戴", len(productIDs), len(relevances))
	}
	if len(productIDs) == 0 {
		return nil
	}
	return t.q.UpsertSearchJudgments(ctx, db.UpsertSearchJudgmentsParams{
		Query: query, ProductIds: productIDs, Relevances: relevances, Judge: judge,
	})
}

// PurgeSearchJudgments 删 ctx 那家店 judged_at 早于 before 的预判，至多 batch 行（保留期任务用）。
func (r *Repo) PurgeSearchJudgments(ctx context.Context, before time.Time, batch int32) (int64, error) {
	return r.purgeInTenant(ctx, func(q *db.Queries) (int64, error) {
		return q.PurgeSearchJudgmentsBefore(ctx, db.PurgeSearchJudgmentsBeforeParams{Before: ts(before), Batch: batch})
	})
}
