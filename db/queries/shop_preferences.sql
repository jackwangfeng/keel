-- 店铺设置里商家自己能改的那一半（00059，契约 GET / PUT /admin/shop-settings）。
-- 数据模型 §2 shop_preferences。
--
-- 全文没有一处 WHERE 写租户：这张表的主键就是租户列，RLS 之下一个事务只看得见
-- 自己那一行（或者一行都没有），所以两条读都不带 WHERE。
-- 注释里一个反引号都不许有，理由见 db/queries/inventories.sql 的第三条说明。

-- name: GetShopPreferences :one
-- 本店的设置。没有这一行（开店不写它、从没改过设置）时 0 行，
-- repository 那一层按列默认值补齐 —— 而不是报错或当成「不自动确认」。
SELECT service_phone, timezone, auto_confirm_days, return_ship_days, after_sale_days, updated_at
  FROM shop_preferences;

-- name: UpsertShopPreferences :one
-- 整体替换（PUT）。没有这一行就插一行，有就整行覆盖 —— 四列都由调用方给全，
-- 请求里没给的可选项（客服电话）写成 NULL，这就是「整体替换」的意思。
-- 租户列由 DEFAULT current_merchant() 补；冲突目标按约束名写，不点名那一列。
INSERT INTO shop_preferences (service_phone, timezone, auto_confirm_days, return_ship_days, after_sale_days)
VALUES (sqlc.narg(service_phone), sqlc.arg(timezone), sqlc.arg(auto_confirm_days),
        sqlc.arg(return_ship_days), sqlc.arg(after_sale_days))
ON CONFLICT ON CONSTRAINT shop_preferences_pkey DO UPDATE
   SET service_phone     = EXCLUDED.service_phone,
       timezone          = EXCLUDED.timezone,
       auto_confirm_days = EXCLUDED.auto_confirm_days,
       return_ship_days  = EXCLUDED.return_ship_days,
       after_sale_days   = EXCLUDED.after_sale_days
RETURNING service_phone, timezone, auto_confirm_days, return_ship_days, after_sale_days, updated_at;
