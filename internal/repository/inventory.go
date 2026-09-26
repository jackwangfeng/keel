package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 库存扣减的失败，刻意做成**四个不同的 sentinel**。
//
// 数据模型 §4：`UPDATE 0` 在 SQL 层是同一个信号，而在业务上是四件毫不相干的
// 事。本轮（00020，库存按门店分）从两种扩成四种，那张表逐条对应到下面四个：
//
//	① ErrSKUNotInTenant     这个 SKU 在本租户不可见（别家的，或已软删）
//	② ErrStoreNotInTenant   这个门店在本租户不可见（别家的，或已软删）
//	③ ErrSKUNotSoldInStore  这家店不卖这件商品（门店排除 / 大区排除 / 门店停业）
//	④ ErrInsufficientStock  库存不足，**含「这家店根本没有这一行」**
//
// 处置完全不同，而这才是分开的理由：
//
//	① ② 无用且不该有人重试 → 422；**告警**，这是 bug 或攻击
//	③   重试无用，**客户端该换一家店** → 422；不告警 —— 这是运营随时会做的
//	    动作与下单之间的正常竞态
//	④   重试有用（补货之后会成功）→ 409；埋点算缺货
//
// 还有第五条，不是 sentinel：完全没设租户上下文时 current_merchant() 抛 42501，
// 错误原样上浮。它不该被归进上面任何一支 —— 那是配置错误，不是库存的任何一种
// 状态。混为一谈的后果很具体：SAGA 会把一次**配置错误**当成一次**库存不足**
// 去补偿，补偿是幂等的、日志是正常的，于是「所有下单都失败」会表现为
// 「所有商品都缺货」。
//
// 为什么用 sentinel 而不是 `(ok bool, err error)`：布尔只有两个取值，第三种
// 情形一定会被挤进其中一个，而挤进去的那一刻没有任何东西会报警。
var (
	// ErrInsufficientStock：这一行可扣的量不够，**或者这家店压根没有这一行**。
	//
	// 「没有那一行」归进这里而不是 ErrSKUNotSoldInStore，是数据模型 §4 里
	// **唯一一处和任务书字面不同的地方**，理由写在那里：缺行 ≡ 可售 0。
	// 归进「不卖」会让新开的门店在录库存之前对每一件商品都回「本店不卖」，
	// 而那是产品明确不要的形态（「开店即营业」）。
	ErrInsufficientStock = errors.New("库存不足")

	// ErrSKUNotInTenant：这个 SKU 在本租户不可见。要么它属于别的商家
	// （RLS 的 USING 谓词把它挡在视野外），要么它已经被软删了。
	//
	// **本轮它的判据从 inventories 换成了 skus。** 旧写法里「别家的 SKU」与
	// 「没有库存行」本来就是混在一起的（这个文件上一版的注释自己写着这一点）。
	// 按门店分之后「没有库存行」会从罕见变成常态（新店、新品、缺货清零都会
	// 缺行），那个混淆不能带进来。
	ErrSKUNotInTenant = errors.New("SKU 在当前租户不可见")

	// ErrStoreNotInTenant：这个门店在本租户不可见（别家的，或已软删）。
	//
	// 对外它与 ① 合用同一个 problem type：对调用方而言两者是同一句话
	// 「你的请求里有一个服务端不认识的 id」，分开告诉他等于替攻击者做区分。
	// 对内是两个 sentinel，日志与告警分得开。
	ErrStoreNotInTenant = errors.New("门店在当前租户不可见")

	// ErrSKUNotSoldInStore：这家店不卖这件商品。
	//
	// 三种成因合成一个 sentinel：门店排除、**大区排除**、门店已停业。
	// 大区排除不单开一支，因为对调用方而言处置完全一样 —— 换一家店
	// （换到别的大区的店去）。要分得清是哪一层排掉的，那是运营页面的事，
	// 由两张 overrides 表自己回答。
	ErrSKUNotSoldInStore = errors.New("这家门店不卖这件商品")
)

