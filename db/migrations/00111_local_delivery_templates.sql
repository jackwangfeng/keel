-- 同城配送模板（2026-09-28）：一套「起送价 + 满额免配送费 + 距离分档」存成模板，门店引用它，
-- 不用每开一家围栏店都从头填一遍；改模板即对引用它的全部门店生效。
--
-- 一家围栏店的配送规则从哪来（repository.LocalDeliveryForPricing，一条查询里判完）：
--
--     store_local_delivery 有这家店的行，且 template_id 非空 → 用那个模板
--     store_local_delivery 有这家店的行，且 template_id 为空 → 用这一行自己的数（自定义）
--     没有这家店的行                                        → 用全店默认模板（is_default）
--     也没有默认模板                                        → 全 0（不设起送价、配送费 0）
--
-- 所以新开的围栏店什么都不用做就按默认模板收费。
-- 删除：被门店引用的模板删不掉（FK RESTRICT，service 先查、报 409）；默认模板也不许直接删，
-- 先取消默认 —— 否则跟着默认走的店会悄悄变成配送费 0。
-- +goose Up
CREATE TABLE local_delivery_templates (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id      BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    name             TEXT        NOT NULL,
    is_default       BOOLEAN     NOT NULL DEFAULT FALSE,
    min_order_cents  BIGINT      NOT NULL DEFAULT 0,
    free_over_cents  BIGINT      NOT NULL DEFAULT 0,
    fee_tiers        JSONB       NOT NULL DEFAULT '[]',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_ld_template_name CHECK (length(name) BETWEEN 1 AND 50),
    CONSTRAINT chk_ld_template_amounts CHECK (min_order_cents >= 0 AND free_over_cents >= 0),
    CONSTRAINT chk_ld_template_tiers CHECK (jsonb_typeof(fee_tiers) = 'array' AND jsonb_array_length(fee_tiers) <= 10),
    UNIQUE (id, merchant_id)
);
CREATE UNIQUE INDEX uk_local_delivery_templates_default ON local_delivery_templates(merchant_id) WHERE is_default;
CREATE UNIQUE INDEX uk_local_delivery_templates_name ON local_delivery_templates(merchant_id, name);

CREATE OR REPLACE TRIGGER touch_local_delivery_templates_updated_at
    BEFORE UPDATE ON local_delivery_templates FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

COMMENT ON TABLE local_delivery_templates IS '同城配送模板：门店引用，没配的围栏店用默认模板（00111）。';

ALTER TABLE local_delivery_templates ENABLE ROW LEVEL SECURITY;
ALTER TABLE local_delivery_templates FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON local_delivery_templates
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

GRANT SELECT, INSERT, UPDATE, DELETE ON local_delivery_templates TO keel_app;

-- 门店引用模板：template_id 非空时这一行自己的数不用（保留着，切回「自定义」时还在）。
ALTER TABLE store_local_delivery ADD COLUMN template_id BIGINT;
ALTER TABLE store_local_delivery ADD CONSTRAINT fk_store_local_delivery_template
    FOREIGN KEY (template_id, merchant_id) REFERENCES local_delivery_templates(id, merchant_id);
CREATE INDEX idx_store_local_delivery_template ON store_local_delivery(merchant_id, template_id)
    WHERE template_id IS NOT NULL;

-- +goose Down
ALTER TABLE store_local_delivery DROP COLUMN template_id;
DROP TABLE local_delivery_templates;
