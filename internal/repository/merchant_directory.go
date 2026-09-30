package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/tenant"
)

// 商家目录：列表、详情、改名与停用 / 启用（契约 GET/PATCH /admin/merchants*）。
//
// ===========================================================================
// 写入不碰 merchants 那一行
// ===========================================================================
//
// keel_app 在 merchants 上只有 SELECT + INSERT（00005 收窄、00021 还回 INSERT），
// **没有 UPDATE**——给了就等于任何租户上下文都能改商家目录（00005 实测过的
// 「租户 1 的上下文里把租户 2 停用」）。改名与停用改为在**平台作用域**里追加一行
// merchant_revisions（00024），而那张表的 INSERT 策略是 WITH CHECK (platform_scope())：
// 租户作用域里的事务插不进来，数据库层就是 42501。
//
// 这条路与开店（WithNewTenant）是同一个形状：平台作用域 + 一条 GRANT 面上的
// INSERT。差别只在 INSERT 进哪张表——开店插 merchants 本身（碰不到已有行），
// 这里插修订（碰的是「当前状态」，所以要 RLS 再挡一道）。
//
// ===========================================================================
// 为什么是裸 SQL 而不是 sqlc
// ===========================================================================
//
// 「当前状态」要和租户解析层读同一份文本（tenant.EffectiveMerchantFrom）：
// 解析层判一家店停没停用、这里显示它停没停用，两处写得不一样的后果是
// 「后台显示已停用，买家照样打得开」。解析层与 ActiveMerchants 都是裸 SQL
// （它们发生在还没有租户的时候），这里跟它们共用常量，而不是在 sqlc 的 .sql 里
// 再抄一份 LATERAL。
//
// 这些查询里出现的 merchant_id 是**目录的连接键**（修订属于哪家店、
// shop_settings 属于哪家店），不是应用层的租户过滤——scripts/check_query_tenancy.py
// 挡的那件事（在本该由 RLS 过滤的查询里再写一遍 merchant_id）在这里不存在：
// 商家目录本来就是跨租户的，平台级操作员看的就是全部。

// ErrMerchantNotFound：商家不存在或已软删（契约里那个 404）。
var ErrMerchantNotFound = errors.New("商家不存在")

// merchantSelect 是目录读接口共用的列清单。
const merchantSelect = `
	SELECT m.id, m.code, ` + tenant.EffectiveName + `, ` + tenant.EffectiveStatus + `,
	       m.created_at, s.domain, rev.created_at
	  FROM ` + tenant.EffectiveMerchantFrom + `
	  LEFT JOIN shop_settings s ON s.merchant_id = m.id`

func scanMerchant(row pgx.Row) (Merchant, error) {
	var m Merchant
	var revised *time.Time
	if err := row.Scan(&m.ID, &m.Code, &m.Name, &m.Status, &m.CreatedAt, &m.Domain, &revised); err != nil {
		return Merchant{}, err
	}
	m.RevisedAt = revised
	return m, nil
}

// ListMerchants 返回未软删的全部商家，**含停用与待审核的**，按 id 升序。
//
// 含停用的是契约要求：平台要看得见停掉的店，才有入口把它启用回来。
func (r *Repo) ListMerchants(ctx context.Context, limit, offset int64) ([]Merchant, int64, error) {
	var total int64
	if err := r.poolFor(ctx, "ListMerchants").QueryRow(ctx,
		`SELECT count(*) FROM merchants WHERE deleted_at IS NULL`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计商家失败: %w", err)
	}
	rows, err := r.poolFor(ctx, "ListMerchants").Query(ctx,
		merchantSelect+` WHERE m.deleted_at IS NULL ORDER BY m.id LIMIT $1 OFFSET $2`,
		limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("读商家列表失败: %w", err)
	}
	defer rows.Close()
	var out []Merchant
	for rows.Next() {
		m, err := scanMerchant(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, m)
	}
	return out, total, rows.Err()
}

// GetMerchant 取一家未软删的商家（含停用）。查不到返回 ErrMerchantNotFound。
func (r *Repo) GetMerchant(ctx context.Context, id int64) (Merchant, error) {
	m, err := scanMerchant(r.poolFor(ctx, "GetMerchant").QueryRow(ctx,
		merchantSelect+` WHERE m.deleted_at IS NULL AND m.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Merchant{}, ErrMerchantNotFound
	}
	return m, err
}

// ReviseMerchant 改名和 / 或改状态：在平台作用域里追加一行完整快照。
//
// name / status 都是「给了就改」，没给的沿用当前值——所以要先读当前值再写，
// 而两个并发的修改（一个改名、一个停用）各读各的就会互相覆盖掉对方那一半。
// 行锁拿不到（SELECT ... FOR UPDATE 要 UPDATE 权限，而那正是这张表刻意没给的），
// 用一把按商家 id 的事务级 advisory lock 把同一家店的修改排成队：
// 它不需要任何表权限，事务结束自动释放。
//
// changedBy 是调用者的 staff.id（平台级操作员），审计用。
func (r *Repo) ReviseMerchant(ctx context.Context, id int64, name *string, status *int16,
	changedBy int64) (Merchant, error) {

	tx, err := r.poolFor(ctx, "ReviseMerchant").Begin(ctx)
	if err != nil {
		return Merchant{}, err
	}
	defer tx.Rollback(ctx)

	// 平台作用域：merchant_revisions 的 INSERT 策略只在这里放行。
	// is_local = true，理由见 WithPlatform。
	if _, err := tx.Exec(ctx,
		`SELECT set_config('app.platform_scope', 'on', true)`); err != nil {
		return Merchant{}, err
	}
	// 第一个参数是一个固定的命名空间，免得和别处的 advisory lock 撞 key。
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('merchant_revisions'), $1::int)`, id); err != nil {
		return Merchant{}, err
	}

	cur, err := scanMerchant(tx.QueryRow(ctx,
		merchantSelect+` WHERE m.deleted_at IS NULL AND m.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Merchant{}, ErrMerchantNotFound
	}
	if err != nil {
		return Merchant{}, err
	}

	newName, newStatus := cur.Name, cur.Status
	if name != nil {
		newName = *name
	}
	if status != nil {
		newStatus = *status
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO merchant_revisions (merchant_id, name, status, changed_by)
		 VALUES ($1, $2, $3, $4)`, id, newName, newStatus, changedBy); err != nil {
		return Merchant{}, err
	}

	out, err := scanMerchant(tx.QueryRow(ctx,
		merchantSelect+` WHERE m.deleted_at IS NULL AND m.id = $1`, id))
	if err != nil {
		return Merchant{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Merchant{}, err
	}
	return out, nil
}
