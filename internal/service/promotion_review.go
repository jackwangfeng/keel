package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/keel/keel/internal/repository"
)

// 活动 / 券复盘：MCP 工具 promotion_review（AI 经营 M10 计算工具，docs/AI经营-M10M11设计.md §2）。
//
// 输入 promotion_id 或 coupon_template_id 二选一：
//
//   - 活动：窗口是 [starts_at, min(ends_at, now))，与紧邻的前一个等长窗口对比——销售额、单量、
//     客单价，以及参与 SKU 的销量。「参与 SKU」只对限时折扣 / 秒杀（promo_type 3/4）成立——
//     它们的报价逐 SKU 落在 promotion_skus 里。满减满折（1/2）没有这么一张表可数（范围按
//     分类 / 商品 / 大区 / 门店这类规则表达，不是一份 SKU 名单），按全店口径算，与 shop_overview
//     同一份 ReportOrderTotals。
//   - 券模板：发出多少张、核销多少张、核销率，以及这些券带来的已支付订单与优惠合计。
//
// 判权：requireMerchantWideReader——活动与券都是全店范围的经营信息（同 admin_promotion.go /
// service/agent_brief.go 文件头的判据）。
//
// 金额一律不过浮点：客单价用 report.go deriveMetrics 同一个「分子分母」整数四舍五入技巧。

// PromotionReviewQuery 是复盘参数：PromotionID 与 CouponTemplateID 二选一。
type PromotionReviewQuery struct {
	PromotionID      *int64
	CouponTemplateID *int64
}

// PromotionReviewWindow 是复盘的一段时间窗口，半开区间 [Start, End)。
type PromotionReviewWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// PromotionReviewMetrics 是一段窗口里的经营指标。UnitsSold 只在能数清参与 SKU 时才有
// （限时折扣 / 秒杀）；满减满折按全店口径算，没有单独的「参与 SKU」，这里是 null。
type PromotionReviewMetrics struct {
	SalesCents int64  `json:"sales_cents"`
	SalesYuan  string `json:"sales_yuan"`
	OrderCount int64  `json:"order_count"`
	AOVCents   int64  `json:"aov_cents"`
	AOVYuan    string `json:"aov_yuan"`
	UnitsSold  *int64 `json:"units_sold,omitempty"`
}

// PromotionReview 是一个活动的复盘。
type PromotionReview struct {
	PromotionID int64  `json:"promotion_id"`
	Name        string `json:"name"`
	PromoType   int16  `json:"promo_type"`
	// ShopWide 为真：满减满折，没有 SKU 级别的范围表可数，按全店口径算（Current/Previous 的
	// units_sold 恒为 null）；为假：限时折扣 / 秒杀，按参与 SKU 算。
	ShopWide       bool                   `json:"shop_wide"`
	Window         PromotionReviewWindow  `json:"window"`
	PreviousWindow PromotionReviewWindow  `json:"previous_window"`
	Current        PromotionReviewMetrics `json:"current"`
	Previous       PromotionReviewMetrics `json:"previous"`
}

// CouponTemplateReview 是一张券模板的复盘。ClaimedCount 是已发出总数（issued_count 口径，
// 含领券中心自领与定向发放）；UseRate 是已核销 ÷ 已发出，已发出为 0 时是 null。
type CouponTemplateReview struct {
	TemplateID   int64    `json:"template_id"`
	Name         string   `json:"name"`
	ClaimedCount int32    `json:"claimed_count"`
	UsedCount    int32    `json:"used_count"`
	UseRate      *float64 `json:"use_rate"`
	// OrderCount 是用这张券付过款的订单数，**含后来整单退款的**；RefundedOrderCount 是其中整单退款的单数，
	// 那些单的券退回了买家（不再算已核销），所以 UsedCount 通常 = OrderCount − RefundedOrderCount。
	OrderCount         int64  `json:"order_count" jsonschema:"用这张券付过款的订单数，含后来整单退款的"`
	RefundedOrderCount int64  `json:"refunded_order_count" jsonschema:"其中整单退款的订单数；这些单的券已退回买家，不计入 used_count，所以 used_count 会比 order_count 少这么多"`
	SalesCents         int64  `json:"sales_cents"`
	SalesYuan          string `json:"sales_yuan"`
	DiscountCents      int64  `json:"discount_cents"`
	DiscountYuan       string `json:"discount_yuan"`
}

