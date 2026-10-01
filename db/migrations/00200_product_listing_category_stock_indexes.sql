-- +goose NO TRANSACTION
--
-- 买家商品列表的两条索引（2026-10-01，性能压测 docs/性能压测-2026-10.md 六 ② ⑧）。
--
-- ### idx_products_listing_category：小类目按类目直接取页
--
-- 类目筛选原来写成 category_id IN (按 path 区间取子树)，规划器猜不出子树里有几件商品
-- （估 2.5 万、实际 4 件），于是沿 idx_products_listing_published 把全店在架商品逐行过滤一遍
-- （Rows Removed by Filter: 100019，38.9 ms；无货段再来一遍）。现在 service 先把子树解析成 id 数组，
-- 列表对每个类目各沿这条索引按 published_at DESC, id DESC 取前 offset+limit 行再合并
-- （db/queries/products.sql 的 ListProductsByStockInCategories），计数也按类目逐个走它。
--
-- 列与 WHERE 照 idx_products_listing_published（00064）逐列对齐，只是在 merchant_id 后面插了 category_id：
-- merchant_id 那一段由 RLS 谓词补上（查询里照例一个字都不写），部分索引条件与列表的上架条件逐字一致，
-- 规划器才认得这条索引能用。
--
-- ### idx_product_store_stock_in_stock：「只看有货」计数不回表
--
-- CountProductsInStock 原来对 product_store_stock 按主键 (store_id, product_id) 位图扫描再回表取 in_stock
-- （4.9 万行，110 ms）。部分索引只收 in_stock 的行，列里带上 merchant_id（RLS 谓词要它），
-- 「这家店有货的商品」就是一次仅索引扫描。
--
-- 两张表都在 scripts/check_migrations.py 的 BIG_TABLES 里，所以 CONCURRENTLY + NO TRANSACTION
-- （CONTRIBUTING.md「迁移怎么写」第 2、3 条）。CONCURRENTLY 失败会留下一条 INVALID 索引，
-- IF NOT EXISTS 会把它当成已存在跳过：重跑前先 DROP INDEX CONCURRENTLY 对应的那一条。

-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_products_listing_category
    ON products(merchant_id, category_id, published_at DESC NULLS LAST, id DESC)
    WHERE deleted_at IS NULL AND status = 1;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_product_store_stock_in_stock
    ON product_store_stock(merchant_id, store_id, product_id)
    WHERE in_stock;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_product_store_stock_in_stock;
DROP INDEX CONCURRENTLY IF EXISTS idx_products_listing_category;
