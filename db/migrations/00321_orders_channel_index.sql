-- +goose NO TRANSACTION
--
-- orders.channel_order_id 的索引（00320 的外键要它；按渠道单找 keel 订单也走它）。
-- 部分索引只收渠道单（source = 1），自营单不进这条索引，自营写路径不多维护一条索引项。
-- orders 在 scripts/check_migrations.py 的 BIG_TABLES 里，所以 CONCURRENTLY + NO TRANSACTION。
-- CONCURRENTLY 失败会留下一条 INVALID 索引，IF NOT EXISTS 会把它当成已存在跳过：
-- 重跑前先 DROP INDEX CONCURRENTLY idx_orders_channel。

-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_orders_channel
    ON orders(merchant_id, channel_order_id) WHERE source = 1;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_orders_channel;
