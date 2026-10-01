package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// SearchEvalTx 是离线评测集（cmd/keel-searcheval，语义检索层 §9.1）的取数。
//
// 只给离线工具用：候选不按门店可见性与价格过滤，问的只是「这件商品与这个查询相不相关」。
// 理由写在 db/queries/search_eval.sql 的文件头。
type SearchEvalTx interface {
	// SearchEvalQueries 返回本店 since 之后搜过的查询与次数，次数多的在前，至多 limit 条。
	SearchEvalQueries(ctx context.Context, since time.Time, limit int32) ([]SearchEvalQuery, error)
	// SearchEvalVectorNeighbors 返回与查询向量最近的 limit 件在售商品（距离近的在前）。
	SearchEvalVectorNeighbors(ctx context.Context, embedding []float32, limit int32) ([]SearchEvalCandidate, error)
	// SearchEvalKeywordHits 返回 bigram tsquery 命中的在售商品（ts_rank_cd 高的在前），至多 limit 件。
	SearchEvalKeywordHits(ctx context.Context, tsquery string, limit int32) ([]SearchEvalCandidate, error)
	// SearchEvalDistances 返回这批商品与查询向量的余弦距离；没有向量行的不在结果里。
	SearchEvalDistances(ctx context.Context, embedding []float32, productIDs []int64) (map[int64]float64, error)
}

// SearchEvalQuery 是一条查询与它被搜过的次数。
type SearchEvalQuery struct {
	Query string
	Hits  int64
}

// SearchEvalCandidate 是一件候选商品。Distance 只在向量近邻里有意义，Rank 只在关键词命中里有意义。
type SearchEvalCandidate struct {
	ID           int64
	Title        string
	Subtitle     string
	CategoryName string
	Distance     float64
	Rank         float64
}

func (t tenantTx) SearchEvalQueries(ctx context.Context, since time.Time, limit int32) ([]SearchEvalQuery, error) {
	rows, err := t.q.ListSearchEvalQueries(ctx, db.ListSearchEvalQueriesParams{
		Since: pgtype.Timestamptz{Time: since, Valid: true}, RowLimit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]SearchEvalQuery, 0, len(rows))
	for _, r := range rows {
		out = append(out, SearchEvalQuery{Query: r.Query, Hits: r.Hits})
	}
	return out, nil
}

func (t tenantTx) SearchEvalVectorNeighbors(ctx context.Context, embedding []float32, limit int32) ([]SearchEvalCandidate, error) {
	lit, err := vectorLiteral("查询向量", embedding)
	if err != nil {
		return nil, err
	}
	rows, err := t.q.SearchEvalVectorNeighbors(ctx, db.SearchEvalVectorNeighborsParams{QueryEmbedding: lit, RowLimit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]SearchEvalCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, SearchEvalCandidate{ID: r.ID, Title: r.Title, Subtitle: textOrEmpty(r.Subtitle),
			CategoryName: r.CategoryName, Distance: r.Distance})
	}
	return out, nil
}

func (t tenantTx) SearchEvalKeywordHits(ctx context.Context, tsquery string, limit int32) ([]SearchEvalCandidate, error) {
	rows, err := t.q.SearchEvalKeywordHits(ctx, db.SearchEvalKeywordHitsParams{Tsquery: tsquery, RowLimit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]SearchEvalCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, SearchEvalCandidate{ID: r.ID, Title: r.Title, Subtitle: textOrEmpty(r.Subtitle),
			CategoryName: r.CategoryName, Rank: r.Rank})
	}
	return out, nil
}

func (t tenantTx) SearchEvalDistances(ctx context.Context, embedding []float32, productIDs []int64) (map[int64]float64, error) {
	out := make(map[int64]float64, len(productIDs))
	if len(productIDs) == 0 {
		return out, nil
	}
	lit, err := vectorLiteral("查询向量", embedding)
	if err != nil {
		return nil, err
	}
	rows, err := t.q.SearchEvalDistances(ctx, db.SearchEvalDistancesParams{QueryEmbedding: lit, ProductIds: productIDs})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ProductID] = r.Distance
	}
	return out, nil
}

func textOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
