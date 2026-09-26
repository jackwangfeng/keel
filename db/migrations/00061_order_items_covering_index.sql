-- 商品排行（GET /admin/reports/products）在大数据量下的索引：idx_order_items_order 换成覆盖索引。
-- DDL 照抄数据模型设计 §5，那里是唯一真相源。
--
-- ===========================================================================
-- 实测（这一轮，本地 PostgreSQL 16 默认配置：shared_buffers 128MB、work_mem 4MB）
-- ===========================================================================
--
-- 数据：主店 130 万单（支付时间均匀铺满一年）、195 万订单行、300 件商品、6 家门店；
-- 另两家店各 20 万单（让租户前缀真的有区分度）。全店范围、按销售额取前 10，三次取中位数：
--
--                         7 天     30 天     90 天     365 天
--   原查询 + 原索引       108ms    557ms    1321ms    3226ms
--   原查询 + 覆盖索引     101ms    493ms    1261ms    3165ms
--   新查询 + 原索引        81ms    297ms     875ms    3607ms
--   新查询 + 覆盖索引      61ms    244ms     842ms    3560ms
--
-- 结论分两半：
--
-- 1. **只加覆盖索引几乎没用**（30 天 557 → 493ms）。原查询的计划是把这家店的**全部**订单行
--    （195 万行）按租户前缀扫一遍，再与窗口里的 10 万单做哈希连接 —— 覆盖索引只是把那 195 万行
--    的回表省掉了，行数一行没少。订单行没有时间列，窗口只能从订单那边来，
--    而规划器认为「逐单去订单行索引里点查 10 万次」比「整段扫一遍」贵（random_page_cost
--    调到 1.1 它也不改主意）。
-- 2. **真正的改进在查询本身**（db/queries/reports.sql 的 ReportProductRanking）：
--    ① 用窗口里订单 id 的最小值与最大值给订单行加一个范围条件 —— 订单 id 是自增的、与支付时间
--    高度相关，于是订单行索引上是一段范围扫描（30 天只扫 16 万行，不是 195 万）；
--    ② 先按（商品, 订单）归并一次，订单数改成 count(*)，去掉 count(DISTINCT) 那一步落盘的整体排序。
--    新旧两版在 72 组参数（窗口 × 门店 × 大区 × 类目 × 排序）下结果逐行相同。
--    在这之上，覆盖索引让那 16 万行的点查变成只读索引（index-only scan，Heap Fetches: 0），
--    30 天再从 297 降到 244ms。
--
-- 365 天慢了约 12%（3.2s → 3.6s）：窗口覆盖了全部订单时，范围条件不再收窄任何东西，
-- 多出来的是两级归并在 4MB work_mem 下的落盘。一年的排行是契约允许的最大窗口，
-- 常用的是 7 天 / 30 天，按常用的那一头优化。真要让一年也快，是按日汇总表（00057 文件头
-- 说了为什么现在不建），不是再加索引。
--
-- ===========================================================================
-- 为什么是替换，而不是再加一条
-- ===========================================================================
--
-- 覆盖索引的键与原来的 idx_order_items_order 完全相同（merchant_id, order_id），
-- 原来那条能服务的查询（订单详情取订单行、退款取可退行）它全都能服务。并存的话
-- 每次下单要多维护一棵树，而原来那条从此没有任何查询会选它。
--
-- 代价写清楚：
--   · 更大。INCLUDE 的六列让它是原索引的约 3 倍（195 万行：66MB → 209MB）。
--   · refunded_qty / refunded_cents 进了索引，退款到账回写这两列时那次 UPDATE 不再是 HOT 更新，
--     要连带改索引。退款回写的频率比下单低一到两个数量级，而订单行别的列从不更新。
--   · 不带这两列就做不成只读索引（排行要读已退件数与金额），那样还不如不换。
--
-- 不用 CREATE INDEX CONCURRENTLY：理由与 00035 / 00057 相同（goose 把迁移包在事务里）。
-- 大表升级到这一版时建索引期间 order_items 的写会被挡住，应当安排在低峰期。

-- +goose Up
CREATE INDEX idx_order_items_order_cov ON order_items(merchant_id, order_id)
    INCLUDE (product_id, quantity, amount_cents, discount_cents, refunded_qty, refunded_cents);
DROP INDEX idx_order_items_order;
ALTER INDEX idx_order_items_order_cov RENAME TO idx_order_items_order;

-- +goose Down
CREATE INDEX idx_order_items_order_narrow ON order_items(merchant_id, order_id);
DROP INDEX idx_order_items_order;
ALTER INDEX idx_order_items_order_narrow RENAME TO idx_order_items_order;
