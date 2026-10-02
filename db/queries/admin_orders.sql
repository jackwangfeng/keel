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
-- 收货人手机号比的是冗余列 receiver_phone（00170，触发器维护），不是
-- receiver_snapshot->>'phone'：->> 不是 leakproof，RLS 下进不了 Index Cond。
SELECT o.id, o.order_no, o.user_id, o.store_id, o.region_id, o.status,
       o.goods_amount_cents, o.freight_cents, o.freight_discount_cents,
       o.discount_cents, o.payable_cents,
       o.paid_cents, o.refunded_cents, o.refund_status, o.expire_at, o.paid_at,
       o.shipped_at, o.finished_at, o.created_at, o.user_coupon_id, o.coupon_name,
       o.promotion_discount_cents, o.promotions, o.source, o.channel_order_id,
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
        OR o.receiver_phone = sqlc.narg(phone)::text
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
        OR o.receiver_phone = sqlc.narg(phone)::text
        OR o.user_id = (SELECT u.id FROM users u
                         WHERE u.phone = sqlc.narg(phone)::text AND u.deleted_at IS NULL))
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR o.store_id IN (SELECT st.id FROM stores st
                           WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR o.store_id = ANY(sqlc.narg(only_store_ids)::bigint[]));

-- ### 稀疏的精确条件单独成句：ByNo / ByPhone（2026-09-30 架构审查）
--
-- 上面两条是「每个条件都写成 (参数 IS NULL OR ...)」的万能查询。pgx 按语句缓存预备语句，
-- 同一条语句执行几次之后 PostgreSQL 可能改用**通用计划**：通用计划里参数值未知，
-- (参数 IS NULL OR 列 = 参数) 折不掉，于是哪个条件都进不了 Index Cond。实测每店 4 万单、
-- 强制通用计划：按手机号 14.6 ms、按单号 7.7 ms（全店扫、Rows Removed by Filter: 39998）；
-- 定制计划 0.05 ms / 0.03 ms。今天 plan_cache_mode = auto 下多数时候拿到的是定制计划，
-- 但「多数时候」取决于代价估算，不是一个能依赖的性质。
--
-- 单号（全局唯一）与手机号是后台最常用、也最稀疏的两个条件：一个命中至多一行，一个命中
-- 几行。给它们各自一条**不带 IS NULL 分支**的语句，通用计划与定制计划就是同一个 ——
-- 单号走 orders_order_no_key，手机号走 idx_orders_receiver_phone_col 与 idx_orders_user
-- 的 BitmapOr。其余条件（状态、门店、时间、范围）照旧是 narg，对那一两行做过滤，便宜。
--
-- 由 repository 按参数选（admin_order.go 的 AdminListOrders）：有单号走 ByNo（手机号
-- 若也给了，仍作为 narg 条件留在里面）、否则有手机号走 ByPhone、否则走万能那条。
-- 契约与返回形状不变。四条的谓词必须与上面逐字一致（除了被提成必填的那一个）；
-- internal/handler/admin_order_test.go 的筛选用例（单号、两种手机号、单号 + 手机号交叉）
-- 走的正是这两条分支。

-- name: AdminListOrdersByNo :many
-- 同 AdminListOrders，单号必填。
SELECT o.id, o.order_no, o.user_id, o.store_id, o.region_id, o.status,
       o.goods_amount_cents, o.freight_cents, o.freight_discount_cents,
       o.discount_cents, o.payable_cents,
       o.paid_cents, o.refunded_cents, o.refund_status, o.expire_at, o.paid_at,
       o.shipped_at, o.finished_at, o.created_at, o.user_coupon_id, o.coupon_name,
       o.promotion_discount_cents, o.promotions, o.source, o.channel_order_id,
       o.receiver_snapshot, o.store_snapshot,
       EXISTS (SELECT 1 FROM refunds r
                WHERE r.order_id = o.id AND r.status IN (10, 20, 30)) AS has_open_refund
  FROM orders o
 WHERE o.status <> 0
   AND (sqlc.narg(status)::smallint IS NULL OR o.status = sqlc.narg(status)::smallint)
   AND (sqlc.narg(store_id)::bigint IS NULL OR o.store_id = sqlc.narg(store_id)::bigint)
   AND (sqlc.narg(created_from)::timestamptz IS NULL OR o.created_at >= sqlc.narg(created_from)::timestamptz)
   AND (sqlc.narg(created_to)::timestamptz IS NULL OR o.created_at < sqlc.narg(created_to)::timestamptz)
   AND o.order_no = sqlc.arg(order_no)::text
   AND (sqlc.narg(phone)::text IS NULL
        OR o.receiver_phone = sqlc.narg(phone)::text
        OR o.user_id = (SELECT u.id FROM users u
                         WHERE u.phone = sqlc.narg(phone)::text AND u.deleted_at IS NULL))
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR o.store_id IN (SELECT st.id FROM stores st
                           WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR o.store_id = ANY(sqlc.narg(only_store_ids)::bigint[]))
 ORDER BY o.created_at DESC, o.id DESC
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: AdminCountOrdersByNo :one
-- 条件必须与 AdminListOrdersByNo 逐字一致。
SELECT count(*)
  FROM orders o
 WHERE o.status <> 0
   AND (sqlc.narg(status)::smallint IS NULL OR o.status = sqlc.narg(status)::smallint)
   AND (sqlc.narg(store_id)::bigint IS NULL OR o.store_id = sqlc.narg(store_id)::bigint)
   AND (sqlc.narg(created_from)::timestamptz IS NULL OR o.created_at >= sqlc.narg(created_from)::timestamptz)
   AND (sqlc.narg(created_to)::timestamptz IS NULL OR o.created_at < sqlc.narg(created_to)::timestamptz)
   AND o.order_no = sqlc.arg(order_no)::text
   AND (sqlc.narg(phone)::text IS NULL
        OR o.receiver_phone = sqlc.narg(phone)::text
        OR o.user_id = (SELECT u.id FROM users u
                         WHERE u.phone = sqlc.narg(phone)::text AND u.deleted_at IS NULL))
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR o.store_id IN (SELECT st.id FROM stores st
                           WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR o.store_id = ANY(sqlc.narg(only_store_ids)::bigint[]));

