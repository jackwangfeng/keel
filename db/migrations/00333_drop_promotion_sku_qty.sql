-- 删掉 promotion_skus.stock_qty / sold_qty —— 00058 建的原始配额列。
--
-- 为什么现在能删：00075 把配额搬到 activity_stocks，00180 又在同一表加 quota_qty 只留定义。
-- 这两条迁移的注释、PROGRESS.md 与 CHANGELOG.md 都写着「下一版删」，这条就是在履约。
--
-- 删列的连带影响，都不是写个 DROP COLUMN 就能躲掉的：
--   一、chk_promotion_sku_qty 会被 PostgreSQL 自动删掉 —— 约束只要涉及被删列，整条一起消失，
--       而不是只拆掉相关那一项。里面 per_user_limit >= 0 还有意义，这里重建成
--       chk_promotion_sku_limit。
--   二、00131 建的 agent_ro.promotion_skus 视图 SELECT 了这两列，删列后它连带失效。与其让
--       CASCADE 静默处理，不如显式 DROP VIEW 再 CREATE VIEW（与 00331 同一写法），授权照
--       00131 补回。顺带解决一个坑：视图里那两列是 00075 那一刻的冻结值，AI 读它当库存会误判。
--   三、00075 的 Down 会把 activity_stocks 写回这两列，删列后无法回滚到 00075 之前。不建议改
--       历史迁移；本迁移的 Down 也还原不了数据 —— 两列早在 00075 之后就没再写过，与
--       activity_stocks 已经分叉，没有保留价值。
--
-- Go 侧不用动：service / handler / 测试消费的是 PriceOffer.StockQty / SoldQty，由库存服务与
-- activity_stocks 回填，从来不读这两列。repository 里的同名字段是 sqlc 照 schema 生成的，
-- 跑完 make generate 自己就没了。

-- +goose Up
DROP VIEW agent_ro.promotion_skus;

ALTER TABLE promotion_skus
    DROP COLUMN stock_qty,
    DROP COLUMN sold_qty;

ALTER TABLE promotion_skus
    ADD CONSTRAINT chk_promotion_sku_limit CHECK (per_user_limit >= 0);

CREATE VIEW agent_ro.promotion_skus WITH (security_barrier) AS
SELECT id, promotion_id, sku_id, promo_price_cents, discount_rate, per_user_limit
  FROM public.promotion_skus WHERE merchant_id = current_merchant();

GRANT SELECT ON agent_ro.promotion_skus TO keel_agent_ro;

-- +goose Down
DROP VIEW agent_ro.promotion_skus;

ALTER TABLE promotion_skus
    ADD COLUMN stock_qty INT NOT NULL DEFAULT 0,
    ADD COLUMN sold_qty  INT NOT NULL DEFAULT 0,
    DROP CONSTRAINT chk_promotion_sku_limit,
    ADD CONSTRAINT chk_promotion_sku_qty CHECK (
        per_user_limit >= 0 AND stock_qty >= 0 AND sold_qty >= 0
        AND (stock_qty = 0 OR sold_qty <= stock_qty)
    );

CREATE VIEW agent_ro.promotion_skus WITH (security_barrier) AS
SELECT id, promotion_id, sku_id, promo_price_cents, discount_rate, per_user_limit, stock_qty, sold_qty
  FROM public.promotion_skus WHERE merchant_id = current_merchant();

GRANT SELECT ON agent_ro.promotion_skus TO keel_agent_ro;
