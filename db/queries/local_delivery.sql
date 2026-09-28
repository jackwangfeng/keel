-- 同城配送（00110）：有围栏、不是默认店的门店按它收配送费。

-- name: GetStoreLocalDelivery :one
-- 后台读一家店的配置。没有这一行由 repository 翻成全 0（不设起送价、配送费 0）。
SELECT min_order_cents, free_over_cents, fee_tiers, updated_at
  FROM store_local_delivery
 WHERE store_id = $1;

-- name: UpsertStoreLocalDelivery :one
-- 整份替换（PUT 语义）。merchant_id 取 current_merchant() 的列默认值，FK 保证门店属于本店。
INSERT INTO store_local_delivery (store_id, min_order_cents, free_over_cents, fee_tiers)
VALUES (sqlc.arg(store_id), sqlc.arg(min_order_cents), sqlc.arg(free_over_cents), sqlc.arg(fee_tiers))
ON CONFLICT ON CONSTRAINT store_local_delivery_pkey DO UPDATE
   SET min_order_cents = EXCLUDED.min_order_cents,
       free_over_cents = EXCLUDED.free_over_cents,
       fee_tiers       = EXCLUDED.fee_tiers
RETURNING min_order_cents, free_over_cents, fee_tiers, updated_at;

-- name: LocalDeliveryForPricing :one
-- 计价用：这家店走不走同城配送（有围栏且不是默认店）、它的配置、门店到收货坐标的球面距离（米）。
-- 距离与 ResolveStoresByFence 的 distance_m 同一个算法（geography 上的 ST_Distance），买家在首页看到的
-- 距离与结算时计费的距离一致。算不出（没传坐标、门店没坐标）给哨兵 -1，repository 翻成 nil。
SELECT (st.fence IS NOT NULL AND NOT st.is_default)::boolean AS local,
       COALESCE(d.min_order_cents, 0)::bigint                AS min_order_cents,
       COALESCE(d.free_over_cents, 0)::bigint                AS free_over_cents,
       COALESCE(d.fee_tiers, '[]'::jsonb)                    AS fee_tiers,
       (CASE WHEN st.location IS NULL OR NOT sqlc.arg(has_point)::boolean THEN -1
             ELSE ST_Distance(st.location,
                              ST_SetSRID(ST_MakePoint(sqlc.arg(lng)::float8, sqlc.arg(lat)::float8),
                                         4326)::geography)
        END)::float8                                          AS distance_m
  FROM stores st
  LEFT JOIN store_local_delivery d ON d.store_id = st.id
 WHERE st.id = sqlc.arg(store_id) AND st.deleted_at IS NULL;
