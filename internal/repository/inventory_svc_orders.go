package repository

// 库存服务仓储的阶段 1b 部分：按单号的扣减 / 回补与活动配额（微服务拆分阶段 1b）。
// 与 inventory_svc.go 同一个 invTx、同一条规矩 —— 只调 db/queries/inventory_svc.sql 里的查询。
// 分开一个文件只是为了让阶段 1a 的那一半保持可读；判定（够不够、被拒还是扣成、放回多少）
// 一律在 inventory 包（saga.go），这一层不解释「0 行」是什么意思。

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

func (t invTx) LockStoreStock(ctx context.Context, storeID int64, skuIDs []int64) ([]StockLevel, error) {
	if storeID <= 0 {
		return nil, fmt.Errorf("锁库存行必须指名门店，实得 store_id = %d", storeID)
	}
	if len(skuIDs) == 0 {
		return []StockLevel{}, nil
	}
	rows, err := t.q.InvLockStoreStock(ctx, db.InvLockStoreStockParams{StoreID: storeID, SkuIds: skuIDs})
	if err != nil {
		return nil, err
	}
	out := make([]StockLevel, 0, len(rows))
	for _, r := range rows {
		out = append(out, StockLevel{SKUID: r.SkuID, Available: r.AvailableQty, Warning: r.WarningQty})
	}
	return out, nil
}

func (t invTx) LockActivity(ctx context.Context, promotionID, skuID int64) (ActivityRow, bool, error) {
	r, err := t.q.InvLockActivity(ctx, db.InvLockActivityParams{PromotionID: promotionID, SkuID: skuID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ActivityRow{PromotionID: promotionID, SKUID: skuID}, false, nil
	}
	if err != nil {
		return ActivityRow{}, false, err
	}
	return ActivityRow{PromotionID: promotionID, SKUID: skuID, Quota: r.Quota, Sold: r.Sold}, true, nil
}

func (t invTx) DeductLocked(ctx context.Context, skuID, storeID int64, qty int32) (int32, error) {
	if qty <= 0 {
		// 非正数会让这条语句变成一次回补（减去负数），而调用方以为自己在扣减。
		return 0, fmt.Errorf("扣减数量 %d 必须为正", qty)
	}
	after, err := t.q.InvDeductLocked(ctx, db.InvDeductLockedParams{Qty: qty, SkuID: skuID, StoreID: storeID})
	if errors.Is(err, pgx.ErrNoRows) {
		// 锁住并判过「够」之后还扣不动：判定与写入不一致，是 bug，不是缺货。
		return 0, fmt.Errorf("sku %d 在门店 %d 锁住判过够之后扣 %d 件失败——InvLockStoreStock 与 InvDeductLocked 不一致",
			skuID, storeID, qty)
	}
	return after, err
}

func (t invTx) AddStock(ctx context.Context, skuID, storeID int64, qty int32) (int32, error) {
	if qty <= 0 {
		return 0, fmt.Errorf("加回数量 %d 必须为正", qty)
	}
	if storeID <= 0 || skuID <= 0 {
		return 0, fmt.Errorf("加回库存缺门店或 SKU（store=%d sku=%d）", storeID, skuID)
	}
	return t.q.InvAddStock(ctx, db.InvAddStockParams{SkuID: skuID, StoreID: storeID, Qty: qty})
}

func (t invTx) AddActivitySold(ctx context.Context, promotionID, skuID int64, qty int32) error {
	if qty <= 0 {
		return fmt.Errorf("扣活动配额的数量 %d 必须为正", qty)
	}
	return t.q.InvAddActivitySold(ctx, db.InvAddActivitySoldParams{Qty: qty, PromotionID: promotionID, SkuID: skuID})
}

func (t invTx) ReleaseActivitySold(ctx context.Context, promotionID, skuID int64, qty int32) (bool, error) {
	n, err := t.q.InvReleaseActivitySold(ctx, db.InvReleaseActivitySoldParams{
		Qty: qty, PromotionID: promotionID, SkuID: skuID,
	})
	return n == 1, err
}

func (t invTx) AppendBizLog(ctx context.Context, e BizLogEntry) error {
	if e.BizID == "" || e.StoreID <= 0 || e.SKUID <= 0 {
		// 一行说不出是哪张单、哪家店的流水对不了账，而它不会报错，只会在盘点时对不平。
		return fmt.Errorf("流水缺单号、门店或 SKU（biz_id=%q store=%d sku=%d）", e.BizID, e.StoreID, e.SKUID)
	}
	return t.q.InvAppendBizLog(ctx, db.InvAppendBizLogParams{
		SkuID: e.SKUID, StoreID: e.StoreID, ChangeQty: e.ChangeQty, BizType: e.BizType, BizID: e.BizID,
		BeforeAvailable: e.Before, AfterAvailable: e.After, Reason: e.Reason,
	})
}

func (t invTx) BizTrail(ctx context.Context, bizID string) ([]TrailEntry, error) {
	rows, err := t.q.InvBizTrail(ctx, bizID)
	if err != nil {
		return nil, err
	}
	out := make([]TrailEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, TrailEntry{SKUID: r.SkuID, StoreID: r.StoreID, BizType: r.BizType,
			ChangeQty: r.ChangeQty, Before: r.BeforeAvailable, After: r.AfterAvailable,
			Reason: r.Reason, Warning: r.WarningQty})
	}
	return out, nil
}

