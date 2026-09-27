package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/keel/keel/internal/repository/internal/db"
	"github.com/keel/keel/internal/tenant"
)

// 超时补偿定时任务（Task 6）在 repository 边界上的那一面。
//
// # 定时任务的租户从哪来 —— 这一层给出的是那个问题的前半段
//
// 定时任务跑在任何 HTTP 请求之外：没有 Host，没有请求上下文，没有当前用户。
// 而这个仓库里租户进入 ctx 的入口只有两个，一个是 tenant.Resolver（吃 Host），
// 另一个是 dtm.TenantContextFromGID（吃 gid）—— 两个都不适用。
//
// 答案是 **ActiveMerchants**：先把「有哪些活跃商家」读出来，再一家一家进
// WithTenant。这条路走得通，是因为 merchants 是 tenant-root 类
// （db/tenancy.json）—— 它**没有 RLS**，而且刻意没有：租户解析要在
// SET LOCAL 之前读它，挂上策略会让整站在第一跳 404。
//
// 于是定时任务不需要任何 RLS 豁免、不需要平台角色、也不需要给 keel_app 一份
// 能绕过 RLS 的凭据。最后那一条是硬的：db.NewPool 的自检会当场拒绝一个能绕过
// RLS 的角色启动（internal/db/pool_test.go 的 TestNewPoolRejectsRLSBypassingRole），
// 而那道自检存在的理由与 KEEL_DTM_DSN 那段是同一条 —— 应用进程握着能绕过 RLS
// 的连接，比任何一次越权读取都更难发现。
//
// **这是与数据模型 §12 的一处冲突，写在这里而不是悄悄绕过。** §12 说
// 「worker 出队以平台身份执行，不走 RLS 注入」，那是 jobs 表跨租户公平出队的
// 前提。本仓库没有那个身份，也不打算有。代价是扫描按租户切成 N 段，不能靠
// 一条 ORDER BY 给出全局顺序 —— 公平调度因此落在应用层（service/sweep.go），
// 形状照 §12 的「上限 + 兜底」。

