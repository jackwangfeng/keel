package inventory

import (
	"context"
	"errors"
	"fmt"

	"github.com/keel/keel/internal/repository"
)

// Local 是进程内实现：直接用库存仓储。它就是库存服务的业务本体 ——
// 拆分形态下库存进程里跑的也是它（HTTP handler 只是把它挂到内网上）。
type Local struct{ store *repository.InventoryStore }

// NewLocal 建进程内实现。store 必须建在库存池上（repository.NewInventoryStore(invPool)）。
func NewLocal(store *repository.InventoryStore) *Local { return &Local{store: store} }

var _ Service = (*Local)(nil)

// maxBatch 是一次批量读的 SKU 上限。core 的调用方都是「一页」量级（详情一件商品的规格、
// 一辆车至多 100 行、检索召回窗口），这个上限只挡 bug：一个把全租户 SKU 塞进来的调用
// 在这里报错，而不是变成一条慢查询。
const maxBatch = 5000

func checkBatch(n int) error {
	if n > maxBatch {
		return fmt.Errorf("%w: 一次最多问 %d 个 SKU，实得 %d 个", ErrInvalid, maxBatch, n)
	}
	return nil
}

func (l *Local) StoreStock(ctx context.Context, storeID int64, skuIDs []int64) (map[int64]Level, error) {
	ids := dedup(skuIDs)
	out := make(map[int64]Level, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	if err := checkBatch(len(ids)); err != nil {
		return nil, err
	}
	if storeID <= 0 {
		// 零值 store_id 匹配不到任何行，于是整页都是「可售 0」—— 症状（全店缺货）
		// 与真因（调用方忘了把门店传下来）之间没有任何线索。
		return nil, fmt.Errorf("%w: 查水位必须指名门店，实得 store_id = %d", ErrInvalid, storeID)
	}
	err := l.store.WithTenant(ctx, func(tx repository.InventoryStoreTx) error {
		rows, err := tx.StoreStock(ctx, storeID, ids)
		if err != nil {
			return err
		}
		for _, r := range rows {
			out[r.SKUID] = Level{Available: r.Available, Warning: r.Warning, Exists: true, UpdatedAt: r.UpdatedAt}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (l *Local) SKUTotals(ctx context.Context, skuIDs []int64) (map[int64]Total, error) {
	ids := dedup(skuIDs)
	out := make(map[int64]Total, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	if err := checkBatch(len(ids)); err != nil {
		return nil, err
	}
	err := l.store.WithTenant(ctx, func(tx repository.InventoryStoreTx) error {
		rows, err := tx.SKUTotals(ctx, ids)
		if err != nil {
			return err
		}
		for _, r := range rows {
			out[r.SKUID] = Total{Available: r.Available, Warning: r.Warning}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (l *Local) HealthySKUs(ctx context.Context, storeID int64) ([]int64, error) {
	if storeID <= 0 {
		return nil, fmt.Errorf("%w: 必须指名门店，实得 store_id = %d", ErrInvalid, storeID)
	}
	var out []int64
	err := l.store.WithTenant(ctx, func(tx repository.InventoryStoreTx) error {
		var err error
		out, err = tx.HealthySKUIDs(ctx, storeID)
		return err
	})
	return out, err
}

func (l *Local) LowStock(ctx context.Context, q LowStockQuery) (LowStockPage, error) {
	if q.Limit <= 0 {
		return LowStockPage{}, fmt.Errorf("%w: limit 必须为正，实得 %d", ErrInvalid, q.Limit)
	}
	out := LowStockPage{Items: []LowStockRow{}}
	if len(q.StoreIDs) == 0 {
		// 空门店范围 = 一条都没有（见 LowStockQuery 的注释）。不开事务。
		return out, nil
	}
	err := l.store.WithTenant(ctx, func(tx repository.InventoryStoreTx) error {
		rows, total, err := tx.LowStock(ctx, dedup(q.StoreIDs), dedup(q.ExcludeSKUIDs), int32(q.Limit))
		if err != nil {
			return err
		}
		out.Total = total
		for _, r := range rows {
			out.Items = append(out.Items, LowStockRow{StoreID: r.StoreID, SKUID: r.SKUID,
				Available: r.Available, Warning: r.Warning})
		}
		return nil
	})
	if err != nil {
		return LowStockPage{}, err
	}
	return out, nil
}

// Set 是比较并设置。判定顺序与拆分前两条语句一致：先「有没有这一行」（仅 AllowInsert 为假时
// 是 404），再「前提成不成立」（409，带当前值）。
//
// 流水排在写库存之后、同一个事务里：CAS 成功就证明写之前的值**恰好是 Expected**
// （缺行时 Expected 只能是 0，缺行 ≡ 可售 0），所以 before / change 不用再读一次就是精确的。
// 数量没变（只改预警线，或设成原值）时不写：流水记的是库存变动，一行 +0 只是噪音。
func (l *Local) Set(ctx context.Context, r SetRequest) (Stock, error) {
	if r.SKUID <= 0 || r.StoreID <= 0 {
		return Stock{}, fmt.Errorf("%w: 比较并设置缺 SKU 或门店（sku=%d store=%d）", ErrInvalid, r.SKUID, r.StoreID)
	}
	if r.Available < 0 || r.Expected < 0 || (r.Warning != nil && *r.Warning < 0) {
		// 负的 expected 永远匹配不上任何一行，症状是「怎么改都 409」—— 而 409 的含义是
		// 「重读一次再试就能成功」，于是调用方会一直试下去。core 已经按契约挡过（422），
		// 这里再挡是因为这一层看不见调用方是谁。
		return Stock{}, fmt.Errorf("%w: 数量不能为负", ErrInvalid)
	}
	var out Stock
	err := l.store.WithTenant(ctx, func(tx repository.InventoryStoreTx) error {
		w, err := tx.SetStock(ctx, repository.StockSet{
			SKUID: r.SKUID, StoreID: r.StoreID, Available: r.Available, Expected: r.Expected,
			Warning: r.Warning, AllowInsert: r.AllowInsert,
		})
		if err != nil {
			return err
		}
		if !w.CurrentExists && !r.AllowInsert {
			return fmt.Errorf("sku %d 在门店 %d: %w", r.SKUID, r.StoreID, ErrNotFound)
		}
		if !w.Written {
			return &ConflictError{Current: stockOf(r.StoreID, w.Current)}
		}
		if w.New.Available != r.Expected {
			if r.BizID == "" {
				return fmt.Errorf("%w: sku %d 的库存覆盖没有 biz_id，拒绝写一行说不清来源的流水", ErrInvalid, r.SKUID)
			}
			if err := tx.AppendManualLog(ctx, repository.ManualLogEntry{
				SKUID: r.SKUID, StoreID: r.StoreID, ChangeQty: w.New.Available - r.Expected,
				Before: r.Expected, After: w.New.Available, BizID: r.BizID,
			}); err != nil {
				return err
			}
		}
		out = stockOf(r.StoreID, w.New)
		return nil
	})
	if err != nil {
		return Stock{}, err
	}
	return out, nil
}

// Adjust 是相对调整，按 BizID 幂等。
//
// ===========================================================================
// 幂等为什么落在这里
// ===========================================================================
//
// 拆分前幂等靠 core 的 idempotentTx：抢占幂等键、调库存、写流水、存档在**同一个事务**里，
// 回滚一起回滚。库存搬走之后这四件事跨了两个库，没有一个事务装得下它们，而 core 在
// 「结果未知」（超时、5xx）时只能拿同一把幂等键重试 —— 重试的这一次必须不再加一遍。
// 所以「同一次调整只生效一次」由库存服务自己保证，钥匙就是流水里本来就有的 biz_id
// （「adj:员工:幂等键」）：
//
//  1. 事务里先按 biz_id 拿一把 advisory lock（两次并发到达时串行化，见 InvLockBizID）；
//  2. 查这个 biz_id 的手工流水：有，就是之前生效过 —— SKU、门店、数量都对得上时按那一行
//     回原结果（Replayed），对不上回 ErrBizIDReused；
//  3. 没有，才调整并在同一个事务里写流水。调整失败（扣完会变负）什么都不写，
//     补完货之后同一个 biz_id 可以再来 —— 失败的请求等于没有发生过，与拆分前一致。
//
// 代价：流水不会被清理，所以同一个员工在幂等存档过期（24 小时）之后复用同一把
// Idempotency-Key 做一次同 SKU、同门店、同数量的调整，会被当成重放。客户端的幂等键
// 按约定是一次性的随机串（契约），这个代价只落在违反约定的客户端上。
func (l *Local) Adjust(ctx context.Context, r AdjustRequest) (AdjustResult, error) {
	if r.SKUID <= 0 || r.StoreID <= 0 {
		return AdjustResult{}, fmt.Errorf("%w: 相对调整缺 SKU 或门店（sku=%d store=%d）", ErrInvalid, r.SKUID, r.StoreID)
	}
	if r.Delta == 0 {
		// 0 会在流水里留一行 before = after 的噪声；core 已经按契约拒过（422）。
		return AdjustResult{}, fmt.Errorf("%w: sku %d 的相对调整 delta 为 0", ErrInvalid, r.SKUID)
	}
	if r.BizID == "" {
		return AdjustResult{}, fmt.Errorf("%w: sku %d 的相对调整没有 biz_id —— 流水会追不回那一次请求，重试也会加两遍", ErrInvalid, r.SKUID)
	}
	var out AdjustResult
	err := l.store.WithTenant(ctx, func(tx repository.InventoryStoreTx) error {
		if err := tx.LockBizID(ctx, r.BizID); err != nil {
			return err
		}
		prev, err := tx.FindManualLog(ctx, r.BizID)
		switch {
		case err == nil:
			if prev.SKUID != r.SKUID || prev.StoreID != r.StoreID || prev.ChangeQty != r.Delta {
				return fmt.Errorf("%w: biz_id %q 上一次是门店 %d 的 sku %d 调整 %d",
					ErrBizIDReused, r.BizID, prev.StoreID, prev.SKUID, prev.ChangeQty)
			}
			// 重放：水位回那一次调整之后的值（流水里的 after），预警线取当前值 ——
			// 相对调整不改预警线，当前值就是那一次之后的值（除非之后有人用 PUT 改过，
			// 那时当前值才是对的）。
			cur, err := tx.StoreStock(ctx, r.StoreID, []int64{r.SKUID})
			if err != nil {
				return err
			}
			out = AdjustResult{Replayed: true, Stock: Stock{SKUID: r.SKUID, StoreID: r.StoreID,
				Available: prev.After, UpdatedAt: prev.CreatedAt}}
			if len(cur) == 1 {
				out.Warning = cur[0].Warning
			}
			return nil
		case errors.Is(err, repository.ErrManualLogNotFound):
		default:
			return err
		}

		w, err := tx.AdjustStock(ctx, r.SKUID, r.StoreID, r.Delta)
		if err != nil {
			return err
		}
		if !w.Written {
			return &InsufficientError{Delta: r.Delta, Current: stockOf(r.StoreID, w.Current)}
		}
		// before = after - delta 是精确的：那条语句写的就是「+ delta」，插入那一支（缺行）
		// 也是 0 + delta。不再读一次 —— 多读一次就是多一个快照。
		after := w.New.Available
		if err := tx.AppendManualLog(ctx, repository.ManualLogEntry{
			SKUID: r.SKUID, StoreID: r.StoreID, ChangeQty: r.Delta,
			Before: after - r.Delta, After: after, BizID: r.BizID, Reason: r.Reason,
		}); err != nil {
			return err
		}
		out = AdjustResult{Stock: stockOf(r.StoreID, w.New)}
		return nil
	})
	if err != nil {
		return AdjustResult{}, err
	}
	return out, nil
}

func (l *Local) InitSKUs(ctx context.Context, rows []InitRow) error {
	if len(rows) == 0 {
		return nil
	}
	if err := checkBatch(len(rows)); err != nil {
		return err
	}
	for _, r := range rows {
		if r.SKUID <= 0 || r.StoreID <= 0 || r.Available < 0 || r.Warning < 0 {
			return fmt.Errorf("%w: 建库存行的入参不合法（sku=%d store=%d available=%d warning=%d）",
				ErrInvalid, r.SKUID, r.StoreID, r.Available, r.Warning)
		}
	}
	return l.store.WithTenant(ctx, func(tx repository.InventoryStoreTx) error {
		for _, r := range rows {
			if _, err := tx.InitSKU(ctx, r.SKUID, r.StoreID, r.Available, r.Warning); err != nil {
				return fmt.Errorf("sku %d 的库存行没建成（这个 SKU 会表现为缺货）: %w", r.SKUID, err)
			}
		}
		return nil
	})
}

func stockOf(storeID int64, l repository.StockLevel) Stock {
	return Stock{SKUID: l.SKUID, StoreID: storeID, Available: l.Available, Warning: l.Warning, UpdatedAt: l.UpdatedAt}
}
