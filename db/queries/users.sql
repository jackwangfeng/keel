-- 买家身份与会话的查询。
--
-- 全文没有一处 WHERE 写租户：租户由 RLS 在数据库层过滤，写法与理由见
-- db/queries/products.sql。这张表上尤其不能破例 —— 登录是**未认证**的入口，
-- 它按手机号点查 users，而「这个手机号属于哪家店」正是隔离本身。应用层再加
-- 一遍条件之后，「RLS 到底有没有生效」就再也测不出来了：跨店登录失败既可能是
-- RLS 拦的，也可能只是那个 WHERE 拦的。
--
-- 注释里一个反引号都不许有，理由见 db/queries/inventories.sql 的第三条说明。

-- name: GetUserByPhone :one
-- 按手机号取买家，用于密码登录。
--
-- deleted_at IS NULL 与 uk_users_phone 那个部分唯一索引是同一件事的两面
-- （数据模型 §9）：号码会被放号复用，注销的旧主人必须查不出来，
-- 否则新主人一登录就登进了别人的历史账号。
--
-- status 不在这里过滤：封禁（2）与注销（3）要回 403，而「查无此人」要回 401，
-- 两者对调用方是不同的事实。在 SQL 里一起滤掉的话，服务层就再也分不清了。
SELECT id, phone, password_hash, nickname, avatar_url, gender, status,
       last_login_at, created_at
  FROM users
 WHERE phone = $1
   AND deleted_at IS NULL;

-- name: GetUserByID :one
-- 按 id 取买家。刷新令牌时要用它重新填契约里那个必填的 user 对象 ——
-- 不能直接把签发时的快照塞回去：封禁与注销就发生在两次请求之间。
SELECT id, phone, password_hash, nickname, avatar_url, gender, status,
       last_login_at, created_at
  FROM users
 WHERE id = $1
   AND deleted_at IS NULL;

-- name: TouchUserLogin :exec
-- 记一次登录。updated_at 由触发器负责（00007），这里不写它。
UPDATE users SET last_login_at = now() WHERE id = $1;

-- name: CreateUserToken :one
-- 建一个会话。**租户列不出现在这条语句里** —— 它的默认值是 current_merchant()，
-- 也就是本事务 set_config 进来的那个租户（见 00010 的说明）。
-- 于是调用方没有那个参数可以传错，生成的 Go 函数签名里根本没有它。
INSERT INTO user_tokens (user_id, token_hash, expire_at)
VALUES ($1, $2, $3)
RETURNING id;

-- name: FindLiveUserToken :one
-- 按 sha256 取一个还活着的会话。
--
-- 三个条件缺一不可，而且都要留在 SQL 里而不是取回来再判：吊销与过期的判定
-- 必须和查询在同一个快照里，否则「取出来 → 判断 → 用」中间那个窗口正是
-- 一次已吊销令牌还能换出新令牌的窗口。
--
-- :one 让「查不到」变成 pgx.ErrNoRows，调用方拿不到一个能被当成正常的零值。
SELECT id, user_id
  FROM user_tokens
 WHERE token_hash = $1
   AND revoked_at IS NULL
   AND expire_at > now();

-- name: RotateUserToken :one
-- 刷新时轮换：旧的 hash 当场作废，换成新的。
--
-- 为什么是轮换而不是「签一个新的、旧的留着」：refresh_token 的寿命以月计，
-- 它泄露之后如果旧值一直有效，攻击者和用户会一直并存。轮换之后，两者之中
-- 谁先用谁才继续有效 —— 这不解决泄露，但把「永久并存」压成「一次竞争」。
--
-- 条件里再写一次 revoked_at IS NULL 与 expire_at > now()：上一步 FindLiveUserToken
-- 已经查过，但那是另一条语句、另一个时刻。并发的两次刷新都拿着同一个旧令牌时，
-- 这里的 UPDATE 是串行化的，第二次会更新 0 行（因为 hash 已经被换掉了），
-- :one 于是把它变成一次 ErrNoRows，而不是两个都成功。
--
-- **「hash 已经被换掉」要靠 token_hash = 旧 hash 这一条才成立。** 原来 WHERE 里只有 id，
-- 这段注释说的事并没有发生：多实例实测，同一个 refresh_token 并发刷新 5 次，5 次全是 200，
-- 会话被连转 5 次，只有最后一次发出去的令牌是活的 —— 其余 4 个客户端手里拿着一个
-- 当场就作废的令牌，下次刷新被登出；泄露的旧令牌也能和真用户同时换出新令牌而不被察觉。
UPDATE user_tokens
   SET token_hash = sqlc.arg(new_token_hash),
       expire_at  = sqlc.arg(expire_at)
 WHERE id = sqlc.arg(id)
   AND token_hash = sqlc.arg(old_token_hash)
   AND revoked_at IS NULL
   AND expire_at > now()
RETURNING id;

-- name: RevokeUserToken :one
-- 退出登录：把会话标记为已吊销。
--
-- 只吊销这一个会话，不是这个用户的全部会话：手机上退出不该把平板也踢下线。
-- 「登出所有设备」是另一个动作，契约里今天没有它。
--
-- :one 让「这个会话不存在 / 已经被吊销过」在服务层可见。它在 HTTP 上仍然是
-- 204（退出登录该是幂等的），但服务层要知道自己实际做了什么 ——
-- 写成 :exec 的话，「吊销了一个根本不存在的会话」会被静默吞掉，
-- 而那正是租户校验一旦失效时会出现的症状。
UPDATE user_tokens
   SET revoked_at = now()
 WHERE id = $1
   AND revoked_at IS NULL
