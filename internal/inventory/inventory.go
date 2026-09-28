// Package inventory 是库存服务的接口面（微服务拆分阶段 1a，docs/电商系统-微服务拆分方案.md）。
//
// ===========================================================================
// 一个接口，两个实现
// ===========================================================================
//
// core 里凡是要读写库存（门店库存与活动配额）的地方，都只认 Service 这个接口：
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
// 阶段 1b：下单扣减、关单释放、退款回补、活动配额也过来了
// ===========================================================================
//
// 阶段 1b 之后 core 的代码一处都不碰库存的表（inventories / inventory_logs / activity_stocks），
// 库存的代码一处都不碰 core 的表 —— core 与库存可以真的跑在两个库上：
//
//	下单扣减 / 补偿   SAGA 分支（saga.go），载荷带着订单号、门店与行；屏障记在库存库
//	关单 / 取消       core 事务里入队 outbox 任务，worker 调 ReleaseForOrder（按订单号幂等）
//	退款回补          同上，RestockForRefund（按退款单号幂等）
//	收尾与孤儿核对    OrderTrail：一张单的流水（扣减被拒、跌破预警线、有没有扣过）
//	活动配额          ActivityStock（计价 / 标签 / 后台读）、SetActivityQuotas（后台整组设）
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

	// StockoutDays 是最近 days 天（1–90，按 IANA 时区 tz 切天）里每个 SKU「收盘时可售 ≤ 0」的天数。
	// 补货计算（core 的 service/restock.go）用它把断货的天从日均销量的分母里去掉。流水在库存服务的库里，
	// 所以只能由库存服务回答。结果里没有的 sku_id 即 0 天。
	StockoutDays(ctx context.Context, storeID int64, skuIDs []int64, days int, tz string) (map[int64]int, error)

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

	// —— 阶段 1b ——

	// ActivityStock 批量取活动配额与已售。q 里 SKUIDs 与 PromotionIDs 恰好给一个。
	// 结果里没有的 (活动, SKU) 即「还没同步配额」（计价按报价不生效处理）。
	ActivityStock(ctx context.Context, q ActivityQuery) (map[ActivityKey]Activity, error)

	// SetActivityQuotas 整组设一个活动的配额：items 里的 SKU 设成给定配额（已售不动），
	// 不在 items 里的删掉。可以放心重复调用。出路：nil / *ActivityRuleError（卖出过的 SKU
	// 被移除、或配额低于已售 —— core 回 422）/ ErrInvalid / ErrOutcomeUnknown。
	SetActivityQuotas(ctx context.Context, promotionID int64, items []ActivityQuota) error

	// ReleaseForOrder 把一笔关掉的订单占着的库存与活动配额放回（超时关单 / 买家取消）。
	// **按流水放**：只放这一单在自己的流水里还没补回来的部分（扣过多少、补过多少），
	// 所以扣减从没发生（库存分支还在重试、或被拒）时什么都不放；按订单号幂等，
	// 第二次调用什么都不做。放回之后这一单再来的扣减会被拒绝（saga.go 的「关单守卫」）。
	ReleaseForOrder(ctx context.Context, r ReleaseRequest) (ReleaseResult, error)

	// RestockForRefund 未发货的退款到账后把货加回门店库存。按退款单号幂等：
	// 第二次调用什么都不做（Replayed）。活动配额不放回（货已经卖出去了，配额兑现过了）。
	RestockForRefund(ctx context.Context, r RestockRequest) (ReleaseResult, error)

	// OrderTrail 一张单（订单号或退款单号）在库存服务里的全部流水，按写入顺序，
	// 每一行带着那一行库存此刻的预警线。
	OrderTrail(ctx context.Context, bizID string) ([]TrailEntry, error)

	// —— 阶段 2 ——

	// StockKeys 按 (sku_id, store_id) 升序分页列出本租户库存行的键：严格排在 q.After 之后的
	// 至多 q.Limit 行（<= 0 取 DefaultKeysPage，上限 maxBatch）。回来不足一页即到底。
	// 只给 core 的对账用（service/inventory_reconcile.go）：拆分之后两边没有外键，
	// SKU / 门店在 core 里不在了，库存行不会跟着走，要由 core 拿着键回自己的库里比。
	StockKeys(ctx context.Context, q KeysQuery) ([]StockKey, error)
}

// StockKey 是一行库存的键。
type StockKey struct {
	SKUID   int64
	StoreID int64
}

