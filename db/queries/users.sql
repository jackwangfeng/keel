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
UPDATE user_tokens
   SET token_hash = $2,
       expire_at  = $3
 WHERE id = $1
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
