package repository

import (
	"context"
	"time"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 券的精准发放（service/customer_segments.go、service/agent_proposal_kinds.go 的 coupon 带 target 那一支）
// 在 repository 边界上的那一面，见 db/queries/coupon_targets.sql。

// CustomerStat 是一个在用买家的下单统计。时间缺席表示从没有过。
type CustomerStat struct {
	UserID       int64
	PaidOrders   int64
	Paid90dCents int64
	LastPaidAt   *time.Time
	LastCartAt   *time.Time
}

// CouponTargetGroup 是精准发券的一组（发券组或对照组）在复盘窗口里的表现。
type CouponTargetGroup struct {
	Treated   bool
	Members   int64
	Converted int64
	PaidCents int64
}

type CouponTargetTx interface {
	CustomerStats(ctx context.Context, now time.Time) ([]CustomerStat, error)
	InsertCouponTargetMember(ctx context.Context, proposalID, userID int64, treated bool, userCouponID *int64) error
	CouponTargetGroups(ctx context.Context, proposalID int64, from, to time.Time) ([]CouponTargetGroup, error)
}

func (t tenantTx) CustomerStats(ctx context.Context, now time.Time) ([]CustomerStat, error) {
	rows, err := t.q.CustomerStats(ctx, ts(now))
	if err != nil {
		return nil, err
	}
	out := make([]CustomerStat, 0, len(rows))
	for _, r := range rows {
		out = append(out, CustomerStat{UserID: r.UserID, PaidOrders: r.PaidOrders, Paid90dCents: r.Paid90dCents,
			LastPaidAt: optTime(r.LastPaidAt), LastCartAt: optTime(r.LastCartAt)})
	}
	return out, nil
}

func (t tenantTx) InsertCouponTargetMember(ctx context.Context, proposalID, userID int64, treated bool, userCouponID *int64) error {
	return t.q.InsertCouponTargetMember(ctx, db.InsertCouponTargetMemberParams{ProposalID: proposalID, UserID: userID,
		Treated: treated, UserCouponID: userCouponID})
}

func (t tenantTx) CouponTargetGroups(ctx context.Context, proposalID int64, from, to time.Time) ([]CouponTargetGroup, error) {
	rows, err := t.q.CouponTargetGroups(ctx, db.CouponTargetGroupsParams{ProposalID: proposalID, FromAt: ts(from), ToAt: ts(to)})
	if err != nil {
		return nil, err
	}
	out := make([]CouponTargetGroup, 0, len(rows))
	for _, r := range rows {
		out = append(out, CouponTargetGroup{Treated: r.Treated, Members: r.Members, Converted: r.Converted, PaidCents: r.PaidCents})
	}
	return out, nil
}
