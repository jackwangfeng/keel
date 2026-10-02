-- 渠道适配层的核心表（docs/superpowers/specs/2026-10-02-channel-adapter-design.md §5.2）。
--
-- 只在接了渠道的商家下有行；KEEL_CHANNELS 关闭时没有任何代码读写它们（不变量「不配渠道零开销」）。
-- 渠道订单（channel_orders / channel_order_requests）与 orders 的改动在第三期（00320 起），
-- 对账表在第五期（00340 起）。
-- +goose Up

-- 一个商家接的一个渠道账号。roles 是位：1 商品源、2 库存源、4 销售渠道。
-- status：1 启用、2 停用、3 凭据失效（续期失败，停推送并告警）。
-- secrets 只写不读：仓储的普通读路径不选这一列（repository/channel.go），接口不回显。
CREATE TABLE channel_bindings (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id      BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id) ON DELETE CASCADE,
    channel          TEXT        NOT NULL CHECK (channel ~ '^[a-z][a-z0-9_.]{0,31}$'),
    external_account TEXT        NOT NULL CHECK (length(external_account) BETWEEN 1 AND 200),
    name             TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
    roles            SMALLINT    NOT NULL CHECK (roles BETWEEN 1 AND 7),
    status           SMALLINT    NOT NULL DEFAULT 2 CHECK (status IN (1, 2, 3)),
    config           JSONB       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(config) = 'object'),
    secrets          JSONB       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(secrets) = 'object'),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uk_channel_bindings_account UNIQUE (merchant_id, channel, external_account)
);
CREATE UNIQUE INDEX uk_channel_bindings_id_merchant ON channel_bindings(id, merchant_id);
CREATE TRIGGER touch_channel_bindings_updated_at
    BEFORE UPDATE ON channel_bindings FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- keel 门店 ↔ 渠道门店（Shopify location gid / 美团 app_poi_code / 饿了么 shop_id）。
CREATE TABLE channel_store_links (
    merchant_id       BIGINT      NOT NULL DEFAULT current_merchant(),
    binding_id        BIGINT      NOT NULL,
    store_id          BIGINT      NOT NULL,
    external_store_id TEXT        NOT NULL CHECK (length(external_store_id) BETWEEN 1 AND 200),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, binding_id, store_id),
    CONSTRAINT uk_channel_store_links_external UNIQUE (merchant_id, binding_id, external_store_id),
    FOREIGN KEY (binding_id, merchant_id) REFERENCES channel_bindings(id, merchant_id) ON DELETE CASCADE,
    FOREIGN KEY (store_id, merchant_id)   REFERENCES stores(id, merchant_id) ON DELETE CASCADE
);
CREATE INDEX idx_channel_store_links_store ON channel_store_links(merchant_id, store_id);

-- keel 商品 / SKU ↔ 外部 ID（参照 user_identities）。kind：1 商品、2 SKU。
-- extra 放渠道附带的 ID（如 Shopify 的 inventoryItem gid）。
CREATE TABLE channel_item_links (
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant(),
    binding_id  BIGINT      NOT NULL,
    kind        SMALLINT    NOT NULL CHECK (kind IN (1, 2)),
    keel_id     BIGINT      NOT NULL,
    external_id TEXT        NOT NULL CHECK (length(external_id) BETWEEN 1 AND 200),
    extra       JSONB       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(extra) = 'object'),
    synced_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, binding_id, kind, keel_id),
    CONSTRAINT uk_channel_item_links_external UNIQUE (merchant_id, binding_id, kind, external_id),
    FOREIGN KEY (binding_id, merchant_id) REFERENCES channel_bindings(id, merchant_id) ON DELETE CASCADE
);

-- 价格规则：sku_id 为空是渠道级（只用 markup_bp），非空是 SKU 级覆盖（fixed_cents 优先于 markup_bp）。
CREATE TABLE channel_price_rules (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant(),
    binding_id  BIGINT      NOT NULL,
    sku_id      BIGINT,
    markup_bp   INT         NOT NULL DEFAULT 0 CHECK (markup_bp BETWEEN -9000 AND 100000),
    fixed_cents BIGINT      CHECK (fixed_cents > 0),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uk_channel_price_rules_scope UNIQUE NULLS NOT DISTINCT (merchant_id, binding_id, sku_id),
    CONSTRAINT chk_channel_price_rules_fixed_needs_sku CHECK (fixed_cents IS NULL OR sku_id IS NOT NULL),
    FOREIGN KEY (binding_id, merchant_id) REFERENCES channel_bindings(id, merchant_id) ON DELETE CASCADE,
    FOREIGN KEY (sku_id, merchant_id)     REFERENCES skus(id, merchant_id) ON DELETE CASCADE
);

