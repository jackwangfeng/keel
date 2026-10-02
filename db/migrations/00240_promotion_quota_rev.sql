-- 活动配额同步改成「载荷带定义 + 版本」（2026-10-02，docs/电商系统-微服务部署方案.md 4.1）。
--
-- 以前：core 改了活动配额 → 二阶段消息通知库存服务 → 库存服务**回 core 读**当前定义。那是下层调上层（反向依赖），
-- 库存服务因此要配 core 的地址。现在：core 在写活动的同一个本地事务里把版本 +1、读出定义，一起放进消息载荷；
-- 库存服务按版本只接受更新的那份，不再回 core 读。
--
-- ① promotions.quota_rev：每次登记配额同步消息 +1（UPDATE ... RETURNING，同一个事务）。那条 UPDATE 锁住活动行，
--   并发改同一场活动的两个事务因此按提交顺序拿到递增的版本，后提交的那份定义一定包含先提交的改动——「版本大 = 更新」成立。
--   加列只改目录（常量默认值，PG 11 起不重写表）。
-- ② activity_sync_revs：库存服务记每场活动已经应用到的版本。归库存服务（与 activity_stocks 同一个处境）：
--   单体库里由这里建；拆分部署的库存库由 db/migrations-inventory/00240 建，两份逐字一致。
-- +goose Up
ALTER TABLE promotions ADD COLUMN quota_rev BIGINT NOT NULL DEFAULT 0;
COMMENT ON COLUMN promotions.quota_rev IS '配额同步消息的版本：每登记一次 +1（00240），库存服务按它只接受更新的定义';

CREATE TABLE activity_sync_revs (
    merchant_id  BIGINT      NOT NULL DEFAULT current_merchant(),
    promotion_id BIGINT      NOT NULL,
    rev          BIGINT      NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT activity_sync_revs_pkey PRIMARY KEY (merchant_id, promotion_id)
);
COMMENT ON TABLE activity_sync_revs IS
    '库存服务已应用到的活动配额定义版本（00240）：载荷里的版本不大于它的消息直接忽略（乱序、重复）。';

ALTER TABLE activity_sync_revs ENABLE ROW LEVEL SECURITY;
ALTER TABLE activity_sync_revs FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON activity_sync_revs
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());
CREATE TRIGGER touch_activity_sync_revs_updated_at
    BEFORE UPDATE ON activity_sync_revs FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON activity_sync_revs TO keel_app;

-- +goose Down
DROP TABLE activity_sync_revs;
ALTER TABLE promotions DROP COLUMN quota_rev;