// DeductInventory 扣减 skuID 在 storeID 这家店的可售库存，返回扣减后的水位。
//
// 五条出路，形状各不相同（前四条是 sentinel，第五条是异常原样上浮）：
//
//	err == nil                             扣成功，返回扣减后的 available_qty
//	errors.Is(err, ErrSKUNotInTenant)      ① 这个 SKU 在本租户不可见
//	errors.Is(err, ErrStoreNotInTenant)    ② 这个门店在本租户不可见
//	errors.Is(err, ErrSKUNotSoldInStore)   ③ 这家店不卖这件商品
//	errors.Is(err, ErrInsufficientStock)   ④ 不够扣，或这家店没有这一行
//
// **判定顺序是 ① → ② → ③ → ④，不能反。** 一件被下架的商品通常同时也没库存行，
// 先判 ④ 会报「缺货」，而用户会一直等一个永远不会来的补货。
//
// store_id 从 orders 读，不从 gid 解析（数据模型 §4）：gid 是屏障幂等的键，
// 改它的文法等于改那把钥匙的形状。
func (t tenantTx) DeductInventory(ctx context.Context, skuID, storeID int64, qty int32) (int32, error) {
	if qty <= 0 {
		// 非正数会让这条语句变成一次**回补**（减去负数），而调用方以为自己在扣减。
		// 挡在这里而不是靠 CHECK：chk_qty_nonneg 只管水位不为负，加库存它不拦。
		return 0, fmt.Errorf("扣减数量 %d 必须为正", qty)
	}
	if storeID <= 0 {
		// 零值 store_id 会让那条 UPDATE 匹配不到任何行，于是被判成「缺货」——
		// 而真因是调用方忘了把门店传下来。这一条挡在最前面，
		// 因为它的症状（全店缺货）与真因之间没有任何线索。
		return 0, fmt.Errorf("扣减库存必须指名门店，实得 store_id = %d", storeID)
	}
	row, err := t.q.DeductInventory(ctx, db.DeductInventoryParams{
		SkuID: skuID, StoreID: storeID, Qty: qty,
	})
	if err != nil {
		return 0, err
	}
	switch {
	case row.SkuVisibleRows == 0:
		return 0, fmt.Errorf("sku %d: %w", skuID, ErrSKUNotInTenant)
	case row.StoreVisibleRows == 0:
		return 0, fmt.Errorf("store %d: %w", storeID, ErrStoreNotInTenant)
	case row.SellableRows == 0:
		return 0, fmt.Errorf("sku %d 在门店 %d: %w", skuID, storeID, ErrSKUNotSoldInStore)
	case row.DeductedRows == 0:
		return 0, fmt.Errorf("sku %d 在门店 %d 扣减 %d: %w",
			skuID, storeID, qty, ErrInsufficientStock)
	}
	if row.AfterAvailable == nil {
		// 扣成功却没回传水位，只可能是那条 SQL 被改坏了。不要静默返回 0：
		// 0 是一个合法的库存水位，它会一路写进 inventory_logs.after_available。
		return 0, fmt.Errorf("sku %d 扣减成功但没有回传水位——DeductInventory 的 SQL 被改坏了", skuID)
	}
	return *row.AfterAvailable, nil
}

// RestoreInventory 回补 skuID 在 storeID 这家店的可售库存
// （SAGA 补偿、超时关单释放、退款回补）。
//
// 没有「数量不够」那一支，所以只有两条出路：成功，或 ErrSKUNotInTenant。
// 补偿路径是最容易直接攥着 sku_id 回滚的地方（数据模型 §4 点名说了），
// 而那正是 RLS 在这张表上要挡的动作——挡住之后必须有声音，不能是一次静默的
// rows_affected = 0。
//
// **补偿刻意不重新判「这家店还卖不卖」。** 一件在下单后被运营下架的商品，
// 它的回补必须照样执行 —— 补的是一件已经扣掉的货，「现在还卖不卖」与
// 「当时扣没扣」是两个问题。这一处与 DeductInventory 的不对称是有意的。
func (t tenantTx) RestoreInventory(ctx context.Context, skuID, storeID int64, qty int32) (int32, error) {
	if qty <= 0 {
		return 0, fmt.Errorf("回补数量 %d 必须为正", qty)
	}
	if storeID <= 0 {
		return 0, fmt.Errorf("回补库存必须指名门店，实得 store_id = %d", storeID)
	}
	after, err := t.q.RestoreInventory(ctx, db.RestoreInventoryParams{
		SkuID: skuID, StoreID: storeID, Qty: qty,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("sku %d 门店 %d: %w", skuID, storeID, ErrSKUNotInTenant)
	}
	if err != nil {
		return 0, err
	}
	return after, nil
}