func (t invTx) ActivityBySKUs(ctx context.Context, skuIDs []int64) ([]ActivityRow, error) {
	if len(skuIDs) == 0 {
		return []ActivityRow{}, nil
	}
	rows, err := t.q.InvActivityBySKUs(ctx, skuIDs)
	if err != nil {
		return nil, err
	}
	out := make([]ActivityRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ActivityRow{PromotionID: r.PromotionID, SKUID: r.SkuID, Quota: r.Quota, Sold: r.Sold})
	}
	return out, nil
}

func (t invTx) ActivityByPromotions(ctx context.Context, promotionIDs []int64) ([]ActivityRow, error) {
	if len(promotionIDs) == 0 {
		return []ActivityRow{}, nil
	}
	rows, err := t.q.InvActivityByPromotions(ctx, promotionIDs)
	if err != nil {
		return nil, err
	}
	out := make([]ActivityRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ActivityRow{PromotionID: r.PromotionID, SKUID: r.SkuID, Quota: r.Quota, Sold: r.Sold})
	}
	return out, nil
}

func (t invTx) LockPromotionActivity(ctx context.Context, promotionID int64) ([]ActivityRow, error) {
	rows, err := t.q.InvLockPromotionActivity(ctx, promotionID)
	if err != nil {
		return nil, err
	}
	out := make([]ActivityRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ActivityRow{PromotionID: promotionID, SKUID: r.SkuID, Quota: r.Quota, Sold: r.Sold})
	}
	return out, nil
}

func (t invTx) UpsertActivityQuota(ctx context.Context, promotionID, skuID int64, quota int32) error {
	if quota < 0 {
		return fmt.Errorf("活动配额 %d 不能为负", quota)
	}
	return t.q.InvUpsertActivityQuota(ctx, db.InvUpsertActivityQuotaParams{
		PromotionID: promotionID, SkuID: skuID, Quota: quota,
	})
}

func (t invTx) DeleteActivityExcept(ctx context.Context, promotionID int64, keep []int64) (int64, error) {
	return t.q.InvDeleteActivityExcept(ctx, db.InvDeleteActivityExceptParams{
		PromotionID: promotionID, KeepSkuIds: nonNil(keep),
	})
}

func (t invTx) AdvanceActivitySyncRev(ctx context.Context, promotionID, rev int64) (bool, error) {
	n, err := t.q.InvAdvanceActivitySyncRev(ctx, db.InvAdvanceActivitySyncRevParams{PromotionID: promotionID, Rev: rev})
	return n == 1, err
}
