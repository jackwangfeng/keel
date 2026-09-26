-- 购物车（数据模型 §10）。契约里 /cart 那 7 个操作一直在，表一直没建。
--
-- ### 与 §10 的 DDL 只差三处，三处都已回写进 §10
--
--   ① 两张表的 merchant_id 都带 DEFAULT current_merchant()。理由同 00030：
--      租户取自本事务 set_config 进来的那一个，调用方手里没有这个参数可以传错。
--   ② cart_items.quantity 的 CHECK 从「> 0」收紧成「BETWEEN 1 AND 999」。
--      999 是契约写死的上限（加购累加后超过 999 返回 422 cart-quantity-exceeded，
--      不做静默截断）。服务端用一条带条件的 upsert 判它，这个 CHECK 是最后一道：
--      那条 upsert 的条件哪天被改坏，数据库照样拒绝一行 1000 件的购物车。
--      上限同时也是金额不溢出的前提 —— 与 priceOrder 的 maxLineQuantity 同一个数。
--   ③ RLS 策略写成 USING + WITH CHECK 的显式形状（与 00020 之后的表一致）。
--      USING 本来就兼作 WITH CHECK，显式写出来只是让读的人不必记住这条规则。
--
-- ### 购物车不存价格
--
-- 这一条是 §10 的刻意决策，本迁移照做：cart_items 上没有任何价格列。
-- 展示价在读的时候按门店现算，走的是与 /orders/preview 同一条定价查询
-- （db/queries/orders.sql 的 ListSKUsForPricing），所以购物车显示的价与试算的价
-- 不可能分叉 —— 根本没有第二个价存在库里。
--
-- ### 下架、删除的商品留在车里
--
-- cart_items 对 skus / products 的外键**没有** ON DELETE CASCADE：两张父表都是
-- 软删（00018），行本来就不会被物理删掉；而「下架了就从车里消失」是 §10 明确
-- 不要的行为（「替用户默默删东西，比让他看到一条划掉的商品更讨人嫌」）。
-- 失效行在读的时候标出来，见 db/queries/carts.sql。
--
-- ### db/tenancy.json 不需要新条目
--
-- 两张表都是标准的 tenant 类（自带 merchant_id、策略是直接的列比较）。
-- 两条首列不带 merchant_id 的唯一索引 —— carts(user_id) 与
-- cart_items(cart_id, sku_id) —— 早就前瞻地登记在 unique_global_ok 里了，
-- 理由是「首列已蕴含租户」。

-- +goose Up

CREATE TABLE carts (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    user_id     BIGINT      NOT NULL UNIQUE,        -- 用户已隐含租户
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 供 cart_items 做复合外键
    UNIQUE (id, merchant_id),
    FOREIGN KEY (user_id, merchant_id) REFERENCES users(id, merchant_id)
);

CREATE TABLE cart_items (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    cart_id     BIGINT      NOT NULL,
    sku_id      BIGINT      NOT NULL,
    product_id  BIGINT      NOT NULL,
    quantity    INT         NOT NULL CHECK (quantity BETWEEN 1 AND 999),
    selected    BOOLEAN     NOT NULL DEFAULT TRUE,   -- 结算勾选态
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (cart_id, merchant_id)
        REFERENCES carts(id, merchant_id) ON DELETE CASCADE,
    FOREIGN KEY (sku_id, merchant_id)     REFERENCES skus(id, merchant_id),
    FOREIGN KEY (product_id, merchant_id) REFERENCES products(id, merchant_id)
);
CREATE UNIQUE INDEX uk_cart_items ON cart_items(cart_id, sku_id);
CREATE INDEX idx_cart_items_cart ON cart_items(merchant_id, cart_id);

ALTER TABLE carts ENABLE ROW LEVEL SECURITY;
ALTER TABLE carts FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON carts
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

ALTER TABLE cart_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE cart_items FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON cart_items
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

-- 00007 那个 DO 循环只在它自己那一次迁移里跑过，之后新建的表要自己挂 ——
-- TestUpdatedAtIsMaintainedByTrigger 会盯着。
CREATE OR REPLACE TRIGGER touch_carts_updated_at
    BEFORE UPDATE ON carts FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
CREATE OR REPLACE TRIGGER touch_cart_items_updated_at
    BEFORE UPDATE ON cart_items FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- tenant 类的默认 GRANT 面（db/tenancy.json），TestAppRoleGrantSurface 逐表比对。
GRANT SELECT, INSERT, UPDATE, DELETE ON carts, cart_items TO keel_app;

-- +goose Down
REVOKE ALL ON carts, cart_items FROM keel_app;
DROP POLICY tenant ON cart_items;
DROP POLICY tenant ON carts;
DROP TABLE cart_items;
DROP TABLE carts;
