-- 后台 SKU 与库存的写路径（契约 /admin/products/{id}/skus、/admin/skus/*）。
-- M4 Task 2。
--
-- 同样刻意不带 WHERE merchant_id，理由见 db/queries/products.sql 与
-- db/queries/admin_products.sql 的文件头。注释里一个反引号都不许有。

-- name: AdminListProductSKUs :many
-- 后台视角的规格列表：比前台那条多出 cost_cents（成本，前台永远不该出现）、
-- weight_gram 与 warning_qty，并且**不筛 status** —— 停售的规格在后台要看得见，
-- 否则商家没有任何入口把它改回在售。
--
-- 软删的不返回（契约：AdminProductDetail.skus 不含已软删的 SKU）。
--
-- LEFT JOIN 而不是 JOIN：一个没有库存行的 SKU 真实的样子是「在售、可售 0 件」，
-- 用 JOIN 它会整个从列表里消失，而那正是「这件商品永远缺货」那类故障最难查的
-- 部分 —— 症状里看不到那个 SKU。
--
-- **本轮（00020）它变成了一次跨门店的聚合，理由要写下来。** 库存主键是
-- (sku_id, store_id)，直接 LEFT JOIN 会让一个 SKU 在五家店的五行把这条列表
-- 撑成五倍 —— 那不是「多了几行」，是同一个规格出现五次、每次一个不同的水位。
--
-- 聚合口径选 **sum（全部门店合计）**，因为这里是**租户视角**：AdminProduct 上
-- 的价格区间同一轮定成了「基准价区间，不含任何覆盖」，理由一字不差 ——
-- 后台的商品页没有「当前门店」这个概念（契约在 AdminProduct 上明写了这一点）。
-- 按门店看库存有自己的端点（GET /admin/stores/{store_id}/inventories）。
-- 单店商家的两个数因此完全相同，多店商家看到的是一个对「这款还剩多少」
-- 有意义的答案。warning_qty 取 max：预警线是一个阈值不是一个总量，
-- 把五家店的阈值加起来没有任何含义。
SELECT s.id, s.product_id, s.sku_code, s.spec_values, s.price_cents, s.cost_cents,
       s.weight_gram, s.image_url, s.status, s.created_at, s.updated_at,
       COALESCE(agg.qty,  0)::int AS available_qty,
       COALESCE(agg.warn, 0)::int AS warning_qty
  FROM skus s
  LEFT JOIN LATERAL (
        SELECT sum(i.available_qty) AS qty, max(i.warning_qty) AS warn
          FROM inventories i WHERE i.sku_id = s.id
       ) agg ON TRUE
 WHERE s.product_id = $1
   AND s.deleted_at IS NULL
 ORDER BY s.id;

-- name: AdminGetSKU :one
-- 单个 SKU，软删的不返回（契约：软删之后一律 404）。
SELECT s.id, s.product_id, s.sku_code, s.spec_values, s.price_cents, s.cost_cents,
       s.weight_gram, s.image_url, s.status, s.created_at, s.updated_at,
       COALESCE(agg.qty,  0)::int AS available_qty,
       COALESCE(agg.warn, 0)::int AS warning_qty
  FROM skus s
  LEFT JOIN LATERAL (
        SELECT sum(i.available_qty) AS qty, max(i.warning_qty) AS warn
          FROM inventories i WHERE i.sku_id = s.id
       ) agg ON TRUE
 WHERE s.id = $1
   AND s.deleted_at IS NULL;

-- name: CreateSKU :one
-- 建 SKU。**库存行由 CreateInventoryRow 在同一个事务里紧接着建出来**，
-- 契约在这条端点上明写了这一条，理由值得抄在这里：
--
--   inventories.sku_id 是主键，而下单 SAGA 的正向分支是
--   「UPDATE ... WHERE sku_id = $1 AND available_qty >= $2」。
--   没有那一行时 rows_affected = 0，而 SAGA 把 0 判成**库存不足**。
--   于是一个漏建库存行的 SKU 表现为「这件商品永远缺货」，
--   而排查方向（去查库存水位、查扣减逻辑）从第一步就是错的。
--
-- 为什么不把两条 INSERT 压进一条语句（WITH new_sku AS (INSERT ... RETURNING)
-- INSERT INTO inventories SELECT FROM new_sku）：**RLS 会拒绝**。inventories
-- 是 parent-scoped 表，它的 WITH CHECK 是对 skus 的 EXISTS 子查询，而同一条
-- 语句里数据修改 CTE 的结果对语句的其余部分不可见 —— 那个 EXISTS 看不到刚插
-- 出来的 SKU，于是每一次建 SKU 都会以 42501 失败。本轮实测确认过这一条。
-- 所以是两条语句、一个事务，由 repository 的 CreateSKU 收口。
INSERT INTO skus (product_id, sku_code, spec_values, price_cents, cost_cents,
                  weight_gram, image_url, status)
VALUES (sqlc.arg(product_id), sqlc.arg(sku_code), sqlc.arg(spec_values),
        sqlc.arg(price_cents), sqlc.arg(cost_cents), sqlc.arg(weight_gram),
        sqlc.narg(image_url), sqlc.arg(status))
RETURNING id, product_id, sku_code, spec_values, price_cents, cost_cents,
          weight_gram, image_url, status, created_at, updated_at;

-- name: CreateInventoryRow :execrows
-- 见 CreateSKU 的注释。它只在建 SKU 的那个事务里被调用，
-- 而 repository 那一层不给调用方单独调它的机会。
--
-- **本轮（00020）它只给默认门店建那一行，而且可能一行都不建。**
--
-- 库存按门店分之后，「给这个新 SKU 建库存行」不再是一个有唯一答案的动作：
-- 给每一家店都建一行，就是把 inventories 变成 门店数 × SKU 数 的那张表 ——
-- 正是数据模型 §4 否掉「包含表」时算过的那个量级；而挑一家店建，
-- 那家店只能是默认店（回落目标，单店商家唯一的那一家）。
--
-- 没有默认店时一行都不建，**这不是失败**：缺行 ≡ 可售 0（§4 把这条写死了），
-- 一家刚开的店在录库存之前每个 SKU 都缺行，那是产品要的形态。
-- 所以返回的是行数而不是行 —— 0 行是一个合法的结果，:one 会让它变成
-- pgx.ErrNoRows，而那会把一次正常的建 SKU 报成失败。
INSERT INTO inventories (sku_id, store_id, available_qty, warning_qty)
SELECT sqlc.arg(sku_id), st.id, sqlc.arg(available_qty), sqlc.arg(warning_qty)
  FROM stores st
 WHERE st.is_default AND st.deleted_at IS NULL;

-- name: UpdateSKU :one
-- 部分更新，含改价。**没有 available_qty** —— 库存走 SetInventoryByCAS，
-- 理由见那条查询：它要表达「基于我看到的值改」，而一个什么都能改的 PATCH
-- 表达不了乐观并发；把库存混进来，一次改价就能把并发下单扣掉的量抹掉。
--
-- 与 UpdateProduct 同一个形状：两个 CTE、一个快照，把「不可见」与「没改成」
-- 分开回传。image_url 要一个 set_image_url 开关，理由同 brand_id：
-- 契约里它是 integer | null，「清空小图」与「不动」是两件事。
--
-- sku_code 撞了会以 23505（uk_skus_code）失败，那条错误由 repository 翻成
-- 契约的 409 sku-code-duplicated。不在这里先查一遍：先查后改之间的窗口里
-- 另一个会话可以把同一个货号插进去，而唯一索引是唯一真正能挡住它的东西。
WITH cur AS (
    SELECT s.id FROM skus s WHERE s.id = sqlc.arg(id) AND s.deleted_at IS NULL
), upd AS (
    UPDATE skus u
       SET sku_code    = COALESCE(sqlc.narg(sku_code), u.sku_code),
           spec_values = COALESCE(sqlc.narg(spec_values), u.spec_values),
           price_cents = COALESCE(sqlc.narg(price_cents), u.price_cents),
           cost_cents  = COALESCE(sqlc.narg(cost_cents), u.cost_cents),
           weight_gram = COALESCE(sqlc.narg(weight_gram), u.weight_gram),
           status      = COALESCE(sqlc.narg(status), u.status),
           image_url   = CASE WHEN sqlc.arg(set_image_url)::boolean
                              THEN sqlc.narg(image_url)::text ELSE u.image_url END
     WHERE u.id = sqlc.arg(id) AND u.deleted_at IS NULL
    RETURNING u.id, u.product_id, u.sku_code, u.spec_values, u.price_cents,
              u.cost_cents, u.weight_gram, u.image_url, u.status,
              u.created_at, u.updated_at
)
SELECT (SELECT count(*) FROM cur) AS visible_rows,
       (SELECT count(*) FROM upd) AS updated_rows,
       w.id, w.product_id, w.sku_code, w.spec_values, w.price_cents,
       w.cost_cents, w.weight_gram, w.image_url, w.status,
       w.created_at, w.updated_at
  FROM (SELECT 1) anchor
  LEFT JOIN upd w ON true;

-- name: SoftDeleteSKU :one
-- 软删。硬删在这个 schema 下根本做不到：order_items / cart_items /
-- inventories / inventory_logs 四张表都对 skus 有外键，一个卖过一次的 SKU
-- 永远删不掉。
--
-- 闸门：**在架商品（status = 1）的最后一个 SKU 删不得**（409），
-- 那会让一个前台可见的商品变成没有任何可买规格 —— 与「没有 SKU 不能上架」
-- 是同一条闸门的另一侧。
--
-- 闸门写进 UPDATE 的 WHERE，不是先查后改：两次快照之间，另一个会话可以把
-- 兄弟 SKU 删掉，于是「删的时候还有两个」在提交时变成了「删完一个不剩」。
--
-- 回传 sibling_rows（含自己在内、同商品未软删的 SKU 数）与 product_status，
-- 调用方据此把 updated_rows = 0 分成 404 与 409 两支。
--
-- product_status 走 LEFT JOIN 而不是标量子查询，理由与 DeductInventory 末尾
-- 那一段一模一样：标量子查询会被 sqlc 推断成**非空** int16，而这个 SKU 不可见
-- 时它恰恰是 NULL —— 于是 Scan 会在 404 那条路径上直接报错，
-- 而 404 正是这条查询最该说清楚的一支。
WITH cur AS (
    SELECT s.id, s.product_id FROM skus s
     WHERE s.id = sqlc.arg(id) AND s.deleted_at IS NULL
), sibling AS (
    SELECT count(*) AS n FROM skus s2
     WHERE s2.product_id = (SELECT product_id FROM cur) AND s2.deleted_at IS NULL
), prod AS (
    SELECT p.status FROM products p WHERE p.id = (SELECT product_id FROM cur)
), del AS (
    UPDATE skus u
       SET deleted_at = now()
     WHERE u.id = sqlc.arg(id) AND u.deleted_at IS NULL
       AND NOT ((SELECT status FROM prod) = 1 AND (SELECT n FROM sibling) <= 1)
    RETURNING u.id, u.product_id
)
SELECT (SELECT count(*) FROM cur) AS visible_rows,
       (SELECT count(*) FROM del) AS deleted_rows,
       (SELECT n FROM sibling)    AS sibling_rows,
       pr.status                  AS product_status,
       c.product_id               AS product_id
  FROM (SELECT 1) anchor
  LEFT JOIN cur  c  ON true
  LEFT JOIN prod pr ON true;

-- name: SetInventoryByCAS :one
-- 比较并设置（契约 PUT /admin/skus/{sku_id}/inventory）。
--
-- ### 本轮（00020）多了一个 store_id 参数，而路径上没有它
--
-- 库存主键变成 (sku_id, store_id) 之后，「这个 SKU 的库存」不再是一个有定义的
-- 东西。契约把这条路径的语义写死成「**本租户恰好有一家未软删的门店时，
-- 它就是那一家；否则 409 store-ambiguous**」，那一步判断在 repository 里做
-- （CountStoresForTenant），不在这条语句里 —— 让 SQL 自己挑一家，
-- 就等于在最热的写路径上默认了一个猜测，而猜错的后果是把另一家店的水位
-- 覆盖掉，没有任何东西会响。
--
-- 为什么不让它「默认落到默认门店」：那正是这条接口最不该做的事（契约原话）。
-- 是「恰好一家才可用，否则显式报错」，不是「猜一家」。
--
-- ### 这条接口和下单 SAGA 抢同一行
--
-- 下单正向分支是「UPDATE inventories SET available_qty = available_qty - $2
-- WHERE sku_id = $1 AND available_qty >= $2」。如果后台这里写一个无条件的
-- 绝对值覆盖，那么「商家看到 10、页面停了三分钟、期间卖掉 4 件、商家把它改成
-- 20」的结果是 20，而正确答案是 16 —— 并发下单扣掉的 4 件被一次后台覆盖抹掉了，
-- 而且没有任何东西会响。所以条件是 「available_qty = $expected」。
--
-- 不需要给 inventories 加 version 列：数据模型 §4 已经论证过这张表用的是
-- **条件原子更新**，这里只是把条件从 「>= $n」 换成 「= $expected」。
--
-- ### 两种 rows_affected = 0 必须在同一条语句里分开
--
--   ① CAS 不匹配              → 409 inventory-precondition-failed，
--                               并把**当前真实值**放进 Problem 的 current；
--   ② 这个 SKU 不在本租户 / 不存在 / 已软删 → 404。
--
-- 契约把这两条刻意分开，并写明了理由：把「不是你的 SKU」也报成 409 会让
-- 调用方以为重读一次再试就能成功，而那个循环永远不会结束。
--
-- 做法与 db/queries/inventories.sql 的 DeductInventory 同构：两个 CTE 看到的是
-- **同一个 MVCC 快照**，中间没有别的事务能把行删掉或改掉租户归属，于是
-- 「可见但没改成」与「根本不可见」不会因为竞态互换。拆成「先 SELECT 确认可见、
-- 再 UPDATE」是两次快照，而那个窗口正是这条接口存在的全部理由。
--
-- ### 第三种失败不在返回值里
--
-- 完全没设租户上下文时 current_merchant() 直接 RAISE，整条语句以 42501 失败，
-- 根本走不到返回值。它是配置错误，不是库存的任何一种状态。
--
-- ### 可见性为什么要 JOIN skus
--
-- inventories 是 parent-scoped 表，RLS 已经按 skus.merchant_id 挡住了别家的行。
-- 但 skus.deleted_at 是本轮（00018）新加的，RLS 不看它 —— 一个软删掉的 SKU
-- 的库存行仍在 RLS 视野内。契约说软删的 SKU 一律 404，所以这里显式加上。
-- 不加的话，后台能给一个「已经不存在」的规格改库存，而前台永远看不到它。
--
-- ### 两处 sqlc 逼出来的写法
--
-- 一、每个 CTE 里的表都带别名（inv / upd / sk / sk2）：sqlc 把整条语句的关系
--     拍平成一张表，同一张表出现两次就让每个裸列名都报 ambiguous。
-- 二、末尾那个 「FROM (SELECT 1) anchor LEFT JOIN ...」：换成标量子查询的话
--     sqlc 会把它推断成非空，而它在 CAS 失败那一支恰恰是 NULL，
--     于是 Scan 会在最该被区分开的那条路径上直接报错。
WITH cur AS (
    SELECT inv.sku_id, inv.available_qty, inv.warning_qty, inv.updated_at
      FROM inventories inv
      JOIN skus sk ON sk.id = inv.sku_id
     WHERE inv.sku_id = sqlc.arg(sku_id) AND inv.store_id = sqlc.arg(store_id)
       AND sk.deleted_at IS NULL
), upd AS (
    UPDATE inventories u
       SET available_qty = sqlc.arg(available_qty),
           warning_qty   = COALESCE(sqlc.narg(warning_qty), u.warning_qty),
           updated_at    = now()
     WHERE u.sku_id = sqlc.arg(sku_id) AND u.store_id = sqlc.arg(store_id)
       AND u.available_qty = sqlc.arg(expected_available_qty)
       AND EXISTS (SELECT 1 FROM skus sk2
                    WHERE sk2.id = u.sku_id AND sk2.deleted_at IS NULL)
    RETURNING u.sku_id, u.available_qty, u.warning_qty, u.updated_at
)
SELECT (SELECT count(*) FROM cur) AS visible_rows,
       (SELECT count(*) FROM upd) AS updated_rows,
       w.available_qty AS new_available_qty,
       w.warning_qty   AS new_warning_qty,
       w.updated_at    AS new_updated_at,
       c.available_qty AS current_available_qty,
       c.warning_qty   AS current_warning_qty,
       c.updated_at    AS current_updated_at
  FROM (SELECT 1) anchor
  LEFT JOIN cur c ON true
  LEFT JOIN upd w ON true;