-- name: AdminListOrdersByPhone :many
-- 同 AdminListOrders，手机号必填、单号不参与（有单号时走 ByNo）。
SELECT o.id, o.order_no, o.user_id, o.store_id, o.region_id, o.status,
       o.goods_amount_cents, o.freight_cents, o.freight_discount_cents,
       o.discount_cents, o.payable_cents,
       o.paid_cents, o.refunded_cents, o.refund_status, o.expire_at, o.paid_at,
       o.shipped_at, o.finished_at, o.created_at, o.user_coupon_id, o.coupon_name,
       o.promotion_discount_cents, o.promotions, o.source, o.channel_order_id,
       o.receiver_snapshot, o.store_snapshot,
       EXISTS (SELECT 1 FROM refunds r
                WHERE r.order_id = o.id AND r.status IN (10, 20, 30)) AS has_open_refund
  FROM orders o
 WHERE o.status <> 0
   AND (sqlc.narg(status)::smallint IS NULL OR o.status = sqlc.narg(status)::smallint)
   AND (sqlc.narg(store_id)::bigint IS NULL OR o.store_id = sqlc.narg(store_id)::bigint)
   AND (sqlc.narg(created_from)::timestamptz IS NULL OR o.created_at >= sqlc.narg(created_from)::timestamptz)
   AND (sqlc.narg(created_to)::timestamptz IS NULL OR o.created_at < sqlc.narg(created_to)::timestamptz)
   AND (o.receiver_phone = sqlc.arg(phone)::text
        OR o.user_id = (SELECT u.id FROM users u
                         WHERE u.phone = sqlc.arg(phone)::text AND u.deleted_at IS NULL))
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR o.store_id IN (SELECT st.id FROM stores st
                           WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR o.store_id = ANY(sqlc.narg(only_store_ids)::bigint[]))
 ORDER BY o.created_at DESC, o.id DESC
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: AdminCountOrdersByPhone :one
-- 条件必须与 AdminListOrdersByPhone 逐字一致。
SELECT count(*)
  FROM orders o
 WHERE o.status <> 0
   AND (sqlc.narg(status)::smallint IS NULL OR o.status = sqlc.narg(status)::smallint)
   AND (sqlc.narg(store_id)::bigint IS NULL OR o.store_id = sqlc.narg(store_id)::bigint)
   AND (sqlc.narg(created_from)::timestamptz IS NULL OR o.created_at >= sqlc.narg(created_from)::timestamptz)
   AND (sqlc.narg(created_to)::timestamptz IS NULL OR o.created_at < sqlc.narg(created_to)::timestamptz)
   AND (o.receiver_phone = sqlc.arg(phone)::text
        OR o.user_id = (SELECT u.id FROM users u
                         WHERE u.phone = sqlc.arg(phone)::text AND u.deleted_at IS NULL))
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
       o.promotion_discount_cents, o.promotions, o.source, o.channel_order_id,
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
       o.status AS order_status, o.shipped_at AS order_shipped_at, o.store_snapshot,
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
       o.status AS order_status, o.shipped_at AS order_shipped_at, o.store_snapshot,
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
       o.status AS order_status, o.shipped_at AS order_shipped_at, o.store_snapshot,
       r.audited_by, sa.name AS audited_by_name,
       r.received_at, r.received_by, sr.name AS received_by_name
  FROM refunds r
  JOIN orders o   ON o.id = r.order_id
  JOIN payments p ON p.id = r.payment_id
  LEFT JOIN staff sa ON sa.id = r.audited_by
  LEFT JOIN staff sr ON sr.id = r.received_by
 WHERE r.order_id = $1
 ORDER BY r.created_at DESC, r.id DESC;
