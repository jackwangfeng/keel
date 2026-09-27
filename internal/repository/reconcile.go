package repository

import (
	"context"
	"time"
)

// 库存对账的 core 一侧（微服务拆分阶段 2）：只读，SQL 在 db/queries/reconcile.sql。
// 库存那一侧的数由 service 经 inventory.Service 问回来，差集在 service 里算。

// EntityState 是一个 id 在 core 里的状态：Exists 为假即 core 里没有（硬删、或属于别的租户）。
type EntityState struct {
	Exists  bool
	Deleted bool
}

// PromotionSKUKey 是一行活动商品的键。
type PromotionSKUKey struct {
	PromotionID, SKUID int64
}

// ReconcileTx 是对账在一个租户事务里要问 core 的几件事。
type ReconcileTx interface {
	// SKUStates / StoreStates 对 ids 里的每一个都给出状态（没回来的填 Exists = false）。
	SKUStates(ctx context.Context, ids []int64) (map[int64]EntityState, error)
	StoreStates(ctx context.Context, ids []int64) (map[int64]EntityState, error)
	// LivePromotionSKUs 上线中且未结束的活动的 id 与它们的活动商品（now 由调用方给）。
	LivePromotionSKUs(ctx context.Context, now time.Time) ([]int64, []PromotionSKUKey, error)
}

func statesOf(ids []int64, found map[int64]bool) map[int64]EntityState {
	out := make(map[int64]EntityState, len(ids))
	for _, id := range ids {
		d, ok := found[id]
		out[id] = EntityState{Exists: ok, Deleted: d}
	}
	return out
}

func (t tenantTx) SKUStates(ctx context.Context, ids []int64) (map[int64]EntityState, error) {
	found := map[int64]bool{}
	if len(ids) > 0 {
		rows, err := t.q.ReconcileSKUStates(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			found[r.ID] = r.Deleted
		}
	}
	return statesOf(ids, found), nil
}

func (t tenantTx) StoreStates(ctx context.Context, ids []int64) (map[int64]EntityState, error) {
	found := map[int64]bool{}
	if len(ids) > 0 {
		rows, err := t.q.ReconcileStoreStates(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			found[r.ID] = r.Deleted
		}
	}
	return statesOf(ids, found), nil
}

func (t tenantTx) LivePromotionSKUs(ctx context.Context, now time.Time) ([]int64, []PromotionSKUKey, error) {
	ids, err := t.q.ReconcileLivePromotionIDs(ctx, ts(now))
	if err != nil {
		return nil, nil, err
	}
	rows, err := t.q.ReconcileLivePromotionSkus(ctx, ts(now))
	if err != nil {
		return nil, nil, err
	}
	keys := make([]PromotionSKUKey, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, PromotionSKUKey{PromotionID: r.PromotionID, SKUID: r.SkuID})
	}
	if ids == nil {
		ids = []int64{}
	}
	return ids, keys, nil
}
