-- 提案复盘（00122，service/agent_proposal_outcome.go）与成绩单。一个 merchant_id 都没有：租户由 RLS 过滤。
-- 「卖出」与补货计算同一个口径（restock.sql 的 StoreSKUSales）：已支付 / 已发货 / 已完成 / 售后中的单都算。

-- name: SetAgentProposalExecuted :exec
UPDATE agent_proposals
   SET executed_at = now(), outcome_due_at = sqlc.narg(outcome_due_at), outcome = sqlc.narg(outcome),
       outcome_at = CASE WHEN sqlc.narg(outcome)::jsonb IS NULL THEN NULL ELSE now() END
 WHERE id = sqlc.arg(id) AND status = 20;

-- name: DueAgentProposalOutcomes :many
SELECT id, kind, store_id, sku_id, payload, executed_at
  FROM agent_proposals
 WHERE status = 20 AND outcome_at IS NULL AND outcome_due_at IS NOT NULL AND outcome_due_at <= now()
 ORDER BY outcome_due_at
 LIMIT 100;

-- name: SaveAgentProposalOutcome :exec
UPDATE agent_proposals SET outcome = sqlc.arg(outcome), outcome_at = now()
 WHERE id = sqlc.arg(id) AND status = 20 AND outcome_at IS NULL;

-- name: DeferAgentProposalOutcome :exec
-- 到点了但统计窗口还没走完（outcome_due_at 被提前了）：推迟到窗口关闭，不算半截数据。
UPDATE agent_proposals SET outcome_due_at = sqlc.arg(due_at)
 WHERE id = sqlc.arg(id) AND status = 20 AND outcome_at IS NULL;

-- name: SKUUnitsSoldBetween :one
-- 一组 SKU 在 [from, to) 内卖出的件数与金额（分）。store_id 为空 = 全部门店。
SELECT COALESCE(sum(oi.quantity), 0)::bigint AS qty,
       COALESCE(sum(oi.amount_cents - oi.discount_cents), 0)::bigint AS amount_cents
  FROM order_items oi
  JOIN orders o ON o.id = oi.order_id
 WHERE oi.sku_id = ANY(sqlc.arg(sku_ids)::bigint[])
   AND (sqlc.narg(store_id)::bigint IS NULL OR o.store_id = sqlc.narg(store_id)::bigint)
   AND o.status IN (20, 30, 40, 50)
   AND o.paid_at >= sqlc.arg(from_at) AND o.paid_at < sqlc.arg(to_at);

-- name: ProductUnitsSoldBetween :one
SELECT COALESCE(sum(oi.quantity), 0)::bigint AS qty
  FROM order_items oi
  JOIN orders o ON o.id = oi.order_id
  JOIN skus s ON s.id = oi.sku_id
 WHERE s.product_id = sqlc.arg(product_id)
   AND o.status IN (20, 30, 40, 50)
   AND o.paid_at >= sqlc.arg(from_at) AND o.paid_at < sqlc.arg(to_at);

-- name: CouponTemplateUsage :one
-- 一张券模板：领了多少、用了多少（status 3 已使用；2 锁定是下单进行中，也算用了；4 过期不算）。
SELECT count(*)::bigint AS claimed,
       count(*) FILTER (WHERE uc.status IN (2, 3))::bigint AS used
  FROM user_coupons uc
 WHERE uc.template_id = $1;

-- name: AgentScorecardByKind :many
-- 成绩单：这名 AI 员工近 since 以来按种类的提案数、各状态数、verdict 分布。
SELECT p.kind,
       count(*)::bigint                                                  AS proposed,
       count(*) FILTER (WHERE p.status IN (20, 40))::bigint               AS approved,
       count(*) FILTER (WHERE p.status = 20)::bigint                      AS executed,
       count(*) FILTER (WHERE p.status = 40)::bigint                      AS failed,
       count(*) FILTER (WHERE p.status = 30)::bigint                      AS rejected,
       count(*) FILTER (WHERE p.status = 50)::bigint                      AS expired,
       count(*) FILTER (WHERE p.status IN (10, 15))::bigint               AS open,
       count(*) FILTER (WHERE p.outcome->>'verdict' = 'positive')::bigint AS positive,
       count(*) FILTER (WHERE p.outcome->>'verdict' = 'neutral')::bigint  AS neutral,
       count(*) FILTER (WHERE p.outcome->>'verdict' = 'negative')::bigint AS negative
  FROM agent_proposals p
 WHERE p.agent_staff_id = sqlc.arg(agent_staff_id) AND p.created_at >= sqlc.arg(since)
 GROUP BY p.kind
 ORDER BY p.kind;

