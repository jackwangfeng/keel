-- products.total_stock：**本版只停用，不删列**（数据模型 §3，待确认事项第 17 条）。
--
-- 本来这条迁移是 DROP COLUMN。审查时实测撞上了滚动发布：compose 的 migrate 服务
-- 在**旧 app 还在跑的时候**执行，而旧版（2c9247a）的买家商品列表 ListProducts 仍然
-- SELECT p.total_stock —— 迁移一提交，旧实例的 GET /products 全部 500，直到新 app 起来；
-- 回滚 app 而不回滚库则一直坏着。
--
-- 所以按 expand / contract 分两步：
--   · 本版（这条迁移所在的版本）：代码里已经没有任何一处读写这一列，列留着，值恒为 0；
--   · 下一版：旧版本全部下线之后，再用一条新迁移 DROP COLUMN（编号取那时 main 上的最高号之后）。
-- 这条迁移本身不改任何东西，只占住 00062 这个号（它已经在 main 上出现过，不能复用）。

-- +goose Up
SELECT 1;

-- +goose Down
SELECT 1;
