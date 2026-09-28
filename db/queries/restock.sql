-- 补货计算（AI 经营 M9 任务 3，service/restock.go）的 core 这一半：候选 SKU 与已付款销量。
-- 水位与断货天数在库存服务那边（inventory.Service.StoreStock / StockoutDays）。
-- 和这个目录里别的文件一样，一个 merchant_id 都没有：租户由 RLS 过滤。

-- name: RestockCandidates :many
-- 全部在售 SKU（商品在架、SKU 在售、都没软删），带人读的标签与上架时间（有效样本天数不能早于它）。
SELECT s.id AS sku_id, s.product_id, p.title AS product_title, s.sku_code,
       COALESCE(s.spec_values::text, '{}')::text AS spec_values, s.created_at
  FROM skus s
  JOIN products p ON p.id = s.product_id
 WHERE s.deleted_at IS NULL AND s.status = 1
   AND p.deleted_at IS NULL AND p.status = 1
 ORDER BY s.product_id, s.id;

-- name: StoreSKUSales :many
-- 一家门店自 since 以来已付款订单的件数，按 SKU。状态 20 已支付 / 30 已发货 / 40 已完成 / 50 售后中都算卖出；
-- 10 待支付、90 已关闭不算；60 已退款的整单退了，也不算。部分退款不扣（件数口径，退款多在事后，补货看的是需求）。
SELECT oi.sku_id, sum(oi.quantity)::bigint AS qty
  FROM order_items oi
  JOIN orders o ON o.id = oi.order_id
 WHERE o.store_id = sqlc.arg(store_id)
   AND o.status IN (20, 30, 40, 50)
   AND o.paid_at >= sqlc.arg(since)
 GROUP BY oi.sku_id;
