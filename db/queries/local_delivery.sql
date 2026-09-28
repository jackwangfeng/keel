-- 同城配送（00110 / 00111）：有围栏、不是默认店的门店按它收配送费。

-- name: GetStoreLocalDelivery :one
-- 后台读一家店自己的那一行。没有这一行由 repository 翻成「跟随默认模板」。
SELECT min_order_cents, free_over_cents, fee_tiers, template_id, updated_at
  FROM store_local_delivery
 WHERE store_id = $1;

-- name: UpsertStoreLocalDelivery :one
-- 整份替换（PUT 语义）。template_id 非空 = 引用模板，此时自己的三个数原样保留（切回自定义时还在）。
-- merchant_id 取 current_merchant() 的列默认值，FK 保证门店与模板都属于本店。
INSERT INTO store_local_delivery (store_id, min_order_cents, free_over_cents, fee_tiers, template_id)
VALUES (sqlc.arg(store_id), sqlc.arg(min_order_cents), sqlc.arg(free_over_cents), sqlc.arg(fee_tiers),
        sqlc.narg(template_id))
ON CONFLICT ON CONSTRAINT store_local_delivery_pkey DO UPDATE
   SET min_order_cents = CASE WHEN EXCLUDED.template_id IS NULL THEN EXCLUDED.min_order_cents
                              ELSE store_local_delivery.min_order_cents END,
       free_over_cents = CASE WHEN EXCLUDED.template_id IS NULL THEN EXCLUDED.free_over_cents
                              ELSE store_local_delivery.free_over_cents END,
       fee_tiers       = CASE WHEN EXCLUDED.template_id IS NULL THEN EXCLUDED.fee_tiers
                              ELSE store_local_delivery.fee_tiers END,
       template_id     = EXCLUDED.template_id
RETURNING store_id;

-- name: DeleteStoreLocalDelivery :exec
-- 「跟随默认模板」：删掉这家店自己的那一行。
DELETE FROM store_local_delivery WHERE store_id = $1;

-- name: LocalDeliveryForPricing :one
-- 计价用：这家店走不走同城配送（有围栏且不是默认店）、生效的配置从哪来、门店到收货坐标的球面距离（米）。
--
-- 来源（source）：有这家店的行且引用模板 → template；有行不引用 → custom；没有行 → 默认模板（default_template）；
-- 连默认模板都没有 → none（全 0）。引用的模板一定存在（FK RESTRICT）。
-- 距离与 ResolveStoresByFence 的 distance_m 同一个算法（geography 上的 ST_Distance）：买家在首页看到的
-- 距离与结算时计费的距离一致。算不出（没传坐标、门店没坐标）给哨兵 -1，repository 翻成 nil。
SELECT (st.fence IS NOT NULL AND NOT st.is_default)::boolean AS local,
       (CASE WHEN d.store_id IS NOT NULL AND d.template_id IS NULL THEN 'custom'
             WHEN d.template_id IS NOT NULL THEN 'template'
             WHEN t.id IS NOT NULL THEN 'default_template'
             ELSE 'none' END)::text                                           AS source,
       t.id                                                                    AS template_id,
       t.name                                                                  AS template_name,
       (CASE WHEN d.store_id IS NOT NULL AND d.template_id IS NULL THEN d.min_order_cents
             ELSE COALESCE(t.min_order_cents, 0) END)::bigint                  AS min_order_cents,
       (CASE WHEN d.store_id IS NOT NULL AND d.template_id IS NULL THEN d.free_over_cents
             ELSE COALESCE(t.free_over_cents, 0) END)::bigint                  AS free_over_cents,
       (CASE WHEN d.store_id IS NOT NULL AND d.template_id IS NULL THEN d.fee_tiers
             ELSE COALESCE(t.fee_tiers, '[]'::jsonb) END)::jsonb               AS fee_tiers,
       (CASE WHEN st.location IS NULL OR NOT sqlc.arg(has_point)::boolean THEN -1
             ELSE ST_Distance(st.location,
                              ST_SetSRID(ST_MakePoint(sqlc.arg(lng)::float8, sqlc.arg(lat)::float8),
                                         4326)::geography)
        END)::float8                                                           AS distance_m
  FROM stores st
  LEFT JOIN store_local_delivery d ON d.store_id = st.id
  LEFT JOIN local_delivery_templates t
         ON (d.template_id IS NOT NULL AND t.id = d.template_id)
         OR (d.store_id IS NULL AND t.is_default)
 WHERE st.id = sqlc.arg(store_id) AND st.deleted_at IS NULL;

-- ===========================================================================
-- 模板
-- ===========================================================================

-- name: ListLocalDeliveryTemplates :many
-- 全部模板（一家店至多几十个，不分页），带「几家门店在用」：显式引用的 + 默认模板时没有自己那一行的围栏店。
SELECT t.id, t.name, t.is_default, t.min_order_cents, t.free_over_cents, t.fee_tiers, t.created_at, t.updated_at,
       ((SELECT count(*) FROM store_local_delivery d WHERE d.template_id = t.id)
        + (CASE WHEN t.is_default THEN
             (SELECT count(*) FROM stores st
               WHERE st.deleted_at IS NULL AND st.fence IS NOT NULL AND NOT st.is_default
                 AND NOT EXISTS (SELECT 1 FROM store_local_delivery d2 WHERE d2.store_id = st.id))
           ELSE 0 END))::bigint AS store_count
  FROM local_delivery_templates t
 ORDER BY t.is_default DESC, t.id;

-- name: GetLocalDeliveryTemplate :one
SELECT id, name, is_default, min_order_cents, free_over_cents, fee_tiers, created_at, updated_at
  FROM local_delivery_templates
 WHERE id = $1;

-- name: InsertLocalDeliveryTemplate :one
INSERT INTO local_delivery_templates (name, is_default, min_order_cents, free_over_cents, fee_tiers)
VALUES (sqlc.arg(name), sqlc.arg(is_default), sqlc.arg(min_order_cents), sqlc.arg(free_over_cents), sqlc.arg(fee_tiers))
RETURNING id;

-- name: UpdateLocalDeliveryTemplate :execrows
UPDATE local_delivery_templates
   SET name = sqlc.arg(name), is_default = sqlc.arg(is_default), min_order_cents = sqlc.arg(min_order_cents),
       free_over_cents = sqlc.arg(free_over_cents), fee_tiers = sqlc.arg(fee_tiers)
 WHERE id = sqlc.arg(id);

-- name: ClearOtherDefaultLocalDeliveryTemplates :exec
-- 设一个为默认之前先清掉别的（uk_local_delivery_templates_default 保证至多一个；同一事务里先清再置）。
UPDATE local_delivery_templates SET is_default = FALSE WHERE is_default AND id <> sqlc.arg(keep_id);

-- name: CountStoresReferencingTemplate :one
SELECT count(*) FROM store_local_delivery WHERE template_id = $1;

-- name: DeleteLocalDeliveryTemplate :execrows
DELETE FROM local_delivery_templates WHERE id = $1;
