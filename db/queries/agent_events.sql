-- AI 员工的事件、游标与 webhook（00121，AI 经营 M10 §3，service/agent_event.go）。一个租户列都没有：租户由 RLS 过滤。

-- name: InsertAgentEvent :one
-- 写一条事件。同一个 dedupe_key 已有 → 不写（返回 0 行，调用方记成「已有」）。
-- ON CONFLICT 不写冲突目标，理由同 jobs.sql 的 EnqueueJob（目标里要提租户列）；这张表上的唯一约束只有
-- 主键（IDENTITY，撞不了）、(id, 租户)（同上）与 uk_agent_events_dedupe，所以两种写法等价。
INSERT INTO agent_events (type, store_id, payload, dedupe_key)
VALUES (sqlc.arg(type), sqlc.narg(store_id), sqlc.arg(payload), sqlc.arg(dedupe_key))
ON CONFLICT DO NOTHING
RETURNING id;

-- name: EmitProposalDecidedEvent :many
-- 一条提案有了结果（已执行 / 已驳回 / 执行失败 / 已过期）→ proposal_decided。在写结果的同一个事务里调。
-- 状态名在这里翻，不在 Go 里：提案的结构体随提案种类在变，这条只认 agent_proposals 的列。
INSERT INTO agent_events (type, store_id, payload, dedupe_key)
SELECT 'proposal_decided', p.store_id,
       jsonb_build_object('proposal_id', p.id, 'kind', p.kind, 'agent_staff_id', p.agent_staff_id,
                          'status', CASE p.status WHEN 20 THEN 'executed' WHEN 30 THEN 'rejected'
                                                  WHEN 40 THEN 'failed' ELSE 'expired' END),
       'proposal_decided:' || p.id
  FROM agent_proposals p
 WHERE p.id = sqlc.arg(proposal_id) AND p.status IN (20, 30, 40, 50)
ON CONFLICT DO NOTHING
RETURNING id;

-- name: EmitExpiredProposalEvents :many
-- 过期扫描之后补事件：最近一天里转成 50 已过期、还没有 proposal_decided 的提案各一条。
-- 「最近一天」只是给扫描范围封顶（过期扫描每 10 分钟一轮，同一事务里刚置的 50 一定在里面）；
-- 已经有事件的由 dedupe_key 挡掉。
INSERT INTO agent_events (type, store_id, payload, dedupe_key)
SELECT 'proposal_decided', p.store_id,
       jsonb_build_object('proposal_id', p.id, 'kind', p.kind, 'agent_staff_id', p.agent_staff_id, 'status', 'expired'),
       'proposal_decided:' || p.id
  FROM agent_proposals p
 WHERE p.status = 50 AND p.updated_at > now() - interval '1 day'
ON CONFLICT DO NOTHING
RETURNING id;

-- name: RecentStockLowEvents :many
-- 最近 24 小时已经发过 stock_low 的（门店，SKU）：扫描跳过它们（每对每 24 小时至多一条）。
SELECT e.store_id::bigint AS store_id, (e.payload->>'sku_id')::bigint AS sku_id
  FROM agent_events e
 WHERE e.type = 'stock_low' AND e.created_at > now() - interval '24 hours';

-- name: SearchZeroSpikes :many
-- 近 1 小时的无结果词（口径同经营报表：去首尾空白、转小写；ranked_ids 为空或 fallback 即无结果，00140），出现 ≥ min_count 次的。
SELECT lower(btrim(l.query))::text AS term, count(*)::bigint AS zero_count
  FROM search_logs l
 WHERE l.created_at >= now() - interval '1 hour'
   AND (COALESCE(cardinality(l.ranked_ids), 0) = 0 OR l.fallback)
   AND btrim(l.query) <> ''
 GROUP BY 1
HAVING count(*) >= sqlc.arg(min_count)::bigint
 ORDER BY 2 DESC, 1
 LIMIT sqlc.arg(row_limit);

