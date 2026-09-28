-- AI 员工的提案（00091，AI 经营 M9，service/agent_proposal.go）。一个 merchant_id 都没有：租户由 RLS 过滤。

-- name: InsertAgentProposal :one
INSERT INTO agent_proposals (agent_staff_id, kind, store_id, sku_id, target_key, payload, title, evidence,
                             expected_impact, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id;

-- name: GetAgentProposal :one
SELECT p.id, p.agent_staff_id, p.kind, p.store_id, p.sku_id, p.payload, p.title, p.evidence, p.expected_impact,
       p.status, p.decided_by, p.decided_at, p.reject_reason, p.result, p.expires_at, p.created_at, p.updated_at,
       p.executed_at, p.outcome, p.outcome_at, p.auto_approved,
       a.name AS agent_name, st.name AS store_name, d.name AS decided_by_name
  FROM agent_proposals p
  JOIN staff a ON a.id = p.agent_staff_id
  LEFT JOIN stores st ON st.id = p.store_id
  LEFT JOIN staff d ON d.id = p.decided_by
 WHERE p.id = $1;

-- name: OpenAgentProposalFor :one
-- 同一个（kind，作用对象）现在那条待处理 / 执行中的提案（重复提案时告诉 agent 是哪一条）。
SELECT id FROM agent_proposals
 WHERE kind = $1 AND target_key = $2 AND status IN (10, 15);

-- name: ListAgentProposals :many
-- 后台列表：按状态（空 = 全部）、门店范围（store_ids 为 NULL = 不收窄；收窄时全店类提案一律不出现 ——
-- 门店范围的人批不了它们）、种类筛；待处理的在前，新的在前。
SELECT p.id, p.agent_staff_id, p.kind, p.store_id, p.sku_id, p.payload, p.title, p.evidence, p.expected_impact,
       p.status, p.decided_by, p.decided_at, p.reject_reason, p.result, p.expires_at, p.created_at, p.updated_at,
       p.executed_at, p.outcome, p.outcome_at, p.auto_approved,
       a.name AS agent_name, st.name AS store_name, d.name AS decided_by_name
  FROM agent_proposals p
  JOIN staff a ON a.id = p.agent_staff_id
  LEFT JOIN stores st ON st.id = p.store_id
  LEFT JOIN staff d ON d.id = p.decided_by
 WHERE (sqlc.narg(status)::smallint IS NULL OR p.status = sqlc.narg(status)::smallint)
   AND (sqlc.narg(agent_staff_id)::bigint IS NULL OR p.agent_staff_id = sqlc.narg(agent_staff_id)::bigint)
   AND (sqlc.narg(store_ids)::bigint[] IS NULL OR p.store_id = ANY(sqlc.narg(store_ids)::bigint[]))
   AND (sqlc.narg(kind)::text IS NULL OR p.kind = sqlc.narg(kind)::text)
 ORDER BY (p.status IN (10, 15)) DESC, p.created_at DESC, p.id DESC
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountAgentProposals :one
SELECT count(*) FROM agent_proposals p
 WHERE (sqlc.narg(status)::smallint IS NULL OR p.status = sqlc.narg(status)::smallint)
   AND (sqlc.narg(agent_staff_id)::bigint IS NULL OR p.agent_staff_id = sqlc.narg(agent_staff_id)::bigint)
   AND (sqlc.narg(store_ids)::bigint[] IS NULL OR p.store_id = ANY(sqlc.narg(store_ids)::bigint[]))
   AND (sqlc.narg(kind)::text IS NULL OR p.kind = sqlc.narg(kind)::text);

-- name: ClaimAgentProposal :one
-- 批准：10 待处理（或 15 执行中 —— 上次执行结果没写回，重来）→ 15，记下批准的人。没过期才行。
-- 条件 UPDATE：两个人同时点批准只有一个拿到行。
UPDATE agent_proposals
   SET status = 15, decided_by = sqlc.arg(decided_by), decided_at = now()
 WHERE id = sqlc.arg(id) AND status IN (10, 15) AND expires_at > now()
RETURNING id;

-- name: FinishAgentProposal :execrows
-- 执行结果写回：15 → 20 已执行 / 40 执行失败。
UPDATE agent_proposals SET status = sqlc.arg(status), result = sqlc.arg(result)
 WHERE id = sqlc.arg(id) AND status = 15;

-- name: RejectAgentProposal :execrows
UPDATE agent_proposals
   SET status = 30, decided_by = sqlc.arg(decided_by), decided_at = now(), reject_reason = sqlc.arg(reason)
 WHERE id = sqlc.arg(id) AND status = 10;

-- name: ExpireAgentProposals :execrows
-- 过期：待处理超过 expires_at → 50。执行中的不动（它们在等结果写回）。
UPDATE agent_proposals SET status = 50 WHERE status = 10 AND expires_at <= now();
