package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

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
// 后者不筛）。
//
// **没有 InStockOnly**（微服务拆分阶段 1a）：库存归库存服务，召回查询不再 JOIN
// inventories，「只看有货」变成 service 在召回之后的过滤（service/search.go）。
// 这一层留一个不执行的开关，等于让调用方以为 SQL 在替它筛。
type SearchFilters struct {
	CategoryID    *int64
	MinPriceCents *int64
	MaxPriceCents *int64
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
	// InStock 这一层不填（恒为 false）：由 service 按库存服务的回答算（阶段 1a）。
	InStock bool

	// MainImageUploadID 同 Product.MainImageUploadID：主图的 upload id，没有图为 nil。
	MainImageUploadID *int64

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

	// OnSaleSKUsOfProducts 返回这批商品各自的在售 SKU（键是 product_id），
	// 检索据此算 in_stock（阶段 1a：水位由库存服务回答）。
	OnSaleSKUsOfProducts(ctx context.Context, productIDs []int64) (map[int64][]int64, error)

	// InsertSearchLog 写一行检索日志（数据模型 §8，迁移 00027）。
	InsertSearchLog(ctx context.Context, l SearchLog) error

	// SearchLogRankedIDs 读 trace_id 那一行的 ranked_ids（这次检索真正返回的那几条）。
	// 行不存在、或属于别的租户（RLS 过滤掉了），都是 ErrSearchLogNotFound。
	SearchLogRankedIDs(ctx context.Context, traceID string) ([]int64, error)

	// SetSearchLogBehavior 回填一个行为列（POST /search/events）。
	// **首次为准**：那一列已有值时一行都不改，返回 false；写下了返回 true。
	// 行不存在时同样返回 false —— 调用方应当先用 SearchLogRankedIDs 判存在。
	SetSearchLogBehavior(ctx context.Context, traceID string, col SearchBehavior,
		productID int64) (bool, error)
}

// SearchBehavior 是 search_logs 的三个行为列，一一对应契约里 /search/events 的三种事件。
type SearchBehavior int

const (
	BehaviorClicked SearchBehavior = iota + 1 // clicked_id ← click
	BehaviorCarted                            // carted_id  ← add_cart
	BehaviorOrdered                           // ordered_id ← order
)

// ErrSearchLogNotFound：trace_id 在当前租户下查无此行。
var ErrSearchLogNotFound = errors.New("检索日志不存在或不属于当前租户")

func (t tenantTx) SearchLogRankedIDs(ctx context.Context, traceID string) ([]int64, error) {
	ids, err := t.q.GetSearchLogRankedIDs(ctx, traceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSearchLogNotFound
	}
	return ids, err
}

func (t tenantTx) SetSearchLogBehavior(ctx context.Context, traceID string,
	col SearchBehavior, productID int64) (bool, error) {
	var (
		n   int64
		err error
	)
	switch col {
	case BehaviorClicked:
		n, err = t.q.SetSearchLogClicked(ctx, db.SetSearchLogClickedParams{ProductID: productID, TraceID: traceID})
	case BehaviorCarted:
		n, err = t.q.SetSearchLogCarted(ctx, db.SetSearchLogCartedParams{ProductID: productID, TraceID: traceID})
	case BehaviorOrdered:
		n, err = t.q.SetSearchLogOrdered(ctx, db.SetSearchLogOrderedParams{ProductID: productID, TraceID: traceID})
	default:
		return false, fmt.Errorf("未知的行为列 %d", col)
	}
	return n > 0, err
}

// SearchLog 是一行检索日志在这一层的形状。
//
// 只收 POST /search 这一刻知道的那些列；user_id / session_id / parsed_intent
// 不在这里，三个行为列由 SetSearchLogBehavior 事后回填 —— 见 db/queries/search_logs.sql。
type SearchLog struct {
	Query     string
	RecallIDs []int64
	RankedIDs []int64
	LatencyMs int32
	TraceID   string
	Strategy  string
	Stages    []string

	// ModelName / ModelVersion 是**这一次**给查询做 embedding 的那个模型；
	// 向量路没跑成时为 nil（不是空串 —— 空串会被读成「有个模型，名字是空的」）。
	ModelName    *string
	ModelVersion *string
}

func (t tenantTx) InsertSearchLog(ctx context.Context, l SearchLog) error {
	// 两个数组列不收 nil：pgx 把 nil 切片编码成 NULL，而「召回了 0 条」与
	// 「不知道召回了什么」是两件事，前者该是空数组。
	recall, ranked, stages := l.RecallIDs, l.RankedIDs, l.Stages
	if recall == nil {
		recall = []int64{}
	}
	if ranked == nil {
		ranked = []int64{}
	}
	if stages == nil {
		stages = []string{}
	}
	lat := l.LatencyMs
	return t.q.InsertSearchLog(ctx, db.InsertSearchLogParams{
		Query: l.Query, RecallIds: recall, RankedIds: ranked,
		LatencyMs: &lat, TraceID: l.TraceID, Strategy: l.Strategy,
		Stages: stages, ModelName: l.ModelName, ModelVersion: l.ModelVersion,
	})
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
			Distance:          r.Distance,
			MainImageUploadID: mainImageOf(r.MainImageUploadID),
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
			Rank:              r.Rank,
			MainImageUploadID: mainImageOf(r.MainImageUploadID),
		})
	}
	return out, nil
}

// OnSaleSKUsOfProducts 返回这批商品各自的在售 SKU（未软删、status = 1），键是 product_id。
// 检索据此向库存服务问水位、算 in_stock（阶段 1a）。
func (t tenantTx) OnSaleSKUsOfProducts(ctx context.Context, productIDs []int64) (map[int64][]int64, error) {
	out := make(map[int64][]int64, len(productIDs))
	if len(productIDs) == 0 {
		return out, nil
	}
	rows, err := t.q.ListOnSaleSKUsOfProducts(ctx, productIDs)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ProductID] = append(out[r.ProductID], r.ID)
	}
	return out, nil
}
