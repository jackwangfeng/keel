package service

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/keel/keel/internal/repository"
)

// 券的精准发放：顾客人群（customer_segments，MCP 只读工具；发券提案的 target 也按这里划人）。
// 设计：docs/superpowers/specs/2026-10-16-ai-ops-onboarding-restock-coupon-design.md 第三节。
//
// 只给人数与画像，**不给手机号等个人信息**：AI 员工决定「给哪群人发什么券」不需要知道具体是谁，
// 名单只在批准执行时由 Keel 自己算、自己发。

const (
	SegmentLapsed      = "lapsed"
	SegmentOneTime     = "one_time"
	SegmentCartAbandon = "cart_abandon"
	SegmentHighValue   = "high_value"

	segmentLapsedDefaultDays = 30
	segmentLapsedMinDays     = 7
	segmentLapsedMaxDays     = 180
	segmentOneTimeAge        = 7 * 24 * time.Hour  // 只买过一次、且那一单已经过去 7 天以上
	segmentCartWindow        = 7 * 24 * time.Hour  // 近 7 天加过购
	segmentHighValueShare    = 0.2                 // 近 90 天付款金额排前 20%
)

var segmentLabels = map[string][2]string{
	SegmentLapsed:      {"沉睡顾客", "付过款，但最近 N 天（days，默认 30）没有下单"},
	SegmentOneTime:     {"只买过一次", "只有一笔已付款订单，且已经过去 7 天以上"},
	SegmentCartAbandon: {"加购未买", "最近 7 天加过购，之后没有付款"},
	SegmentHighValue:   {"高价值顾客", "近 90 天付款金额排在前 20%"},
}

// ValidSegment 回答一个人群名认不认得。
func ValidSegment(s string) bool { _, ok := segmentLabels[s]; return ok }

// CustomerSegment 是一个人群的人数与画像。
type CustomerSegment struct {
	Segment    string `json:"segment"`
	Label      string `json:"label"`
	Definition string `json:"definition"`
	Size       int    `json:"size"`
	// AvgPaid90dCents 是这群人近 90 天人均付款（分）；AvgDaysSinceLastPaid 是距上次付款的平均天数，
	// 一个付过款的都没有时缺席。
	AvgPaid90dCents      int64    `json:"avg_paid_90d_cents"`
	AvgDaysSinceLastPaid *float64 `json:"avg_days_since_last_paid,omitempty"`
}

type CustomerSegmentsResult struct {
	GeneratedAt    time.Time         `json:"generated_at"`
	TotalCustomers int               `json:"total_customers"`
	LapsedDays     int               `json:"lapsed_days"`
	Segments       []CustomerSegment `json:"segments"`
}

