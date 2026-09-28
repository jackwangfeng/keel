-- 地址簿（/addresses，数据模型 §9 user_addresses）。
--
-- 全文没有一处 WHERE 写租户：租户由 RLS 在数据库层过滤，理由见
-- db/queries/products.sql 与 scripts/check_query_tenancy.py。
--
-- **每一条都带 user_id = 当前买家**。那不是租户过滤，是越权过滤：RLS 只保证
-- 这一行属于本店，不保证它属于这个买家。契约约定 4 与数据模型 §1 写明，
-- 地址用自增 id 对外，越权防护靠服务端按 user_id 强制过滤，查不到即 404 ——
-- 所以「别人的地址」与「不存在的地址」在这一层同形，这是对的：
-- 分开报就是一个探测别人地址簿的口子。
--
-- 每一条也都带 deleted_at IS NULL：软删的地址对买家不存在（契约：已软删的地址不返回）。
--
-- 注释里一个反引号都不许有，理由见 db/queries/inventories.sql 的第三条说明。

-- name: ListUserAddresses :many
-- 地址簿。默认地址排在首位，其余按更新时间倒序（契约原话）。id 兜底让顺序确定：
-- 同一事务里改过的两行 updated_at 相同（now() 是事务开始时刻）。
SELECT id, receiver_name, phone, province, city, district, street, detail,
       region_code, postal_code, tag, is_default, lat, lng, created_at, updated_at
  FROM user_addresses
 WHERE user_id = $1
   AND deleted_at IS NULL
 ORDER BY is_default DESC, updated_at DESC, id DESC;

-- name: FindUserAddress :one
-- 取一条。查不到（含属于别人、已软删）即 ErrNoRows。
SELECT id, receiver_name, phone, province, city, district, street, detail,
       region_code, postal_code, tag, is_default, lat, lng, created_at, updated_at
  FROM user_addresses
 WHERE id = $1
   AND user_id = $2
   AND deleted_at IS NULL;

-- name: InsertUserAddress :one
-- 新增一条。租户列不出现在这条语句里：00030 给了它 DEFAULT current_merchant()，
-- 调用方手里没有那个参数可以传错。
--
-- is_default 为真时，调用方必须已经在同一事务里清掉了旧默认
-- （ClearDefaultAddress），否则这里撞 uk_user_addresses_default。
INSERT INTO user_addresses (user_id, receiver_name, phone, province, city, district,
                            street, detail, region_code, postal_code, tag, is_default, lat, lng)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, sqlc.narg(lat), sqlc.narg(lng))
RETURNING id, receiver_name, phone, province, city, district, street, detail,
          region_code, postal_code, tag, is_default, lat, lng, created_at, updated_at;

-- name: UpdateUserAddress :one
-- 整体替换（PUT）。**is_default 不在 SET 里**：契约说 is_default 只在新增时有效，
-- 切换默认走专用接口 —— 让这条语句根本写不了那一列，比在服务层记得不传更硬。
UPDATE user_addresses
   SET receiver_name = sqlc.arg(receiver_name),
       phone         = sqlc.arg(phone),
       province      = sqlc.arg(province),
       city          = sqlc.arg(city),
       district      = sqlc.arg(district),
       street        = sqlc.arg(street),
       detail        = sqlc.arg(detail),
       region_code   = sqlc.narg(region_code),
       postal_code   = sqlc.narg(postal_code),
       tag           = sqlc.arg(tag),
       lat           = sqlc.narg(lat),
       lng           = sqlc.narg(lng)
 WHERE id = sqlc.arg(id)
   AND user_id = sqlc.arg(user_id)
   AND deleted_at IS NULL
RETURNING id, receiver_name, phone, province, city, district, street, detail,
          region_code, postal_code, tag, is_default, lat, lng, created_at, updated_at;

-- name: ClearDefaultAddress :exec
-- 清掉这个买家现有的默认地址，除了 keep_id 那一条（新增时传 0）。
--
-- 「先清旧、再置新」必须在同一事务里、按这个顺序（数据模型 §9）：反过来的话
-- 新的那一行先变成默认，自己撞上 uk_user_addresses_default。
-- 排除 keep_id 是为了让「对已是默认的地址再设一次」不经过一个
-- 「零个默认」的中间态 —— 那个中间态在同一事务里没人看得见，但它会白写一行。
UPDATE user_addresses
   SET is_default = FALSE
 WHERE user_id = sqlc.arg(user_id)
   AND is_default
   AND deleted_at IS NULL
   AND id <> sqlc.arg(keep_id);

-- name: MarkDefaultAddress :one
-- 把一条设为默认。查不到（含属于别人、已软删）即 ErrNoRows，服务层翻成 404。
UPDATE user_addresses
   SET is_default = TRUE
 WHERE id = $1
   AND user_id = $2
   AND deleted_at IS NULL
RETURNING id, receiver_name, phone, province, city, district, street, detail,
          region_code, postal_code, tag, is_default, lat, lng, created_at, updated_at;

-- name: SoftDeleteUserAddress :one
-- 软删（契约：写 deleted_at）。地址与订单是快照关系，删它不影响任何历史订单。
--
-- 顺手把 is_default 置回假：部分唯一索引本来就不看已软删的行，这一列留着
-- 不会撞任何东西；置假是为了让「这一行曾是默认」不被将来某条忘了写
-- deleted_at IS NULL 的查询读成「这是默认地址」。
UPDATE user_addresses
   SET deleted_at = now(),
       is_default = FALSE
 WHERE id = $1
   AND user_id = $2
   AND deleted_at IS NULL
RETURNING id;
