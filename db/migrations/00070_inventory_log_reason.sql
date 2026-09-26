-- inventory_logs 补 reason（数据模型 §4，§15 待确认事项第 12 / 18 条）。
-- DDL 照抄数据模型设计 §4，那里是唯一真相源。
--
-- 相对调整（POST .../inventory/adjustments，biz_type = 5 手工调整）让人手写的库存变动
-- 第一次进了流水。前四种 biz_type 的「为什么」都由 biz_id 说清楚了 —— 那是一个订单号，
-- 顺着它能查到下单、关单、退款的全部来龙去脉。手工调整的 biz_id 只能说「谁、哪一次请求」
-- （staff id + Idempotency-Key），说不出「为什么」：进货、盘点盘亏、退货验货入库，
-- 在流水里是同一个 +N / -N。对账时对不平的那一行，最需要的恰恰就是这句话。
--
-- 为什么不塞进 biz_id：biz_id 上有索引（idx_inv_logs_biz），语义是「按业务单号查流水」；
-- 往里拼一段自由文本会让它不再是一个可以精确匹配的键。
-- 为什么可空：前四种 biz_type 没有这句话，也不该为它们编一句。
-- 200 字符与契约 InventoryAdjustRequest.reason 的 maxLength 对齐；库里再兜一道，
-- 是因为这张表是对账的唯一依据，不该靠调用方自觉。
--
-- 纯加一列可空、无默认值：PG 只改目录，不重写表，也不挡下单扣减的写。

-- +goose Up
ALTER TABLE inventory_logs ADD COLUMN reason TEXT;
ALTER TABLE inventory_logs ADD CONSTRAINT chk_inv_logs_reason_len
    CHECK (reason IS NULL OR char_length(reason) <= 200);

-- +goose Down
ALTER TABLE inventory_logs DROP CONSTRAINT chk_inv_logs_reason_len;
ALTER TABLE inventory_logs DROP COLUMN reason;
