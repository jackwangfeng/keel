-- 库存服务已应用到的活动配额定义版本。与 core 的 db/migrations/00240 里那张表逐字一致（这个目录的规矩，见 00001 文件头），
-- 理由写在那一份里。单体库上 core 那份先建好了，这里 IF NOT EXISTS 是空操作；拆分部署的库存库上由这里建。
-- +goose Up
CREATE TABLE IF NOT EXISTS activity_sync_revs (
    merchant_id  BIGINT      NOT NULL DEFAULT current_merchant(),
    promotion_id BIGINT      NOT NULL,
    rev          BIGINT      NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT activity_sync_revs_pkey PRIMARY KEY (merchant_id, promotion_id)
);
ALTER TABLE activity_sync_revs ENABLE ROW LEVEL SECURITY;
ALTER TABLE activity_sync_revs FORCE  ROW LEVEL SECURITY;

-- 库存服务的应用角色：与 00001 同一个可配名字（KEEL_INVENTORY_ROLE，默认 keel_app）。
-- +goose ENVSUB ON
SET LOCAL keel.inventory_role = '${KEEL_INVENTORY_ROLE:-keel_app}';
-- +goose ENVSUB OFF

-- +goose StatementBegin
DO $$
DECLARE r TEXT := current_setting('keel.inventory_role');
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = current_schema()
                    AND tablename = 'activity_sync_revs' AND policyname = 'tenant') THEN
        CREATE POLICY tenant ON activity_sync_revs
          USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'touch_activity_sync_revs_updated_at'
                    AND tgrelid = 'activity_sync_revs'::regclass) THEN
        CREATE TRIGGER touch_activity_sync_revs_updated_at
            BEFORE UPDATE ON activity_sync_revs FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
    END IF;
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON activity_sync_revs TO %I', r);
END $$;
-- +goose StatementEnd

-- +goose Down
-- 什么都不做，理由与 00001 的 Down 相同：单体库里这张表归 core 的 00240 所有。
SELECT 1;
