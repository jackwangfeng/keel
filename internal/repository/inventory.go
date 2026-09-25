package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 库存扣减的两种失败，刻意做成**两个不同的 sentinel**。
//
// 数据模型 §4 与 M2 计划第三条：`UPDATE 0` 有两种成因，它们在 SQL 层的信号
// 完全一样，但在业务上是两件毫不相干的事——
//
//   - ErrInsufficientStock 是一条正常的业务分支，SAGA 收到它就该走全局补偿；
//   - ErrSKUNotInTenant 是 bug 或攻击。补偿它没有任何意义，掩盖它才是代价：
//     补偿是幂等的、日志是正常的，于是一次越权尝试在监控上只表现为库存波动。
//
// 为什么用两个错误而不是 `(ok bool, err error)`：布尔只有两个取值，第三种情形
// 一定会被挤进其中一个，而挤进去的那一刻没有任何东西会报警。做成两个 sentinel
// 之后，把 ② 当 ① 处理这句话必须写成一次显式的 errors.Is(err, ErrSKUNotInTenant)
// —— 写得出来，但写出来是看得见的。
var (
	// ErrInsufficientStock：这一行在本租户可见，但 available_qty 不够扣。
	ErrInsufficientStock = errors.New("库存不足")

	// ErrSKUNotInTenant：这一行在本租户根本不可见。要么这个 sku_id 属于别的
	// 商家（RLS 的 USING 谓词把它挡在视野外），要么它根本没有库存行。
	// 两者在 M2 的写路径上都是「不该发生」——下单前的试算已经读过这些 SKU。
	ErrSKUNotInTenant = errors.New("SKU 在当前租户不可见")
)

// DeductInventory 扣减 skuID 的可售库存，返回扣减后的水位。
//
// 三条出路，形状各不相同：
//
//	err == nil                            扣成功，返回值是扣减后的 available_qty
//	errors.Is(err, ErrInsufficientStock)  库存不足，业务分支
//	errors.Is(err, ErrSKUNotInTenant)     这一行在本租户不可见
//
// 还有第四条：完全没设租户上下文时 current_merchant() 抛 42501，错误原样上浮。
// 它不该被归进上面任何一支——那是配置错误，不是库存的任何一种状态。
func (t tenantTx) DeductInventory(ctx context.Context, skuID int64, qty int32) (int32, error) {
	if qty <= 0 {
		// 非正数会让这条语句变成一次**回补**（减去负数），而调用方以为自己在扣减。
		// 挡在这里而不是靠 CHECK：chk_qty_nonneg 只管水位不为负，加库存它不拦。
		return 0, fmt.Errorf("扣减数量 %d 必须为正", qty)
	}
	row, err := t.q.DeductInventory(ctx, db.DeductInventoryParams{
		SkuID: skuID, AvailableQty: qty,
	})
	if err != nil {
		return 0, err
	}
	if row.VisibleRows == 0 {
		return 0, fmt.Errorf("sku %d: %w", skuID, ErrSKUNotInTenant)
	}
	if row.DeductedRows == 0 {
		return 0, fmt.Errorf("sku %d 扣减 %d: %w", skuID, qty, ErrInsufficientStock)
	}
	if row.AfterAvailable == nil {
		// 扣成功却没回传水位，只可能是那条 SQL 被改坏了。不要静默返回 0：
		// 0 是一个合法的库存水位，它会一路写进 inventory_logs.after_available。
		return 0, fmt.Errorf("sku %d 扣减成功但没有回传水位——DeductInventory 的 SQL 被改坏了", skuID)
	}
	return *row.AfterAvailable, nil
}

// RestoreInventory 回补 skuID 的可售库存（SAGA 补偿、超时关单释放、退款回补）。
//
// 没有「数量不够」那一支，所以只有两条出路：成功，或 ErrSKUNotInTenant。
// 补偿路径是最容易直接攥着 sku_id 回滚的地方（数据模型 §4 点名说了），
// 而那正是 RLS 在这张表上要挡的动作——挡住之后必须有声音，不能是一次静默的
// rows_affected = 0。
func (t tenantTx) RestoreInventory(ctx context.Context, skuID int64, qty int32) (int32, error) {
	if qty <= 0 {
		return 0, fmt.Errorf("回补数量 %d 必须为正", qty)
	}
	after, err := t.q.RestoreInventory(ctx, db.RestoreInventoryParams{
		SkuID: skuID, AvailableQty: qty,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("sku %d: %w", skuID, ErrSKUNotInTenant)
	}
	if err != nil {
		return 0, err
	}
	return after, nil
}
