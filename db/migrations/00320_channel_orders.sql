-- 渠道层第三期：渠道订单（docs/superpowers/specs/2026-10-02-channel-adapter-design.md §5.2、§7）。
--
-- 一、orders 放开买家可空并标来源：source 0 自营（user_id 必填，chk_order_buyer 钉住）、
--     1 渠道单（没有 keel 买家，user_id 为空；fk_orders_user 是 MATCH SIMPLE，空值不检查）。
--     channel_order_id 指回 channel_orders。默认值让自营写路径一行不改。
-- 二、chk_discount_sources：渠道单的优惠来自平台（没有券、也不是 keel 的活动），
--     对 source = 1 放开「没券时优惠恰好等于活动那一份」，活动优惠的上下界照旧。
-- 三、channel_orders（平台订单的规整快照，一张平台单一行）与 channel_order_requests
--     （平台发起的取消 / 部分退款 / 缺货调整申请）。只在接了渠道的商家下有行。
--
-- orders 是大表：这里加的约束与外键一律 NOT VALID（只拿短锁、不扫表，新写入照样检查），
-- 00321 用 CONCURRENTLY 建 idx_orders_channel，00322 在另一个事务里 VALIDATE（不挡写）。
-- +goose Up

ALTER TABLE orders ADD COLUMN source SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE orders ADD CONSTRAINT chk_order_source CHECK (source IN (0, 1)) NOT VALID;
ALTER TABLE orders ADD COLUMN channel_order_id BIGINT;
ALTER TABLE orders ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE orders ADD CONSTRAINT chk_order_buyer CHECK (source <> 0 OR user_id IS NOT NULL) NOT VALID;
ALTER TABLE orders ADD CONSTRAINT chk_order_channel CHECK (source = 1 OR channel_order_id IS NULL) NOT VALID;

ALTER TABLE orders DROP CONSTRAINT chk_discount_sources;
ALTER TABLE orders ADD CONSTRAINT chk_discount_sources CHECK (
    promotion_discount_cents >= 0 AND promotion_discount_cents <= discount_cents
    AND (source = 1 OR user_coupon_id IS NOT NULL OR discount_cents = promotion_discount_cents)
) NOT VALID;

-- 平台订单。status 是规整状态：1 待付款 2 新单 3 已接单 4 已发货 5 已完成 6 已取消 7 已拒单。
-- version 单调（Shopify 用 updatedAt 毫秒）：旧版本只留档不改状态。
-- exception 是需要人处理的原因（缺货 / 行没映射 / 发货后被取消……），处理掉清空。
-- amounts：{goods,freight,platform_subsidy,merchant_subsidy,commission,tax,merchant_receivable,buyer_paid,refunded}（分）
--   + taxes_included（价内税：行价已含税，keel 实付 = 顾客付的总价）；
-- lines：[{external_line_id, external_sku_id, sku_id|null, title, qty, price_cents, refunded_qty}]；
-- receiver：{name, phone, phone_kind(0 真实 1 隐私号), address{...}}。
-- order_no 引用 orders(order_no) 是单列外键（order_no 全局唯一，见 db/tenancy.json fk_single_column_ok）。
CREATE TABLE channel_orders (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id         BIGINT      NOT NULL DEFAULT current_merchant(),
    binding_id          BIGINT      NOT NULL,
    external_order_id   TEXT        NOT NULL CHECK (length(external_order_id) BETWEEN 1 AND 200),
    external_order_name TEXT        NOT NULL DEFAULT '',
    store_id            BIGINT,
    order_no            TEXT        REFERENCES orders(order_no),
    platform_status     TEXT        NOT NULL,
    status              SMALLINT    NOT NULL CHECK (status BETWEEN 1 AND 7),
    exception           TEXT,
    accept_deadline     TIMESTAMPTZ,
    pick_deadline       TIMESTAMPTZ,
    delivery_mode       SMALLINT    NOT NULL DEFAULT 0 CHECK (delivery_mode >= 0),
    rider               JSONB       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(rider) = 'object'),
    amounts             JSONB       NOT NULL CHECK (jsonb_typeof(amounts) = 'object'),
    lines               JSONB       NOT NULL CHECK (jsonb_typeof(lines) = 'array'),
    receiver            JSONB       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(receiver) = 'object'),
    version             BIGINT      NOT NULL,
    last_payload        JSONB       NOT NULL,
    test                BOOLEAN     NOT NULL DEFAULT false,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uk_channel_orders_external UNIQUE (merchant_id, binding_id, external_order_id),
    CONSTRAINT uk_channel_orders_order_no UNIQUE (merchant_id, order_no),
    UNIQUE (id, merchant_id),
    -- 有渠道单的 binding 不许删（NO ACTION：商家整体级联删除时同一语句里一起删掉不报错）
    FOREIGN KEY (binding_id, merchant_id) REFERENCES channel_bindings(id, merchant_id),
    FOREIGN KEY (store_id, merchant_id)   REFERENCES stores(id, merchant_id)
);
CREATE INDEX idx_channel_orders_binding_status ON channel_orders(merchant_id, binding_id, status);
CREATE INDEX idx_channel_orders_exception ON channel_orders(merchant_id, updated_at DESC)
    WHERE exception IS NOT NULL;
