-- 消息通知（数据模型 §16，迁移 00053）：写入（outbox 那一半）、买家消息中心、
-- 后台铃铛、外发投递记录、保留期清理。
--
-- 租户由 RLS 过滤，这里一处都不写（check_query_tenancy.py）。
-- 注释里不许有反引号，理由见 db/queries/inventories.sql 的第三条说明。
--
-- ### 同一租户内的范围过滤（后台那几条）
--
-- only_region_ids / only_store_ids 与 db/queries/admin_orders.sql 同一个约定：
-- NULL 即不限（管理员、操作员），空数组即一个都不给；值由 service/authz.go 的
-- orderListScope 给出 —— 铃铛里看得见的，正是订单 / 售后列表里看得见的那些门店的事。
-- 大区按门店**此刻**所属的大区展开，含已软删的门店（理由同订单列表）。

-- name: InsertNotification :one
-- 写一条通知。跑在**做状态变化的那个事务里**（outbox）。
--
-- ON CONFLICT 不写冲突目标，理由同 db/queries/jobs.sql 的 EnqueueJob：冲突目标里
-- 必须出现租户列那个词。这张表上的唯一约束是主键 id（GENERATED ALWAYS，撞不了）、
-- (id, 租户) 那一条（同上）与 uk_notifications_dedupe，所以撞上的只可能是后者 ——
-- 同一件事已经通知过了。那时返回零行（pgx.ErrNoRows），调用方记作「已去重」，
-- 外发任务也不入队。
INSERT INTO notifications (audience, user_id, store_id, kind, title, body,
                           target_type, order_no, refund_no, sku_id, dedupe_key)
VALUES (sqlc.arg(audience), sqlc.narg(user_id), sqlc.narg(store_id), sqlc.arg(kind),
        sqlc.arg(title), sqlc.arg(body), sqlc.arg(target_type), sqlc.narg(order_no),
        sqlc.narg(refund_no), sqlc.narg(sku_id), sqlc.arg(dedupe_key))
ON CONFLICT DO NOTHING
RETURNING id;

-- name: ListUserNotifications :many
-- 买家消息中心，一页。user_id 是**越权过滤**（同一家店里 A 买家看不到 B 买家的），
-- 不是租户过滤。
SELECT id, kind, title, body, target_type, order_no, refund_no, store_id, sku_id,
       read_at, created_at
  FROM notifications
 WHERE audience = 1 AND user_id = sqlc.arg(user_id)
   AND (NOT sqlc.arg(unread_only)::bool OR read_at IS NULL)
 ORDER BY created_at DESC, id DESC
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountUserNotifications :one
-- 条件必须与 ListUserNotifications 逐字一致。
SELECT count(*)
  FROM notifications
 WHERE audience = 1 AND user_id = sqlc.arg(user_id)
   AND (NOT sqlc.arg(unread_only)::bool OR read_at IS NULL);

-- name: CountUserUnreadNotifications :one
-- 买家未读数。走 idx_notifications_user_unread。
SELECT count(*)
  FROM notifications
 WHERE audience = 1 AND user_id = sqlc.arg(user_id) AND read_at IS NULL;

-- name: MarkUserNotificationRead :one
-- 买家标一条已读。返回「这条通知是不是他的」（0 / 1）：不是他的（或不存在）
-- 与「已经读过」要分开 —— 前者 404，后者照样 200（标已读是设置，不是累加）。
WITH target AS (
    SELECT n.id FROM notifications n
     WHERE n.id = sqlc.arg(id) AND n.audience = 1 AND n.user_id = sqlc.arg(user_id)
), marked AS (
    UPDATE notifications u SET read_at = now()
      FROM target
     WHERE u.id = target.id AND u.read_at IS NULL
    RETURNING u.id
)
SELECT count(*) FROM target;

-- name: MarkAllUserNotificationsRead :execrows
-- 买家全部标已读。
UPDATE notifications SET read_at = now()
 WHERE audience = 1 AND user_id = sqlc.arg(user_id) AND read_at IS NULL;

-- name: ListStaffNotifications :many
-- 后台铃铛，一页：这家店的商家通知，按员工的门店范围收窄，read_at 是**这个员工**的已读时间。
SELECT n.id, n.kind, n.title, n.body, n.target_type, n.order_no, n.refund_no,
       n.store_id, n.sku_id, r.read_at, n.created_at
  FROM notifications n
  LEFT JOIN notification_reads r
         ON r.notification_id = n.id AND r.staff_id = sqlc.arg(staff_id)
 WHERE n.audience = 2
   AND (NOT sqlc.arg(unread_only)::bool OR r.read_at IS NULL)
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR n.store_id IN (SELECT st.id FROM stores st
                           WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR n.store_id = ANY(sqlc.narg(only_store_ids)::bigint[]))
 ORDER BY n.created_at DESC, n.id DESC
 LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountStaffNotifications :one
-- 条件必须与 ListStaffNotifications 逐字一致。
SELECT count(*)
  FROM notifications n
  LEFT JOIN notification_reads r
         ON r.notification_id = n.id AND r.staff_id = sqlc.arg(staff_id)
 WHERE n.audience = 2
   AND (NOT sqlc.arg(unread_only)::bool OR r.read_at IS NULL)
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR n.store_id IN (SELECT st.id FROM stores st
                           WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR n.store_id = ANY(sqlc.narg(only_store_ids)::bigint[]));

