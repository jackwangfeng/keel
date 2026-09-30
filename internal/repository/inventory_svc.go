package repository

// 库存服务的仓储（微服务拆分阶段 1a，docs/电商系统-微服务拆分方案.md）。
//
// ===========================================================================
// 为什么它是一个单独的类型，而不是 Tx 上多几个方法
// ===========================================================================
//
// 方案要求「库存服务的仓储只用库存池、只碰库存的表」。这两条都要能被编译器看见，
// 而不是靠自觉：
//
//   - **只用库存池**：InventoryStore 由 NewInventoryStore(invPool) 建，手里只有那一个池。
//     core 的 Repo（业务池）上没有任何一个库存服务的方法，反过来 InventoryStore 上也
//     拿不到任何一个业务方法 —— 两者之间唯一的共享是 withTenantTx 那一段「开事务 →
//     设租户 → 提交」（set_config 那一句只能有一份实现，理由写在 tenant.go）。
//   - **只碰库存的表**：InventoryStoreTx 的实现 invTx 只调 db/queries/inventory_svc.sql
//     里的查询，而那个文件只引用 inventories / inventory_logs。
//
// 单体形态下库存池就是业务池（同一个 *pgxpool.Pool），两者连的是同一个库；
// 拆分形态下库存池连库存库。代码路径一模一样，差的只是池指向哪里。
//
// 阶段 1b 把剩下的几处搬了过来：下单 SAGA 的扣减与补偿（带屏障，WithSagaBranch —— 屏障记在
// 库存池指向的那个库里）、关单释放与退款回补的流水核对、活动配额（activity_stocks）。
// 预警通知的展示上下文（商品名、门店名）没有搬：那是 core 的数据，由 core 自己查。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/repository/internal/db"
)

// InventoryStore 是库存服务的数据入口。零值不可用，用 NewInventoryStore 建。
type InventoryStore struct{ r *Repo }

// NewInventoryStore 建库存服务的仓储。pool 必须是库存池（KEEL_INVENTORY_DSN；
// 没配时 app 传进来的就是业务池本身）。
func NewInventoryStore(pool *pgxpool.Pool) *InventoryStore {
	return &InventoryStore{r: New(pool)}
}

// WithTenant 在一个设好租户上下文的事务里执行 fn。租户从 ctx 取（库存服务的 HTTP
// 入口由 rpc.RequireTenant 放进去，进程内调用沿用 core 请求的 ctx）。
func (s *InventoryStore) WithTenant(ctx context.Context, fn func(InventoryStoreTx) error) error {
	return s.r.withLockingTenantTx(ctx, func(tx pgx.Tx, _ Tx) error {
		return fn(invTx{q: db.New(tx)})
	})
}

// WithSagaBranch 在一个「设好租户 + 过了屏障」的事务里执行库存服务的 SAGA 分支
// （下单扣减 / 补偿，inventory 包 saga.go）。与 Repo.WithSagaBranch 同一个屏障算法
// （saga.go 的 decideBarrier），差别只在池：屏障记录必须与业务变更同事务提交，
// 而库存分支的业务变更在库存库里，所以屏障也记在库存库里（微服务拆分阶段 1b）。
// 单体形态下库存池就是业务池，两个入口写的是同一张 barrier 表 —— 分支的 gid / branch_id
// 各不相同，不会互相占位。
//
// 租户从 ctx 取（inventory 包的分支经 dtm.TenantContextFromGID 从 gid 解出来），
// 三种判定与 Repo.WithSagaBranch 一致，都是正常路径。
func (s *InventoryStore) WithSagaBranch(ctx context.Context, gid, branchID, op string,
	fn func(InventoryStoreTx) error) (Decision, error) {
	if gid == "" || branchID == "" {
		return decisionNone, fmt.Errorf("屏障需要非空的 gid 与 branch_id，实得 gid=%q branch_id=%q",
			gid, branchID)
	}
	if !knownOps[op] {
		return decisionNone, fmt.Errorf("%w: %q", ErrUnknownBranchOp, op)
	}
	if fn == nil {
		return decisionNone, errors.New("屏障分支的业务函数为空")
	}
	decision := decisionNone
	err := s.r.withLockingTenantTx(ctx, func(tx pgx.Tx, _ Tx) error {
		d, err := decideBarrier(ctx, tx, gid, branchID, op)
		if err != nil {
			return err
		}
		decision = d
		if d != DecisionExecute {
			return nil
		}
		return fn(invTx{q: db.New(tx)})
	})
	if err != nil {
		return decisionNone, err
	}
	return decision, nil
}