// PromotionReviewOut 是 promotion_review 的返回：两个分支恰好填一个（另一个是 null）。
type PromotionReviewOut struct {
	Promotion *PromotionReview      `json:"promotion,omitempty"`
	Coupon    *CouponTemplateReview `json:"coupon,omitempty"`
}

// PromotionReviewService 算 promotion_review。
type PromotionReviewService struct {
	repo tenantRunner
	now  func() time.Time
}

func NewPromotionReviewService(r tenantRunner) *PromotionReviewService {
	return &PromotionReviewService{repo: r, now: time.Now}
}

// Review 实现 promotion_review。
func (s *PromotionReviewService) Review(ctx context.Context, q PromotionReviewQuery) (PromotionReviewOut, error) {
	if err := requireMerchantWideReader(ctx); err != nil {
		return PromotionReviewOut{}, err
	}
	switch {
	case q.PromotionID != nil && q.CouponTemplateID != nil:
		return PromotionReviewOut{}, fmt.Errorf("%w: promotion_id 与 coupon_template_id 只能给一个", ErrPromotionBadRequest)
	case q.PromotionID != nil:
		r, err := s.reviewPromotion(ctx, *q.PromotionID)
		if err != nil {
			return PromotionReviewOut{}, err
		}
		return PromotionReviewOut{Promotion: &r}, nil
	case q.CouponTemplateID != nil:
		r, err := s.reviewCoupon(ctx, *q.CouponTemplateID)
		if err != nil {
			return PromotionReviewOut{}, err
		}
		return PromotionReviewOut{Coupon: &r}, nil
	default:
		return PromotionReviewOut{}, fmt.Errorf("%w: promotion_id 与 coupon_template_id 必须给一个", ErrPromotionBadRequest)
	}
}

func (s *PromotionReviewService) reviewPromotion(ctx context.Context, id int64) (PromotionReview, error) {
	var out PromotionReview
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		p, err := tx.AdminGetPromotion(ctx, id)
		if errors.Is(err, repository.ErrPromotionNotFound) {
			return fmt.Errorf("%w: promotion_id=%d", ErrPromotionNotFound, id)
		}
		if err != nil {
			return err
		}
		curStart, curEnd, prevStart, prevEnd := promotionReviewWindows(p.StartsAt, p.EndsAt, s.now())
		shopWide := p.Type != repository.PromoLimitedPrice && p.Type != repository.PromoFlashSale
		var skuIDs []int64
		if !shopWide {
			skus, err := tx.ListPromotionSkus(ctx, []int64{id})
			if err != nil {
				return err
			}
			for _, sk := range skus[id] {
				skuIDs = append(skuIDs, sk.SKUID)
			}
		}
		cur, err := promotionWindowMetrics(ctx, tx, shopWide, skuIDs, curStart, curEnd)
		if err != nil {
			return err
		}
		prev, err := promotionWindowMetrics(ctx, tx, shopWide, skuIDs, prevStart, prevEnd)
		if err != nil {
			return err
		}
		out = PromotionReview{PromotionID: p.ID, Name: p.Name, PromoType: p.Type, ShopWide: shopWide,
			Window:         PromotionReviewWindow{Start: curStart.UTC(), End: curEnd.UTC()},
			PreviousWindow: PromotionReviewWindow{Start: prevStart.UTC(), End: prevEnd.UTC()},
			Current:        cur, Previous: prev}
		return nil
	})
	return out, err
}

