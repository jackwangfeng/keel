package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/keel/keel/internal/tenant"
)

// 商家目录：列表、详情、改名 / 停用 / 启用与自有域名登记
// （契约 GET/PATCH /admin/merchants*）。
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
// 自有域名同理，但落在另一张表：merchant_domains（00340）。它不是追加式日志而是
// **当前态**，一家店一行、没有那一行就是没登记，而 keel_app 在它上面没有 UPDATE——
// 换绑 = 同一个事务里先删后插，摘掉 = 删掉那一行。两条写策略同样钉在
// platform_scope()。为什么非要是当前态而不是再来一份日志：域名上的全表 UNIQUE
// 是这条接口的安全核心（两家店绑同一个域名就是真的互相劫持），而日志里
// 「哪家店当前用着这个域名」是派生值，唯一索引表达不了。论证在 00340 的文件头。
//
// 这三条路（开店、追加修订、登记域名）是同一个形状：平台作用域 + 数据库层的写入闸门。
// 差别只在插进哪张表——开店插 merchants 本身（碰不到已有行），这里插修订与换域名，
// 碰的是「当前状态」，所以要 RLS 再挡一道。
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
// 域名登记在哪家店名下），不是应用层的租户过滤——scripts/check_query_tenancy.py
// 挡的那件事（在本该由 RLS 过滤的查询里再写一遍 merchant_id）在这里不存在：
// 商家目录本来就是跨租户的，平台级操作员看的就是全部。

// ErrMerchantNotFound：商家不存在或已软删（契约里那个 404）。
var ErrMerchantNotFound = errors.New("商家不存在")

// ErrMerchantDomainTaken：这个自有域名已经被另一家店登记了。
//
// 它是 merchant_domains.domain 上那个**全表** UNIQUE 撞出来的（00340 从
// shop_settings 搬过来的那一条），不是这里先查后写的判断结果 —— 先查后写在两家店
// 同时登记同一个域名时两边都会通过检查，而数据库只让一边提交。
// 把它翻成 409 而不是让约束冲突冒 500：调用方要的是「这个域名归别家用了」，
// 与 merchant-code-taken 同一类。
var ErrMerchantDomainTaken = errors.New("这个域名已被登记")

// MerchantEdit 是 PATCH /admin/merchants/{id} 那一个补丁。
//
// 三个「改不改」的形状刻意不一样：
//   - name / status 是指针：没传 = 不动。
//   - domain 是**指针 + 一个 bool**：因为「不传」与「显式传 null」在这条接口上是
//     两件事（清空它是一个功能 —— 域名转让给别人之前得能先摘下来），
//     而一个 *string 把这两者折叠成同一个 nil。判别不在结构体里做，由 handler 读
//     请求体的键集合做掉（internal/handler/admin_catalog.go 的 bindPatchBody：
//     null 进指针一律是 nil，*json.RawMessage 也一样），
//     到这里只剩「要不要写、写什么」两种明确意图。
type MerchantEdit struct {
	Name        *string
	Status      *int16
	Domain      *string
	ClearDomain bool
}

// merchantSelect 是目录读接口共用的列清单。domain 走 LEFT JOIN：一家店没有登记
// 就是没有那一行，读出来是 NULL 而不是「查不到这家店」。
const merchantSelect = `
	SELECT m.id, m.code, ` + tenant.EffectiveName + `, ` + tenant.EffectiveStatus + `,
	       m.created_at, d.domain, rev.created_at
	  FROM ` + tenant.EffectiveMerchantFrom + `
	  LEFT JOIN merchant_domains d ON d.merchant_id = m.id`

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

