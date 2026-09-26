package repository

import (
	"context"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 混合检索的两路召回在 repository 边界上的那一面（M3 Task 4）。
//
// 这一层只做三件事：把领域类型翻成 sqlc 的参数、把向量过一遍那道闸门、
// 把行翻回领域类型。融合（RRF）与降级都在 service —— 它们是业务规则。
//
// **这一层没有任何一处写 merchant_id，也没有一处设 hnsw.* GUC。**
// 前者由 RLS 管（db/queries 的纪律），后者由 withTenantTx 管，而两者是同一件
// 事的两半：RLS 注入的那个谓词让 HNSW 的 post-filter 必然发生，
// 所以补偿必须和注入发生在同一层。完整论证与实测在 tenant.go 那三个常量上。

// SearchFilters 是契约 SearchFilters 在这一层的形状。
//
// 指针表示「没传」：category_id 传 0 与不传是两件事（前者会筛掉一切，
// 后者不筛）。InStockOnly 不是指针 —— 契约给了它 default: true，
// 「没传」在契约里就等于 true，service 负责把这件事落定，到这一层时它已经
// 是一个确定的布尔值。
type SearchFilters struct {
	CategoryID    *int64
	MinPriceCents *int64
	MaxPriceCents *int64
	InStockOnly   bool
}

// SearchHit 是一路召回里的一条。
//
// Distance 与 Rank 只有对应那一路才有意义（向量路填 Distance、关键词路填
// Rank），另一个是零值。**不把它们合成一个 Score 字段**：余弦距离与
// ts_rank_cd 量纲完全不同，合成一个字段等于邀请调用方去比较两个不可比的数 ——
// 而 RRF 存在的全部理由就是「不要比较它们，只比较名次」（语义检索层 §4）。
type SearchHit struct {
	ID            int64
	Title         string
	Subtitle      *string
	MinPriceCents int64
	MaxPriceCents int64
	SalesCount    int32
	Status        int16
	InStock       bool

	// Distance 是余弦距离（0 = 完全一致，2 = 完全相反），向量路专用。
	Distance float64

	// Rank 是 ts_rank_cd 的覆盖密度得分（越大越相关），关键词路专用。
	Rank float64
}

// SearchTx 是混合检索需要的仓储能力。
type SearchTx interface {
	// SearchProductsByVector 按余弦距离召回，近的在前。
	//
	// embedding 必须是 inference.Dim 维且 L2 归一化的，否则返回
	// ErrVectorWrongDim / ErrVectorNotNormalized 且一条 SQL 都不发。
	SearchProductsByVector(ctx context.Context, sc StoreScope, embedding []float32,
		f SearchFilters, limit int32) ([]SearchHit, error)

	// SearchProductsByKeyword 按 bigram tsquery 召回，ts_rank_cd 高的在前。
	//
	// tsquery 是 internal/search.Bigram 的输出用 " | " 拼成的串。
	// 空串会让 to_tsquery 报语法错，所以调用方必须先判空 —— 这一层不替它
	// 兜底：一个「查询切不出任何词」的请求走到这里已经是上面的逻辑错了，
	// 悄悄返回空列表会把那个错藏起来。
	SearchProductsByKeyword(ctx context.Context, sc StoreScope, tsquery string,
		f SearchFilters, limit int32) ([]SearchHit, error)
}

func (t tenantTx) SearchProductsByVector(ctx context.Context, sc StoreScope,
	embedding []float32, f SearchFilters, limit int32) ([]SearchHit, error) {

	// 查询向量过的是与入库同一道闸门。理由写在 vectorLiteral 上：
	// `<=>` 对查询侧的模长同样敏感，而一个没归一化的查询向量不会报错。
	lit, err := vectorLiteral("查询向量", embedding)
	if err != nil {
		return nil, err
	}
	rows, err := t.q.SearchProductsByVector(ctx, db.SearchProductsByVectorParams{
		QueryEmbedding: lit,
		StoreID:        sc.StoreID,
		RegionID:       sc.RegionID,
		CategoryID:     f.CategoryID,
		MinPriceCents:  f.MinPriceCents,
		MaxPriceCents:  f.MaxPriceCents,
		InStockOnly:    f.InStockOnly,
		RowLimit:       limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]SearchHit, 0, len(rows))
	for _, r := range rows {
		out = append(out, SearchHit{
			ID: r.ID, Title: r.Title, Subtitle: r.Subtitle,
			MinPriceCents: r.MinPriceCents, MaxPriceCents: r.MaxPriceCents,
			SalesCount: r.SalesCount, Status: r.Status,
			InStock: r.InStock, Distance: r.Distance,
		})
	}
	return out, nil
}

func (t tenantTx) SearchProductsByKeyword(ctx context.Context, sc StoreScope,
	tsquery string, f SearchFilters, limit int32) ([]SearchHit, error) {

	rows, err := t.q.SearchProductsByKeyword(ctx, db.SearchProductsByKeywordParams{
		Tsquery:       tsquery,
		StoreID:       sc.StoreID,
		RegionID:      sc.RegionID,
		CategoryID:    f.CategoryID,
		MinPriceCents: f.MinPriceCents,
		MaxPriceCents: f.MaxPriceCents,
		InStockOnly:   f.InStockOnly,
		RowLimit:      limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]SearchHit, 0, len(rows))
	for _, r := range rows {
		out = append(out, SearchHit{
			ID: r.ID, Title: r.Title, Subtitle: r.Subtitle,
			MinPriceCents: r.MinPriceCents, MaxPriceCents: r.MaxPriceCents,
			SalesCount: r.SalesCount, Status: r.Status,
			InStock: r.InStock, Rank: r.Rank,
		})
	}
	return out, nil
}
