-- 删掉 products.total_stock（数据模型 §3，待确认事项第 17 条）。
--
-- 00019 删两列冗余价格时把它留下了，理由是「它牵动对账任务的去留，是一次独立的决定」。
-- 那个对账任务从来没有做出来，而自 00019 起没有任何一处写这一列：
--   · 买家列表那条查询读了它但 handler 从没用过（有没有货看 in_stock，现算）；
--   · 后台 AdminProduct.total_stock 从 inventories 现算（db/queries/admin_products.sql）；
--   · 多门店之后它连「哪家店」都没有，是一个永远为 0 的汇总。
-- 留一个永远为 0 的列，代价不是那 4 个字节，是后面接手的人一定会误用它
-- （同一理由删过 inventories.reserved_qty 与 00019 那两列）。
--
-- 契约里 AdminProduct.total_stock 不动：它本来就是现算的，与这一列无关。

-- +goose Up
ALTER TABLE products DROP COLUMN total_stock;

-- +goose Down
-- 回滚只恢复列形状，值一律 0 —— 这正是它被删之前的真实状态。
ALTER TABLE products ADD COLUMN total_stock INT NOT NULL DEFAULT 0;
