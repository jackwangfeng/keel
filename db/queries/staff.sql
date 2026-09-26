-- 后台身份：staff 与 staff_tokens（数据模型 §14）。
--
-- 全文没有一处 WHERE 写租户，理由与 db/queries/users.sql 一字不差：租户由 RLS
-- 在数据库层过滤，应用层再加一遍条件之后「RLS 到底有没有生效」就再也测不出来了。
--
-- 这张表上这条规矩尤其不能破例，因为它的策略不是标准的列比较：
-- `merchant_id IS NOT DISTINCT FROM staff_scope_merchant()`（00017）。
-- 那条谓词在两种作用域下给出两种完全不同的可见集合，而这里每一条查询都
-- **不知道**自己跑在哪一种作用域里 —— 那正是想要的：作用域由
-- repository.WithTenant / WithPlatform 决定，查询本身没有那个参数可以传错。
--
-- 同理，INSERT 里一个 merchant_id 都没有：那一列的默认值是 staff_scope_merchant()，
-- 也就是本事务的作用域（见 00017 与数据模型 §14 认证流程 ④「新员工的 merchant_id
-- 继承自调用者，不接受前端传入」）。生成的 Go 函数签名里根本没有这个参数。
--
-- 注释里一个反引号都不许有，理由见 db/queries/inventories.sql 的第三条说明。

-- name: BootstrapChannelState :one
-- 引导通道的状态，一次查两个数。**只在平台作用域里调用**，于是它数的是
-- 平台级那一抽屉里的行 —— 而引导要建的正是一个平台级管理员。
--
-- 两个数各自回答一件事，合在一条语句里是为了让它们来自同一个快照：
--
--   admins      有没有一个还在岗的平台级管理员。没有 = 这个部署还没人能进后台，
--               引导要开；有 = 引导已经做过了。
--   live_tokens 有没有一串还没被用掉的引导 token。它决定
--               POST /admin/auth/bootstrap 在 token 对不上时回 401 还是 409：
--               窗口还开着的时候一串错 token 就是一串错 token（401），
--               窗口关了之后任何 token 都只说明「引导通道已关闭」（409）。
--
-- 分两条语句查的话，两个数之间会隔着一次别的事务的提交，
-- 而这两个数正是用来判断「窗口开没开」的 —— 它们必须是同一时刻的。
SELECT
  (SELECT count(*) FROM staff
    WHERE role = 1 AND status = 1 AND deleted_at IS NULL)           AS admins,
  (SELECT count(*) FROM staff_tokens
    WHERE kind = 1 AND used_at IS NULL AND revoked_at IS NULL
      AND expire_at > now())                                        AS live_tokens;

-- name: CreateStaff :one
-- 建一个操作员。**merchant_id 不在这条语句里** —— 它的默认值是
-- staff_scope_merchant()，也就是本事务的作用域（00017）。平台作用域里它落成
-- NULL（平台级操作员），租户作用域里落成那家店。调用方没有那个参数可以传错。
INSERT INTO staff (email, name, role, created_by)
VALUES ($1, $2, $3, $4)
RETURNING id, merchant_id, email, name, role, status, last_login_at, created_at;

-- name: GetStaffByID :one
-- 按 id 取一个**没被软删**的操作员。查不到（包括「不在本作用域里」）返回 ErrNoRows。
SELECT id, merchant_id, email, name, role, status, last_login_at, created_at
  FROM staff
 WHERE id = $1 AND deleted_at IS NULL;

-- name: ListStaff :many
-- 员工列表。契约：平台级看见平台操作员，商家级只看见自己店的 —— 而这句话
-- 在这条 SQL 里**一个字都没有**，它由作用域和 RLS 给出。
SELECT id, merchant_id, email, name, role, status, last_login_at, created_at
  FROM staff
 WHERE deleted_at IS NULL
 ORDER BY id
 LIMIT $1 OFFSET $2;

-- name: CountStaff :one
SELECT count(*) FROM staff WHERE deleted_at IS NULL;

-- name: SetStaffEmail :one
-- 引导账号换会话时补上自己的邮箱（§14 认证流程 ①）。
--
-- 只允许改一次：条件里带着旧邮箱（占位符），于是这条语句在邮箱已经被补过之后
-- 更新 0 行。不带这个条件的话，它就成了一条「拿引导 token 把任意管理员的邮箱
-- 改掉」的路 —— 而邮箱是这套认证唯一的信任根。
UPDATE staff
   SET email = $2
 WHERE id = $1 AND email = $3 AND deleted_at IS NULL
RETURNING id, merchant_id, email, name, role, status, last_login_at, created_at;

