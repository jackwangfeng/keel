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

// newQuotaSync 建配额同步的发送方：库存的接收分支按部署形态解析（单体 local://，拆分 http://），
// 回查分支按协调器形态解析（嵌入式 local://，独立部署时是 core 自己的内网地址）。
func newQuotaSync(s SplitConfig, pool *pgxpool.Pool, self dtm.BranchResolver) (*service.QuotaSync, error) {
	res := dtm.BranchResolver{}
	if s.Role == RoleCore {
		var err error
		if res, err = dtm.NewBranchResolver(s.InventoryURL, s.InternalSecret); err != nil {
			return nil, err
		}
	}
	return service.NewQuotaSync(repository.New(pool), res, self), nil
}

// QuotaSyncBranches 是配额同步要注册的分支：回查（core），以及库存在进程内时的接收分支。
// src 只给单体：没有载荷的旧消息（00240 之前登记的）还能回源读定义。
func QuotaSyncBranches(q *service.QuotaSync, local *inventory.Local, src inventory.QuotaSource) map[string]dtm.BranchFuncEx {
	out := map[string]dtm.BranchFuncEx{service.BranchPromotionQuotaQuery: dtm.Ex(q.QueryBranch())}
	if local != nil {
		out[inventory.BranchActivitySync] = local.ActivitySyncBranch(src)
	}
	return out
}

func WithQuotaSync(q *service.QuotaSync) RouterOption {
	return func(o *routerOptions) { o.quotaSync = q }
}