CREATE TRIGGER touch_channel_orders_updated_at
    BEFORE UPDATE ON channel_orders FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

ALTER TABLE orders ADD CONSTRAINT fk_orders_channel_order
    FOREIGN KEY (channel_order_id, merchant_id) REFERENCES channel_orders(id, merchant_id) NOT VALID;

-- 平台发起的申请。kind：1 取消 2 部分退款 3 缺货调整。
-- status：1 待处理 2 已同意 3 已拒绝 4 超时自动同意 5 平台已撤销。decided_by 是处理的员工（单列外键指向 staff）。
CREATE TABLE channel_order_requests (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id         BIGINT      NOT NULL DEFAULT current_merchant(),
    channel_order_id    BIGINT      NOT NULL,
    external_request_id TEXT        NOT NULL CHECK (length(external_request_id) BETWEEN 1 AND 200),
    kind                SMALLINT    NOT NULL CHECK (kind IN (1, 2, 3)),
    lines               JSONB       NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(lines) = 'array'),
    amount_cents        BIGINT      NOT NULL DEFAULT 0 CHECK (amount_cents >= 0),
    reason              TEXT        NOT NULL DEFAULT '',
    status              SMALLINT    NOT NULL DEFAULT 1 CHECK (status BETWEEN 1 AND 5),
    deadline            TIMESTAMPTZ,
    decided_by          BIGINT      REFERENCES staff(id),
    decided_at          TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uk_channel_order_requests_external UNIQUE (merchant_id, channel_order_id, external_request_id),
    FOREIGN KEY (channel_order_id, merchant_id) REFERENCES channel_orders(id, merchant_id) ON DELETE CASCADE
);
CREATE INDEX idx_channel_order_requests_pending ON channel_order_requests(merchant_id, deadline)
    WHERE status = 1;
CREATE TRIGGER touch_channel_order_requests_updated_at
    BEFORE UPDATE ON channel_order_requests FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- +goose StatementBegin
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['channel_orders','channel_order_requests'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY tenant ON %I USING (merchant_id = current_merchant()) '
                       'WITH CHECK (merchant_id = current_merchant())', t);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO keel_app', t);
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
-- 有渠道单（source = 1、user_id 为空）时，最后那条 SET NOT NULL 会直接失败——
-- 刻意不在这里 DELETE 订单：交易数据不该被一次回滚悄悄删掉，要回滚先人工处理那些单。
ALTER TABLE orders DROP CONSTRAINT fk_orders_channel_order;
DROP TABLE channel_order_requests;
DROP TABLE channel_orders;
ALTER TABLE orders DROP CONSTRAINT chk_discount_sources;
ALTER TABLE orders ADD CONSTRAINT chk_discount_sources CHECK (
    promotion_discount_cents >= 0 AND promotion_discount_cents <= discount_cents
    AND (user_coupon_id IS NOT NULL OR discount_cents = promotion_discount_cents)
);
ALTER TABLE orders DROP CONSTRAINT chk_order_channel;
ALTER TABLE orders DROP CONSTRAINT chk_order_buyer;
ALTER TABLE orders DROP CONSTRAINT chk_order_source;
ALTER TABLE orders ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE orders DROP COLUMN channel_order_id;
ALTER TABLE orders DROP COLUMN source;
