-- 开了渠道的商家（渠道适配层，docs/superpowers/specs/2026-10-02-channel-adapter-design.md §5.4 / §8）。
--
-- 这张表属于**库存服务**：库存服务在可售数变化时，只给这里登记过的商家发 stock.changed（二阶段消息），
-- 其余商家的库存写路径与没有渠道层时完全一样（不变量「不配渠道零开销」）。行由 core 在启用 / 停用
-- 销售渠道 binding 时经二阶段消息维护（service/channel.go → inventory/channel_msg.go），库存服务进程内缓存。
--
-- 与 db/migrations-inventory/00300 逐字一致（那个目录的规矩，见其 00001 文件头）：单体库上先由这里建，
-- 拆分部署的库存库上由那一份建。
-- +goose Up
CREATE TABLE IF NOT EXISTS channel_merchants (
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant(),
    enabled_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT channel_merchants_pkey PRIMARY KEY (merchant_id)
);
ALTER TABLE channel_merchants ENABLE ROW LEVEL SECURITY;
ALTER TABLE channel_merchants FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON channel_merchants
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());
GRANT SELECT, INSERT, UPDATE, DELETE ON channel_merchants TO keel_app;

-- +goose Down
DROP TABLE IF EXISTS channel_merchants;
