-- 开了渠道的商家。与 core 的 db/migrations/00300 逐字一致（这个目录的规矩，见 00001 文件头），
-- 理由写在那一份里。单体库上 core 那份先建好了，这里 IF NOT EXISTS 是空操作；拆分部署的库存库上由这里建。
-- +goose Up
-- 库存服务的应用角色：与 00001 同一个可配名字（KEEL_INVENTORY_ROLE，默认 keel_app）。
-- +goose ENVSUB ON
SET LOCAL keel.inventory_role = '${KEEL_INVENTORY_ROLE:-keel_app}';
-- +goose ENVSUB OFF
CREATE TABLE IF NOT EXISTS channel_merchants (
    merchant_id BIGINT      NOT NULL DEFAULT current_merchant(),
    enabled_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT channel_merchants_pkey PRIMARY KEY (merchant_id)
);
ALTER TABLE channel_merchants ENABLE ROW LEVEL SECURITY;
ALTER TABLE channel_merchants FORCE  ROW LEVEL SECURITY;
-- +goose StatementBegin
DO $$
DECLARE r TEXT := current_setting('keel.inventory_role');
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = current_schema()
                    AND tablename = 'channel_merchants' AND policyname = 'tenant') THEN
        CREATE POLICY tenant ON channel_merchants
          USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());
    END IF;
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON channel_merchants TO %I', r);
END $$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS channel_merchants;
