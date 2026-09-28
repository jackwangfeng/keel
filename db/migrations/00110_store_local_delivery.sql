-- 同城配送（2026-09-28）：有围栏的门店按「起送价 + 按距离分档的配送费 + 满额免配送费」收配送费，
-- 不再读运费模板（模板是照全国快递设计的，按省定价；围栏店是几公里内配送，省这一维没有意义，
-- 而商品单独挂的快递模板会盖掉门店自己的配送费 —— 这两处是它们凑在一起时的真问题）。
-- 默认门店（全国兜底，靠快递）与没有围栏的门店照旧走运费模板。
--
-- 一家店一行，没有这一行 = 全部为 0（不设起送价、配送费 0）。
-- fee_tiers：[{"within_m": 3000, "fee_cents": 300}, {"within_m": 5000, "fee_cents": 500}]，within_m 严格递增，
-- 距离（门店坐标到收货地址坐标的球面距离）落进第一个 within_m ≥ 距离的档；超出最后一档（围栏比最后一档大）
-- 按最后一档收；算不出距离（地址或门店没有坐标）也按最后一档收 —— 宁可多收几块，不按最近一档少收。
-- 形状由 service 校验（档数、递增、非负），这里只守类型与非负。
-- +goose Up
CREATE TABLE store_local_delivery (
    merchant_id      BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    store_id         BIGINT      NOT NULL,
    min_order_cents  BIGINT      NOT NULL DEFAULT 0,
    free_over_cents  BIGINT      NOT NULL DEFAULT 0,
    fee_tiers        JSONB       NOT NULL DEFAULT '[]',
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_local_delivery_amounts CHECK (min_order_cents >= 0 AND free_over_cents >= 0),
    CONSTRAINT chk_local_delivery_tiers CHECK (jsonb_typeof(fee_tiers) = 'array' AND jsonb_array_length(fee_tiers) <= 10),
    PRIMARY KEY (merchant_id, store_id),
    FOREIGN KEY (store_id, merchant_id) REFERENCES stores(id, merchant_id) ON DELETE CASCADE
);

CREATE OR REPLACE TRIGGER touch_store_local_delivery_updated_at
    BEFORE UPDATE ON store_local_delivery FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

COMMENT ON TABLE store_local_delivery IS '有围栏门店的同城配送：起送价、按距离分档的配送费、满额免配送费（00110）。';

ALTER TABLE store_local_delivery ENABLE ROW LEVEL SECURITY;
ALTER TABLE store_local_delivery FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON store_local_delivery
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

GRANT SELECT, INSERT, UPDATE, DELETE ON store_local_delivery TO keel_app;

-- +goose Down
DROP TABLE store_local_delivery;
