-- AI 员工写的经营简报（00092，AI 经营 M9，service/agent_brief.go）。一个 merchant_id 都没有：租户由 RLS 过滤。

-- name: InsertAgentBrief :one
INSERT INTO agent_briefs (agent_staff_id, title, body, period_start, period_end)
VALUES ($1, $2, $3, $4, $5)
RETURNING id;

-- name: GetAgentBrief :one
SELECT b.id, b.agent_staff_id, a.name AS agent_name, b.title, b.body, b.period_start, b.period_end, b.created_at
  FROM agent_briefs b
  JOIN staff a ON a.id = b.agent_staff_id
 WHERE b.id = $1;

-- name: ListAgentBriefs :many
SELECT b.id, b.agent_staff_id, a.name AS agent_name, b.title, b.body, b.period_start, b.period_end, b.created_at
  FROM agent_briefs b
  JOIN staff a ON a.id = b.agent_staff_id
 ORDER BY b.created_at DESC, b.id DESC
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountAgentBriefs :one
SELECT count(*) FROM agent_briefs;