-- name: MarkStaffNotificationRead :one
-- 员工标一条已读。返回「这条通知在不在他的范围里」（0 / 1）：不在（或不存在）404，
-- 已经读过照样 200。范围谓词与 ListStaffNotifications 逐字一致 —— 铃铛里看不见的，
-- 也标不了。
WITH target AS (
    SELECT n.id FROM notifications n
     WHERE n.id = sqlc.arg(id) AND n.audience = 2
       AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
            OR n.store_id IN (SELECT st.id FROM stores st
                               WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
       AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
            OR n.store_id = ANY(sqlc.narg(only_store_ids)::bigint[]))
), marked AS (
    INSERT INTO notification_reads (notification_id, staff_id)
    SELECT target.id, sqlc.arg(staff_id) FROM target
    ON CONFLICT DO NOTHING
    RETURNING notification_id
)
SELECT count(*) FROM target;

-- name: MarkAllStaffNotificationsRead :execrows
-- 员工把范围内的商家通知全部标已读。范围谓词同上。
INSERT INTO notification_reads (notification_id, staff_id)
SELECT n.id, sqlc.arg(staff_id)
  FROM notifications n
 WHERE n.audience = 2
   AND NOT EXISTS (SELECT 1 FROM notification_reads r
                    WHERE r.notification_id = n.id AND r.staff_id = sqlc.arg(staff_id))
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR n.store_id IN (SELECT st.id FROM stores st
                           WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR n.store_id = ANY(sqlc.narg(only_store_ids)::bigint[]))
ON CONFLICT DO NOTHING;

-- name: GetNotificationForDelivery :one
-- 外发 worker 读回它要投递的那一条。
SELECT id, audience, user_id, store_id, kind, title, body, target_type,
       order_no, refund_no, sku_id, created_at
  FROM notifications
 WHERE id = $1;

-- name: ListFinishedDeliveryChannels :many
-- 这条通知已经有定论的渠道（发出 / 未配置跳过）。重试时这几路不再碰 ——
-- 一条短信发出去了、邮件失败了，重试只该重发邮件。
SELECT DISTINCT channel
  FROM notification_deliveries
 WHERE notification_id = $1 AND status IN (1, 2);

-- name: InsertNotificationDelivery :exec
-- 记一次外发尝试。
INSERT INTO notification_deliveries (notification_id, channel, status, attempt, detail)
VALUES (sqlc.arg(notification_id), sqlc.arg(channel), sqlc.arg(status), sqlc.arg(attempt),
        sqlc.narg(detail));

-- name: PurgeExpiredNotifications :execrows
-- 保留期清理：删掉 cutoff 之前的通知，一次至多 batch 条。reads / deliveries 级联。
-- 有界的 DELETE：清理永远不会某一次突然锁住半张表（同 jobs 的 PurgeFinishedJobs）。
DELETE FROM notifications
 WHERE id IN (SELECT n.id FROM notifications n
               WHERE n.created_at < sqlc.arg(cutoff)
               ORDER BY n.created_at
               LIMIT sqlc.arg(batch));

-- name: ListAutoConfirmReminders :many
-- 「自动确认收货即将到期」的候选：仍停在 30、发货早于 remind_before、没有在途售后、
-- 还没提醒过的订单。remind_before = now - (N - 1) 天，即到期前一天起进入候选。
--
-- 「还没提醒过」查的是 uk_notifications_dedupe 那一格：dedupe_key 的形状与
-- service/notification.go 里 notifyAutoConfirmSoon 写的那一个逐字一致。
-- 有在途售后的单不提醒：自动确认对它是暂停的（service/auto_confirm.go 文件头），
-- 告诉买家「明天要自动确认了」是一句假话。
-- 渠道单（00320，user_id 为空）没有 keel 买家可提醒，排除掉：否则它每轮都是候选、
-- 却永远写不出那条去重通知，白占批次。
SELECT o.id, o.order_no, o.user_id, o.shipped_at
  FROM orders o
 WHERE o.status = 30 AND o.shipped_at < sqlc.arg(remind_before)
   AND o.user_id IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM refunds r
                    WHERE r.order_id = o.id AND r.status IN (10, 20, 30))
   AND NOT EXISTS (SELECT 1 FROM notifications n
                    WHERE n.dedupe_key = 'order_auto_confirm_soon:' || o.order_no)
 ORDER BY o.shipped_at
 LIMIT sqlc.arg(batch);

-- name: GetLowStockContext :one
-- 库存预警要的展示上下文：商品名、规格、门店名。预警线与水位是库存服务的数（下单 SAGA 的
-- 收尾分支从库存服务的流水里取，service/order_saga.go），这里只补 core 自己的名字 ——
-- 拆分前这条语句 JOIN 了 inventories，库存搬走之后它只碰 core 的表（微服务拆分阶段 1b）。
-- 查不到（SKU 或门店被硬删，正常路径上没有硬删）即 ErrNotificationNotFound。
SELECT p.title AS product_title, s.spec_values::text AS spec_values, st.name AS store_name
  FROM skus s
  JOIN products p ON p.id = s.product_id
  JOIN stores st  ON st.id = sqlc.arg(store_id)
 WHERE s.id = sqlc.arg(sku_id);