// ActiveMerchants 返回全部可服务商家的 id，按 id 升序。
//
// 「可服务」的判据与 tenant.Resolver.byCode 逐字一致：status = 1 且未软删。
// 两处写得不一样的后果是一家停用的店仍然被定时任务扫到 —— 它的订单会继续被
// 关、库存继续被回补，而它在 HTTP 上已经 404 了。
//
// 走裸 SQL 而不是 sqlc：sqlc 的产物挂在 tenantTx 上，而那个类型的每一个方法
// 都跑在一个设好 app.merchant_id 的事务里。这一条查询恰恰是在**还没有租户**
// 的时候发的，它不属于那一面。tenant/resolver.go 读 merchants 时同此惯例。
func (r *Repo) ActiveMerchants(ctx context.Context) ([]int64, error) {
	// 「当前状态」取最新一行修订（00024），文本与解析层共用同一份。
	rows, err := r.pool.Query(ctx, `
		SELECT m.id FROM `+tenant.EffectiveMerchantFrom+`
		 WHERE m.deleted_at IS NULL AND `+tenant.EffectiveStatus+` = 1
		 ORDER BY m.id`)
	if err != nil {
		return nil, fmt.Errorf("读活跃商家清单失败: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ErrOrderNotClaimed：想占下这一单来处置，而它已经不在预期的状态上了。
//
// 它是**正常路径**，不是故障：扫描与处置之间，支付回调可能刚把这一单推到 20，
// 或者另一个 worker 抢先关掉了它。两种都该跳过这一单继续下一单。
//
// 做成 sentinel 而不是 (ok bool, err error)：这个仓库的测试要能区分
// 「我守的这件事坏了」和「附近有别的东西坏了」，而一个布尔的 false 同时covers
// 「被别人抢走了」和「SQL 被改坏了所以谁也匹配不上」——后者必须是红的。
var ErrOrderNotClaimed = errors.New("这一单已经不在预期状态上")

// ErrDraftHasInventoryLog：一笔 status = 0 的孤儿草稿身上挂着库存流水。
//
// **这是不变量被破坏，不是一种业务状态。** 孤儿清理之所以敢不回补库存，
// 全靠「建单在前、库存在后」这条编排顺序（service/order.go 的文件头）：
// 订单还停在 0，说明建单分支的正向没成功，而库存分支排在它后面，连开始都没开始。
//
// 那条不变量住在**另一个文件里的一个常量**（service/order_saga.go 的 sagaSteps）。
// 哪天有人把那两行对调，孤儿清理就会开始静默地漏掉库存回补 —— 水位、订单状态、
// 日志全都正常，只有对账能发现，而且是几个月之后。所以这里花一次点查把它变成
// 一次响亮的失败。
var ErrDraftHasInventoryLog = errors.New("孤儿草稿身上有库存流水")

// ExpiredOrder 是扫描扫出来的一行。
//
// 只有 id 与 order_no：处置那一步靠 order_no 做条件 UPDATE，重新读一次状态是
// 多余的（真正的判定在 UPDATE 的 rows_affected 上），而 id 是读订单行时要用的。
type ExpiredOrder struct {
	ID      int64
	OrderNo string
	// StoreID 是这一单的履约门店。回补要回补到当初扣减的那一家 ——
	// 而这条清扫路径跑在任何请求之外，它对那一单的记忆只有这几列。
	StoreID int64
	// UserID 是下单的买家（00058）：关单时放回每人限购，那个计数按买家记。
	UserID int64
}

// SweepTx 是超时补偿在一次租户事务里能做的事。
type SweepTx interface {
	// ListExpiredPendingOrders 扫第一类：status = 10 且已过期。
	// 它们进过 SAGA，库存已真实扣减。
	ListExpiredPendingOrders(ctx context.Context, limit int32) ([]ExpiredOrder, error)

	// ListExpiredDraftOrders 扫第二类：status = 0 的孤儿草稿。
	// 它们没进过 SAGA，一件库存都没扣。
	ListExpiredDraftOrders(ctx context.Context, limit int32) ([]ExpiredOrder, error)

	// ClaimExpiredPendingOrder 把一笔超时未支付的订单原子地关到 90。
	// 没占下（被支付回调抢先，或已被别的 worker 处理）返回 ErrOrderNotClaimed。
	ClaimExpiredPendingOrder(ctx context.Context, orderNo string) error

	// CloseExpiredDraftOrder 把一笔孤儿草稿关到 90。
	// 没占下（SAGA 的建单分支刚把它推到 10）返回 ErrOrderNotClaimed。
	CloseExpiredDraftOrder(ctx context.Context, orderNo string) error
}

func (t tenantTx) ListExpiredPendingOrders(ctx context.Context, limit int32) ([]ExpiredOrder, error) {
	rows, err := t.q.ListExpiredPendingOrders(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ExpiredOrder, 0, len(rows))
	for _, r := range rows {
		out = append(out, ExpiredOrder{ID: r.ID, OrderNo: r.OrderNo, StoreID: r.StoreID, UserID: r.UserID})
	}
	return out, nil
}

func (t tenantTx) ListExpiredDraftOrders(ctx context.Context, limit int32) ([]ExpiredOrder, error) {
	rows, err := t.q.ListExpiredDraftOrders(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ExpiredOrder, 0, len(rows))
	for _, r := range rows {
		out = append(out, ExpiredOrder{ID: r.ID, OrderNo: r.OrderNo, StoreID: r.StoreID, UserID: r.UserID})
	}
	return out, nil
}

func (t tenantTx) ClaimExpiredPendingOrder(ctx context.Context, orderNo string) error {
	n, err := t.q.ClaimExpiredPendingOrder(ctx, orderNo)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("order %s: %w（status 已不是 10，或 expire_at 被延长了）",
			orderNo, ErrOrderNotClaimed)
	}
	return nil
}

func (t tenantTx) CloseExpiredDraftOrder(ctx context.Context, orderNo string) error {
	n, err := t.q.CloseExpiredDraftOrder(ctx, orderNo)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("order %s: %w（status 已不是 0）", orderNo, ErrOrderNotClaimed)
	}
	return nil
}

// 编译期确认 db 包的两个 Row 类型确实只有这两列 —— 多一列时上面的转换会漏掉它，
// 而漏掉一列不会有编译错误。这两行在 db/queries 里的 SELECT 改了列时会红。
var (
	_ = db.ListExpiredPendingOrdersRow{ID: 0, OrderNo: ""}
	_ = db.ListExpiredDraftOrdersRow{ID: 0, OrderNo: ""}
)