-- name: AgentRecentOutcomes :many
-- 成绩单的明细：近 since 以来已经量过效果的提案，新的在前。
SELECT p.id, p.kind, p.title, p.outcome, p.outcome_at
  FROM agent_proposals p
 WHERE p.agent_staff_id = sqlc.arg(agent_staff_id) AND p.outcome_at IS NOT NULL AND p.outcome_at >= sqlc.arg(since)
 ORDER BY p.outcome_at DESC
 LIMIT 50;

-- ===========================================================================
-- 自动执行策略（00130）
-- ===========================================================================

-- name: ListAgentAutoPolicies :many
SELECT agent_staff_id, kind, enabled, max_units, min_discount_rate, max_discount_cents, daily_limit, updated_by, updated_at
  FROM agent_auto_policies
 WHERE agent_staff_id = $1
 ORDER BY kind;

-- name: GetAgentAutoPolicy :one
SELECT agent_staff_id, kind, enabled, max_units, min_discount_rate, max_discount_cents, daily_limit, updated_by, updated_at
  FROM agent_auto_policies
 WHERE agent_staff_id = $1 AND kind = $2;

-- name: LockAgentAutoPolicy :one
-- 自动执行判定用：锁住这一行，让同一个 AI 员工 × 种类的并发判定排队 —— 「数 24 小时内已自动执行几条
-- → 认领」在锁之下做，后到的那次读得到先到的那次已提交的认领（READ COMMITTED 每条语句一个新快照）。
-- 2026-09-28 破坏性测试：12 路并发提案、daily_limit=2，自动执行了 4 条。
SELECT agent_staff_id, kind, enabled, max_units, min_discount_rate, max_discount_cents, daily_limit, updated_by, updated_at
  FROM agent_auto_policies
 WHERE agent_staff_id = $1 AND kind = $2
   FOR UPDATE;

-- name: UpsertAgentAutoPolicy :exec
INSERT INTO agent_auto_policies (agent_staff_id, kind, enabled, max_units, min_discount_rate, max_discount_cents,
                                 daily_limit, updated_by)
VALUES (sqlc.arg(agent_staff_id), sqlc.arg(kind), sqlc.arg(enabled), sqlc.arg(max_units), sqlc.arg(min_discount_rate),
        sqlc.arg(max_discount_cents), sqlc.arg(daily_limit), sqlc.arg(updated_by))
ON CONFLICT ON CONSTRAINT agent_auto_policies_pkey DO UPDATE
   SET enabled = EXCLUDED.enabled, max_units = EXCLUDED.max_units, min_discount_rate = EXCLUDED.min_discount_rate,
       max_discount_cents = EXCLUDED.max_discount_cents, daily_limit = EXCLUDED.daily_limit,
       updated_by = EXCLUDED.updated_by;

-- name: CountAutoApprovedSince :one
SELECT count(*) FROM agent_proposals
 WHERE agent_staff_id = $1 AND kind = $2 AND auto_approved AND decided_at >= $3;

-- name: ClaimAgentProposalAuto :one
-- 按策略自动执行：10 → 15，decided_by 为空、auto_approved = true。
UPDATE agent_proposals
   SET status = 15, decided_at = now(), auto_approved = TRUE
 WHERE id = $1 AND status = 10 AND expires_at > now()
RETURNING id;

-- ===========================================================================
-- 公开的 AI 经营日志（00132）
-- ===========================================================================

-- name: GetPublicAILog :one
SELECT public_ai_log FROM shop_preferences LIMIT 1;

-- name: SetPublicAILog :exec
-- 没有 shop_preferences 那一行时建一行（其余列取默认值）。
INSERT INTO shop_preferences (public_ai_log) VALUES (sqlc.arg(enabled))
ON CONFLICT ON CONSTRAINT shop_preferences_pkey DO UPDATE SET public_ai_log = EXCLUDED.public_ai_log;

-- name: PublicAILogProposals :many
-- 最近的提案（全部 AI 员工）：公开页只给种类、标题、状态、是否自动执行、复盘结论与时间 —— 不给证据全文与执行参数。
SELECT p.id, p.kind, p.title, p.status, p.auto_approved, COALESCE(p.outcome->>'verdict', '')::text AS verdict, p.created_at, p.decided_at,
       a.name AS agent_name
  FROM agent_proposals p
  JOIN staff a ON a.id = p.agent_staff_id
 ORDER BY p.created_at DESC
 LIMIT 30;

-- name: PublicAILogSummary :one
-- 近 30 天的总数。
SELECT count(*)::bigint                                                   AS proposed,
       count(*) FILTER (WHERE status = 20)::bigint                        AS executed,
       count(*) FILTER (WHERE status = 20 AND auto_approved)::bigint      AS auto_executed,
       count(*) FILTER (WHERE status = 30)::bigint                        AS rejected,
       count(*) FILTER (WHERE outcome->>'verdict' = 'positive')::bigint   AS positive,
       count(*) FILTER (WHERE outcome->>'verdict' = 'negative')::bigint   AS negative
  FROM agent_proposals
 WHERE created_at >= now() - interval '30 days';
