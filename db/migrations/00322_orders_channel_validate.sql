-- 校验 00320 在 orders 上以 NOT VALID 加的约束与外键。
--
-- 与 00320 分开：同一事务里 ADD … NOT VALID 紧跟 VALIDATE，等于直接加一条要扫全表、
-- 期间挡写的约束。VALIDATE 只拿 SHARE UPDATE EXCLUSIVE，扫表期间读写照常。
-- 存量订单全是自营单（source = 0、user_id 非空、channel_order_id 为空），校验必过。
-- +goose Up
ALTER TABLE orders VALIDATE CONSTRAINT chk_order_source;
ALTER TABLE orders VALIDATE CONSTRAINT chk_order_buyer;
ALTER TABLE orders VALIDATE CONSTRAINT chk_order_channel;
ALTER TABLE orders VALIDATE CONSTRAINT chk_discount_sources;
ALTER TABLE orders VALIDATE CONSTRAINT fk_orders_channel_order;

-- +goose Down
-- 已校验的约束没有「改回 NOT VALID」的写法，也没有必要：回滚 00320 时它们会被一起删掉。
SELECT 1;