-- 库存分配规则：对外可售数 = clamp(floor(可售 × ratio_bp / 10000) − safety_qty, 0, cap_qty)。
-- 粒度：渠道（store、sku 都空）/ 渠道 × 门店（sku 空）/ 渠道 × 门店 × SKU。只有 SKU 没有门店不成立。
CREATE TABLE channel_stock_rules (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant(),
    binding_id  BIGINT      NOT NULL,
    store_id    BIGINT,
    sku_id      BIGINT,
    ratio_bp    INT         NOT NULL DEFAULT 10000 CHECK (ratio_bp BETWEEN 0 AND 10000),
    safety_qty  INT         NOT NULL DEFAULT 0 CHECK (safety_qty >= 0),
    cap_qty     INT         CHECK (cap_qty >= 0),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uk_channel_stock_rules_scope UNIQUE NULLS NOT DISTINCT (merchant_id, binding_id, store_id, sku_id),
    CONSTRAINT chk_channel_stock_rules_sku_needs_store CHECK (sku_id IS NULL OR store_id IS NOT NULL),
    FOREIGN KEY (binding_id, merchant_id) REFERENCES channel_bindings(id, merchant_id) ON DELETE CASCADE,
    FOREIGN KEY (store_id, merchant_id)   REFERENCES stores(id, merchant_id) ON DELETE CASCADE,
    FOREIGN KEY (sku_id, merchant_id)     REFERENCES skus(id, merchant_id) ON DELETE CASCADE
);
CREATE TRIGGER touch_channel_price_rules_updated_at
    BEFORE UPDATE ON channel_price_rules FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
CREATE TRIGGER touch_channel_stock_rules_updated_at
    BEFORE UPDATE ON channel_stock_rules FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 最近一次推给渠道的状态：与待推值相同就不推；对账时作 keel 侧值；Shopify CAS 的 changeFromQuantity。
-- version 每次推送成功加一（幂等键 binding:store:sku:version）。
CREATE TABLE channel_listings (
    merchant_id     BIGINT      NOT NULL DEFAULT current_merchant(),
    binding_id      BIGINT      NOT NULL,
    store_id        BIGINT      NOT NULL,
    sku_id          BIGINT      NOT NULL,
    published_qty   INT         NOT NULL CHECK (published_qty >= 0),
    published_cents BIGINT      NOT NULL CHECK (published_cents >= 0),
    version         BIGINT      NOT NULL DEFAULT 1,
    pushed_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    PRIMARY KEY (merchant_id, binding_id, store_id, sku_id),
    FOREIGN KEY (binding_id, merchant_id) REFERENCES channel_bindings(id, merchant_id) ON DELETE CASCADE,
    FOREIGN KEY (store_id, merchant_id)   REFERENCES stores(id, merchant_id) ON DELETE CASCADE,
    FOREIGN KEY (sku_id, merchant_id)     REFERENCES skus(id, merchant_id) ON DELETE CASCADE
);

-- 回调去重与留档。status：0 待处理、1 已处理、2 忽略（binding 停用 / 没有处理器）、3 处理失败。
CREATE TABLE channel_inbound_events (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id       BIGINT      NOT NULL DEFAULT current_merchant(),
    binding_id        BIGINT      NOT NULL,
    external_event_id TEXT        NOT NULL CHECK (length(external_event_id) BETWEEN 1 AND 200),
    topic             TEXT        NOT NULL CHECK (length(topic) BETWEEN 1 AND 100),
    payload           JSONB       NOT NULL,
    status            SMALLINT    NOT NULL DEFAULT 0 CHECK (status IN (0, 1, 2, 3)),
    error             TEXT,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at      TIMESTAMPTZ,
    CONSTRAINT uk_channel_inbound_events_external UNIQUE (merchant_id, binding_id, external_event_id),
    FOREIGN KEY (binding_id, merchant_id) REFERENCES channel_bindings(id, merchant_id) ON DELETE CASCADE
);
CREATE INDEX idx_channel_inbound_events_received ON channel_inbound_events(merchant_id, received_at);

-- +goose StatementBegin
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['channel_bindings','channel_store_links','channel_item_links',
                             'channel_price_rules','channel_stock_rules','channel_listings',
                             'channel_inbound_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY tenant ON %I USING (merchant_id = current_merchant()) '
                       'WITH CHECK (merchant_id = current_merchant())', t);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO keel_app', t);
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE channel_inbound_events;
DROP TABLE channel_listings;
DROP TABLE channel_stock_rules;
DROP TABLE channel_price_rules;
DROP TABLE channel_item_links;
DROP TABLE channel_store_links;
DROP TABLE channel_bindings;