// promotionReviewWindows 算当前窗口（[starts_at, min(ends_at, now))）与紧邻的前一个等长窗口，
// 纯函数（单测覆盖边界）。now 早于 starts_at（活动还没开始）时当前窗口长度是 0，前一个窗口随之
// 也是空的——两段都还没有任何一天可统计，报零而不是报错。
func promotionReviewWindows(startsAt, endsAt, now time.Time) (curStart, curEnd, prevStart, prevEnd time.Time) {
	curStart = startsAt
	curEnd = endsAt
	if now.Before(curEnd) {
		curEnd = now
	}
	if curEnd.Before(curStart) {
		curEnd = curStart
	}
	length := curEnd.Sub(curStart)
	prevEnd = curStart
	prevStart = curStart.Add(-length)
	return curStart, curEnd, prevStart, prevEnd
}

// promotionWindowMetrics 算一段窗口的指标：shopWide 时按全店口径（与 shop_overview 同一份
// ReportOrderTotals，不筛门店 / 大区）；否则按参与 SKU 筛「含这些 SKU 的已支付订单」。
func promotionWindowMetrics(ctx context.Context, tx repository.Tx, shopWide bool, skuIDs []int64,
	start, end time.Time) (PromotionReviewMetrics, error) {
	if !end.After(start) {
		return promotionMetricsOf(0, 0, nil), nil
	}
	if shopWide {
		tot, err := tx.ReportOrderTotals(ctx, repository.ReportFilter{Start: start, End: end})
		if err != nil {
			return PromotionReviewMetrics{}, err
		}
		return promotionMetricsOf(tot.OrderCount, tot.PaidCents, nil), nil
	}
	if len(skuIDs) == 0 {
		zero := int64(0)
		return promotionMetricsOf(0, 0, &zero), nil
	}
	st, err := tx.PromotionOrderStats(ctx, start, end, skuIDs)
	if err != nil {
		return PromotionReviewMetrics{}, err
	}
	units := st.UnitsSold
	return promotionMetricsOf(st.OrderCount, st.PaidCents, &units), nil
}

// promotionMetricsOf 拼出一段窗口的指标，纯函数（单测覆盖客单价的四舍五入）。
func promotionMetricsOf(orderCount, salesCents int64, unitsSold *int64) PromotionReviewMetrics {
	aov := promotionReviewAOV(salesCents, orderCount)
	return PromotionReviewMetrics{SalesCents: salesCents, SalesYuan: yuan(salesCents), OrderCount: orderCount,
		AOVCents: aov, AOVYuan: yuan(aov), UnitsSold: unitsSold}
}

// promotionReviewAOV 是客单价：销售额 ÷ 单量，整数四舍五入到分（不经浮点，同 report.go
// deriveMetrics 的分子分母技巧：金额非负时 (2a + b) / 2b 就是 round(a / b)）。单量为 0 时是 0。
func promotionReviewAOV(salesCents, orderCount int64) int64 {
	if orderCount == 0 {
		return 0
	}
	return (2*salesCents + orderCount) / (2 * orderCount)
}

func (s *PromotionReviewService) reviewCoupon(ctx context.Context, id int64) (CouponTemplateReview, error) {
	var out CouponTemplateReview
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		t, err := tx.AdminGetCouponTemplate(ctx, id)
		if errors.Is(err, repository.ErrCouponTemplateNotFound) {
			return fmt.Errorf("%w: coupon_template_id=%d", ErrCouponTemplateNotFound, id)
		}
		if err != nil {
			return err
		}
		stats, err := tx.CouponTemplateStats(ctx, []int64{id}, s.now())
		if err != nil {
			return err
		}
		st := stats[id]
		os, err := tx.CouponOrderStats(ctx, id)
		if err != nil {
			return err
		}
		out = CouponTemplateReview{TemplateID: t.Rule.TemplateID, Name: t.Rule.Name,
			ClaimedCount: st.Issued, UsedCount: st.Used, UseRate: ratio(int64(st.Used), int64(st.Issued)),
			OrderCount: os.OrderCount, RefundedOrderCount: os.RefundedOrderCount, SalesCents: os.PaidCents, SalesYuan: yuan(os.PaidCents),
			DiscountCents: os.DiscountCents, DiscountYuan: yuan(os.DiscountCents)}
		return nil
	})
	return out, err
}