RETURNING id;

-- name: LockUserRow :one
-- 锁住这个买家的 users 行，直到本事务结束。它是「每个买家一把互斥锁」：
--
-- 地址簿的「设为默认」要先清旧、再置新，两个并发的切换若不排队，第二个会在
-- 清旧那一步看不见第一个刚置上的新默认（它还没提交），于是置新时撞上
-- uk_user_addresses_default 报 500。契约专门把切换收进一个接口，就是要让
-- 唯一性由服务端事务负责、而不是让客户端并发点两下时看到 500。
-- 解绑第三方身份的「最后一个凭据」判定同理：数一数还剩几个、再删，两步之间
-- 不能让另一个解绑插进来。
--
-- 锁 users 行而不是锁地址行：新增地址时还没有那一行可锁，而一个买家总有 users 行。
-- 查不到（已注销软删）即 ErrNoRows。
SELECT id FROM users WHERE id = $1 AND deleted_at IS NULL FOR UPDATE;

-- name: UpdateUserProfile :one
-- 改资料（PATCH /me）。三个字段都是「给了才改」：nickname 与 gender 用 COALESCE，
-- avatar_url 多一个开关，因为它可以被清空（给空串即清掉头像），而
-- 「清空」与「不改」在一个可空参数上分不开。
UPDATE users
   SET nickname   = COALESCE(sqlc.narg(nickname), nickname),
       gender     = COALESCE(sqlc.narg(gender), gender),
       avatar_url = CASE WHEN sqlc.arg(set_avatar)::boolean
                         THEN sqlc.narg(avatar_url) ELSE avatar_url END
 WHERE id = sqlc.arg(id)
   AND deleted_at IS NULL
RETURNING id, phone, password_hash, nickname, avatar_url, gender, status,
          last_login_at, created_at;

-- name: ListUserIdentities :many
-- 已绑定的第三方身份（GET /me/identities）。
--
-- 只取 union_id 是否为空，不取它本身：契约写明 external_id（openid）与 union_id
-- 属于渠道敏感标识，一律不对外返回。不 SELECT 出来，上层就没有机会把它漏出去。
SELECT id, provider, (union_id IS NOT NULL)::boolean AS has_union_id, created_at
  FROM user_identities
 WHERE user_id = $1
 ORDER BY id;

-- name: CountUserIdentitiesExcept :one
-- 这个买家在 provider 之外还有几条身份。解绑前判「最后一个凭据」用。
SELECT count(*) FROM user_identities WHERE user_id = $1 AND provider <> $2;

-- name: DeleteUserIdentities :execrows
-- 解绑某个 provider 下的全部身份（同一个 provider 下按约定至多一条，
-- 见契约 identity-duplicate-provider；这里不假设它）。
DELETE FROM user_identities WHERE user_id = $1 AND provider = $2;

-- name: LoginLockedUntil :one
-- 这个号此刻是否在锁定中；不在锁定中返回 NULL（没有行也走这一支）。
SELECT max(locked_until)::timestamptz AS locked_until
  FROM login_failures
 WHERE phone = sqlc.arg(phone) AND locked_until > now();

-- name: RecordLoginFailure :one
-- 记一次口令错误，一条语句完成「窗口过期就从 1 重计 / 否则 +1 / 到阈值就锁」。
-- 并发的两次失败在主键上串行：后到的那次看到前一次提交后的 fail_count，不会丢计数。
-- 锁上之后计数清零、窗口从锁定那一刻重开：解锁之后再错五次才再锁。
INSERT INTO login_failures (phone, fail_count, window_start)
VALUES (sqlc.arg(phone), 1, now())
ON CONFLICT ON CONSTRAINT login_failures_pkey DO UPDATE SET
    fail_count = CASE
        WHEN login_failures.window_start < now() - sqlc.arg(fail_window)::interval THEN 1
        WHEN login_failures.fail_count + 1 >= sqlc.arg(max_failures)::int THEN 0
        ELSE login_failures.fail_count + 1 END,
    window_start = CASE
        WHEN login_failures.window_start < now() - sqlc.arg(fail_window)::interval THEN now()
        WHEN login_failures.fail_count + 1 >= sqlc.arg(max_failures)::int THEN now()
        ELSE login_failures.window_start END,
    locked_until = CASE
        WHEN login_failures.window_start >= now() - sqlc.arg(fail_window)::interval
         AND login_failures.fail_count + 1 >= sqlc.arg(max_failures)::int
        THEN now() + sqlc.arg(lock_for)::interval
        ELSE login_failures.locked_until END
RETURNING locked_until;

-- name: ClearLoginFailures :exec
-- 登录成功：清掉这个号的失败记录（记住的是「连续」失败，不是历史总数）。
DELETE FROM login_failures WHERE phone = sqlc.arg(phone);

-- name: PruneLoginFailures :exec
-- 顺手清掉本店早已过期的行（窗口与锁都过去一天以上），每次至多 100 行。
DELETE FROM login_failures
 WHERE ctid IN (SELECT ctid FROM login_failures
                 WHERE window_start < now() - interval '1 day'
                   AND (locked_until IS NULL OR locked_until < now() - interval '1 day')
                 LIMIT 100);
