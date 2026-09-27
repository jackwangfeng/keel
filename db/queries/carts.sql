-- 购物车（/cart，数据模型 §10）。
--
-- 全文没有一处 WHERE 写租户：租户由 RLS 在数据库层过滤（db/queries/products.sql）。
--
-- ### 越权过滤只在一处：carts.user_id
--
-- 「这是谁的车」只由 FindCartID / EnsureCart 按 user_id 回答一次，之后每一条
-- 条目语句都带 cart_id = 那辆车。所以**条目语句上的 cart_id 条件就是越权过滤本身**：
-- 删掉它，一个买家就能按自增 id 改到同一家店里别人车里的条目 —— RLS 挡不住，
-- 那一行确实属于本店。契约要求那种请求是 404（查不到即 404，不是 403），
-- 而 cart_id 条件让「别人的条目」在这一层与「不存在的条目」同形。
--
-- ### 这里没有价格
--
-- 车里的价在读的时候按门店现算，走的是 db/queries/orders.sql 的
-- ListSKUsForPricing —— 与 /orders/preview、POST /orders 同一条查询。
-- 这里另写一份 JOIN sku_prices_by_store 的话，「购物车显示的价」与「试算的价」
-- 就有了两份实现，而它们迟早只更新一份（那条查询的注释里讲过同一件事）。
-- ListCartLines 只给定价查询回答不了的东西：这一行的展示素材、它为什么不可买、
-- 这家店还剩几件。
--
-- 注释里一个反引号都不许有，理由见 db/queries/inventories.sql 的第三条说明。

-- name: FindCartID :one
-- 这个买家的车。没有车（从没加购过）即 ErrNoRows —— 读路径据此回一辆空车，
-- 不为一次 GET 建一行。
SELECT id FROM carts WHERE user_id = $1;

-- name: EnsureCart :one
-- 取这个买家的车，没有就建。写路径用。
--
-- ON CONFLICT DO UPDATE 而不是 DO NOTHING：后者在冲突时不 RETURNING 任何行，
-- 调用方还得再查一次，而两次之间的窗口里什么都可能发生。更新的那一列是它自己，
-- 真正变的只有触发器维护的 updated_at（「这辆车最近被动过」，本来就对）。
-- 租户列不出现：00031 给了它 DEFAULT current_merchant()。
INSERT INTO carts (user_id) VALUES ($1)
ON CONFLICT (user_id) DO UPDATE SET user_id = EXCLUDED.user_id
RETURNING id;

-- name: ListCartLines :many
-- 整辆车，按加购时间倒序，带上判断「这一行现在能不能买」要用的素材。
--
-- 用 JOIN 而不是 LEFT JOIN 连 skus / products：两张表都是软删（00018），
-- 被删掉的规格与商品行仍在，所以 JOIN 不会让任何一行从车里消失 ——
-- 这正是数据模型 §10 要的「下架、删除的商品不从购物车里清掉，查询时标记为失效」。
--
-- on_shelf 与 ListSKUsForPricing 的四个条件逐字对应（s.status、s.deleted_at、
-- p.status、p.deleted_at）。服务层用它区分两种「定价查询没返回这一行」：
-- on_shelf 为假是失效（off_shelf），为真是这家店或它所在大区不卖（not_sold_in_store）。
-- 门店 / 大区那两条排除不在这里再写一遍：它们只在定价查询里有一份。
--
-- 库存按 (sku_id, store_id) 取，LEFT JOIN + COALESCE 把「这家店没有这一行」
-- 记成 0（缺行即可售 0，与 db/queries/products.sql 同一个口径）。
-- 漏掉 store_id 的话这条 JOIN 会匹配到该 SKU 在所有门店的行，一行变多行。
SELECT ci.id, ci.sku_id, ci.product_id, ci.quantity, ci.selected,
       p.title, s.spec_values, s.image_url,
       (s.status = 1 AND s.deleted_at IS NULL
        AND p.status = 1 AND p.deleted_at IS NULL)::boolean AS on_shelf,
       COALESCE(i.available_qty, 0)::int AS available_qty,
       -- SKU 没有自己的图时，购物车行显示商品主图（与订单行快照同一个退路）。
       COALESCE(img.upload_id, 0)::bigint AS main_image_upload_id
  FROM cart_items ci
  JOIN skus s     ON s.id = ci.sku_id
  JOIN products p ON p.id = ci.product_id
  LEFT JOIN LATERAL (
        SELECT pi.upload_id FROM product_images pi
         WHERE pi.product_id = p.id
         ORDER BY pi.sort_order, pi.id
         LIMIT 1
       ) img ON TRUE
  LEFT JOIN inventories i ON i.sku_id = ci.sku_id AND i.store_id = sqlc.arg(store_id)
 WHERE ci.cart_id = sqlc.arg(cart_id)
 ORDER BY ci.created_at DESC, ci.id DESC;

