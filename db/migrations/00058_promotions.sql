-- 营销活动：满减、满折、限时折扣（特价）、秒杀、新人礼。
-- DDL 照抄数据模型设计 §7「营销活动」（与 §5 orders / order_items 的增补），那里是唯一真相源。
--
-- ===========================================================================
-- 计价顺序（与并行的运费模板约定，数据模型 §7「优惠计算顺序」）
-- ===========================================================================
--
--   门店最终价（sku_prices_by_store）
--     → ① 限时折扣 / 秒杀改单价：活动价 = min(门店价, 特价)
--     → ② 满减 / 满折按行分摊（每行至多参与一个）
--     → ③ 优惠券（门槛与计算基数是 ② 之后的每行金额）
--     → ④ 运费（按优惠后应付商品金额判包邮）
--
-- ① 改的是**价**，不是优惠：order_items.price_cents 就是活动价，goods_amount_cents
-- 按活动价累加，门店价另存进 list_price_cents 供展示与对账。② ③ 是**优惠**，按行
-- 分摊进 order_items.discount_cents（活动 + 券的合计），其中活动那一份再单独记在
-- promotion_discount_cents。于是退款公式 net = amount − discount 一个字都不用改，
-- 「一行退完 = 这一行实付」恒等照旧成立（§11）。
--
-- ===========================================================================
-- 与 00026 的几处衔接
-- ===========================================================================
--
-- · chk_discount_needs_coupon 换成 chk_discount_sources：订单上的优惠现在有两个来源，
--   「没挂券就必须是 0」变成「没挂券就必须**恰好等于活动优惠**」。00026 的注释说过
--   「哪天有了第二个优惠来源，这条 CHECK 要跟着改，而那正是该有人停下来想一想的时刻」——
--   这就是那个时刻。想下来的结论是：约束不放松，只是把「优惠 = 0」换成「优惠 = 活动那一份」，
--   一笔凭空打了折的订单仍然写不进去。
-- · 与 00056（运费）的 chk_freight_discount 并存：包邮券抵掉的运费也算在 discount_cents 里、
--   订单一定挂着那张券，于是「没挂券 ⇒ 优惠 = 活动那一份」对抵运费的订单同样成立；
--   order_items 上不记运费，行上的 discount_cents 仍只是商品优惠（活动 + 商品券）。
-- · user_coupons.source 多一个取值 3（新人礼）：新人礼复用券的发放能力，但它不是
--   「商家定向发放」—— 报表要能分开「运营手工发的」与「系统按规则自动发的」。
-- · 适用范围 promotion_scopes 与 coupon_scopes 同一套取值与语义（1–4 挑行，5–6 挑门店），
--   计算复用 coupon_calc.go 的 lineEligible / storeAllowed —— 两套范围语义各写一份，
--   「分类含子孙」这种规则迟早只在其中一份里被修对。
--
-- ===========================================================================
-- 秒杀不超卖：活动配额的条件 UPDATE，与库存扣减同构
-- ===========================================================================
--
-- promotion_skus.sold_qty 的扣减是
--   UPDATE promotion_skus SET sold_qty = sold_qty + n
--    WHERE promotion_id = ? AND sku_id = ? AND (stock_qty = 0 OR sold_qty + n <= stock_qty)
-- READ COMMITTED 下并发的第二个 UPDATE 在最新版本上重评 WHERE（与 00026 领券同一个机制），
-- 抢最后几件时后到者受影响 0 行。chk_promotion_sku_qty 是兜底。
-- 它跑在下单 SAGA 的**库存分支**里、与门店库存扣减同一个事务：秒杀配额不是独立的实物库存，
-- 每一件照样从门店库存扣；配额只限制「按秒杀价最多卖几件」。
--
-- 每人限购记在 promotion_purchases（按 活动 × SKU × 买家 累计件数），同一个事务里在
-- promotion_skus 那一行的行锁之下 upsert —— 同一买家的两笔并发订单在那把锁上排队，
-- 后到者看得见前者已提交的件数。

-- +goose Up

