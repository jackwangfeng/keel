-- 门店 / 大区两个作用域下的商品可见性、生效价与库存
-- （契约 /admin/stores/{id}/products、/admin/regions/{id}/products、
--  两条 .../listing、/admin/stores/{id}/inventories 的 SKU 一半与按门店改库存前的可售判定）。
-- 数据模型 §4。
--
-- 不带 WHERE merchant_id（RLS 过滤），注释里不许有反引号。
--
-- ===========================================================================
-- 这两条列表为什么是「租户目录左连两张排除表」，不是「读上架清单」
-- ===========================================================================
--
-- 商品目录是**租户级**的（products 不动）。大区与门店各自只决定「我不卖哪些」，
-- 而那两张表是**排除表**：缺一行即在售（「开店即营业」）。
-- 「这家店的上架清单」那张表不存在，而且刻意不存在 —— 两张排除表上的
-- CHECK (status = 0) 让它们在结构上表达不出「只卖这几件」。
--
-- 于是 listed 是一次 LEFT JOIN 的结果为空，effective_listed 是两层都为空。
-- 两个字段刻意分开：一件被大区排掉的商品，门店这一层设成 listed = true 也
-- 捞不回来（两层是**与**不是**或**）。合成一个字段的话，后台会显示「已上架」
-- 而买家看不到，而那种不一致没有任何东西会报出来。

-- name: ScopedListStoreProducts :many
-- 这家店的商品可见性与生效价。
--
-- price_source 取**最内的那一层**（有任何一个 SKU 用了门店价就是 3）：
-- 它回答的是「这一行有没有被本地覆盖过」，运营要按它筛。一件商品的多个 SKU
-- 落在不同层上是常态，取 max 而不是取第一个 —— 取第一个的话结果取决于
-- SKU 的 id 顺序，而那是一个没人知道自己依赖了的东西。
--
-- listed 为空（没有 override 行）即在售。region_listed 同理。
SELECT p.id AS product_id, p.title, p.status,
       (so.product_id IS NULL)::boolean AS listed,
       (so.product_id IS NULL AND ro.product_id IS NULL)::boolean AS effective_listed,
       COALESCE(agg.min_price, 0)::bigint AS min_price_cents,
       COALESCE(agg.max_price, 0)::bigint AS max_price_cents,
       COALESCE(agg.src, 1)::int          AS price_source
  FROM products p
  LEFT JOIN store_product_overrides so
         ON so.store_id = sqlc.arg(store_id) AND so.product_id = p.id AND so.status = 0
  LEFT JOIN region_product_overrides ro
         ON ro.region_id = sqlc.arg(region_id) AND ro.product_id = p.id AND ro.status = 0
  LEFT JOIN LATERAL (
        SELECT min(v.price_cents) AS min_price, max(v.price_cents) AS max_price,
               max(v.price_source) AS src
          FROM sku_prices_by_store v
         WHERE v.store_id = sqlc.arg(store_id) AND v.product_id = p.id
       ) agg ON TRUE
 WHERE p.deleted_at IS NULL
   AND (sqlc.narg(listed)::boolean IS NULL
        OR (so.product_id IS NULL) = sqlc.narg(listed)::boolean)
 ORDER BY p.id
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: ScopedCountStoreProducts :one
-- 条件必须与 ScopedListStoreProducts 逐字一致。
SELECT count(*)
  FROM products p
  LEFT JOIN store_product_overrides so
         ON so.store_id = sqlc.arg(store_id) AND so.product_id = p.id AND so.status = 0
 WHERE p.deleted_at IS NULL
   AND (sqlc.narg(listed)::boolean IS NULL
        OR (so.product_id IS NULL) = sqlc.narg(listed)::boolean);

-- name: ScopedListRegionProducts :many
-- 与上面同构，少一层覆盖：price_source 只会是 1 或 2，
-- 而 effective_listed 与 listed 在这一层恒等（大区是最外层，没有更外的东西
-- 能再排除它）。两个字段仍然都返回，因为契约用的是同一个 schema，
-- 而让其中一个在这条端点上缺席，客户端就要为两条同构的端点写两套解析。
SELECT p.id AS product_id, p.title, p.status,
       (ro.product_id IS NULL)::boolean AS listed,
       (ro.product_id IS NULL)::boolean AS effective_listed,
       COALESCE(agg.min_price, 0)::bigint AS min_price_cents,
       COALESCE(agg.max_price, 0)::bigint AS max_price_cents,
       COALESCE(agg.src, 1)::int          AS price_source
  FROM products p
  LEFT JOIN region_product_overrides ro
         ON ro.region_id = sqlc.arg(region_id) AND ro.product_id = p.id AND ro.status = 0
  LEFT JOIN LATERAL (
        SELECT min(v.price_cents) AS min_price, max(v.price_cents) AS max_price,
               max(v.price_source) AS src
          FROM sku_prices_by_region v
         WHERE v.region_id = sqlc.arg(region_id) AND v.product_id = p.id
       ) agg ON TRUE
 WHERE p.deleted_at IS NULL
   AND (sqlc.narg(listed)::boolean IS NULL
        OR (ro.product_id IS NULL) = sqlc.narg(listed)::boolean)
 ORDER BY p.id
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: ScopedCountRegionProducts :one
-- 条件必须与 ScopedListRegionProducts 逐字一致。
SELECT count(*)
  FROM products p
  LEFT JOIN region_product_overrides ro
         ON ro.region_id = sqlc.arg(region_id) AND ro.product_id = p.id AND ro.status = 0
 WHERE p.deleted_at IS NULL
   AND (sqlc.narg(listed)::boolean IS NULL
        OR (ro.product_id IS NULL) = sqlc.narg(listed)::boolean);

