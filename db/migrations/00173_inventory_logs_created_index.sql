-- +goose NO TRANSACTION
--
-- inventory_logs 按时间清理要的索引（2026-09-30 架构审查：只增不删的表）。
--
-- 保留期清理（service/retention.go）每批删「本店 created_at 早于保留期的 5000 行」。
-- 现有两条索引都不以 (merchant_id, created_at) 打头：idx_inv_logs_sku_time 是
-- (merchant_id, store_id, sku_id, created_at DESC)，idx_inv_logs_biz 是 (merchant_id, biz_id)。
-- 没有这一条，每一批都要把全店流水扫一遍才凑得出 5000 行最老的 —— 一张只增不删的表上，
-- 清理本身就会变成最慢的那条查询。
--
-- 大表建索引：NO TRANSACTION + CONCURRENTLY（CONTRIBUTING.md「迁移怎么写」）。
-- 库存库自己的迁移目录里有一份同样的（db/migrations-inventory/00173），单体库上那一份因为
-- IF NOT EXISTS 是空操作；两边建出来的索引逐字一致，拆分部署下库存库也有它。
--
-- CONCURRENTLY 失败会留下一条 INVALID 索引，IF NOT EXISTS 会把它当成已存在跳过：
-- 重跑前先 DROP INDEX CONCURRENTLY idx_inv_logs_created。

-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_inv_logs_created
    ON inventory_logs (merchant_id, created_at);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_inv_logs_created;
