-- 商品列表 / 检索缺的索引（数据模型 §3 早就写了，迁移一直没建）。
--
-- 一、idx_skus_product (merchant_id, product_id)
--   数据模型 §3 的 DDL 里有这一行，00001 起的迁移里没有 —— 文档与库漂移了。
--   后果：skus 上只有主键、(id, merchant_id) 与 sku_code 唯一索引，每一处
--   `s.product_id = p.id`（列表页的门店价聚合 sku_prices_by_store、有货判断、
--   检索的五条召回）都要把本商家的全部 SKU 过一遍。审查时实测：2 万商品 / 6 万 SKU，
--   GET /products 一页 54.8 秒、关键词检索 56.6 秒（Index Scan using skus_id_merchant_id_key
--   … loops=18023 … Rows Removed by Filter: 60023）；建上它之后 104 ms / 117 ms。
--
-- 二、idx_products_listing_published（部分索引）
--   ListProducts 按 `published_at DESC NULLS LAST, id DESC` 排序，而 idx_products_listing
--   以 category_id 打头，不带类目筛选时用不上 —— 于是先给全部在架商品算完价格再取前 20。
--   这条索引与 ORDER BY 逐列一致、谓词与 WHERE 一致，LIMIT 能先生效：实测 104 ms → 0.15 ms。
--
-- 三、idx_categories_path / idx_categories_parent
--   同样是 §3 里写了没建的两条。类目子树按 path 前缀取（text_pattern_ops 才能走 LIKE 'x%'），
--   子类目按 parent_id 取。类目表小，今天不是瓶颈；补上是为了让文档与库重新一致。
--
-- 不用 CREATE INDEX CONCURRENTLY：理由与 00035 / 00057 相同（goose 把迁移包在事务里）。
-- 建索引期间挡写不挡读；这几张表的写入频率很低（上下架、改商品），代价可接受。
-- lock_timeout：拿不到锁就失败重来，而不是排在一条长查询后面、同时挡住后面所有写。

-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE INDEX idx_skus_product ON skus(merchant_id, product_id);
CREATE INDEX idx_products_listing_published
    ON products(merchant_id, published_at DESC NULLS LAST, id DESC)
    WHERE deleted_at IS NULL AND status = 1;
CREATE INDEX idx_categories_path
    ON categories USING btree (merchant_id, path text_pattern_ops);
CREATE INDEX idx_categories_parent
    ON categories(merchant_id, parent_id) WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX idx_categories_parent;
DROP INDEX idx_categories_path;
DROP INDEX idx_products_listing_published;
DROP INDEX idx_skus_product;
