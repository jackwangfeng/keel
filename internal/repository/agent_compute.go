package repository

import (
	"context"
	"time"

	"github.com/keel/keel/internal/repository/internal/db"
)

// AI 经营 M10 计算工具（service/slow_movers.go、service/promotion_review.go，
// docs/AI经营-M10M11设计.md §2）在 repository 边界上的那一面。
//
// slow_movers 复用 restock.go 的 RestockCandidates / StoreSKUSales（同一个「候选 SKU + 已付款销量」
// 口径，只是反过来看周转慢的）；这里只加 promotion_review 单独要的两条查询，见 db/queries/agent_compute.sql。

// PromotionOrderStats 是一个窗口内、含给定 SKU 的已支付订单聚合：单量、这些订单的销售额
// （整单实收）、这些 SKU 本身卖出的件数。
type PromotionOrderStats struct {
	OrderCount int64
	PaidCents  int64
	UnitsSold  int64
}

// CouponOrderStats 是一张券模板带来的已支付订单聚合：单量、销售额、这些订单里由这张券
// 让出的优惠（discount_cents 减去满减满折的那一份）。
type CouponOrderStats struct {
	OrderCount    int64
	PaidCents     int64
	DiscountCents int64
}

// AgentComputeTx 是 promotion_review 用到的、别处没有的查询。
type AgentComputeTx interface {
	// PromotionOrderStats 见上。skuIDs 为空时直接返回零值，不查库
	// （没有参与 SKU，问了也是白问——ANY 在空数组上恒假）。
	PromotionOrderStats(ctx context.Context, start, end time.Time, skuIDs []int64) (PromotionOrderStats, error)
	CouponOrderStats(ctx context.Context, templateID int64) (CouponOrderStats, error)
}

func (t tenantTx) PromotionOrderStats(ctx context.Context, start, end time.Time,
	skuIDs []int64) (PromotionOrderStats, error) {
	if len(skuIDs) == 0 {
		return PromotionOrderStats{}, nil
	}
	r, err := t.q.PromotionSkuOrderStats(ctx, db.PromotionSkuOrderStatsParams{
		WindowStart: ts(start), WindowEnd: ts(end), SkuIds: skuIDs,
	})
	if err != nil {
		return PromotionOrderStats{}, err
	}
	return PromotionOrderStats{OrderCount: r.OrderCount, PaidCents: r.PaidCents, UnitsSold: r.UnitsSold}, nil
}

func (t tenantTx) CouponOrderStats(ctx context.Context, templateID int64) (CouponOrderStats, error) {
	r, err := t.q.CouponTemplateOrderStats(ctx, templateID)
	if err != nil {
		return CouponOrderStats{}, err
	}
	return CouponOrderStats{OrderCount: r.OrderCount, PaidCents: r.PaidCents, DiscountCents: r.CouponDiscountCents}, nil
}