-- name: UpdateStaffRoleStatus :one
-- 改角色或状态。两个参数都是「给了就改，没给就保持」，用 coalesce 表达，
-- 于是 PATCH 的部分更新语义不必在应用层拼 SQL。
UPDATE staff
   SET role   = coalesce(sqlc.narg('role'),   role),
       status = coalesce(sqlc.narg('status'), status)
 WHERE id = sqlc.arg('id') AND deleted_at IS NULL
RETURNING id, merchant_id, email, name, role, status, last_login_at, created_at;

-- name: CountOtherLiveAdmins :one
-- 除了这一个人之外，本作用域里还有几个在岗管理员。
--
-- 它兑现的是数据模型 §14 那条「一条进不了数据库的约束」：系统里必须至少有一个
-- 平台级管理员，且每个商家至少有一个自己的管理员。跨行约束，CHECK 表达不了，
-- 只能在停用 / 降级的业务逻辑里拦（契约里对应那个 409）。
--
-- 「本作用域里」同样不写在 SQL 里：平台作用域下它数的是平台管理员，
-- 租户作用域下它数的是那家店的管理员 —— 正好就是那条约束的两半。
SELECT count(*) FROM staff
 WHERE id <> $1 AND role = 1 AND status = 1 AND deleted_at IS NULL;

-- name: TouchStaffLogin :exec
UPDATE staff SET last_login_at = now() WHERE id = $1;

-- name: CreateStaffToken :one
-- 签发一串 token。只存 sha256 的十六进制，明文只在签发的那一瞬间存在
-- （§14：后台 token 和支付密钥是同一个量级的东西）。
INSERT INTO staff_tokens (staff_id, token_hash, kind, expire_at)
VALUES ($1, $2, $3, $4)
RETURNING id;

-- name: FindLiveOneTimeStaffToken :one
-- 按 hash 取一串**还活着**的一次性 token（kind 1 引导 / 2 邮件链接），
-- 连带取出它属于谁。
--
-- 四个条件缺一不可，而且都要留在 SQL 里而不是取回来再判（同 users.sql 的
-- FindLiveUserToken）：used_at / revoked_at / expire_at 的判定必须和查询在
-- 同一个快照里，否则「取出来 → 判断 → 用」中间那个窗口正是一串已经用过的
-- 引导 token 还能再换一次会话的窗口。
SELECT t.id, t.staff_id, t.kind,
       s.merchant_id, s.email, s.name, s.role, s.status, s.last_login_at, s.created_at
  FROM staff_tokens t
  JOIN staff s ON s.id = t.staff_id
 WHERE t.token_hash = $1
   AND t.kind = $2
   AND t.used_at IS NULL
   AND t.revoked_at IS NULL
   AND t.expire_at > now()
   AND s.deleted_at IS NULL;

-- name: ConsumeStaffToken :one
-- 一次性 token 用掉即失效。
--
-- 条件里再写一次 used_at IS NULL：上一步 FindLiveOneTimeStaffToken 已经查过，
-- 但那是另一条语句、另一个时刻。两个请求同时拿着同一串引导 token 时，
-- 这里的 UPDATE 是串行化的，第二个更新 0 行，:one 于是把它变成一次 ErrNoRows
-- —— 而不是两个都换出一个会话。
UPDATE staff_tokens
   SET used_at = now()
 WHERE id = $1 AND used_at IS NULL AND revoked_at IS NULL AND expire_at > now()
RETURNING id;

-- name: TouchLiveStaffSession :one
-- 会话校验：按 hash 取一条还活着的 kind=3 会话，顺手记一次 last_seen_at。
--
-- **校验与续活写在同一条语句里**，不是为了省一次往返，是因为分开写就有一个
-- 窗口：先 SELECT 确认活着，再 UPDATE 记时间，中间那一刻会话可以被吊销，
-- 而这个请求已经决定放行了。写成一条 UPDATE ... RETURNING 之后，
-- 「这条会话还活着」这个事实和放行这个动作发生在同一次行锁里。
--
-- 代价说清楚：**每一个后台请求因此都是一次写事务**。后台流量本来就小
-- （运营与客服在用），而这一列换来的是「这个会话最后什么时候还在用」——
-- 撤销一串泄露的 token 时，那是唯一能回答「它被用过没有」的东西。
--
-- staff 那一侧连带取出 role 与 status：它们**不进令牌**。一个降级或停用的
-- 操作员必须在下一个请求就失去权限，而 7 天的会话里烤着一个旧 role 的话，
-- 「已经把他降成操作员了」这句话在一周之内都是假的。
UPDATE staff_tokens t
   SET last_seen_at = now()
  FROM staff s
 WHERE t.token_hash = $1
   AND t.kind = 3
   AND t.used_at IS NULL
   AND t.revoked_at IS NULL
   AND t.expire_at > now()
   AND s.id = t.staff_id
   AND s.deleted_at IS NULL
RETURNING t.id, s.id AS staff_id, s.merchant_id, s.role, s.status;