// StockLevel 是一行门店库存。
type StockLevel struct {
	SKUID     int64
	Available int32
	Warning   int32
	UpdatedAt time.Time
}

// StockTotal 是一个 SKU 跨全部门店的合计：可售 sum、预警线 max。
type StockTotal struct {
	SKUID     int64
	Available int32
	Warning   int32
}

// LowStockRow 是库存预警的一行（只有 id 与数，名字由 core 补）。
type LowStockRow struct {
	StoreID   int64
	SKUID     int64
	Available int32
	Warning   int32
}

// StockSet 是一次比较并设置的入参。AllowInsert 的两种取值对应拆分前的两条语句，
// 见 db/queries/inventory_svc.sql 的 InvSetStock。
type StockSet struct {
	SKUID       int64
	StoreID     int64
	Available   int32
	Expected    int32
	Warning     *int32
	AllowInsert bool
}

// StockWrite 是 InvSetStock / InvAdjustStock 回来的那一行，原样交给上层判定：
// 这一层不解释「0 行」是什么意思 —— 那是库存服务的业务（inventory.Local）。
type StockWrite struct {
	// CurrentExists：快照里有这一行（InvAdjustStock 不回这个计数，按 Current 字段是否为空推）。
	CurrentExists bool
	Written       bool
	Current       StockLevel // CurrentExists 为假时是零值（缺行 ≡ 可售 0）
	New           StockLevel // Written 为假时是零值
}

// ManualLog 是一行手工流水里重放要用的那几列。
type ManualLog struct {
	SKUID     int64
	StoreID   int64
	ChangeQty int32
	After     int32
	CreatedAt time.Time
}

// ManualLogEntry 是写一行手工流水的入参（biz_type 固定为 5）。
type ManualLogEntry struct {
	SKUID, StoreID int64
	ChangeQty      int32
	Before, After  int32
	BizID          string
	Reason         *string
}

// ErrManualLogNotFound：这个 biz_id 还没有手工流水。
var ErrManualLogNotFound = errors.New("没有这个 biz_id 的手工流水")

// BizLogEntry 是一行按单号记的流水（下单扣减 / 补偿 / 关单释放 / 退款回补 / 扣减被拒）。
type BizLogEntry struct {
	SKUID, StoreID int64
	ChangeQty      int32
	BizType        int16
	BizID          string
	Before, After  int32
	Reason         *string
}

// TrailEntry 是一张单的一行流水，连同那一行库存此刻的预警线（缺行为 0）。
type TrailEntry struct {
	SKUID, StoreID int64
	BizType        int16
	ChangeQty      int32
	Before, After  int32
	Reason         *string
	Warning        int32
}

// ActivityRow 是一行活动配额。
type ActivityRow struct {
	PromotionID, SKUID int64
	Quota, Sold        int32
}

