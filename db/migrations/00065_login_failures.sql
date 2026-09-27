-- 口令登录失败计数挪进库里（数据模型 §9）。
--
-- 上一版的锁定计数在进程内存里。多实例实测（3 个实例 + nginx 轮询，不开 sticky）：
-- 同一个号要连错 15 次（3 × 5）才被锁，而且任何一次重启都把锁清零 —— 部署一次，
-- 所有被锁的号当场解锁。计数放进 Postgres：所有实例看的是同一行，重启不丢。
--
-- 每次失败一条原子 upsert（db/queries/users.sql 的 RecordLoginFailure），窗口过期就从 1 重新计，
-- 到阈值写 locked_until。并发的两次失败在主键上串行，不会丢一次计数。
-- 主键 (merchant_id, phone)：同一个号在两家店是两个人（§9），锁也各锁各的；
-- 不存在的号同样有一行 —— 只锁存在的号等于告诉对方「这个号在这家店有账号」。
-- 行数受「被试过的号」约束；失败路径顺手删掉本店已经过期很久的行，表不会无限长。

-- +goose Up
CREATE TABLE login_failures (
    merchant_id  BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id) ON DELETE CASCADE,
    phone        TEXT        NOT NULL,
    fail_count   INT         NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    locked_until TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, phone),
    CONSTRAINT chk_login_failures CHECK (fail_count >= 0 AND phone <> '' AND length(phone) <= 32)
);

ALTER TABLE login_failures ENABLE ROW LEVEL SECURITY;
ALTER TABLE login_failures FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON login_failures
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

CREATE OR REPLACE TRIGGER touch_login_failures_updated_at
    BEFORE UPDATE ON login_failures FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

GRANT SELECT, INSERT, UPDATE, DELETE ON login_failures TO keel_app;

-- +goose Down
DROP TABLE login_failures;