-- ---------------------------------------------------------------------------
-- promotions（活动本体）
-- ---------------------------------------------------------------------------
CREATE TABLE promotions (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id       BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    name              TEXT        NOT NULL,
    promo_type        SMALLINT    NOT NULL,              -- 1满减 2满折 3限时折扣 4秒杀 5新人礼
    threshold_unit    SMALLINT    NOT NULL DEFAULT 0,    -- 满减满折：1金额（分） 2件数；其余 0
    stack_with_coupon BOOLEAN     NOT NULL DEFAULT TRUE, -- 能否与券同享
    gift_template_id  BIGINT,                            -- 新人礼：发哪一批券
    starts_at         TIMESTAMPTZ NOT NULL,
    ends_at           TIMESTAMPTZ NOT NULL,
    status            SMALLINT    NOT NULL DEFAULT 0,    -- 0下线 1上线
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_promotion_shape CHECK (
        (promo_type IN (1, 2) AND threshold_unit IN (1, 2) AND gift_template_id IS NULL) OR
        (promo_type IN (3, 4) AND threshold_unit = 0       AND gift_template_id IS NULL) OR
        (promo_type = 5       AND threshold_unit = 0       AND gift_template_id IS NOT NULL)
    ),
    CONSTRAINT chk_promotion_window CHECK (ends_at > starts_at),
    CONSTRAINT chk_promotion_status CHECK (status IN (0, 1)),
    -- 供 promotion_* 子表与 order_items.price_promotion_id 做复合外键
    UNIQUE (id, merchant_id),
    FOREIGN KEY (gift_template_id, merchant_id) REFERENCES coupon_templates(id, merchant_id)
);
CREATE INDEX idx_promotions_list ON promotions(merchant_id, id DESC);
-- 计价热路径：取「此刻上线中」的活动。上线中的活动一家店通常只有个位数到几十个。
CREATE INDEX idx_promotions_live ON promotions(merchant_id, ends_at) WHERE status = 1;

-- ---------------------------------------------------------------------------
-- promotion_tiers（满减 / 满折的阶梯）
-- ---------------------------------------------------------------------------
CREATE TABLE promotion_tiers (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id    BIGINT   NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    promotion_id   BIGINT   NOT NULL,
    threshold      BIGINT   NOT NULL,            -- 按活动的 threshold_unit：分或件
    discount_cents BIGINT   NOT NULL DEFAULT 0,  -- 满减的减免额
    discount_rate  SMALLINT NOT NULL DEFAULT 0,  -- 满折，千分比：900 = 9 折
    CONSTRAINT chk_promotion_tier CHECK (
        threshold > 0 AND (
        (discount_cents > 0 AND discount_rate = 0) OR
        (discount_cents = 0 AND discount_rate BETWEEN 1 AND 999))
    ),
    FOREIGN KEY (promotion_id, merchant_id) REFERENCES promotions(id, merchant_id)
);
CREATE UNIQUE INDEX uk_promotion_tiers ON promotion_tiers(merchant_id, promotion_id, threshold);

-- ---------------------------------------------------------------------------
-- promotion_scopes（适用范围：与 coupon_scopes 同一套取值与语义）
-- ---------------------------------------------------------------------------
CREATE TABLE promotion_scopes (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id  BIGINT   NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    promotion_id BIGINT   NOT NULL,
    scope_type   SMALLINT NOT NULL,  -- 1全场 2分类 3商品 4品牌 5大区 6门店
    target_id    BIGINT,
    include      BOOLEAN  NOT NULL DEFAULT TRUE,
    CONSTRAINT chk_promotion_scope CHECK (
        (scope_type = 1 AND target_id IS NULL AND include) OR
        (scope_type BETWEEN 2 AND 6 AND target_id IS NOT NULL)
    ),
    FOREIGN KEY (promotion_id, merchant_id) REFERENCES promotions(id, merchant_id)
);
CREATE UNIQUE INDEX uk_promotion_scopes
    ON promotion_scopes(merchant_id, promotion_id, scope_type, target_id) NULLS NOT DISTINCT;