// InventoryStoreTx 是库存服务仓储在一个事务里能做的全部事情。
type InventoryStoreTx interface {
	StoreStock(ctx context.Context, storeID int64, skuIDs []int64) ([]StockLevel, error)
	// StockoutDays：最近 days 天里收盘时可售 ≤ 0 的天数（天按 tz 切）。没回来的 sku_id 即 0 天。
	StockoutDays(ctx context.Context, storeID int64, skuIDs []int64, days int32, tz string) (map[int64]int32, error)
	SKUTotals(ctx context.Context, skuIDs []int64) ([]StockTotal, error)
	HealthySKUIDs(ctx context.Context, storeID int64) ([]int64, error)
	// LowStock 返回前 limit 条与总条数。storeIDs 为空时一条都没有（不是「全部门店」）。
	LowStock(ctx context.Context, storeIDs, excludeSKUIDs []int64, limit int32) ([]LowStockRow, int64, error)

	SetStock(ctx context.Context, p StockSet) (StockWrite, error)
	AdjustStock(ctx context.Context, skuID, storeID int64, delta int32) (StockWrite, error)

	// LockBizID 在本事务内按 biz_id 串行化（事务结束即释放）。
	LockBizID(ctx context.Context, bizID string) error
	// FindManualLog 查不到返回 ErrManualLogNotFound。
	FindManualLog(ctx context.Context, bizID string) (ManualLog, error)
	AppendManualLog(ctx context.Context, e ManualLogEntry) error

	// InitSKU 给新 SKU 建第一行库存；返回是否真的插了一行（已经有了就是 false）。
	InitSKU(ctx context.Context, skuID, storeID int64, available, warning int32) (bool, error)

	// —— 阶段 1b：按单号的扣减 / 回补（调用方先 LockBizID(单号) 再用下面这些）

	// LockStoreStock 按 sku_id 升序锁住这家店这批 SKU 的库存行并读出水位。缺行不回。
	LockStoreStock(ctx context.Context, storeID int64, skuIDs []int64) ([]StockLevel, error)
	// LockActivity 锁住一行活动配额；ok 为假表示没有这一行（还没同步配额）。
	LockActivity(ctx context.Context, promotionID, skuID int64) (row ActivityRow, ok bool, err error)
	// DeductLocked 扣门店库存（只在 LockStoreStock 判过够之后调），返回扣减后的水位。
	DeductLocked(ctx context.Context, skuID, storeID int64, qty int32) (int32, error)
	// AddStock 加回门店库存（缺行按 0 建），返回加回后的水位。
	AddStock(ctx context.Context, skuID, storeID int64, qty int32) (int32, error)
	// AddActivitySold 扣活动配额（已售 + qty，只在 LockActivity 判过之后调）。
	AddActivitySold(ctx context.Context, promotionID, skuID int64, qty int32) error
	// ReleaseActivitySold 放回活动配额；返回是否真的放回了（假：没有这一行或已售不够减）。
	ReleaseActivitySold(ctx context.Context, promotionID, skuID int64, qty int32) (bool, error)
	// AppendBizLog 记一行按单号的流水。
	AppendBizLog(ctx context.Context, e BizLogEntry) error
	// BizTrail 一张单（订单号 / 退款单号）的全部流水，按写入顺序。
	BizTrail(ctx context.Context, bizID string) ([]TrailEntry, error)

	// —— 阶段 1b：活动配额

	ActivityBySKUs(ctx context.Context, skuIDs []int64) ([]ActivityRow, error)
	ActivityByPromotions(ctx context.Context, promotionIDs []int64) ([]ActivityRow, error)
	// LockPromotionActivity 锁住一个活动现有的全部配额行（整组设配额的第一步）。
	LockPromotionActivity(ctx context.Context, promotionID int64) ([]ActivityRow, error)
	UpsertActivityQuota(ctx context.Context, promotionID, skuID int64, quota int32) error
	// DeleteActivityExcept 删掉不在 keep 里、且没卖过的配额行，返回删了几行。
	DeleteActivityExcept(ctx context.Context, promotionID int64, keep []int64) (int64, error)

	// —— 阶段 2：对账

	// StockKeys 按主键 (sku_id, store_id) 键集分页列出库存行的键：严格在 after 之后的至多 limit 行。
	StockKeys(ctx context.Context, after StockKey, limit int32) ([]StockKey, error)
}

// StockKey 是一行库存的主键。
type StockKey struct {
	SKUID, StoreID int64
}

func (t invTx) StockKeys(ctx context.Context, after StockKey, limit int32) ([]StockKey, error) {
	rows, err := t.q.InvListStockKeys(ctx, db.InvListStockKeysParams{
		AfterSkuID: after.SKUID, AfterStoreID: after.StoreID, RowLimit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]StockKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, StockKey{SKUID: r.SkuID, StoreID: r.StoreID})
	}
	return out, nil
}

// invTx 只包着 *db.Queries，而且只调 inventory_svc.sql 里的查询。
// 它与 tenantTx 是两个类型：core 的 Tx 上够不着这里的任何一个方法。
type invTx struct{ q *db.Queries }