-- name: GetStoreProductListing :one
-- 单件商品在这家店下的那一行，PUT .../listing 之后回显用。
-- 谓词与 ScopedListStoreProducts 逐字一致，否则「改完之后看到的」与
-- 「列表里看到的」会是两回事。
SELECT p.id AS product_id, p.title, p.status,
       (so.product_id IS NULL)::boolean AS listed,
       (so.product_id IS NULL AND ro.product_id IS NULL)::boolean AS effective_listed,
       COALESCE(agg.min_price, 0)::bigint AS min_price_cents,
       COALESCE(agg.max_price, 0)::bigint AS max_price_cents,
       COALESCE(agg.src, 1)::int          AS price_source
  FROM products p
  LEFT JOIN store_product_overrides so
         ON so.store_id = sqlc.arg(store_id) AND so.product_id = p.id AND so.status = 0
  LEFT JOIN region_product_overrides ro
         ON ro.region_id = sqlc.arg(region_id) AND ro.product_id = p.id AND ro.status = 0
  LEFT JOIN LATERAL (
        SELECT min(v.price_cents) AS min_price, max(v.price_cents) AS max_price,
               max(v.price_source) AS src
          FROM sku_prices_by_store v
         WHERE v.store_id = sqlc.arg(store_id) AND v.product_id = p.id
       ) agg ON TRUE
 WHERE p.id = sqlc.arg(product_id) AND p.deleted_at IS NULL;

-- name: GetRegionProductListing :one
-- 同上，大区那一层。
SELECT p.id AS product_id, p.title, p.status,
       (ro.product_id IS NULL)::boolean AS listed,
       (ro.product_id IS NULL)::boolean AS effective_listed,
       COALESCE(agg.min_price, 0)::bigint AS min_price_cents,
       COALESCE(agg.max_price, 0)::bigint AS max_price_cents,
       COALESCE(agg.src, 1)::int          AS price_source
  FROM products p
  LEFT JOIN region_product_overrides ro
         ON ro.region_id = sqlc.arg(region_id) AND ro.product_id = p.id AND ro.status = 0
  LEFT JOIN LATERAL (
        SELECT min(v.price_cents) AS min_price, max(v.price_cents) AS max_price,
               max(v.price_source) AS src
          FROM sku_prices_by_region v
         WHERE v.region_id = sqlc.arg(region_id) AND v.product_id = p.id
       ) agg ON TRUE
 WHERE p.id = sqlc.arg(product_id) AND p.deleted_at IS NULL;

-- name: DelistProductInStore :exec
-- 这家店不卖这件商品：写一行排除表。
--
-- ON CONFLICT DO UPDATE 而不是 DO NOTHING：updated_by 与 updated_at 要跟着刷新，
-- 而运营最常问的问题正是「这件商品在这家店为什么不卖了、是谁什么时候干的」。
-- DO NOTHING 会让第二次下架静默保留第一次的署名。
INSERT INTO store_product_overrides (store_id, product_id, status, updated_by)
VALUES (sqlc.arg(store_id), sqlc.arg(product_id), 0, sqlc.narg(updated_by))
ON CONFLICT (store_id, product_id)
DO UPDATE SET updated_by = excluded.updated_by, updated_at = now();

-- name: RelistProductInStore :exec
-- 重新上架就是删掉那一行，**不加 deleted_at**：给一张排除表再加软删标记
-- 等于让「不卖」有两种写法，而查询要同时认得两种。
--
-- 本来就没有那一行时也成功（PUT 是幂等的，客户端不必先知道当前状态）。
DELETE FROM store_product_overrides WHERE store_id = $1 AND product_id = $2;

-- name: DelistProductInRegion :exec
-- 大区那一层。**这一层排掉的，下面的门店捞不回来。**
INSERT INTO region_product_overrides (region_id, product_id, status, updated_by)
VALUES (sqlc.arg(region_id), sqlc.arg(product_id), 0, sqlc.narg(updated_by))
ON CONFLICT (region_id, product_id)
DO UPDATE SET updated_by = excluded.updated_by, updated_at = now();

-- name: RelistProductInRegion :exec
DELETE FROM region_product_overrides WHERE region_id = $1 AND product_id = $2;

-- ===========================================================================
-- 门店维度的库存
-- ===========================================================================