// KeysQuery 是 StockKeys 的入参。After 的零值即「从头开始」（id 都是正数）。
type KeysQuery struct {
	After StockKey
	Limit int
}

// DefaultKeysPage 是 StockKeys 不给 Limit 时的页大小。
const DefaultKeysPage = 1000

// ActivityKey 是一行活动配额的键。
type ActivityKey struct {
	PromotionID int64
	SKUID       int64
}

// Activity 是一行活动配额：Quota 为 0 表示不限（限时折扣），Sold 含待支付。
type Activity struct {
	Quota int32
	Sold  int32
}

// ActivityQuery 是 ActivityStock 的入参：按一批 SKU（计价、标签），或按一批活动（后台）。
type ActivityQuery struct {
	SKUIDs       []int64
	PromotionIDs []int64
}

// ActivityQuota 是整组设配额的一项。
type ActivityQuota struct {
	SKUID int64
	Quota int32
}

// ActivityRuleError 是整组设配额违反了「卖出过的 SKU 不能移出活动 / 配额不能低于已售」。
// 判定在库存服务的事务里、配额行的行锁之下（它看得见已售，core 看不见）。
type ActivityRuleError struct {
	SKUID   int64
	Sold    int32
	Quota   int32 // Removed 为真时无意义
	Removed bool
}

func (e *ActivityRuleError) Error() string {
	if e.Removed {
		return fmt.Sprintf("sku %d 已按活动价卖出 %d 件，不能移出活动", e.SKUID, e.Sold)
	}
	return fmt.Sprintf("sku %d 已卖出 %d 件，配额不能改成 %d", e.SKUID, e.Sold, e.Quota)
}

func (e *ActivityRuleError) Unwrap() error { return ErrActivityRule }

// OrderLine 是一笔订单的一行：扣减载荷、关单释放、退款回补共用。
// PromotionID 非空表示这一行按活动价成交（要扣 / 放活动配额）。
type OrderLine struct {
	SKUID       int64  `json:"sku_id"`
	Qty         int32  `json:"qty"`
	PromotionID *int64 `json:"promotion_id,omitempty"`
}

// ReleaseRequest 是关单释放的入参。BizType 是流水的 biz_type：3 超时关单释放 / 6 买家取消释放。
type ReleaseRequest struct {
	OrderNo string
	StoreID int64
	BizType int16
	Lines   []OrderLine
}

// RestockRequest 是退款回补的入参。Lines 的 PromotionID 被忽略（退款不放回配额）。
type RestockRequest struct {
	RefundNo string
	StoreID  int64
	Lines    []OrderLine
}

// ReleaseResult 是放回 / 加回了多少件。Replayed 为真表示这张单之前已经处理过，这一次什么都没改。
type ReleaseResult struct {
	Qty      int32
	Replayed bool
}

// TrailEntry 是一行流水。Reason 只有扣减被拒（BizType 7）有，是拒绝码（Reject*）。
type TrailEntry struct {
	SKUID   int64
	StoreID int64
	BizType int16
	Change  int32
	Before  int32
	After   int32
	Warning int32
	Reason  string
}

// 流水的 biz_type（与 repository.InventoryLog* 同值；库存服务自己的这一份是它的协议面）。
const (
	BizOrderDeduct    int16 = 1 // 下单扣减
	BizSagaCompensate int16 = 2 // SAGA 补偿回补
	BizTimeoutRelease int16 = 3 // 超时关单释放
	BizRefundRestock  int16 = 4 // 退款回补
	BizManual         int16 = 5 // 手工调整
	BizBuyerCancel    int16 = 6 // 买家取消释放
	BizOrderRejected  int16 = 7 // 下单扣减被拒（Reason 是拒绝码）
)

// 扣减被拒的拒绝码（TrailEntry.Reason）。core 的收尾分支据此回 409 的哪一种。
const (
	RejectInsufficient = "insufficient" // 门店库存不够（含这家店没有这一行）
	RejectSoldOut      = "sold_out"     // 活动配额不够（含配额还没同步）
	RejectReleased     = "released"     // 这一单已经被关单释放过了（取消 / 超时抢在扣减之前）
)

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

	// ErrActivityRule：整组设配额违反了活动配额的规则。errors.As 取 *ActivityRuleError。
	ErrActivityRule = errors.New("活动配额的修改违反了已售规则")
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
