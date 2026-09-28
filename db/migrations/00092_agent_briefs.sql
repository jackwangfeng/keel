-- AI 员工写的经营简报（AI 经营 M9 任务 5，docs/AI经营-M9设计.md §6）。
--
-- 巡店日报之类：AI 员工用 MCP 工具 post_brief 写一份 markdown，后台「AI 员工 → 简报」按时间倒序看。
-- 简报是全店口径的经营信息，只给全店范围的人（管理员 / 操作员）看。
-- body 按不可信输入处理：后台渲染 markdown 时不渲染 HTML。
-- M9 不发站内通知（通知的种类与跳转目标在契约和三端客户端里都是枚举，改动面大）—— M10 与事件触发一起加。
-- +goose Up
CREATE TABLE agent_briefs (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id    BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    agent_staff_id BIGINT      NOT NULL,
    title          TEXT        NOT NULL,
    body           TEXT        NOT NULL,
    period_start   DATE        NOT NULL,
    period_end     DATE        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_agent_brief_text CHECK (length(title) BETWEEN 1 AND 100 AND length(body) BETWEEN 1 AND 8192),
    CONSTRAINT chk_agent_brief_period CHECK (period_start <= period_end),
    FOREIGN KEY (agent_staff_id, merchant_id) REFERENCES staff(id, merchant_id)
);
CREATE INDEX idx_agent_briefs_recent ON agent_briefs(merchant_id, created_at DESC);

COMMENT ON TABLE agent_briefs IS 'AI 员工写的经营简报（00092，AI 经营 M9）。';

ALTER TABLE agent_briefs ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_briefs FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON agent_briefs
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

GRANT SELECT, INSERT, UPDATE, DELETE ON agent_briefs TO keel_app;

-- +goose Down
DROP TABLE agent_briefs;
