-- 经营报表（GET /admin/reports/*，契约 Report tag）的四条索引。
-- DDL 照抄数据模型设计 §5 / §4 / §8 / §11，那里是唯一真相源。
--
-- ===========================================================================
-- 为什么要新索引：报表按「支付时间 / 到账时间 / 检索时间」切窗口，而此前没有一条索引以它们打头
-- ===========================================================================
--
-- 报表的口径是：销售按 orders.paid_at 落窗口，退款按 refunds.refunded_at 落窗口，
-- 搜索按 search_logs.created_at 落窗口（契约 ReportWindow）。此前这三张表上的索引
-- 全是给别的读路径建的：
--
--   orders    idx_orders_admin_created 按 created_at（下单时间），不是 paid_at。
--             下单与支付之间隔着最长 30 分钟的待支付，而且待支付、已关闭的单一大半从不支付 ——
--             按 created_at 扫再过滤 paid_at，窗口边界上会漏单（23:50 下单、00:10 支付），
--             还白读所有没付钱的单。
--   refunds   idx_refunds_admin_created 按 created_at（申请时间）。退款从申请到到账可能隔几天，
--             按申请时间扫同样对不上窗口。
--   search_logs 只有 idx_search_logs_strategy（merchant_id, strategy, created_at）：
--             报表不按 strategy 分，走它等于把每个 strategy 的区间各扫一遍再合并。
--
-- 所以每条报表查询在真实规模下都会退化成「把这家店的订单 / 退款 / 检索日志整表扫一遍」。
-- 演示数据只有几十行，看不出来；一家日均一万单的店，一年是三百多万行。
--
-- ===========================================================================
-- 四条索引，各自回答一个问题
-- ===========================================================================
--
--   idx_orders_paid_at        概览、趋势、门店对比、商品排行：窗口内支付了的单。
--                             部分索引（paid_at IS NOT NULL）：待支付、已关闭、草稿永远进不来，
--                             索引只有「付过钱的单」那么大。INCLUDE 带上聚合要的四列
--                             （store_id / user_id / paid_cents / status），概览与趋势可以
--                             只读索引（index-only scan），不回表。
--                             门店管理员（store_id = ANY）也走这一条：窗口先把行数收窄，
--                             store_id 在 INCLUDE 里就地过滤。不另建 (merchant_id, store_id, paid_at)
--                             —— 门店维度的列表已经有 idx_orders_store，再建一条只为报表，
--                             写路径（每次支付回调）要多维护一棵树。
--   idx_refunds_refunded_at   窗口内到账的退款。部分索引（status = 40）：审核中、驳回、撤回的
--                             单永远不计入退款金额，不必进索引。INCLUDE order_id / amount_cents：
--                             金额就地求和，order_id 用来连回订单取履约门店（按门店收窄）。
--   idx_inventories_warning   库存预警：available_qty <= warning_qty 的门店 SKU。
--                             部分索引的谓词**比较两列**（PostgreSQL 允许，只要是不可变表达式），
--                             正常水位的库存行一行都不进来 —— 预警清单在任何时候都只是
--                             全部库存行里很小的一截，全表扫描为了它读几十万行不划算。
--                             列是 (merchant_id, store_id, sku_id)：按门店收窄（门店管理员、
--                             大区管理员）时前缀对得上。
--   idx_search_logs_created   搜索概况：窗口内的检索日志。
--
-- 每条都以 merchant_id 打头：RLS 的谓词 merchant_id = current_merchant() 由规划器当作
-- 普通的等值条件使用，索引前缀对上它才用得上（同 00006 / 00035 的写法）。
--
-- ===========================================================================
-- 为什么不建物化视图 / 定时汇总表
-- ===========================================================================
--
-- 有了上面四条索引，最重的一条（门店对比 / 商品排行）是「窗口内付过钱的单」这一段的
-- 索引范围扫描 + 按门店 / 商品分组，窗口又被契约限死在 366 天以内。按日汇总表能把它
-- 再快一个数量级，代价是：汇总与明细之间有延迟（「今天」的数字不是现在的数字）、
-- 退款回写要同时改汇总（两份真相）、时区改了要重算全部历史。眼下没有一个
-- 现查撑不住的规模证据，所以不建 —— 真到那一天，加汇总表是纯增量，不改契约。
--
-- 不用 CREATE INDEX CONCURRENTLY：goose 默认把每个迁移包在一个事务里，而 CONCURRENTLY
-- 不能在事务里跑；与 00035 同一个处理。代价是建索引期间这几张表的写会被挡住，
-- 线上大表升级到这一版时应当安排在低峰期。

-- +goose Up

CREATE INDEX idx_orders_paid_at ON orders(merchant_id, paid_at)
    INCLUDE (store_id, user_id, paid_cents, status)
    WHERE paid_at IS NOT NULL;

CREATE INDEX idx_refunds_refunded_at ON refunds(merchant_id, refunded_at)
    INCLUDE (order_id, amount_cents)
    WHERE status = 40;

CREATE INDEX idx_inventories_warning ON inventories(merchant_id, store_id, sku_id)
    WHERE available_qty <= warning_qty;

CREATE INDEX idx_search_logs_created ON search_logs(merchant_id, created_at);

-- +goose Down
DROP INDEX idx_search_logs_created;
DROP INDEX idx_inventories_warning;
DROP INDEX idx_refunds_refunded_at;
DROP INDEX idx_orders_paid_at;
