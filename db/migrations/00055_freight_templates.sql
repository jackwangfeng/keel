-- 运费模板（数据模型 §7「运费模板」）：freight_templates / freight_template_rules，
-- 以及 products.freight_template_id（商品单独挂的模板）。
-- DDL 照抄数据模型设计 §3 / §7，那里是唯一真相源。
--
-- ===========================================================================
-- 一、两种归属，一张表
-- ===========================================================================
--
-- store_id 为 NULL 的是**全店模板**：商品可以挂它，is_default 的那一个是全店兜底。
-- store_id 非 NULL 的是**门店模板**：每家门店至多一个（uk_freight_templates_store），
-- 这家店发货的、没有单独挂模板的商品按它算。一行商品用哪个模板：
--
--     商品挂的模板 → 履约门店的门店模板 → 全店默认模板 → 都没有则这一行不计运费
--
-- 两条部分唯一索引就是这两条「至多一个」本身（与 uk_stores_default 同一个路子）；
-- chk_freight_template_default 让「门店模板设成全店默认」在库里写不出来。
-- 「商品只能挂全店模板」是应用层校验（admin_catalog.go 在同一个事务里 FOR SHARE
-- 锁住模板行再写；模板改归属 / 删除时 FOR UPDATE 锁住同一行再数挂着它的商品，
-- 两边在这一行锁上串行）——一条跨表的条件进不了 CHECK，触发器又不值得。
--
-- ===========================================================================
-- 二、地区用省级区划码的数组，而不是一张地区子表
-- ===========================================================================
--
-- 模板永远整体替换（PUT），规则的读法永远是「整个模板连同全部规则」，没有
-- 「哪些模板覆盖了广东」这种反查。数组让一条规则是一行，读写都简单。
-- 代价是「同一个省只能出现在一条规则里、且不能同时是不配送」进不了约束
-- （跨行的数组不相交要 EXCLUDE + btree_gist/intarray，为这一条引一个扩展不划算），
-- 由 service/admin_freight.go 的 validateFreightTemplate 在写入前校验。
-- 数据库兜住的是**取值**：每个元素必须是 34 个省级行政区之一（chk_*_regions，
-- 与 service/freight_region.go 的 provinces 表是同一份清单，单元测试核对两边一致）。
--
-- 「恰好一条默认规则（region_codes 为空）」：uk_freight_rules_default 管「至多一条」，
-- 「至少一条」在应用层——空模板在写入瞬间就被拒，库里不会有没有默认规则的模板
-- 被读到（整体替换在一个事务里：先删旧规则，再插新规则）。

-- +goose Up

CREATE TABLE freight_templates (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    name        TEXT        NOT NULL,
    store_id    BIGINT,                              -- NULL = 全店模板；非 NULL = 这家门店的门店模板
    charge_mode SMALLINT    NOT NULL,                -- 1 按件 2 按重量（克）
    is_default  BOOLEAN     NOT NULL DEFAULT FALSE,  -- 全店默认模板（只有全店模板能是）
    undeliverable_region_codes TEXT[] NOT NULL DEFAULT '{}',  -- 不配送的省（6 位省级区划码）
    deleted_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_freight_template_mode CHECK (charge_mode IN (1, 2)),
    CONSTRAINT chk_freight_template_default CHECK (NOT is_default OR store_id IS NULL),
    CONSTRAINT chk_freight_template_regions CHECK (undeliverable_region_codes <@ ARRAY[
        '110000','120000','130000','140000','150000','210000','220000','230000',
        '310000','320000','330000','340000','350000','360000','370000',
        '410000','420000','430000','440000','450000','460000',
        '500000','510000','520000','530000','540000',
        '610000','620000','630000','640000','650000','710000','810000','820000']::TEXT[]),
    -- 供 freight_template_rules / products 做复合外键
    UNIQUE (id, merchant_id),
    FOREIGN KEY (store_id, merchant_id) REFERENCES stores(id, merchant_id)
);
CREATE INDEX idx_freight_templates_list ON freight_templates(merchant_id, id DESC)
    WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX uk_freight_templates_default ON freight_templates(merchant_id)
    WHERE is_default AND deleted_at IS NULL;
