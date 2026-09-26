-- name: ListProducts :many
-- 刻意不带 WHERE merchant_id —— 租户由 RLS 在数据库层过滤。
--
-- 这不是偷懒：应用层再加一遍条件会让「RLS 是否真的生效」变得测不出来。
-- 两层都在时，跨租户读不到数据既可能是 RLS 拦住了，也可能只是 WHERE 拦住了，
-- 而 RLS 失效不报错、不变慢、不留痕迹 —— 唯一能发现它的测试恰好被 WHERE 挡住了。
--
-- 进入这条查询的唯一入口是 repository.WithTenant，它保证事务里已经
-- SET LOCAL app.merchant_id；没设的话 current_merchant() 会抛 42501。
--
-- 价格区间是**现算**的（00019 把 products 上那两列冗余价格删了，理由在那个
-- 迁移的文件头），本轮（00020）改成从 sku_prices_by_store 视图算 ——
-- 它的含义因此从「这件商品的基准价区间」变成「**按当前门店**算出来的区间」。
-- 那张视图是全仓库唯一一处写 COALESCE(门店价, 大区价, 基准价) 的地方，
-- 而 db/queries 里禁止直接出现两张价格底表（scripts/check_query_tenancy.py）。
--
-- 两条 NOT EXISTS 是两层可见性排除，**它们是「与」不是「或」**：
-- 大区排掉的，门店捞不回来。缺一行即在售（数据模型 §4 的排除表语义），
-- 所以这里是 anti-join 而不是 semi-join —— 反过来写的话，一家新开的店
-- 什么都不卖，和「开店即营业」正面冲突。
--
-- 两条各是一次按主键的点查，每个候选商品两次。反过来说，包含表在这里更贵：
-- semi-join 的候选集是「这家店的上架清单」，那才是十万行的那张表。
--
-- 为什么 LATERAL 不会把这条查询变成一次全表聚合：ORDER BY 只引用 p 的列，
-- 所以规划器能先按 published_at 取出这一页的 LIMIT 行，再对这几行做嵌套循环 ——
-- 每行多一次 skus 上的索引查找，页大小是 20。本轮实测 EXPLAIN 见下：
--
--     Limit  (actual rows=3)
--       ->  Nested Loop Left Join  (actual rows=3)
--             ->  Sort (products, 3 rows)
--             ->  Aggregate (skus, 每行一次)
SELECT p.id, p.title, p.subtitle,
       COALESCE(agg.min_price, 0)::bigint AS min_price_cents,
       COALESCE(agg.max_price, 0)::bigint AS max_price_cents,
       p.total_stock, p.sales_count, p.status
  FROM products p
  LEFT JOIN LATERAL (
        SELECT min(v.price_cents) AS min_price, max(v.price_cents) AS max_price
          FROM sku_prices_by_store v
         WHERE v.store_id = sqlc.arg(store_id) AND v.product_id = p.id
       ) agg ON TRUE
 WHERE p.deleted_at IS NULL
   AND p.status = 1
   AND NOT EXISTS (SELECT 1 FROM region_product_overrides ro
                    WHERE ro.region_id = sqlc.arg(region_id)
                      AND ro.product_id = p.id AND ro.status = 0)
   AND NOT EXISTS (SELECT 1 FROM store_product_overrides so
                    WHERE so.store_id = sqlc.arg(store_id)
                      AND so.product_id = p.id AND so.status = 0)
 ORDER BY p.published_at DESC NULLS LAST, p.id DESC
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountProducts :one
-- 同样刻意不带 WHERE merchant_id —— 理由与 ListProducts 一模一样。
--
-- 契约的 200 响应是 PageMeta + items，而 PageMeta 的 total 是必填字段。
-- 没有这条查询，total 就只能靠 len(items) 现编，那在「还有下一页」时是错的，
-- 而且错得很安静：客户端据此算出的总页数会少，最后几页谁也翻不到。
--
-- 条件必须与 ListProducts 逐字一致：两边只要有一处不同，total 数的就不是
-- 列表实际会分出来的那批行。
--
-- 它**不需要**那个 LATERAL：价格区间是 SELECT 出来的东西，不是筛选条件，
-- 而这条查询一列都不返回。加上去只会让每一行多做一次聚合，且改不了 count。
-- 两条 NOT EXISTS 则**必须**在：它们是筛选条件，漏掉它们 total 会把这家店
-- 下架掉的商品也数进去，于是最后一页永远翻不满，而客户端会一直重试。
SELECT count(*)
  FROM products p
 WHERE p.deleted_at IS NULL
   AND p.status = 1
   AND NOT EXISTS (SELECT 1 FROM region_product_overrides ro
                    WHERE ro.region_id = sqlc.arg(region_id)
                      AND ro.product_id = p.id AND ro.status = 0)
   AND NOT EXISTS (SELECT 1 FROM store_product_overrides so
                    WHERE so.store_id = sqlc.arg(store_id)
                      AND so.product_id = p.id AND so.status = 0);

