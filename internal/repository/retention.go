package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 保留期清理（service/retention.go）在 repository 边界上的那一面。
//
// 跨租户怎么做：与超时关单、孤儿回收同一套（sweep.go 文件头）—— 先读商家清单，再逐家进
// 租户事务，每条 DELETE 只删 RLS 放行的那一家的行。不需要平台角色，也不需要任何能绕过 RLS
// 的凭据。
//
// 每个方法**自己开一个事务、只删一批**：批与批之间提交，锁与死元组都有上界
// （删法见 db/queries/retention.sql 的文件头）。循环与节流在 service 层。

// AllMerchantIDs 返回**全部**商家的 id（含停用与软删），按 id 升序。
//
// 与 ActiveMerchants 不同：保留期对停用的店同样成立 —— 一家停用的店的检索日志、工具调用
// 记录照样该到期删掉；只扫活跃商家的话，这些行会永远留着。merchants 是 tenant-root，
// 没有 RLS（ActiveMerchants 的注释），这条查询不需要租户。
func (r *Repo) AllMerchantIDs(ctx context.Context) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `SELECT id FROM merchants ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("读商家清单失败: %w", err)
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

// purgeInTenant 在 ctx 里那家店的租户事务里跑一条分批删除，返回删掉的行数。
func (r *Repo) purgeInTenant(ctx context.Context, fn func(*db.Queries) (int64, error)) (int64, error) {
	var n int64
	err := r.withTenantTx(ctx, func(tx pgx.Tx, _ Tx) error {
		var err error
		n, err = fn(db.New(tx))
		return err
	})
	return n, err
}

// PurgeExpiredIdempotencyKeys 删 ctx 那家店 expire_at 早于 before 的幂等存档，至多 batch 行。
func (r *Repo) PurgeExpiredIdempotencyKeys(ctx context.Context, before time.Time, batch int32) (int64, error) {
	return r.purgeInTenant(ctx, func(q *db.Queries) (int64, error) {
		return q.PurgeExpiredIdempotencyKeys(ctx, db.PurgeExpiredIdempotencyKeysParams{Before: ts(before), Batch: batch})
	})
}

// PurgeExpiredPlatformIdempotencyKeys 删平台作用域那一抽屉（merchant_id IS NULL，00028）里
// 过期的幂等存档。同一条语句，作用域换成平台：idempotency_keys 的策略是
// IS NOT DISTINCT FROM staff_scope_merchant()，平台作用域里只看得见 NULL 那一抽屉。
// set_config 必须是事务级的，理由见 WithPlatform。
func (r *Repo) PurgeExpiredPlatformIdempotencyKeys(ctx context.Context, before time.Time, batch int32) (int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_scope', 'on', true)`); err != nil {
		return 0, err
	}
	n, err := db.New(tx).PurgeExpiredIdempotencyKeys(ctx,
		db.PurgeExpiredIdempotencyKeysParams{Before: ts(before), Batch: batch})
	if err != nil {
		return 0, err
	}
	return n, tx.Commit(ctx)
}

// PurgeSearchLogs 删 ctx 那家店 created_at 早于 before 的检索日志，至多 batch 行。
func (r *Repo) PurgeSearchLogs(ctx context.Context, before time.Time, batch int32) (int64, error) {
	return r.purgeInTenant(ctx, func(q *db.Queries) (int64, error) {
		return q.PurgeSearchLogsBefore(ctx, db.PurgeSearchLogsBeforeParams{Before: ts(before), Batch: batch})
	})
}

// PurgeAgentToolCalls 删 ctx 那家店 created_at 早于 before 的 AI 工具调用记录，至多 batch 行。
func (r *Repo) PurgeAgentToolCalls(ctx context.Context, before time.Time, batch int32) (int64, error) {
	return r.purgeInTenant(ctx, func(q *db.Queries) (int64, error) {
		return q.PurgeAgentToolCallsBefore(ctx, db.PurgeAgentToolCallsBeforeParams{Before: ts(before), Batch: batch})
	})
}

// PurgeInventoryLogs 删 ctx 那家店 created_at 早于 before 的库存流水，至多 batch 行。
//
// 挂在 InventoryStore 上而不是 Repo：库存流水在库存池指向的库里（拆分部署下是另一个库）。
// 删了哪些流水会影响什么，见 db/queries/inventory_svc.sql 的 InvPurgeLogsBefore。
func (s *InventoryStore) PurgeInventoryLogs(ctx context.Context, before time.Time, batch int32) (int64, error) {
	return s.r.purgeInTenant(ctx, func(q *db.Queries) (int64, error) {
		return q.InvPurgeLogsBefore(ctx, db.InvPurgeLogsBeforeParams{Before: ts(before), Batch: batch})
	})
}