-- ---------------------------------------------------------------------------
-- promotion_skus（限时折扣 / 秒杀的活动商品与配额）
-- ---------------------------------------------------------------------------
CREATE TABLE promotion_skus (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id       BIGINT   NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    promotion_id      BIGINT   NOT NULL,
    sku_id            BIGINT   NOT NULL,
    promo_price_cents BIGINT   NOT NULL DEFAULT 0,  -- 特价（分）
    discount_rate     SMALLINT NOT NULL DEFAULT 0,  -- 或折扣（千分比），二选一
    per_user_limit    INT      NOT NULL DEFAULT 0,  -- 每人限购，0 = 不限
    stock_qty         INT      NOT NULL DEFAULT 0,  -- 秒杀配额，0 = 不限（限时折扣）
    sold_qty          INT      NOT NULL DEFAULT 0,  -- 已按活动价售出（含待支付）
    CONSTRAINT chk_promotion_sku_price CHECK (
        (promo_price_cents > 0 AND discount_rate = 0) OR
        (promo_price_cents = 0 AND discount_rate BETWEEN 1 AND 999)
    ),
    CONSTRAINT chk_promotion_sku_qty CHECK (
        per_user_limit >= 0 AND stock_qty >= 0 AND sold_qty >= 0
        AND (stock_qty = 0 OR sold_qty <= stock_qty)
    ),
    -- 写成约束而不是唯一索引：整组替换那条 upsert 用 ON CONFLICT ON CONSTRAINT 指名它。
    CONSTRAINT uk_promotion_skus UNIQUE (merchant_id, promotion_id, sku_id),
    FOREIGN KEY (promotion_id, merchant_id) REFERENCES promotions(id, merchant_id),
    FOREIGN KEY (sku_id, merchant_id)       REFERENCES skus(id, merchant_id)
);
-- 计价热路径：按一批 sku 找它们身上的活动。
CREATE INDEX idx_promotion_skus_sku ON promotion_skus(merchant_id, sku_id);

-- ---------------------------------------------------------------------------
-- promotion_purchases（每人限购的累计件数）
-- ---------------------------------------------------------------------------
CREATE TABLE promotion_purchases (
    merchant_id  BIGINT NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    promotion_id BIGINT NOT NULL,
    sku_id       BIGINT NOT NULL,
    user_id      BIGINT NOT NULL,
    qty          INT    NOT NULL DEFAULT 0 CHECK (qty >= 0),
    -- 约束名写出来：累计那条 upsert 用 ON CONFLICT ON CONSTRAINT 指名它，
    -- 而不是列出冲突列 —— 列里有 merchant_id，db/queries 里不许出现这个词。
    CONSTRAINT pk_promotion_purchases PRIMARY KEY (merchant_id, promotion_id, sku_id, user_id),
    FOREIGN KEY (promotion_id, merchant_id) REFERENCES promotions(id, merchant_id),
    FOREIGN KEY (sku_id, merchant_id)       REFERENCES skus(id, merchant_id),
    FOREIGN KEY (user_id, merchant_id)      REFERENCES users(id, merchant_id)
);

-- ---------------------------------------------------------------------------
-- promotion_gift_grants（新人礼发放记录：一个买家在一个活动里至多一张）
-- ---------------------------------------------------------------------------
CREATE TABLE promotion_gift_grants (
    merchant_id    BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    promotion_id   BIGINT      NOT NULL,
    user_id        BIGINT      NOT NULL,
    user_coupon_id BIGINT      NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, promotion_id, user_id),
    FOREIGN KEY (promotion_id, merchant_id)   REFERENCES promotions(id, merchant_id),
    FOREIGN KEY (user_id, merchant_id)        REFERENCES users(id, merchant_id),
    FOREIGN KEY (user_coupon_id, merchant_id) REFERENCES user_coupons(id, merchant_id)
);

-- ---------------------------------------------------------------------------
-- user_coupons.source：3 新人礼
-- ---------------------------------------------------------------------------
ALTER TABLE user_coupons DROP CONSTRAINT chk_user_coupon_source;
ALTER TABLE user_coupons ADD CONSTRAINT chk_user_coupon_source CHECK (source IN (1, 2, 3));

-- ---------------------------------------------------------------------------
-- order_items：活动价、门店价快照、活动分摊
-- ---------------------------------------------------------------------------
ALTER TABLE order_items ADD COLUMN list_price_cents BIGINT;
UPDATE order_items SET list_price_cents = price_cents;
ALTER TABLE order_items ALTER COLUMN list_price_cents SET NOT NULL;
ALTER TABLE order_items ADD COLUMN price_promotion_id BIGINT;
ALTER TABLE order_items ADD COLUMN promotion_discount_cents BIGINT NOT NULL DEFAULT 0;
ALTER TABLE order_items ADD CONSTRAINT order_items_price_promotion_fkey
    FOREIGN KEY (price_promotion_id, merchant_id) REFERENCES promotions(id, merchant_id);
