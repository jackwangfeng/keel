-- AI 员工的工具调用审计（AI 经营 M9 任务 2，docs/AI经营-M9设计.md §7）。
--
-- 每一次 MCP 工具调用一行：谁（AI 员工、哪把密钥）、调了什么、参数、成没成、耗时。args 里不会有密钥（密钥在请求头）。
-- 写在调用返回之后、独立的一个短事务里：审计写失败不影响工具的结果（记一条 ERROR），
-- 反过来工具失败也照样留痕 —— 失败的调用往往最值得看。
-- 保留期：90 天，由通知保留期清理同一类的任务按 created_at 删（M9 任务 2 只建表与写入，清理随任务 4 的过期任务一起上）。
-- +goose Up
CREATE TABLE agent_tool_calls (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id    BIGINT      NOT NULL DEFAULT current_merchant() REFERENCES merchants(id),
    agent_staff_id BIGINT      NOT NULL,
    key_id         BIGINT      NOT NULL,
    tool           TEXT        NOT NULL,
    args           JSONB       NOT NULL DEFAULT '{}'::jsonb,
    ok             BOOLEAN     NOT NULL,
    error_type     TEXT        NOT NULL DEFAULT '',   -- 失败时的 problem type（或 internal）
    duration_ms    INT         NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (agent_staff_id, merchant_id) REFERENCES staff(id, merchant_id)
);
CREATE INDEX idx_agent_tool_calls_recent ON agent_tool_calls(merchant_id, created_at DESC);
CREATE INDEX idx_agent_tool_calls_agent ON agent_tool_calls(merchant_id, agent_staff_id, created_at DESC);

COMMENT ON TABLE agent_tool_calls IS 'AI 员工的 MCP 工具调用审计（00093，AI 经营 M9）。';

ALTER TABLE agent_tool_calls ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_tool_calls FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant ON agent_tool_calls
  USING (merchant_id = current_merchant()) WITH CHECK (merchant_id = current_merchant());

GRANT SELECT, INSERT, UPDATE, DELETE ON agent_tool_calls TO keel_app;

-- +goose Down
DROP TABLE agent_tool_calls;
