-- 通知的第四种跳转目标 channel_orders（第三期 Task 6）：门店的渠道订单页。
--
-- 等人接单的渠道单（AcceptRequired 的渠道，接单前没有 keel 订单）与平台发起的申请要提醒门店，
-- 而 chk_notification_target 的 'order' 要 order_no 非空 —— 这些单还没有 keel 订单号。
-- 新目标只要 store_id（与 'inventory' 一样落在门店上），正文里写明是哪张平台单。
--
-- notifications 是大表：新约束 NOT VALID 加上（短锁、不扫表），换掉旧的，再 VALIDATE（只拿
-- SHARE UPDATE EXCLUSIVE，扫表期间读写照常）。NO TRANSACTION：同一个事务里 ADD 紧跟 VALIDATE 会在扫表期间
-- 一直攥着 ADD 拿的 ACCESS EXCLUSIVE（理由同 00322），这里逐句提交，于是一个文件里就能做完。
-- 新约束比旧的宽（旧的三种照旧），存量行校验必过。
-- +goose NO TRANSACTION
-- +goose Up
ALTER TABLE notifications ADD CONSTRAINT chk_notification_target_v2 CHECK (
    (target_type = 'order'     AND order_no IS NOT NULL)
    OR (target_type = 'refund' AND refund_no IS NOT NULL)
    OR (target_type = 'inventory' AND store_id IS NOT NULL AND sku_id IS NOT NULL)
    OR (target_type = 'channel_orders' AND store_id IS NOT NULL)
) NOT VALID;
ALTER TABLE notifications DROP CONSTRAINT chk_notification_target;
ALTER TABLE notifications VALIDATE CONSTRAINT chk_notification_target_v2;
ALTER TABLE notifications RENAME CONSTRAINT chk_notification_target_v2 TO chk_notification_target;

-- +goose Down
DELETE FROM notification_deliveries WHERE notification_id IN (SELECT id FROM notifications WHERE target_type = 'channel_orders');
DELETE FROM notifications WHERE target_type = 'channel_orders';
ALTER TABLE notifications ADD CONSTRAINT chk_notification_target_v1 CHECK (
    (target_type = 'order'     AND order_no IS NOT NULL)
    OR (target_type = 'refund' AND refund_no IS NOT NULL)
    OR (target_type = 'inventory' AND store_id IS NOT NULL AND sku_id IS NOT NULL)
) NOT VALID;
ALTER TABLE notifications DROP CONSTRAINT chk_notification_target;
ALTER TABLE notifications VALIDATE CONSTRAINT chk_notification_target_v1;
ALTER TABLE notifications RENAME CONSTRAINT chk_notification_target_v1 TO chk_notification_target;