// nonNil 把 nil 切片换成空切片：pgx 把 nil 编码成 SQL NULL，
// 而 x = ANY(NULL) 是 NULL、NOT (x = ANY(NULL)) 也是 NULL —— 排除列表为空时
// 会把**每一行**都排除掉，症状是「一条预警都没有」，看上去完全正常。
func nonNil(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

func (t invTx) StoreStock(ctx context.Context, storeID int64, skuIDs []int64) ([]StockLevel, error) {
	if len(skuIDs) == 0 {
		return []StockLevel{}, nil
	}
	rows, err := t.q.InvStoreStock(ctx, db.InvStoreStockParams{StoreID: storeID, SkuIds: skuIDs})
	if err != nil {
		return nil, err
	}
	out := make([]StockLevel, 0, len(rows))
	for _, r := range rows {
		out = append(out, StockLevel{SKUID: r.SkuID, Available: r.AvailableQty,
			Warning: r.WarningQty, UpdatedAt: r.UpdatedAt.Time})
	}
	return out, nil
}

func (t invTx) StockoutDays(ctx context.Context, storeID int64, skuIDs []int64, days int32, tz string) (map[int64]int32, error) {
	out := make(map[int64]int32, len(skuIDs))
	if len(skuIDs) == 0 || days <= 0 {
		return out, nil
	}
	rows, err := t.q.InvStockoutDays(ctx, db.InvStockoutDaysParams{StoreID: storeID, SkuIds: skuIDs, Days: days, Tz: tz})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.SkuID] = r.StockoutDays
	}
	return out, nil
}

func (t invTx) SKUTotals(ctx context.Context, skuIDs []int64) ([]StockTotal, error) {
	if len(skuIDs) == 0 {
		return []StockTotal{}, nil
	}
	rows, err := t.q.InvSKUTotals(ctx, skuIDs)
	if err != nil {
		return nil, err
	}
	out := make([]StockTotal, 0, len(rows))
	for _, r := range rows {
		out = append(out, StockTotal{SKUID: r.SkuID, Available: r.AvailableQty, Warning: r.WarningQty})
	}
	return out, nil
}

func (t invTx) HealthySKUIDs(ctx context.Context, storeID int64) ([]int64, error) {
	ids, err := t.q.InvHealthySKUIDs(ctx, storeID)
	if err != nil {
		return nil, err
	}
	return nonNil(ids), nil
}

func (t invTx) LowStock(ctx context.Context, storeIDs, excludeSKUIDs []int64, limit int32) ([]LowStockRow, int64, error) {
	if len(storeIDs) == 0 {
		return []LowStockRow{}, 0, nil
	}
	total, err := t.q.InvCountLowStock(ctx, db.InvCountLowStockParams{
		StoreIds: storeIDs, ExcludeSkuIds: nonNil(excludeSKUIDs),
	})
	if err != nil {
		return nil, 0, err
	}
	rows, err := t.q.InvLowStock(ctx, db.InvLowStockParams{
		StoreIds: storeIDs, ExcludeSkuIds: nonNil(excludeSKUIDs), RowLimit: limit,
	})
	if err != nil {
		return nil, 0, err
	}
	out := make([]LowStockRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, LowStockRow{StoreID: r.StoreID, SKUID: r.SkuID,
			Available: r.AvailableQty, Warning: r.WarningQty})
	}
	return out, total, nil
}

func (t invTx) SetStock(ctx context.Context, p StockSet) (StockWrite, error) {
	if p.StoreID <= 0 || p.SKUID <= 0 {
		// 零值 id 会让这条语句匹配不到任何行，于是被判成「缺行」——
		// 对每一个 SKU 都回 404，一个看起来像鉴权问题的 bug。
		return StockWrite{}, fmt.Errorf("库存 CAS 缺门店或 SKU（store=%d sku=%d）", p.StoreID, p.SKUID)
	}
	r, err := t.q.InvSetStock(ctx, db.InvSetStockParams{
		SkuID: p.SKUID, StoreID: p.StoreID, AvailableQty: p.Available,
		WarningQty: p.Warning, AllowInsert: p.AllowInsert, ExpectedAvailableQty: p.Expected,
	})
	if err != nil {
		return StockWrite{}, err
	}
	w := StockWrite{CurrentExists: r.CurrentRows > 0, Written: r.WrittenRows > 0}
	w.Current = levelOf(p.SKUID, r.CurrentAvailableQty, r.CurrentWarningQty, r.CurrentUpdatedAt.Time)
	if w.Written {
		if r.NewAvailableQty == nil || r.NewWarningQty == nil {
			// 写成功却没回传水位，只可能是那条 SQL 被改坏了。不要静默返回 0：
			// 0 是一个合法的库存水位，而且它会被拿去算流水。
			return StockWrite{}, fmt.Errorf("sku %d 写成功但没有回传水位——InvSetStock 的 SQL 被改坏了", p.SKUID)
		}
		w.New = levelOf(p.SKUID, r.NewAvailableQty, r.NewWarningQty, r.NewUpdatedAt.Time)
	}
	return w, nil
}

