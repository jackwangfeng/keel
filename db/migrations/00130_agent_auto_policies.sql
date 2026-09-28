-- 自动执行策略（AI 经营 M11，docs/AI经营-M10M11设计.md §6）：店长按 AI 员工 × 提案种类放开自动执行，带单笔上限
-- 与每日条数上限。提案写入时命中策略且在上限内，当场以 AI 员工身份执行（与人批准走同一段执行代码），
-- decided_by 为空、auto_approved = true；超限的照常进待处理队列。
--
-- 售后审核（refund_decision）不许自动执行：它是资金动作，CHECK 挡住，写不进来。
-- 单笔上限按种类取不同的列（其余列不用）：
--   inventory_adjust  max_units：一条至多加多少件
--   flash_price       min_discount_rate：折扣率不低于它（千分比，900 = 最多打九折）
--   coupon            max_discount_cents：面额（折扣券为封顶）不高于它
--   product_copy      只有每日条数上限
-- +goose Up
CREATE TABLE agent_auto_policies (
    merchant_id        BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    agent_staff_id     BIGINT      NOT NULL,
    kind               TEXT        NOT NULL,
    enabled            BOOLEAN     NOT NULL DEFAULT FALSE,
    max_units          INT         NOT NULL DEFAULT 0,
    min_discount_rate  SMALLINT    NOT NULL DEFAULT 1000,
    max_discount_cents BIGINT      NOT NULL DEFAULT 0,
    daily_limit        INT         NOT NULL DEFAULT 0,
    updated_by         BIGINT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, agent_staff_id, kind),
    CONSTRAINT chk_auto_policy_kind CHECK (kind IN ('inventory_adjust', 'flash_price', 'coupon', 'product_copy')),
    CONSTRAINT chk_auto_policy_limits CHECK (max_units >= 0 AND max_units <= 1000
        AND min_discount_rate BETWEEN 500 AND 1000 AND max_discount_cents BETWEEN 0 AND 10000
        AND daily_limit BETWEEN 0 AND 100),
    FOREIGN KEY (agent_staff_id, merchant_id) REFERENCES staff(id, merchant_id) ON DELETE CASCADE,
    FOREIGN KEY (updated_by, merchant_id)     REFERENCES staff(id, merchant_id)
);

CREATE OR REPLACE TRIGGER touch_agent_auto_policies_updated_at
    BEFORE UPDATE ON agent_auto_policies FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

COMMENT ON TABLE agent_auto_policies IS 'AI 员工按提案种类的自动执行策略（00130，AI 经营 M11）。';

ALTER TABLE agent_auto_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_auto_policies FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON agent_auto_policies
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

GRANT SELECT, INSERT, UPDATE, DELETE ON agent_auto_policies TO keel_app;

ALTER TABLE agent_proposals ADD COLUMN auto_approved BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX idx_agent_proposals_auto_daily ON agent_proposals(merchant_id, agent_staff_id, kind, decided_at)
    WHERE auto_approved;

-- +goose Down
DROP INDEX idx_agent_proposals_auto_daily;
ALTER TABLE agent_proposals DROP COLUMN auto_approved;
DROP TABLE agent_auto_policies;