-- name: FindSKUForCart :one
-- 加购时取这个 SKU 的状态：属于哪件商品、在不在架、这家店还剩几件。
-- 查不到（不存在，或属于别家店 —— RLS 让两者同形）即 ErrNoRows。
SELECT s.id, s.product_id,
       (s.status = 1 AND s.deleted_at IS NULL
        AND p.status = 1 AND p.deleted_at IS NULL)::boolean AS on_shelf,
       COALESCE(i.available_qty, 0)::int AS available_qty
  FROM skus s
  JOIN products p ON p.id = s.product_id
  LEFT JOIN inventories i ON i.sku_id = s.id AND i.store_id = sqlc.arg(store_id)
 WHERE s.id = sqlc.arg(sku_id);

-- name: CountCartLines :one
-- 车里有几种商品。加购前判「至多 100 种」用。
SELECT count(*) FROM cart_items WHERE cart_id = $1;

-- name: FindCartLineBySKU :one
-- 这辆车里某个 SKU 那一行（加购前取当前数量，用于判库存与报 999 上限的 detail）。
SELECT id, quantity FROM cart_items WHERE cart_id = $1 AND sku_id = $2;

-- name: AddCartLine :one
-- 加购。**一条 upsert**，UNIQUE (cart_id, sku_id) 让重复加购必然走合并路径
-- （数据模型 §10 原文的那条语句），不先查再判。
--
-- DO UPDATE 带一个 WHERE：累加后超过 999 就不更新，于是 RETURNING 不返回任何行，
-- 调用方拿到 ErrNoRows 并报 422 cart-quantity-exceeded。上限判在这条语句里而不是
-- 服务层先读后比：两个并发的加购各自读到 600、各自加 300，先读后比的写法两个都放行，
-- 最后是 1200 —— 而这里第二个会看到第一个已提交的 900，再加 300 就不成立。
-- 表上的 CHECK (quantity BETWEEN 1 AND 999) 是这条语句之后的最后一道。
--
-- 重复加购把 selected 重新置真：用户刚刚又加了一次，他要的就是买它。
INSERT INTO cart_items (cart_id, sku_id, product_id, quantity)
VALUES ($1, $2, $3, $4)
ON CONFLICT (cart_id, sku_id) DO UPDATE
    SET quantity = cart_items.quantity + EXCLUDED.quantity,
        selected = TRUE
  WHERE cart_items.quantity + EXCLUDED.quantity <= 999
RETURNING id, quantity;

-- name: FindCartLine :one
-- 这辆车里的一行。不在这辆车里（含别人车里的）即 ErrNoRows。
SELECT id, sku_id, quantity, selected FROM cart_items WHERE id = $1 AND cart_id = $2;

-- name: UpdateCartLine :one
-- 改数量 / 勾选态（PATCH /cart/items/{item_id}）。两个都是「给了才改」。
UPDATE cart_items
   SET quantity = COALESCE(sqlc.narg(quantity), quantity),
       selected = COALESCE(sqlc.narg(selected), selected)
 WHERE id = sqlc.arg(id)
   AND cart_id = sqlc.arg(cart_id)
RETURNING id;

-- name: SetAllCartSelection :exec
-- 全选 / 全不选（PUT /cart/selection 不带 item_ids）。
UPDATE cart_items SET selected = $2 WHERE cart_id = $1 AND selected <> $2;

-- name: SetCartSelection :many
-- 按 id 批量勾选。返回这辆车里**确实存在**的那些 id —— 调用方拿它与请求的 id
-- 集合比，少了就整体回滚并报 404（契约：item_ids 中存在不属于当前用户购物车的条目）。
--
-- 不带 selected <> $3 的条件：那会让「已经是这个状态」的行不出现在 RETURNING 里，
-- 与「不存在」混在一起。
UPDATE cart_items
   SET selected = sqlc.arg(selected)
 WHERE cart_id = sqlc.arg(cart_id)
   AND id = ANY(sqlc.arg(item_ids)::bigint[])
RETURNING id;

-- name: DeleteCartLines :many
-- 按 id 批量删除。返回值的用法同 SetCartSelection：少了就整体回滚并报 404。
DELETE FROM cart_items
 WHERE cart_id = sqlc.arg(cart_id)
   AND id = ANY(sqlc.arg(item_ids)::bigint[])
RETURNING id;

-- name: DeleteSelectedCartLines :exec
-- 删除全部已勾选条目（POST /cart/items/batch-delete 带 selected: true）。
DELETE FROM cart_items WHERE cart_id = $1 AND selected;

-- name: DeleteCartLine :execrows
-- 删一行。返回受影响行数，0 即不在这辆车里（服务层翻成 404）。
DELETE FROM cart_items WHERE id = $1 AND cart_id = $2;

-- name: ClearCart :exec
-- 清空（DELETE /cart）。车本身留着：它只有 user_id 一列，删了下次加购还得再建。
DELETE FROM cart_items WHERE cart_id = $1;