-- name: AdminListStoreInventorySKUs :many
-- 这家店的库存清单（GET /admin/stores/{store_id}/inventories）的 SKU 那一半。
--
-- **缺一行等于可售 0，不等于「这家店不卖」**（数据模型 §4 把这条写死了），所以驱动表
-- 仍然是 skus —— 一家刚开的店在录库存之前每个 SKU 都缺行，清单要把它们显示成 0 而不是漏掉。
--
-- 水位本轮（微服务拆分阶段 1a）由库存服务回答，service 按这一页的 sku_id 批量问一次再合并
-- （缺行记 0 / 0，updated_at 回落到 SKU 自己的 updated_at，与拆分前的 COALESCE 同一个口径）。
--
-- low_stock_only 的判据仍是「水位 ≤ warning_qty」（缺行时 0 ≤ 0 成立，算低库存）。
-- 它要影响分页，所以不能在取完一页之后再过滤：service 先向库存服务要这家店
-- 「水位高于预警线」的 SKU（InvHealthySKUIDs），作为 exclude_sku_ids 递进来 ——
-- 低库存 = 未软删 SKU 除掉这批，与拆分前的 WHERE 逐点相同，total 与分页都精确。
-- 不筛时传空数组。代价：这家店健康 SKU 很多时这个数组会长（门店 × SKU 数量级的上限），
-- 这是后台低频页面，可以接受。
SELECT s.id AS sku_id, s.sku_code, s.updated_at
  FROM skus s
 WHERE s.deleted_at IS NULL
   AND NOT (s.id = ANY(sqlc.arg(exclude_sku_ids)::bigint[]))
 ORDER BY s.id
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: AdminCountStoreInventorySKUs :one
-- 条件必须与 AdminListStoreInventorySKUs 逐字一致。
SELECT count(*)
  FROM skus s
 WHERE s.deleted_at IS NULL
   AND NOT (s.id = ANY(sqlc.arg(exclude_sku_ids)::bigint[]));

-- name: SKUSellableInStore :one
-- 这家店此刻能不能给这个 SKU 录库存：SKU 可见且未软删、门店可见且未软删、
-- 这家店与它所在大区都没有下架这件商品。
--
-- 拆分前它是 SetStoreInventoryByCAS / AdjustStoreInventory 里的 sellable 那个 CTE，
-- 与写在同一个快照里。本轮（微服务拆分阶段 1a）写搬到了库存服务，判定留在 core
-- （它要 JOIN skus / stores / 两张覆盖表，库存库里没有那些表）：service 先调它，
-- 不成立回 404，成立再调库存服务写。两步之间的窗口里商品被下架，结果是多录一次
-- 库存 —— 没有任何地方会用到它，而下单那一侧自己会判「这家店卖不卖」。
-- 条件与拆分前那个 CTE 逐字一致。
SELECT EXISTS (
    SELECT 1
      FROM skus sk
      JOIN stores st ON st.id = sqlc.arg(store_id)
     WHERE sk.id = sqlc.arg(sku_id) AND sk.deleted_at IS NULL
       AND st.deleted_at IS NULL
       AND NOT EXISTS (SELECT 1 FROM store_product_overrides o
                        WHERE o.store_id = st.id AND o.product_id = sk.product_id
                          AND o.status = 0)
       AND NOT EXISTS (SELECT 1 FROM region_product_overrides o2
                        WHERE o2.region_id = st.region_id AND o2.product_id = sk.product_id
                          AND o2.status = 0)
)::boolean AS sellable;

-- name: CountStoresForTenant :one
-- 那条**不带门店**的库存 CAS（PUT /admin/skus/{sku_id}/inventory）用它。
--
-- 契约把那条路径的语义写死成：**本租户恰好有一家未软删的门店时，
-- 它就是那一家；否则 409 store-ambiguous**。不是「落到默认门店」——
-- 库存是唯一真相，猜错一家店的后果是把另一家店的水位覆盖掉，
-- 而且没有任何东西会响。所以这里回的是「几家」与「哪一家」，
-- 由调用方判断能不能用，而不是在 SQL 里挑一家出来。
SELECT count(*)::int             AS store_count,
       COALESCE(min(st.id), 0)::bigint AS only_store_id
  FROM stores st
 WHERE st.deleted_at IS NULL;

-- name: GetStoreEffectivePrice :one
-- 一个 SKU 在这家店的**生效价**与它来自哪一层（1 基准 / 2 大区 / 3 门店）。
--
-- 走视图，不碰两张底表 —— 那是 scripts/check_query_tenancy.py 本轮新加的那条
-- 闸门，也是「就近生效只有一份实现」这条纪律在这一层的兑现。
SELECT price_cents, price_source
  FROM sku_prices_by_store
 WHERE store_id = $1 AND sku_id = $2;

-- name: GetRegionEffectivePrice :one
-- 同上，大区那一层（price_source 只会是 1 或 2）。
--
-- 用的是 sku_prices_by_region 而不是 sku_prices_by_store：大区这一层必须
-- **忽略**门店价，而那张视图的最内层恰恰是门店价。两张视图并存的完整理由
-- 写在 00020 里 sku_prices_by_region 的上方。
SELECT price_cents, price_source
  FROM sku_prices_by_region
 WHERE region_id = $1 AND sku_id = $2;
