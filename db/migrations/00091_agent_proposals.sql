-- AI 员工的提案（AI 经营 M9 任务 4，docs/AI经营-M9设计.md §4）。
--
-- AI 员工的写操作默认不直接执行：它提一条「证据 + 动作 + 预计影响」，人在后台批准后由 Keel 以 AI 员工的身份执行
-- （判权两道：批准的人对这件事要有权，执行时再按 AI 员工的身份判一次 —— 它提案之后范围可能被收窄了）。
--
-- 状态：10 待处理 / 15 执行中 / 20 已执行 / 30 已驳回 / 40 执行失败 / 50 已过期。
-- 15 是批准之后、执行结果写回之前：进程在这之间挂了，再点一次批准会用同一个幂等键重新执行（库存那一笔
-- 按 biz_id 幂等，不会加两次），所以批准接受 10 与 15 两种状态。
--
-- 同一个（kind，门店，SKU）同时只能有一条待处理 / 执行中：agent 重复提会被拒，并被告知已有的那条。
-- +goose Up
CREATE TABLE agent_proposals (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id     BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    agent_staff_id  BIGINT      NOT NULL,
    kind            TEXT        NOT NULL,
    store_id        BIGINT      NOT NULL,
    sku_id          BIGINT,
    payload         JSONB       NOT NULL,
    title           TEXT        NOT NULL,
    evidence        TEXT        NOT NULL,
    expected_impact TEXT        NOT NULL DEFAULT '',
    status          SMALLINT    NOT NULL DEFAULT 10,
    decided_by      BIGINT,
    decided_at      TIMESTAMPTZ,
    reject_reason   TEXT,
    result          JSONB,
    expires_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_agent_proposal_status CHECK (status IN (10, 15, 20, 30, 40, 50)),
    CONSTRAINT chk_agent_proposal_kind CHECK (kind IN ('inventory_adjust')),
    CONSTRAINT chk_agent_proposal_text CHECK (length(title) BETWEEN 1 AND 200 AND length(evidence) BETWEEN 1 AND 8000
                                              AND length(expected_impact) <= 2000),
    FOREIGN KEY (agent_staff_id, merchant_id) REFERENCES staff(id, merchant_id),
    FOREIGN KEY (decided_by, merchant_id)     REFERENCES staff(id, merchant_id),
    FOREIGN KEY (store_id, merchant_id)       REFERENCES stores(id, merchant_id),
    FOREIGN KEY (sku_id, merchant_id)         REFERENCES skus(id, merchant_id)
);
CREATE INDEX idx_agent_proposals_list ON agent_proposals(merchant_id, status, created_at DESC);
CREATE INDEX idx_agent_proposals_agent ON agent_proposals(merchant_id, agent_staff_id, created_at DESC);
CREATE UNIQUE INDEX uk_agent_proposals_open ON agent_proposals(merchant_id, kind, store_id, sku_id)
    WHERE status IN (10, 15);

COMMENT ON TABLE agent_proposals IS 'AI 员工的提案（00091，AI 经营 M9）：人批准后以 AI 员工的身份执行。';

ALTER TABLE agent_proposals ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_proposals FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON agent_proposals
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

CREATE OR REPLACE TRIGGER touch_agent_proposals_updated_at
    BEFORE UPDATE ON agent_proposals FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

GRANT SELECT, INSERT, UPDATE, DELETE ON agent_proposals TO keel_app;

-- +goose Down
DROP TABLE agent_proposals;
