-- AI 员工与接入密钥（00090，AI 经营 M9，docs/AI经营-M9设计.md §2）。
--
-- AI 员工是 kind = 2 的 staff。人的员工查询（staff.sql）一律只认 kind = 1，这里一律只认 kind = 2 ——
-- 两条路互不相通：/admin/staff 改不了 AI 员工（更改不成管理员），AI 员工也换不出后台会话。
-- 和这个目录里别的文件一样，一个 merchant_id 都没有：租户由 RLS 过滤。

-- name: CreateAgentStaff :one
-- email 是合成的（staff.email NOT NULL）：agent-<随机>@agent.keel.invalid。.invalid 顶级域保证不可投递，
-- 于是「邮件登录链接」这条找回通道对 AI 员工天然不通（查令牌那条也只认 kind = 1，这是第二道）。
INSERT INTO staff (email, name, role, kind, created_by)
VALUES ($1, $2, $3, 2, $4)
RETURNING id, name, role, status, created_at;

-- name: GetAgent :one
SELECT id, name, role, status, created_at
  FROM staff
 WHERE id = $1 AND kind = 2 AND deleted_at IS NULL;

-- name: ListAgents :many
SELECT s.id, s.name, s.role, s.status, s.created_at,
       (SELECT count(*) FROM agent_keys k
         WHERE k.staff_id = s.id AND k.revoked_at IS NULL
           AND (k.expires_at IS NULL OR k.expires_at > now()))::bigint AS live_keys,
       (SELECT max(k.last_used_at) FROM agent_keys k WHERE k.staff_id = s.id)::timestamptz AS last_used_at
  FROM staff s
 WHERE s.kind = 2 AND s.deleted_at IS NULL
 ORDER BY s.id;

-- name: UpdateAgent :one
UPDATE staff
   SET name   = coalesce(sqlc.narg('name'),   name),
       role   = coalesce(sqlc.narg('role'),   role),
       status = coalesce(sqlc.narg('status'), status)
 WHERE id = sqlc.arg('id') AND kind = 2 AND deleted_at IS NULL
RETURNING id, name, role, status, created_at;

-- name: CreateAgentKey :one
INSERT INTO agent_keys (staff_id, name, prefix, secret_hash, expires_at, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, staff_id, name, prefix, expires_at, revoked_at, last_used_at, created_at;

-- name: ListAgentKeys :many
SELECT id, staff_id, name, prefix, expires_at, revoked_at, last_used_at, created_at
  FROM agent_keys
 WHERE staff_id = $1
 ORDER BY id DESC;

-- name: RevokeAgentKey :execrows
UPDATE agent_keys SET revoked_at = now()
 WHERE id = $1 AND staff_id = $2 AND revoked_at IS NULL;

-- name: LoadAgentKey :one
-- 接入密钥 → AI 员工。只认：没吊销、没过期、员工是 AI、没软删。员工停用（status = 2）照样返回，
-- 由中间件回 403 account-disabled（与人的会话同一个口径：停用和「密钥不对」是两件事）。
SELECT k.id AS key_id, s.id AS staff_id, s.role, s.status
  FROM agent_keys k
  JOIN staff s ON s.id = k.staff_id
 WHERE k.secret_hash = $1
   AND k.revoked_at IS NULL
   AND (k.expires_at IS NULL OR k.expires_at > now())
   AND s.kind = 2
   AND s.deleted_at IS NULL;

-- name: TouchAgentKey :exec
-- last_used_at 节流：同一把密钥一分钟最多写一次，免得每次工具调用都写一行。
UPDATE agent_keys SET last_used_at = now()
 WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: InsertAgentToolCall :exec
-- 工具调用审计（00093）。key_id 不带外键：密钥删不掉（只吊销），但审计不该因为将来的清理策略卡住。
INSERT INTO agent_tool_calls (agent_staff_id, key_id, tool, args, ok, error_type, duration_ms)
VALUES ($1, $2, $3, $4, $5, $6, $7);