-- name: GetProduct :one
-- 商品详情。谓词与 ListProducts 逐字一致（deleted_at IS NULL AND status = 1），
-- 理由和 CountProducts 那条一样：详情页放行的东西比列表多一件，就等于开了一条
-- 「列表里看不见、知道 id 就点得进去」的后门 —— 草稿商品与软删商品会从这里漏出去。
--
-- 同样刻意不带 WHERE merchant_id：租户由 RLS 挡。拿别家店的 product_id 打过来，
-- 这条查询返回 0 行，服务层把它翻成 404 —— 与「这个 id 不存在」同一个响应，
-- 不给探测器留下区分两者的口子。
--
-- 价格区间按门店现算，公式与 ListProducts 逐字一致（00019 + 00020）。
-- 两边写岔的症状是「列表上 129 元，点进去 159 元」，而那看起来像缓存问题。
--
-- 两条 NOT EXISTS 同样要在，而且这里比列表那边更要紧：少了它们，
-- 一件在这家店下架的商品会变成「列表里看不见、知道 id 就点得进去」，
-- 而它的 SKU 还能加购、还能下单 —— 下单那一侧会以 ErrSKUNotSoldInStore 拒掉，
-- 而用户看到的是一个能打开、能选规格、就是下不了单的页面。
SELECT p.id, p.category_id, p.title, p.subtitle, p.description,
       COALESCE(agg.min_price, 0)::bigint AS min_price_cents,
       COALESCE(agg.max_price, 0)::bigint AS max_price_cents,
       p.sales_count, p.status
  FROM products p
  LEFT JOIN LATERAL (
        SELECT min(v.price_cents) AS min_price, max(v.price_cents) AS max_price
          FROM sku_prices_by_store v
         WHERE v.store_id = sqlc.arg(store_id) AND v.product_id = p.id
       ) agg ON TRUE
 WHERE p.id = sqlc.arg(id)
   AND p.deleted_at IS NULL
   AND p.status = 1
   AND NOT EXISTS (SELECT 1 FROM region_product_overrides ro
                    WHERE ro.region_id = sqlc.arg(region_id)
                      AND ro.product_id = p.id AND ro.status = 0)
   AND NOT EXISTS (SELECT 1 FROM store_product_overrides so
                    WHERE so.store_id = sqlc.arg(store_id)
                      AND so.product_id = p.id AND so.status = 0);

-- name: ListProductSKUs :many
-- 一件商品的全部在售 SKU，带上当前可售水位。契约的 ProductDetail.skus。
--
-- s.status = 1 与 ListSKUsForPricing 的那个条件对齐：详情页列出来的 SKU
-- 必须是真的下得了单的那些，否则用户点进去加购再下单才被 422 拒掉，
-- 而那条错误里没有任何东西指向「这个规格已经下架了」。
--
-- LEFT JOIN 而不是 JOIN：种子里的 SKU-NOSTOCKROW 是一个**有 SKU、没有库存行**
-- 的真实状态（下单链路靠它当靶子）。用 JOIN 的话这类 SKU 会整个从详情里消失，
-- 而它真实的样子是「在售、可售 0 件」。COALESCE 把「没有库存行」记成 0 ——
-- 这一次两者确实同义：都表示一件也买不到。
--
-- 本轮（00020）两处跟着门店走：价从 sku_prices_by_store 取（那是唯一一处
-- 三层 COALESCE），水位按 (sku_id, store_id) 取 —— 漏掉 store_id 的话
-- 这条 JOIN 会匹配到该 SKU 在**所有**门店的行，详情页上的可售数会变成
-- 一个把五家店加在一起、谁也买不到那么多的数字。
--
-- 视图那一侧是 JOIN 而不是 LEFT JOIN：它对每一个 (未软删门店 × 未软删 SKU)
-- 都恰好有一行，缺行意味着这家店或这个 SKU 已经不在了，而那时这一行本来
-- 就不该出现在详情里。inventories 那一侧仍然是 LEFT JOIN（缺行 ≡ 可售 0）。
--
-- s.deleted_at IS NULL 是 M4（00018 给 skus 补软删）时加上的。软删的规格要从
-- **所有**视图里消失，而这一条恰好是买家看到规格矩阵的那个视图 —— 漏掉它，
-- 一个被商家删掉的规格照样出现在详情页上，点进去才在下单时被拒。
SELECT s.id, s.sku_code, s.spec_values, v.price_cents, s.image_url,
       COALESCE(i.available_qty, 0)::int AS available_qty
  FROM skus s
  JOIN sku_prices_by_store v ON v.sku_id = s.id AND v.store_id = sqlc.arg(store_id)
  LEFT JOIN inventories i ON i.sku_id = s.id AND i.store_id = sqlc.arg(store_id)
 WHERE s.product_id = sqlc.arg(product_id)
   AND s.status = 1
   AND s.deleted_at IS NULL
 ORDER BY s.id;