-- name: ListAgentEvents :many
-- AI 员工拉事件：after_id 之后、按 id 升序，按它的管辖范围过滤 ——
-- 全店范围的看全部（含 store_id 为空的全店事件）；否则只看 store_ids 里、或 region_ids 里的大区下的门店的事件，
-- store_id 为空的全店事件看不到（NULL = ANY(...) 不成立）。
SELECT e.id, e.type, e.store_id, e.payload, e.created_at
  FROM agent_events e
 WHERE e.id > sqlc.arg(after_id)
   AND (sqlc.arg(merchant_wide)::boolean
        OR e.store_id = ANY(sqlc.arg(store_ids)::bigint[])
        OR e.store_id IN (SELECT s.id FROM stores s WHERE s.region_id = ANY(sqlc.arg(region_ids)::bigint[])))
 ORDER BY e.id
 LIMIT sqlc.arg(row_limit);

-- name: GetAgentEvent :one
SELECT e.id, e.type, e.store_id, e.payload, e.created_at FROM agent_events e WHERE e.id = $1;

-- name: GetAgentEventCursor :one
SELECT c.last_acked_id FROM agent_event_cursors c WHERE c.agent_staff_id = $1;

-- name: AckAgentEvents :one
-- 确认到 up_to_id：游标只进不退（重复、乱序的 ack 不会把它拨回去）。
INSERT INTO agent_event_cursors (agent_staff_id, last_acked_id)
VALUES (sqlc.arg(agent_staff_id), sqlc.arg(up_to_id))
ON CONFLICT ON CONSTRAINT agent_event_cursors_pkey DO UPDATE
   SET last_acked_id = GREATEST(agent_event_cursors.last_acked_id, EXCLUDED.last_acked_id)
RETURNING last_acked_id;

-- name: ListEnabledAgentWebhookIDs :many
-- 事件写入后给哪些 webhook 入投递任务：全部启用的（范围在投递时按 AI 员工当时的身份判）。
SELECT w.id FROM agent_webhooks w WHERE w.enabled ORDER BY w.id;

-- name: GetAgentWebhookByAgent :one
SELECT w.id, w.agent_staff_id, w.url, w.secret, w.enabled, w.created_at, w.updated_at
  FROM agent_webhooks w WHERE w.agent_staff_id = $1;

-- name: GetAgentWebhook :one
SELECT w.id, w.agent_staff_id, w.url, w.secret, w.enabled, w.created_at, w.updated_at
  FROM agent_webhooks w WHERE w.id = $1;

-- name: InsertAgentWebhook :one
INSERT INTO agent_webhooks (agent_staff_id, url, secret, enabled)
VALUES (sqlc.arg(agent_staff_id), sqlc.arg(url), sqlc.arg(secret), sqlc.arg(enabled))
RETURNING id, agent_staff_id, url, secret, enabled, created_at, updated_at;

-- name: UpdateAgentWebhook :one
-- 改地址 / 开关；secret 非空时换成新密钥（rotate_secret）。
UPDATE agent_webhooks
   SET url = sqlc.arg(url), enabled = sqlc.arg(enabled), secret = COALESCE(sqlc.narg(secret), secret)
 WHERE agent_staff_id = sqlc.arg(agent_staff_id)
RETURNING id, agent_staff_id, url, secret, enabled, created_at, updated_at;

-- name: DeleteAgentWebhook :execrows
DELETE FROM agent_webhooks WHERE agent_staff_id = $1;

-- name: InsertAgentWebhookDelivery :exec
INSERT INTO agent_webhook_deliveries (event_id, webhook_id, attempt, status_code, error)
VALUES (sqlc.arg(event_id), sqlc.arg(webhook_id), sqlc.arg(attempt), sqlc.narg(status_code), sqlc.arg(error));

-- name: ListAgentWebhookDeliveries :many
-- 后台看最近几次投递（新的在前）。
SELECT d.id, d.event_id, d.attempt, d.status_code, d.error, d.delivered_at
  FROM agent_webhook_deliveries d
 WHERE d.webhook_id = sqlc.arg(webhook_id)
 ORDER BY d.delivered_at DESC, d.id DESC
 LIMIT sqlc.arg(row_limit);
