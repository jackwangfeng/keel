-- 后台订单与退款单的读：GET /admin/orders、GET /admin/orders/{order_no}、
-- GET /admin/refunds、GET /admin/refunds/{refund_no}（迁移 00035）。
--
-- 租户由 RLS 过滤，这里一处都不写（check_query_tenancy.py）。
-- 注释里不许有反引号，理由见 db/queries/inventories.sql 的第三条说明。
--
-- ### 同一租户内的范围过滤
--
-- only_region_ids / only_store_ids 为 NULL 即不限（管理员、操作员），
-- 空数组即一个都不给（范围被清空了的大区管理员拿到空列表，而不是全部）。
-- 它们是**同一租户内的权限过滤**，不是租户过滤；值由 service/authz.go 的
-- orderListScope 给出，判据与发货、审核用的 authorizeOrderStore 是同一个：
--   大区看门店**此刻**所属的大区（stores.region_id），含已软删的门店 ——
--   店关了，它名下没发完的货、没退完的钱仍然要有人处理；
--   不看订单上冗余的 region_id（那是下单时的大区，定价用的）。
--
-- ### 列表与计数的谓词必须逐字一致
--
-- AdminCountOrders 与 AdminListOrders、AdminCountRefunds 与 AdminListRefunds
-- 各自共用同一段 WHERE。两者分叉的症状是「total = 21 但第二页是空的」。

-- name: AdminListOrders :many
-- 后台订单列表，一页。has_open_refund 是现查的（有没有 10 / 20 / 30 的退款单），
-- 走 idx_refunds_order。
--
-- phone 同时认收货人手机号与买家账号手机号：后者先经 uk_users_phone 换成 user_id
-- （标量子查询；那条唯一索引保证本租户内至多一行），再走 idx_orders_user。
SELECT o.id, o.order_no, o.user_id, o.store_id, o.region_id, o.status,
       o.goods_amount_cents, o.freight_cents, o.freight_discount_cents,
       o.discount_cents, o.payable_cents,
       o.paid_cents, o.refunded_cents, o.refund_status, o.expire_at, o.paid_at,
       o.shipped_at, o.finished_at, o.created_at, o.user_coupon_id, o.coupon_name,
       o.receiver_snapshot, o.store_snapshot,
       EXISTS (SELECT 1 FROM refunds r
                WHERE r.order_id = o.id AND r.status IN (10, 20, 30)) AS has_open_refund
  FROM orders o
 WHERE o.status <> 0
   AND (sqlc.narg(status)::smallint IS NULL OR o.status = sqlc.narg(status)::smallint)
   AND (sqlc.narg(store_id)::bigint IS NULL OR o.store_id = sqlc.narg(store_id)::bigint)
   AND (sqlc.narg(created_from)::timestamptz IS NULL OR o.created_at >= sqlc.narg(created_from)::timestamptz)
   AND (sqlc.narg(created_to)::timestamptz IS NULL OR o.created_at < sqlc.narg(created_to)::timestamptz)
   AND (sqlc.narg(order_no)::text IS NULL OR o.order_no = sqlc.narg(order_no)::text)
   AND (sqlc.narg(phone)::text IS NULL
        OR o.receiver_snapshot->>'phone' = sqlc.narg(phone)::text
        OR o.user_id = (SELECT u.id FROM users u
                         WHERE u.phone = sqlc.narg(phone)::text AND u.deleted_at IS NULL))
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR o.store_id IN (SELECT st.id FROM stores st
                           WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR o.store_id = ANY(sqlc.narg(only_store_ids)::bigint[]))
 ORDER BY o.created_at DESC, o.id DESC
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: AdminCountOrders :one
-- 条件必须与 AdminListOrders 逐字一致。
SELECT count(*)
  FROM orders o
 WHERE o.status <> 0
   AND (sqlc.narg(status)::smallint IS NULL OR o.status = sqlc.narg(status)::smallint)
   AND (sqlc.narg(store_id)::bigint IS NULL OR o.store_id = sqlc.narg(store_id)::bigint)
   AND (sqlc.narg(created_from)::timestamptz IS NULL OR o.created_at >= sqlc.narg(created_from)::timestamptz)
   AND (sqlc.narg(created_to)::timestamptz IS NULL OR o.created_at < sqlc.narg(created_to)::timestamptz)
   AND (sqlc.narg(order_no)::text IS NULL OR o.order_no = sqlc.narg(order_no)::text)
   AND (sqlc.narg(phone)::text IS NULL
        OR o.receiver_snapshot->>'phone' = sqlc.narg(phone)::text
        OR o.user_id = (SELECT u.id FROM users u
                         WHERE u.phone = sqlc.narg(phone)::text AND u.deleted_at IS NULL))
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR o.store_id IN (SELECT st.id FROM stores st
                           WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR o.store_id = ANY(sqlc.narg(only_store_ids)::bigint[]));

-- name: AdminGetOrderByNo :one
-- 后台按单号取一笔订单（没有买家过滤；租户由 RLS 管，门店范围由 service 判）。
-- 列与 AdminListOrders 逐一对齐，行类型可以直接互转。
SELECT o.id, o.order_no, o.user_id, o.store_id, o.region_id, o.status,
       o.goods_amount_cents, o.freight_cents, o.freight_discount_cents,
       o.discount_cents, o.payable_cents,
       o.paid_cents, o.refunded_cents, o.refund_status, o.expire_at, o.paid_at,
       o.shipped_at, o.finished_at, o.created_at, o.user_coupon_id, o.coupon_name,
       o.receiver_snapshot, o.store_snapshot,
       EXISTS (SELECT 1 FROM refunds r
                WHERE r.order_id = o.id AND r.status IN (10, 20, 30)) AS has_open_refund
  FROM orders o
 WHERE o.order_no = $1
   AND o.status <> 0;