-- 活动价只能比门店价低（活动价 = min(门店价, 特价)）；没有单价类活动时两者相等。
ALTER TABLE order_items ADD CONSTRAINT chk_item_price_promotion CHECK (
    price_cents <= list_price_cents
    AND (price_promotion_id IS NOT NULL OR price_cents = list_price_cents)
);
-- 活动分摊是全部优惠的一部分；全部优惠不超过行金额（chk_item_refund 早已隐含后一半）。
ALTER TABLE order_items ADD CONSTRAINT chk_item_promotion_discount CHECK (
    promotion_discount_cents >= 0 AND promotion_discount_cents <= discount_cents
    AND discount_cents <= amount_cents
);

-- ---------------------------------------------------------------------------
-- orders：活动优惠合计与命中活动的快照
-- ---------------------------------------------------------------------------
ALTER TABLE orders ADD COLUMN promotion_discount_cents BIGINT NOT NULL DEFAULT 0;
ALTER TABLE orders ADD COLUMN promotions JSONB NOT NULL DEFAULT '[]';
ALTER TABLE orders DROP CONSTRAINT chk_discount_needs_coupon;
ALTER TABLE orders ADD CONSTRAINT chk_discount_sources CHECK (
    promotion_discount_cents >= 0 AND promotion_discount_cents <= discount_cents
    AND (user_coupon_id IS NOT NULL OR discount_cents = promotion_discount_cents)
);

-- ---------------------------------------------------------------------------
-- 行级安全：ENABLE 之外必须再加 FORCE（§2「坑一」）
-- ---------------------------------------------------------------------------
ALTER TABLE promotions ENABLE ROW LEVEL SECURITY;
ALTER TABLE promotions FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON promotions
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

ALTER TABLE promotion_tiers ENABLE ROW LEVEL SECURITY;
ALTER TABLE promotion_tiers FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON promotion_tiers
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

ALTER TABLE promotion_scopes ENABLE ROW LEVEL SECURITY;
ALTER TABLE promotion_scopes FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON promotion_scopes
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

ALTER TABLE promotion_skus ENABLE ROW LEVEL SECURITY;
ALTER TABLE promotion_skus FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON promotion_skus
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

ALTER TABLE promotion_purchases ENABLE ROW LEVEL SECURITY;
ALTER TABLE promotion_purchases FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON promotion_purchases
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

ALTER TABLE promotion_gift_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE promotion_gift_grants FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON promotion_gift_grants
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

CREATE OR REPLACE TRIGGER touch_promotions_updated_at
    BEFORE UPDATE ON promotions FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- GRANT 面：tenant 类的四权（db/tenancy.json）。00005 之后新表默认只有 SELECT。
GRANT SELECT, INSERT, UPDATE, DELETE
   ON promotions, promotion_tiers, promotion_scopes, promotion_skus,
      promotion_purchases, promotion_gift_grants TO keel_app;

-- +goose Down

ALTER TABLE orders DROP CONSTRAINT chk_discount_sources;
-- 回退之前，有活动优惠的订单会让旧约束建不起来 —— 那是对的：回退会丢掉「这笔优惠从哪来」。
ALTER TABLE orders ADD CONSTRAINT chk_discount_needs_coupon
    CHECK (user_coupon_id IS NOT NULL OR discount_cents = 0);
ALTER TABLE orders DROP COLUMN promotions;
ALTER TABLE orders DROP COLUMN promotion_discount_cents;
ALTER TABLE order_items DROP CONSTRAINT chk_item_promotion_discount;
ALTER TABLE order_items DROP CONSTRAINT chk_item_price_promotion;
ALTER TABLE order_items DROP CONSTRAINT order_items_price_promotion_fkey;
ALTER TABLE order_items DROP COLUMN promotion_discount_cents;
ALTER TABLE order_items DROP COLUMN price_promotion_id;
ALTER TABLE order_items DROP COLUMN list_price_cents;
ALTER TABLE user_coupons DROP CONSTRAINT chk_user_coupon_source;
ALTER TABLE user_coupons ADD CONSTRAINT chk_user_coupon_source CHECK (source IN (1, 2));
REVOKE ALL ON promotions, promotion_tiers, promotion_scopes, promotion_skus,
              promotion_purchases, promotion_gift_grants FROM keel_app;
DROP TABLE promotion_gift_grants;
DROP TABLE promotion_purchases;
DROP TABLE promotion_skus;
DROP TABLE promotion_scopes;
DROP TABLE promotion_tiers;
DROP TABLE promotions;