// segmentMembers 按逐人统计划出一个人群，返回 user_id（升序）。纯函数：判据全在这里。
func segmentMembers(stats []repository.CustomerStat, segment string, lapsedDays int, now time.Time) []int64 {
	var out []int64
	switch segment {
	case SegmentLapsed:
		cut := now.Add(-time.Duration(lapsedDays) * 24 * time.Hour)
		for _, s := range stats {
			if s.PaidOrders >= 1 && s.LastPaidAt != nil && s.LastPaidAt.Before(cut) {
				out = append(out, s.UserID)
			}
		}
	case SegmentOneTime:
		cut := now.Add(-segmentOneTimeAge)
		for _, s := range stats {
			if s.PaidOrders == 1 && s.LastPaidAt != nil && s.LastPaidAt.Before(cut) {
				out = append(out, s.UserID)
			}
		}
	case SegmentCartAbandon:
		cut := now.Add(-segmentCartWindow)
		for _, s := range stats {
			if s.LastCartAt != nil && !s.LastCartAt.Before(cut) && (s.LastPaidAt == nil || s.LastPaidAt.Before(*s.LastCartAt)) {
				out = append(out, s.UserID)
			}
		}
	case SegmentHighValue:
		var buyers []repository.CustomerStat
		for _, s := range stats {
			if s.Paid90dCents > 0 {
				buyers = append(buyers, s)
			}
		}
		sort.SliceStable(buyers, func(i, j int) bool { return buyers[i].Paid90dCents > buyers[j].Paid90dCents })
		n := int(math.Ceil(float64(len(buyers)) * segmentHighValueShare))
		for _, s := range buyers[:n] {
			out = append(out, s.UserID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func checkLapsedDays(days int) (int, error) {
	if days == 0 {
		return segmentLapsedDefaultDays, nil
	}
	if days < segmentLapsedMinDays || days > segmentLapsedMaxDays {
		return 0, fmt.Errorf("days 取 %d–%d", segmentLapsedMinDays, segmentLapsedMaxDays)
	}
	return days, nil
}

type CustomerSegmentsRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
}

type CustomerSegmentsService struct {
	repo CustomerSegmentsRepository
	now  func() time.Time
}

func NewCustomerSegmentsService(r CustomerSegmentsRepository) *CustomerSegmentsService {
	return &CustomerSegmentsService{repo: r, now: time.Now}
}

// Segments 实现 customer_segments。顾客是全店的：要全店范围（与发券提案同一个判据）。
func (s *CustomerSegmentsService) Segments(ctx context.Context, lapsedDays int) (CustomerSegmentsResult, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return CustomerSegmentsResult{}, err
	}
	days, err := checkLapsedDays(lapsedDays)
	if err != nil {
		return CustomerSegmentsResult{}, fmt.Errorf("%w: %s", ErrAdminListBadRequest, err.Error())
	}
	now := s.now()
	var stats []repository.CustomerStat
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		stats, err = tx.CustomerStats(ctx, now)
		return err
	}); err != nil {
		return CustomerSegmentsResult{}, err
	}
	byID := make(map[int64]repository.CustomerStat, len(stats))
	for _, st := range stats {
		byID[st.UserID] = st
	}
	out := CustomerSegmentsResult{GeneratedAt: now.UTC(), TotalCustomers: len(stats), LapsedDays: days}
	for _, seg := range []string{SegmentLapsed, SegmentOneTime, SegmentCartAbandon, SegmentHighValue} {
		ids := segmentMembers(stats, seg, days, now)
		cs := CustomerSegment{Segment: seg, Label: segmentLabels[seg][0], Definition: segmentLabels[seg][1], Size: len(ids)}
		var paid int64
		var daysSum float64
		var withPaid int
		for _, id := range ids {
			st := byID[id]
			paid += st.Paid90dCents
			if st.LastPaidAt != nil {
				daysSum += now.Sub(*st.LastPaidAt).Hours() / 24
				withPaid++
			}
		}
		if len(ids) > 0 {
			cs.AvgPaid90dCents = paid / int64(len(ids))
		}
		if withPaid > 0 {
			avg := math.Round(daysSum/float64(withPaid)*10) / 10
			cs.AvgDaysSinceLastPaid = &avg
		}
		out.Segments = append(out.Segments, cs)
	}
	return out, nil
}

// splitHoldout 把名单按（提案 id，user_id）的稳定散列排开，前 holdoutPct% 进对照组。
// 用散列而不是 math/rand：同一条提案重算一次分组不变（复盘、排查时能复现），又与 user_id 的大小无关。
func splitHoldout(proposalID int64, members []int64, holdoutPct int) (treated, control []int64) {
	type kv struct {
		id int64
		h  uint64
	}
	xs := make([]kv, len(members))
	for i, id := range members {
		xs[i] = kv{id, splitmix64(uint64(proposalID)*0x9E3779B97F4A7C15 ^ uint64(id))}
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i].h < xs[j].h })
	nc := int(math.Round(float64(len(xs)) * float64(holdoutPct) / 100))
	for i, x := range xs {
		if i < nc {
			control = append(control, x.id)
		} else {
			treated = append(treated, x.id)
		}
	}
	return treated, control
}

func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}