CREATE UNIQUE INDEX uk_freight_templates_store ON freight_templates(merchant_id, store_id)
    WHERE store_id IS NOT NULL AND deleted_at IS NULL;

CREATE TABLE freight_template_rules (
    id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id          BIGINT   NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    template_id          BIGINT   NOT NULL,
    sort_order           INT      NOT NULL DEFAULT 0,     -- 录入顺序
    region_codes         TEXT[]   NOT NULL DEFAULT '{}',  -- 这条规则管哪些省；空 = 默认规则（其余地区）
    first_unit           INT      NOT NULL,               -- 首件件数 / 首重克数
    first_fee_cents      BIGINT   NOT NULL,
    additional_unit      INT      NOT NULL,               -- 续件件数 / 续重克数
    additional_fee_cents BIGINT   NOT NULL,
    free_threshold_cents BIGINT   NOT NULL DEFAULT 0,     -- 满额包邮（优惠后应付商品金额），0 = 不设
    free_quantity        INT      NOT NULL DEFAULT 0,     -- 满件包邮，0 = 不设
    CONSTRAINT chk_freight_rule CHECK (
        first_unit >= 1 AND additional_unit >= 1
        AND first_fee_cents >= 0 AND additional_fee_cents >= 0
        AND free_threshold_cents >= 0 AND free_quantity >= 0
    ),
    CONSTRAINT chk_freight_rule_regions CHECK (region_codes <@ ARRAY[
        '110000','120000','130000','140000','150000','210000','220000','230000',
        '310000','320000','330000','340000','350000','360000','370000',
        '410000','420000','430000','440000','450000','460000',
        '500000','510000','520000','530000','540000',
        '610000','620000','630000','640000','650000','710000','810000','820000']::TEXT[]),
    FOREIGN KEY (template_id, merchant_id) REFERENCES freight_templates(id, merchant_id)
);
CREATE INDEX idx_freight_rules_tpl ON freight_template_rules(merchant_id, template_id, sort_order);
CREATE UNIQUE INDEX uk_freight_rules_default ON freight_template_rules(merchant_id, template_id)
    WHERE region_codes = '{}';

-- 商品单独挂的模板（只能是全店模板，见文件头第一节）。可空：MATCH SIMPLE 下
-- NULL 不做检查，「不单独挂」自动不受约束。
ALTER TABLE products ADD COLUMN freight_template_id BIGINT;
ALTER TABLE products ADD CONSTRAINT products_freight_template_fkey
    FOREIGN KEY (freight_template_id, merchant_id) REFERENCES freight_templates(id, merchant_id);
-- 「还有几件商品挂着这个模板」（删除与改归属前要数）
CREATE INDEX idx_products_freight_template ON products(merchant_id, freight_template_id)
    WHERE freight_template_id IS NOT NULL AND deleted_at IS NULL;

ALTER TABLE freight_templates ENABLE ROW LEVEL SECURITY;
ALTER TABLE freight_templates FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON freight_templates
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

ALTER TABLE freight_template_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE freight_template_rules FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON freight_template_rules
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

CREATE OR REPLACE TRIGGER touch_freight_templates_updated_at
    BEFORE UPDATE ON freight_templates FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

GRANT SELECT, INSERT, UPDATE, DELETE ON freight_templates, freight_template_rules TO keel_app;

-- +goose Down
DROP INDEX idx_products_freight_template;
ALTER TABLE products DROP CONSTRAINT products_freight_template_fkey;
ALTER TABLE products DROP COLUMN freight_template_id;
REVOKE ALL ON freight_templates, freight_template_rules FROM keel_app;
DROP TABLE freight_template_rules;
DROP TABLE freight_templates;