// ReviseMerchant 改名、改状态，和 / 或改自有域名：在平台作用域里追加一行完整快照，
// 域名换绑写的是 merchant_domains。
//
// name / status 都是「给了就改」，没给的沿用当前值——所以要先读当前值再写，
// 而两个并发的修改（一个改名、一个停用）各读各的就会互相覆盖掉对方那一半。
// 行锁拿不到（SELECT ... FOR UPDATE 要 UPDATE 权限，而那正是这张表刻意没给的），
// 用一把按商家 id 的事务级 advisory lock 把同一家店的修改排成队：
// 它不需要任何表权限，事务结束自动释放。
//
// **域名是另一张表，但排在同一个事务、同一把锁里面。** 两个理由：
//
//   - 一次 PATCH 同时带 name 与 domain 时，不能出现「名字改了、域名没改」这种
//     中间态被读到（而 domain 改坏了的后果是这家店的入口没了）。
//   - 约束冲突（ErrMerchantDomainTaken）必须把整笔回滚掉，只留一句「域名归别家」；
//     分开提交的话，改完名再撞域名，运维看到的是半份成功。
//
// merchant_domains 挂着 RLS（读放开、两条写策略钉在 platform_scope()），所以这里
// 的 WHERE merchant_id 不是应用层的租户过滤，而是「改哪一家店」这个**参数本身**：
// 它由路径参数进来、由平台级管理员指名。上面那段文件头把这个区别说清了。
//
// changedBy 是调用者的 staff.id（平台级操作员），审计用。
func (r *Repo) ReviseMerchant(ctx context.Context, id int64, in MerchantEdit,
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

	if err := applyDomainEdit(ctx, tx, id, changedBy, in); err != nil {
		return Merchant{}, err
	}

	newName, newStatus := cur.Name, cur.Status
	if in.Name != nil {
		newName = *in.Name
	}
	if in.Status != nil {
		newStatus = *in.Status
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

// applyDomainEdit 换绑或摘掉这家店的自有域名。
//
// in.Domain == nil 且 !in.ClearDomain 时一个字节都不碰。两者要分开判是刻意的：
// 「不传」与「显式传 null」在这条接口上是两件事（清空它是一个功能 —— 域名转让给
// 别家之前得能先摘下来），判据是请求体里**这个键出现过没有**，由 handler 的
// bindPatchBody 读出来（internal/handler/admin_merchant.go）。
//
// 表上没有 UPDATE 权限，也不该有：可写的只有 domain 一列，而「插一条新的 + 删一条旧的」
// 与它是同一件事，多给一权换不到任何东西（00340 的文件头）。所以两种意图分别是
// 一条 DELETE 与一对 DELETE + INSERT，全在调用方给的那个事务里。
//
// 换绑先删后插：**同一家店**重登记同一个域名也是这一条路径，删掉了自己那一行，
// 于是全表 UNIQUE 不会把自己撞成 409。
func applyDomainEdit(ctx context.Context, tx pgx.Tx, id, changedBy int64, in MerchantEdit) error {
	if in.Domain == nil && !in.ClearDomain {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM merchant_domains WHERE merchant_id = $1`, id); err != nil {
		return domainEditErr(err, id, in.Domain)
	}
	if in.Domain == nil {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO merchant_domains (merchant_id, domain, changed_by) VALUES ($1, $2, $3)`,
		id, *in.Domain, changedBy); err != nil {
		return domainEditErr(err, id, in.Domain)
	}
	return nil
}

// domainEditErr 只把「这个域名归别家用了」那一种约束冲突分出来，其余原样上浮。
//
// 判据取自 pgx 的 pgconn.PgError.Code，而不是比对错误文案。约束名写死成
// merchant_domains_domain_key：将来有人重命名它，这里会退回「原样上浮 → 500」，
// 那比悄悄把别的 23505（比如主键冲突）认成「域名已被占用」诚实。
func domainEditErr(err error, id int64, domain *string) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" && domain != nil {
		return fmt.Errorf("%w: %s", ErrMerchantDomainTaken, *domain)
	}
	return fmt.Errorf("写商家 %d 的自有域名失败: %w", id, err)
}
