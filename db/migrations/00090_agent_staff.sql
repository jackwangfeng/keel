-- AI 员工与接入密钥（AI 经营 M9 任务 1，docs/AI经营-M9设计.md §2）。
--
-- ### AI 员工就是一行 staff
--
-- kind 1 人 / 2 AI。角色与管辖范围（staff_scopes）完全复用 —— AI 员工走和人一模一样的判权，
-- 没有「AI 专用后门」。两条约束写在库上而不是只写在 service 里，因为它们是安全边界：
--   · AI 员工不许是管理员（role = 1 能改店铺设置、设默认门店、管员工，不该交给 agent）；
--   · AI 员工不许是平台级（merchant_id 为 NULL 能跨租户运维）。
-- AI 员工**不能登录后台**：一次性登录令牌的查找与会话续期两条查询都只认 kind = 1（db/queries/staff.sql），
-- 员工管理的列表 / 详情 / 修改也只认 kind = 1 —— AI 员工只从 /admin/agents 管。
--
-- ### 接入密钥 agent_keys
--
-- 明文 kagt_<base64url>，只在创建响应里出现一次；库里只存 sha256。查找与员工会话令牌同一个做法：
-- 租户先由 Host 解析，再在这家店的租户事务里按哈希查 —— 没有跨租户的查找路径，唯一索引收在租户内。
-- +goose Up
ALTER TABLE staff ADD COLUMN kind SMALLINT NOT NULL DEFAULT 1;
ALTER TABLE staff ADD CONSTRAINT chk_staff_kind CHECK (kind IN (1, 2));
ALTER TABLE staff ADD CONSTRAINT chk_staff_agent_not_admin CHECK (kind = 1 OR role <> 1);
ALTER TABLE staff ADD CONSTRAINT chk_staff_agent_has_merchant CHECK (kind = 1 OR merchant_id IS NOT NULL);

CREATE TABLE agent_keys (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id  BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    staff_id     BIGINT      NOT NULL,
    name         TEXT        NOT NULL,
    prefix       TEXT        NOT NULL,            -- 明文前 12 位（含 kagt_），列表里给人认
    secret_hash  TEXT        NOT NULL,            -- hex(sha256(明文))
    expires_at   TIMESTAMPTZ,                     -- NULL = 不过期
    revoked_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    created_by   BIGINT      NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (staff_id, merchant_id)   REFERENCES staff(id, merchant_id),
    FOREIGN KEY (created_by, merchant_id) REFERENCES staff(id, merchant_id)
);
CREATE UNIQUE INDEX uk_agent_keys_hash ON agent_keys(merchant_id, secret_hash);
CREATE INDEX idx_agent_keys_staff ON agent_keys(merchant_id, staff_id);

COMMENT ON TABLE agent_keys IS 'AI 员工的接入密钥（00090，AI 经营 M9）。只存 sha256；明文只在创建响应里出现一次。';

ALTER TABLE agent_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_keys FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON agent_keys
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

GRANT SELECT, INSERT, UPDATE, DELETE ON agent_keys TO keel_app;

-- +goose Down
DROP TABLE agent_keys;
ALTER TABLE staff DROP CONSTRAINT chk_staff_agent_has_merchant;
ALTER TABLE staff DROP CONSTRAINT chk_staff_agent_not_admin;
ALTER TABLE staff DROP CONSTRAINT chk_staff_kind;
ALTER TABLE staff DROP COLUMN kind;
