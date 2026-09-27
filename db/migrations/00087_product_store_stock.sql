-- 商品列表按「这家店有没有货」排序（2026-09-27）：有货在前、无货在后，分页严格正确。
--
-- ### 为什么要一张冗余表
--
-- 列表的排序必须在 SQL 里做（否则分页不对：第二页可能冒出比第一页末尾更该靠前的商品）。
-- 而库存归库存服务（微服务拆分阶段 1a 起）：C 档是另一个库，B 档是业务账号读不到的 schema，
-- 列表那条查询 JOIN 不到 inventories。所以在 core 这边留一份「门店 × 商品 → 有没有货」，
-- 只为排序服务；列表上**显示**的 in_stock 仍然是现问库存服务的（service/product.go fillInStock），
-- 两者可以短暂不一致 —— 最坏是排序晚一轮刷新，显示永远是准的。
--
-- ### 谁来写
--
-- service/stock_flags.go：① 后台三条改库存（按 SKU 设、按门店设、相对调整）成功之后立刻刷这件商品；
-- ② 每分钟一轮全量刷新（KEEL_STOCK_FLAG_INTERVAL），兜住下单扣减、关单 / 退款回补这些不经 core 后台的变动。
-- 行不存在 = 还没刷过，排序按「有货」算（COALESCE(in_stock, TRUE)）—— 宁可不压，不错压。
--
-- 判据与详情页、检索、列表显示同一个：这家店里任意一个在售 SKU 可售数 > 0。
-- +goose Up
CREATE TABLE product_store_stock (
    store_id    BIGINT      NOT NULL,
    product_id  BIGINT      NOT NULL,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    in_stock    BOOLEAN     NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (store_id, product_id),
    FOREIGN KEY (store_id, merchant_id)   REFERENCES stores(id, merchant_id),
    FOREIGN KEY (product_id, merchant_id) REFERENCES products(id, merchant_id)
);
CREATE INDEX idx_product_store_stock_product ON product_store_stock(merchant_id, product_id);

COMMENT ON TABLE product_store_stock IS
    '门店 × 商品有没有货的冗余标记，只给商品列表排序用（00087）；显示仍现问库存服务。刷新见 service/stock_flags.go。';

-- 行级安全：ENABLE 之外必须再加 FORCE（数据模型 §2「坑一」）。
ALTER TABLE product_store_stock ENABLE ROW LEVEL SECURITY;
ALTER TABLE product_store_stock FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON product_store_stock
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

CREATE OR REPLACE TRIGGER touch_product_store_stock_updated_at
    BEFORE UPDATE ON product_store_stock FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- GRANT 面：tenant 类的四权（db/tenancy.json）。00005 之后新表默认只有 SELECT。
GRANT SELECT, INSERT, UPDATE, DELETE ON product_store_stock TO keel_app;

-- +goose Down
DROP TABLE product_store_stock;
