-- name: ListProducts :many
-- 刻意不带 WHERE merchant_id —— 租户由 RLS 在数据库层过滤。
--
-- 这不是偷懒：应用层再加一遍条件会让「RLS 是否真的生效」变得测不出来。
-- 两层都在时，跨租户读不到数据既可能是 RLS 拦住了，也可能只是 WHERE 拦住了，
-- 而 RLS 失效不报错、不变慢、不留痕迹 —— 唯一能发现它的测试恰好被 WHERE 挡住了。
--
-- 进入这条查询的唯一入口是 repository.WithTenant，它保证事务里已经
-- SET LOCAL app.merchant_id；没设的话 current_merchant() 会抛 42501。
SELECT id, title, subtitle, min_price_cents, max_price_cents,
       total_stock, sales_count, status
  FROM products
 WHERE deleted_at IS NULL
   AND status = 1
 ORDER BY published_at DESC NULLS LAST, id DESC
 LIMIT $1 OFFSET $2;
