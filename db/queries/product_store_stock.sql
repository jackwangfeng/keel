-- 门店 × 商品有没有货的冗余标记（00087），只给商品列表排序用。刷新见 service/stock_flags.go。
--
-- 和这个目录里别的文件一样，**一个 merchant_id 都没有** —— 租户由 RLS 在数据库层过滤。

-- name: UpsertProductStoreStock :exec
-- 一家店的一批商品整批写。只在值变了时 UPDATE：每分钟一轮全量刷新，没变的行不该每轮都写一次。
INSERT INTO product_store_stock (store_id, product_id, in_stock)
-- 两个数组在 SELECT 列表里并排 unnest（PG10 起按位置配对）：sqlc 认不了 FROM unnest(a, b) 的多参形式。
SELECT sqlc.arg(store_id)::bigint,
       unnest(sqlc.arg(product_ids)::bigint[]),
       unnest(sqlc.arg(in_stocks)::boolean[])
ON CONFLICT (store_id, product_id) DO UPDATE
   SET in_stock = EXCLUDED.in_stock
 WHERE product_store_stock.in_stock IS DISTINCT FROM EXCLUDED.in_stock;

-- name: ListOnSaleSKUsForStockFlags :many
-- 全部在架商品的在售 SKU（全量刷新用）。判据与 ListOnSaleSKUsOfProducts 相同，只是不按商品过滤。
SELECT s.product_id, s.id
  FROM skus s
  JOIN products p ON p.id = s.product_id
 WHERE s.deleted_at IS NULL AND s.status = 1
   AND p.deleted_at IS NULL AND p.status = 1
 ORDER BY s.product_id, s.id;

-- name: ListStoreIDsForStockFlags :many
-- 要刷新的门店：未软删的都刷（停业的也刷 —— 重新营业那一刻排序就是对的）。
SELECT st.id FROM stores st WHERE st.deleted_at IS NULL ORDER BY st.id;

-- name: ProductOfSKU :one
SELECT s.product_id FROM skus s WHERE s.id = $1;
