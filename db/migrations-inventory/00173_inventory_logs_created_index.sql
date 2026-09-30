-- +goose NO TRANSACTION
--
-- inventory_logs 按时间清理要的索引。与 core 的 db/migrations/00173 逐字一致（这个目录的
-- 规矩：表结构与索引必须与 core 迁移建出来的一样，见 00001 文件头），理由写在那一份里。
--
-- 单体库上 core 那一份先建好了，这里 IF NOT EXISTS 是空操作；拆分部署的库存库上由这里建。
-- 编号取 00173 与 core 那份对齐，便于对照；两个目录各记各的版本表，不会互相当成自己的。

-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_inv_logs_created
    ON inventory_logs (merchant_id, created_at);

-- +goose Down
-- 什么都不做，理由与 00001 的 Down 相同：单体库里这条索引归 core 的 00173 所有，
-- 库存目录回滚时去删它就是替 core 做了一次回滚。
SELECT 1;
