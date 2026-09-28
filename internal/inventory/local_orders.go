package inventory

// Local 的阶段 1b 部分：关单释放、退款回补、流水查询、活动配额（微服务拆分阶段 1b）。
// 扣减与补偿是 SAGA 分支，在 saga.go。

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/keel/keel/internal/repository"
)

func (l *Local) ActivityStock(ctx context.Context, q ActivityQuery) (map[ActivityKey]Activity, error) {
	if (len(q.SKUIDs) == 0) == (len(q.PromotionIDs) == 0) {
		if len(q.SKUIDs) == 0 {
			return map[ActivityKey]Activity{}, nil
		}
		return nil, fmt.Errorf("%w: 活动配额按 SKU 或按活动查，只能给一个", ErrInvalid)
	}
	skus, promos := dedup(q.SKUIDs), dedup(q.PromotionIDs)
	if err := checkBatch(len(skus) + len(promos)); err != nil {
		return nil, err
	}
	out := map[ActivityKey]Activity{}
	err := l.store.WithTenant(ctx, func(tx repository.InventoryStoreTx) error {
		var rows []repository.ActivityRow
		var err error
		if len(skus) > 0 {
			rows, err = tx.ActivityBySKUs(ctx, skus)
		} else {
			rows, err = tx.ActivityByPromotions(ctx, promos)
		}
		if err != nil {
			return err
		}
		for _, r := range rows {
			out[ActivityKey{PromotionID: r.PromotionID, SKUID: r.SKUID}] = Activity{Quota: r.Quota, Sold: r.Sold}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetActivityQuotas 整组设配额。判定在配额行的行锁之下（InvLockPromotionActivity），
// 与下单扣减（它锁同一批行）串行：扣减要么排在判定之前（判定看到新的已售），要么之后
// （扣减看到新的配额）。整组要么全生效、要么一行都不改。
func (l *Local) SetActivityQuotas(ctx context.Context, promotionID int64, items []ActivityQuota) error {
	if promotionID <= 0 {
		return fmt.Errorf("%w: 设活动配额缺 promotion_id", ErrInvalid)
	}
	if err := checkBatch(len(items)); err != nil {
		return err
	}
	want := make(map[int64]int32, len(items))
	keep := make([]int64, 0, len(items))
	for _, it := range items {
		if it.SKUID <= 0 || it.Quota < 0 {
			return fmt.Errorf("%w: 活动配额 sku=%d quota=%d 不合法", ErrInvalid, it.SKUID, it.Quota)
		}
		if _, dup := want[it.SKUID]; dup {
			return fmt.Errorf("%w: sku %d 出现了两次", ErrInvalid, it.SKUID)
		}
		want[it.SKUID] = it.Quota
		keep = append(keep, it.SKUID)
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i] < keep[j] })
	return l.store.WithTenant(ctx, func(tx repository.InventoryStoreTx) error {
		cur, err := tx.LockPromotionActivity(ctx, promotionID)
		if err != nil {
			return err
		}
		for _, c := range cur {
			if c.Sold == 0 {
				continue
			}
			q, ok := want[c.SKUID]
			if !ok {
				// 那部分配额已经兑现给了买家；移除它会让关单时放不回配额。
				return &ActivityRuleError{SKUID: c.SKUID, Sold: c.Sold, Removed: true}
			}
			if q > 0 && q < c.Sold {
				return &ActivityRuleError{SKUID: c.SKUID, Sold: c.Sold, Quota: q}
			}
		}
		for _, id := range keep {
			if err := tx.UpsertActivityQuota(ctx, promotionID, id, want[id]); err != nil {
				return err
			}
		}
		_, err = tx.DeleteActivityExcept(ctx, promotionID, keep)
		return err
	})
}

// ReleaseForOrder 见接口上的注释；「按流水放」与关单守卫的论证在 saga.go 的文件头。
func (l *Local) ReleaseForOrder(ctx context.Context, r ReleaseRequest) (ReleaseResult, error) {
	if r.OrderNo == "" || r.StoreID <= 0 {
		return ReleaseResult{}, fmt.Errorf("%w: 关单释放缺订单号或门店（order_no=%q store=%d）", ErrInvalid, r.OrderNo, r.StoreID)
	}
	if r.BizType != BizTimeoutRelease && r.BizType != BizBuyerCancel {
		return ReleaseResult{}, fmt.Errorf("%w: 关单释放的 biz_type 只能是 %d 或 %d，实得 %d",
			ErrInvalid, BizTimeoutRelease, BizBuyerCancel, r.BizType)
	}
	lines, err := normalizeLines(r.Lines)
	if err != nil {
		return ReleaseResult{}, err
	}
	var out ReleaseResult
	err = l.store.WithTenant(ctx, func(tx repository.InventoryStoreTx) error {
		if err := tx.LockBizID(ctx, r.OrderNo); err != nil {
			return err
		}
		trail, err := tx.BizTrail(ctx, r.OrderNo)
		if err != nil {
			return err
		}
		for _, e := range trail {
			if e.BizType == BizTimeoutRelease || e.BizType == BizBuyerCancel {
				// 放过了（outbox 任务的重试、或回包丢了之后的重放）：什么都不做。
				out = ReleaseResult{Replayed: true}
				return nil
			}
		}
		out.Qty, err = putBack(ctx, tx, r.OrderNo, r.StoreID, r.BizType, lines, true)
		return err
	})
	if err != nil {
		return ReleaseResult{}, err
	}
	return out, nil
}

// RestockForRefund 见接口上的注释。流水 biz_id 记退款单号：一单可以有几张退款单，
// 记订单号的话分不清是哪一张放回来的（与拆分前一致）。
func (l *Local) RestockForRefund(ctx context.Context, r RestockRequest) (ReleaseResult, error) {
	if r.RefundNo == "" || r.StoreID <= 0 {
		return ReleaseResult{}, fmt.Errorf("%w: 退款回补缺退款单号或门店（refund_no=%q store=%d）", ErrInvalid, r.RefundNo, r.StoreID)
	}
	lines, err := normalizeLines(r.Lines)
	if err != nil {
		return ReleaseResult{}, err
	}
	var out ReleaseResult
	err = l.store.WithTenant(ctx, func(tx repository.InventoryStoreTx) error {
		if err := tx.LockBizID(ctx, r.RefundNo); err != nil {
			return err
		}
		trail, err := tx.BizTrail(ctx, r.RefundNo)
		if err != nil {
			return err
		}
		for _, e := range trail {
			if e.BizType == BizRefundRestock {
				out = ReleaseResult{Replayed: true}
				return nil
			}
		}
		// 按 sku_id 升序拿行锁（normalizeLines 已经排过）—— 与扣减、补偿、关单同一个全局顺序。
		if _, err := tx.LockStoreStock(ctx, r.StoreID, skuIDsOf(lines)); err != nil {
			return err
		}
		for _, ln := range lines {
			after, err := tx.AddStock(ctx, ln.SKUID, r.StoreID, ln.Qty)
			if err != nil {
				return err
			}
			if err := tx.AppendBizLog(ctx, repository.BizLogEntry{
				SKUID: ln.SKUID, StoreID: r.StoreID, ChangeQty: ln.Qty, BizType: BizRefundRestock,
				BizID: r.RefundNo, Before: after - ln.Qty, After: after,
			}); err != nil {
				return err
			}
			// 按活动价成交的行：活动配额一起放回（与关单释放同一个做法，saga.go 的 putBack）。
			// 放不回只出声不报错：活动已删了这个 SKU 之类，不为一个计数把库存回补卡死。
			if ln.PromotionID != nil {
				ok, err := tx.ReleaseActivitySold(ctx, *ln.PromotionID, ln.SKUID, ln.Qty)
				if err != nil {
					return err
				}
				if !ok {
					slog.WarnContext(ctx, "退款回补放回活动配额时受影响 0 行：这个 SKU 已不在活动里，或已售不够减",
						"refund_no", r.RefundNo, "promotion_id", *ln.PromotionID, "sku_id", ln.SKUID)
				}
			}
			out.Qty += ln.Qty
		}
		return nil
	})
	if err != nil {
		return ReleaseResult{}, err
	}
	return out, nil
}

func (l *Local) OrderTrail(ctx context.Context, bizID string) ([]TrailEntry, error) {
	if bizID == "" {
		return nil, fmt.Errorf("%w: 查流水缺单号", ErrInvalid)
	}
	var out []TrailEntry
	err := l.store.WithTenant(ctx, func(tx repository.InventoryStoreTx) error {
		rows, err := tx.BizTrail(ctx, bizID)
		if err != nil {
			return err
		}
		out = make([]TrailEntry, 0, len(rows))
		for _, r := range rows {
			e := TrailEntry{SKUID: r.SKUID, StoreID: r.StoreID, BizType: r.BizType, Change: r.ChangeQty,
				Before: r.Before, After: r.After, Warning: r.Warning}
			if r.Reason != nil {
				e.Reason = *r.Reason
			}
			out = append(out, e)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
