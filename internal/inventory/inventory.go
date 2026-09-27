// Package inventory 是库存服务的接口面（微服务拆分阶段 1a，docs/电商系统-微服务拆分方案.md）。
//
// ===========================================================================
// 一个接口，两个实现
// ===========================================================================
//
// core 里凡是要读写门店库存的地方（阶段 1b 的四处除外，见下），都只认 Service 这个接口：
//
//	Local   进程内实现，直接用库存仓储（repository.InventoryStore，只用库存池、只碰库存的表）。
//	        KEEL_ROLE=all 用它；KEEL_ROLE=inventory 也用它 —— 挂在内网 HTTP 上给 core 调。
//	Remote  HTTP 实现，经 rpc.Client 调库存服务的 /internal/v1/inventory/...。
//	        KEEL_ROLE=core 用它（KEEL_INVENTORY_URL 必填）。
//
// 单体与拆分走的是**同一条业务代码路径**：core 永远是「取自己的数据 → 收集 sku_id →
// 批量问一次库存 → 在 Go 里合并」，差别只在这一次问是函数调用还是 HTTP。于是单体上的
// 全量测试覆盖的就是拆分形态的业务逻辑，拆分形态另有一组跨进程测试只验传输层
// （internal/handler/inventory_remote_test.go）。
//
// ===========================================================================
// 边界：跨服务只传 id 与数值
// ===========================================================================
//
// 库存服务不认识 SKU 在不在架、门店是否软删、这家店卖不卖、商品叫什么 —— 那些判断
// 一律由 core 先做完再调这里（方案文档「服务边界」一节）。所以这里的每个入参都是
// 「已经判过的」id：门店范围是显式的 id 列表、要排除的 SKU 是显式的 id 列表。
//
// ===========================================================================
// 两种「没拿到答案」，必须分开
// ===========================================================================
//
//	ErrUnavailable     读：库存服务没回答（连不上、超时、5xx）。读页面据此降级。
//	ErrOutcomeUnknown  写：不知道写没写成。**不能当成失败**去补偿或让用户换个姿势重试 ——
//	                   可能已经提交了，只是回包丢了。core 回 503，客户端原样重试；
//	                   相对调整靠 biz_id 幂等（同一个 biz_id 只生效一次），比较并设置靠
//	                   它自己的前提条件（重试时水位已经不等于 expected，只会 409）。
//
// 单体形态下这两个错误不会出现（进程内调用没有「没回答」这回事），数据库错误原样上浮。
//
// ===========================================================================
// 还没搬过来的（阶段 1b）
// ===========================================================================
//
// 下单 SAGA 的库存分支、超时关单 / 买家取消的回补、退款回补、扣减事务里读预警上下文，
// 以及孤儿草稿关单前的流水核对，仍然在 core 的事务里直接碰库存表。
package inventory

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Level 是一家门店里一个 SKU 的水位。
//
// Exists 为假表示这家店没有这一行 —— **缺行 ≡ 可售 0**（数据模型 §4），Available / Warning
// 此时都是 0。StoreStock 的结果里没有某个 sku_id 与 Exists 为假是同一件事，所以调用方
// 直接 m[skuID] 取零值即可，不需要判 ok。
type Level struct {
	Available int32
	Warning   int32
	Exists    bool
	// UpdatedAt 是库存行的更新时间；缺行时为零值（门店库存清单据此回落到 SKU 自己的时间）。
	UpdatedAt time.Time
}

// Total 是一个 SKU 跨全部门店的合计：Available 取 sum、Warning 取 max（阈值不是总量）。
// 后台商品 / SKU 页的口径，与拆分前 admin_skus.sql 的聚合逐字一致。缺行记 0 / 0。
type Total struct {
	Available int32
	Warning   int32
}

// LowStockQuery 是库存预警报表的入参。
//
// StoreIDs 是**已经判过的**门店范围（未软删、落在筛选与员工范围内），为空即一条都没有 ——
// 不是「全部门店」：让空列表意味着全部，员工范围算出来是空集的那一天就是一次越权。
// ExcludeSKUIDs 是软删 SKU 与软删商品下的 SKU。
type LowStockQuery struct {
	StoreIDs      []int64
	ExcludeSKUIDs []int64
	Limit         int
}

// LowStockRow 是预警的一行：只有 id 与数，名字由 core 补。
type LowStockRow struct {
	StoreID   int64
	SKUID     int64
	Available int32
	Warning   int32
}

// LowStockPage 是前 Limit 条与总条数。
type LowStockPage struct {
	Items []LowStockRow
	Total int64
}

// Stock 是一次写之后（或冲突时）那一行的样子。
type Stock struct {
	SKUID     int64
	StoreID   int64
	Available int32
	Warning   int32
	UpdatedAt time.Time
}

// SetRequest 是比较并设置。
//
// AllowInsert 区分拆分前的两条路径：按门店那条（true，缺行且 Expected = 0 时首次录入）与
// 单店捷径那条（false，缺行即 ErrNotFound）。BizID 是流水的 biz_id（「set:员工:随机串」），
// 数量真的变了才写流水。
type SetRequest struct {
	SKUID       int64
	StoreID     int64
	Available   int32
	Expected    int32
	Warning     *int32
	AllowInsert bool
	BizID       string
}