func (t invTx) AdjustStock(ctx context.Context, skuID, storeID int64, delta int32) (StockWrite, error) {
	if storeID <= 0 || skuID <= 0 {
		return StockWrite{}, fmt.Errorf("库存调整缺门店或 SKU（store=%d sku=%d）", storeID, skuID)
	}
	r, err := t.q.InvAdjustStock(ctx, db.InvAdjustStockParams{SkuID: skuID, StoreID: storeID, Delta: delta})
	if err != nil {
		return StockWrite{}, err
	}
	w := StockWrite{CurrentExists: r.CurrentAvailableQty != nil, Written: r.WrittenRows > 0}
	w.Current = levelOf(skuID, r.CurrentAvailableQty, r.CurrentWarningQty, r.CurrentUpdatedAt.Time)
	if w.Written {
		if r.NewAvailableQty == nil || r.NewWarningQty == nil {
			return StockWrite{}, fmt.Errorf("sku %d 调整成功但没有回传水位——InvAdjustStock 的 SQL 被改坏了", skuID)
		}
		w.New = levelOf(skuID, r.NewAvailableQty, r.NewWarningQty, r.NewUpdatedAt.Time)
	}
	return w, nil
}

func levelOf(skuID int64, avail, warn *int32, at time.Time) StockLevel {
	l := StockLevel{SKUID: skuID, UpdatedAt: at}
	if avail != nil {
		l.Available = *avail
	}
	if warn != nil {
		l.Warning = *warn
	}
	return l
}

func (t invTx) LockBizID(ctx context.Context, bizID string) error {
	return t.q.InvLockBizID(ctx, bizID)
}

func (t invTx) FindManualLog(ctx context.Context, bizID string) (ManualLog, error) {
	r, err := t.q.InvFindManualLog(ctx, bizID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ManualLog{}, ErrManualLogNotFound
	}
	if err != nil {
		return ManualLog{}, err
	}
	return ManualLog{SKUID: r.SkuID, StoreID: r.StoreID, ChangeQty: r.ChangeQty,
		After: r.AfterAvailable, CreatedAt: r.CreatedAt.Time}, nil
}

func (t invTx) AppendManualLog(ctx context.Context, e ManualLogEntry) error {
	if e.BizID == "" {
		// 空 biz_id 的流水说不出是谁、哪一次改的 —— 那正是这一行存在的理由。
		return fmt.Errorf("sku %d 的手工流水没有 biz_id，拒绝写一行说不清来源的流水", e.SKUID)
	}
	return t.q.InvAppendManualLog(ctx, db.InvAppendManualLogParams{
		SkuID: e.SKUID, StoreID: e.StoreID, ChangeQty: e.ChangeQty, BizID: e.BizID,
		BeforeAvailable: e.Before, AfterAvailable: e.After, Reason: e.Reason,
	})
}

func (t invTx) InitSKU(ctx context.Context, skuID, storeID int64, available, warning int32) (bool, error) {
	if available < 0 || warning < 0 {
		// chk_qty_nonneg 也会拦住负的 available_qty，但它给出的是 23514，
		// 而 warning_qty 没有 CHECK 兜着。挡在这里，两者的失败形状才一样。
		return false, fmt.Errorf("初始库存 %d / 预警线 %d 不能为负", available, warning)
	}
	n, err := t.q.InvInitSKU(ctx, db.InvInitSKUParams{
		SkuID: skuID, StoreID: storeID, AvailableQty: available, WarningQty: warning,
	})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
