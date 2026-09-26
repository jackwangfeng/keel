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
-- 迁移的文件头）。公式与别的三条读路径逐字一致：未软删的 SKU，COALESCE 到 0。
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
        SELECT min(s.price_cents) AS min_price, max(s.price_cents) AS max_price
          FROM skus s
         WHERE s.product_id = p.id AND s.deleted_at IS NULL
       ) agg ON TRUE
 WHERE p.deleted_at IS NULL
   AND p.status = 1
 ORDER BY p.published_at DESC NULLS LAST, p.id DESC
 LIMIT $1 OFFSET $2;

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
SELECT count(*)
  FROM products
 WHERE deleted_at IS NULL
   AND status = 1;

-- name: GetProduct :one
-- 商品详情。谓词与 ListProducts 逐字一致（deleted_at IS NULL AND status = 1），
-- 理由和 CountProducts 那条一样：详情页放行的东西比列表多一件，就等于开了一条
-- 「列表里看不见、知道 id 就点得进去」的后门 —— 草稿商品与软删商品会从这里漏出去。
--
-- 同样刻意不带 WHERE merchant_id：租户由 RLS 挡。拿别家店的 product_id 打过来，
-- 这条查询返回 0 行，服务层把它翻成 404 —— 与「这个 id 不存在」同一个响应，
-- 不给探测器留下区分两者的口子。
--
-- 价格区间现算，公式与 ListProducts 逐字一致（00019）。两边写岔的症状是
-- 「列表上 129 元，点进去 159 元」，而那看起来像缓存问题。
SELECT p.id, p.category_id, p.title, p.subtitle, p.description,
       COALESCE(agg.min_price, 0)::bigint AS min_price_cents,
       COALESCE(agg.max_price, 0)::bigint AS max_price_cents,
       p.sales_count, p.status
  FROM products p
  LEFT JOIN LATERAL (
        SELECT min(s.price_cents) AS min_price, max(s.price_cents) AS max_price
          FROM skus s
         WHERE s.product_id = p.id AND s.deleted_at IS NULL
       ) agg ON TRUE
 WHERE p.id = $1
   AND p.deleted_at IS NULL
   AND p.status = 1;

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
-- inventories 没有 merchant_id 列（parent-scoped，00006），它的 RLS 谓词是对
-- skus 的 EXISTS 子查询，所以这条 JOIN 同样在 RLS 之下。
--
-- s.deleted_at IS NULL 是 M4（00018 给 skus 补软删）时加上的。软删的规格要从
-- **所有**视图里消失，而这一条恰好是买家看到规格矩阵的那个视图 —— 漏掉它，
-- 一个被商家删掉的规格照样出现在详情页上，点进去才在下单时被拒。
SELECT s.id, s.sku_code, s.spec_values, s.price_cents, s.image_url,
       COALESCE(i.available_qty, 0)::int AS available_qty
  FROM skus s
  LEFT JOIN inventories i ON i.sku_id = s.id
 WHERE s.product_id = $1
   AND s.status = 1
   AND s.deleted_at IS NULL
 ORDER BY s.id;