// AdjustRequest 是相对调整。BizID（「adj:员工:幂等键」）同时是幂等键：
// 同一个 BizID 只生效一次，第二次按第一次的流水回原结果（AdjustResult.Replayed）。
type AdjustRequest struct {
	SKUID   int64
	StoreID int64
	Delta   int32
	Reason  *string
	BizID   string
}

// AdjustResult 是相对调整之后的水位。Replayed 为真表示这个 BizID 之前已经生效过，
// 这一次什么都没改，Stock 是那一次调整之后的水位（Warning 取当前值）。
type AdjustResult struct {
	Stock
	Replayed bool
}

// InitRow 是「给新 SKU 建第一行库存」的一项。
type InitRow struct {
	SKUID     int64
	StoreID   int64
	Available int32
	Warning   int32
}

// Service 是 core 眼里的库存服务。并发安全。
type Service interface {
	// StoreStock 批量取一家门店一批 SKU 的水位。结果里没有的 sku_id 即缺行（可售 0）。
	StoreStock(ctx context.Context, storeID int64, skuIDs []int64) (map[int64]Level, error)

	// SKUTotals 批量取一批 SKU 的跨门店合计。结果里没有的 sku_id 记 0 / 0。
	SKUTotals(ctx context.Context, skuIDs []int64) (map[int64]Total, error)

	// HealthySKUs 返回这家店水位高于预警线的 SKU（门店库存清单 low_stock_only 的补集）。
	HealthySKUs(ctx context.Context, storeID int64) ([]int64, error)

	// LowStock 是库存预警：水位不高于预警线的 (门店, SKU)，
	// 按「低于预警线多少」升序，其次水位升序、门店 id、SKU id。
	LowStock(ctx context.Context, q LowStockQuery) (LowStockPage, error)

	// Set 是比较并设置。出路：nil / ErrNotFound（AllowInsert 为假且缺行）/
	// *ConflictError（水位不等于 Expected，带当前值）/ ErrInvalid / ErrOutcomeUnknown。
	Set(ctx context.Context, r SetRequest) (Stock, error)

	// Adjust 是相对调整（结果不得为负），按 BizID 幂等。出路：nil /
	// *InsufficientError（扣完会变负，带当前值）/ ErrBizIDReused / ErrInvalid / ErrOutcomeUnknown。
	Adjust(ctx context.Context, r AdjustRequest) (AdjustResult, error)

	// InitSKUs 给新 SKU 建第一行库存。可以放心重复调用：这家店已经有这一行、或这个 SKU
	// 在任何一家店已经有行时什么都不做（见 db/queries/inventory_svc.sql 的 InvInitSKU）。
	InitSKUs(ctx context.Context, rows []InitRow) error
}

var (
	// ErrUnavailable：读的时候库存服务没回答。只在拆分形态出现。
	ErrUnavailable = errors.New("库存服务暂时不可用")

	// ErrOutcomeUnknown：写的时候不知道写没写成。只在拆分形态出现。
	ErrOutcomeUnknown = errors.New("库存写入结果未知")

	// ErrNotFound：比较并设置（AllowInsert 为假）的目标行不存在。
	ErrNotFound = errors.New("库存行不存在")

	// ErrConflict：比较并设置的前提不成立。errors.As 取 *ConflictError 拿当前值。
	ErrConflict = errors.New("库存水位与期望值不符")

	// ErrInsufficient：相对调整扣完会变负。errors.As 取 *InsufficientError 拿当前值。
	ErrInsufficient = errors.New("扣完之后库存会变负")

	// ErrBizIDReused：同一个 biz_id 之前生效过的是**另一次**调整（SKU、门店或数量不同）。
	// core 那一侧的幂等键已经按请求哈希挡过一遍，走到这里说明哈希之外的东西对不上。
	ErrBizIDReused = errors.New("biz_id 已被另一次调整用过")

	// ErrInvalid：入参不合法（负数、缺 id、缺 biz_id）。是调用方的 bug，不是用户的错。
	ErrInvalid = errors.New("库存请求不合法")
)

// IsUnavailable 回答「是不是没拿到库存服务的答案」（读的 ErrUnavailable 或写的 ErrOutcomeUnknown）。
// 公网 handler 据此回 503。
func IsUnavailable(err error) bool {
	return errors.Is(err, ErrUnavailable) || errors.Is(err, ErrOutcomeUnknown)
}

// ConflictError 是比较并设置的冲突，带当前值（缺行时 Available 为 0）。
type ConflictError struct{ Current Stock }

func (e *ConflictError) Error() string {
	return fmt.Sprintf("门店 %d 的 sku %d：当前可售 %d，与期望值不符",
		e.Current.StoreID, e.Current.SKUID, e.Current.Available)
}

func (e *ConflictError) Unwrap() error { return ErrConflict }

// InsufficientError 是相对调整的「扣完会变负」，带当前值（缺行时为 0）。
type InsufficientError struct {
	Delta   int32
	Current Stock
}

func (e *InsufficientError) Error() string {
	return fmt.Sprintf("门店 %d 的 sku %d：当前可售 %d，调整 %d 之后会变负",
		e.Current.StoreID, e.Current.SKUID, e.Current.Available, e.Delta)
}

func (e *InsufficientError) Unwrap() error { return ErrInsufficient }

// dedup 去重（保序）。批量读的入参常常带重复（购物车 + 加购的那一个），
// 发给数据库或对面之前收一遍，省一点带宽，也让 HTTP 请求体可预测。
func dedup(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
