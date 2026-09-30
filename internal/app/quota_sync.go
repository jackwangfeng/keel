package app

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 活动配额同步（二阶段消息，service/promotion_quota_msg.go、inventory 包 activity_msg.go）在 Run 里的装配。
// 与 stock_flags.go 里的跨 0 通知方向相反：发送方是 core（写活动的事务），接收方是库存服务。

// newQuotaSync 建 core 这一侧的发送方。接收分支的地址与下单 SAGA 的库存分支同一个解析器：
// all 是 local://inventory_activity_sync，core 是 <KEEL_INVENTORY_URL>/internal/v1/saga/…（validate 已查过地址）。
func newQuotaSync(s SplitConfig, pool *pgxpool.Pool) (*service.QuotaSync, error) {
	res := dtm.BranchResolver{}
	if s.Role == RoleCore {
		var err error
		if res, err = dtm.NewBranchResolver(s.InventoryURL, s.InternalSecret); err != nil {
			return nil, err
		}
	}
	return service.NewQuotaSync(repository.New(pool), res), nil
}

// QuotaSyncBranches 是配额同步要注册到 core 协调器上的分支：回查（永远在 core）；local 不为 nil（单体）时
// 再加上接收分支 —— 库存在进程内，src 是进程内的回源实现。导出给测试：handler 包的协调器照 Run 的样子注册。
func QuotaSyncBranches(q *service.QuotaSync, local *inventory.Local, src inventory.QuotaSource) map[string]dtm.BranchFunc {
	out := map[string]dtm.BranchFunc{service.BranchPromotionQuotaQuery: q.QueryBranch()}
	if local != nil {
		out[inventory.BranchActivitySync] = local.ActivitySyncBranch(src)
	}
	return out
}

// WithQuotaSync 让后台改活动走二阶段消息同步配额。不给时（部分测试）退回直接调用库存服务。
func WithQuotaSync(q *service.QuotaSync) RouterOption {
	return func(o *routerOptions) { o.quotaSync = q }
}