-- name: StoreRegionAnyState :one
-- 这家门店此刻所属的大区，**含已软删的门店**。给订单与售后的判权用
-- （authorizeOrderStore）：店关了，它名下的单仍然归那个大区的人处理。
-- 与 StoreExists 分开：那一条给「路径里指名一家门店」的接口，软删的就是 404。
SELECT st.region_id FROM stores st WHERE st.id = $1;

-- name: AdminListRefunds :many
-- 后台退款单列表，一页。门店、订单状态、门店快照从所属订单带出来；
-- 审核人与收货人的名字 LEFT JOIN staff —— 平台级员工在租户作用域里读不到（RLS），
-- 那时只剩 id。
SELECT r.id, r.refund_no, r.order_id, o.order_no, o.store_id, p.payment_no, r.user_id,
       r.refund_type, r.reason_code, r.reason_text, r.evidence_urls,
       r.goods_amount_cents, r.freight_cents, r.amount_cents, r.status, r.channel,
       r.channel_refund_id, r.reject_reason, r.audited_at, r.refunded_at,
       r.created_at, r.updated_at,
       r.return_carrier_code, r.return_tracking_no, r.return_submitted_at,
       o.status AS order_status, o.store_snapshot,
       r.audited_by, sa.name AS audited_by_name,
       r.received_at, r.received_by, sr.name AS received_by_name
  FROM refunds r
  JOIN orders o   ON o.id = r.order_id
  JOIN payments p ON p.id = r.payment_id
  LEFT JOIN staff sa ON sa.id = r.audited_by
  LEFT JOIN staff sr ON sr.id = r.received_by
 WHERE (sqlc.narg(status)::smallint IS NULL OR r.status = sqlc.narg(status)::smallint)
   AND (sqlc.narg(store_id)::bigint IS NULL OR o.store_id = sqlc.narg(store_id)::bigint)
   AND (sqlc.narg(created_from)::timestamptz IS NULL OR r.created_at >= sqlc.narg(created_from)::timestamptz)
   AND (sqlc.narg(created_to)::timestamptz IS NULL OR r.created_at < sqlc.narg(created_to)::timestamptz)
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR o.store_id IN (SELECT st.id FROM stores st
                           WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR o.store_id = ANY(sqlc.narg(only_store_ids)::bigint[]))
 ORDER BY r.created_at DESC, r.id DESC
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: AdminCountRefunds :one
-- 条件必须与 AdminListRefunds 逐字一致。
SELECT count(*)
  FROM refunds r
  JOIN orders o ON o.id = r.order_id
 WHERE (sqlc.narg(status)::smallint IS NULL OR r.status = sqlc.narg(status)::smallint)
   AND (sqlc.narg(store_id)::bigint IS NULL OR o.store_id = sqlc.narg(store_id)::bigint)
   AND (sqlc.narg(created_from)::timestamptz IS NULL OR r.created_at >= sqlc.narg(created_from)::timestamptz)
   AND (sqlc.narg(created_to)::timestamptz IS NULL OR r.created_at < sqlc.narg(created_to)::timestamptz)
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR o.store_id IN (SELECT st.id FROM stores st
                           WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR o.store_id = ANY(sqlc.narg(only_store_ids)::bigint[]));

-- name: AdminGetRefundByNo :one
-- 后台按编号取一张退款单。列与 AdminListRefunds 逐一对齐。
SELECT r.id, r.refund_no, r.order_id, o.order_no, o.store_id, p.payment_no, r.user_id,
       r.refund_type, r.reason_code, r.reason_text, r.evidence_urls,
       r.goods_amount_cents, r.freight_cents, r.amount_cents, r.status, r.channel,
       r.channel_refund_id, r.reject_reason, r.audited_at, r.refunded_at,
       r.created_at, r.updated_at,
       r.return_carrier_code, r.return_tracking_no, r.return_submitted_at,
       o.status AS order_status, o.store_snapshot,
       r.audited_by, sa.name AS audited_by_name,
       r.received_at, r.received_by, sr.name AS received_by_name
  FROM refunds r
  JOIN orders o   ON o.id = r.order_id
  JOIN payments p ON p.id = r.payment_id
  LEFT JOIN staff sa ON sa.id = r.audited_by
  LEFT JOIN staff sr ON sr.id = r.received_by
 WHERE r.refund_no = $1;

-- name: AdminListOrderRefunds :many
-- 一个订单的全部退款单（后台视角，带审核记录），按申请时间倒序。
SELECT r.id, r.refund_no, r.order_id, o.order_no, o.store_id, p.payment_no, r.user_id,
       r.refund_type, r.reason_code, r.reason_text, r.evidence_urls,
       r.goods_amount_cents, r.freight_cents, r.amount_cents, r.status, r.channel,
       r.channel_refund_id, r.reject_reason, r.audited_at, r.refunded_at,
       r.created_at, r.updated_at,
       r.return_carrier_code, r.return_tracking_no, r.return_submitted_at,
       o.status AS order_status, o.store_snapshot,
       r.audited_by, sa.name AS audited_by_name,
       r.received_at, r.received_by, sr.name AS received_by_name
  FROM refunds r
  JOIN orders o   ON o.id = r.order_id
  JOIN payments p ON p.id = r.payment_id
  LEFT JOIN staff sa ON sa.id = r.audited_by
  LEFT JOIN staff sr ON sr.id = r.received_by
 WHERE r.order_id = $1
 ORDER BY r.created_at DESC, r.id DESC;
