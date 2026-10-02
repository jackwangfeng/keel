-- 挂零时段（docs/superpowers/specs/2026-10-03-ai-channel-allocation-design.md §3.2）：
-- 渠道上某个（binding, 门店, SKU）格子对外可售数是 0 的起止，给 AI 调渠道分配算「挂零小时数」。
--
-- held：true = keel 有货但分配规则算出来是 0（分配造成的）；false = keel 自己就没货（或下架不可售）。
-- ended_at 为空 = 还挂着；每个格子至多一段还挂着（部分唯一索引 uk_channel_listing_zero_spans_open）。
-- 只在推送成功回写 channel_listings 的同一事务里写（service/channel_worker.go pushStore），KEEL_CHANNELS 关着时不写。
-- 上线前已经是 0 的格子不补历史：从上线后第一次推送起量，满 7 天数据才完整。保留 180 天（housekeep 按批删）。
-- 新表、空表：索引普通建。
-- +goose Up
CREATE TABLE channel_listing_zero_spans (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant(),
    binding_id  BIGINT      NOT NULL,
    store_id    BIGINT      NOT NULL,
    sku_id      BIGINT      NOT NULL,
    held        BOOLEAN     NOT NULL,
    started_at  TIMESTAMPTZ NOT NULL,
    ended_at    TIMESTAMPTZ,
    CONSTRAINT chk_channel_listing_zero_spans_range CHECK (ended_at IS NULL OR ended_at >= started_at),
    FOREIGN KEY (binding_id, merchant_id) REFERENCES channel_bindings(id, merchant_id) ON DELETE CASCADE,
    FOREIGN KEY (store_id, merchant_id)   REFERENCES stores(id, merchant_id) ON DELETE CASCADE,
    FOREIGN KEY (sku_id, merchant_id)     REFERENCES skus(id, merchant_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX uk_channel_listing_zero_spans_open
    ON channel_listing_zero_spans(merchant_id, binding_id, store_id, sku_id) WHERE ended_at IS NULL;
CREATE INDEX idx_channel_listing_zero_spans_store ON channel_listing_zero_spans(merchant_id, store_id, started_at);

ALTER TABLE channel_listing_zero_spans ENABLE ROW LEVEL SECURITY;
ALTER TABLE channel_listing_zero_spans FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON channel_listing_zero_spans USING (merchant_id = current_merchant())
    WITH CHECK (merchant_id = current_merchant());
GRANT SELECT, INSERT, UPDATE, DELETE ON channel_listing_zero_spans TO keel_app;

-- +goose Down
DROP TABLE channel_listing_zero_spans;
