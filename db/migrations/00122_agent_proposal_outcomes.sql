-- 提案执行后的复盘（AI 经营 M10，docs/AI经营-M10M11设计.md §4）：执行后过一段时间，Keel 自己量一次「有没有用」，
-- 写进 outcome，给后台「AI 员工成绩单」与 agent 自己（MCP my_scorecard）看。
--
-- outcome_due_at 在执行成功时定（加库存 / 改文案 7 天后；限时折扣活动结束后 3 天；发券 7 天后），
-- 由复盘扫描（service/agent_proposal_outcome.go）到点计算；售后审核不量效果，执行时直接写一份只有 verdict 的结果。
-- verdict：positive / neutral / negative，规则写死、可解释，写在复盘代码的注释里。
-- +goose Up
ALTER TABLE agent_proposals ADD COLUMN executed_at    TIMESTAMPTZ;
ALTER TABLE agent_proposals ADD COLUMN outcome_due_at TIMESTAMPTZ;
ALTER TABLE agent_proposals ADD COLUMN outcome        JSONB;
ALTER TABLE agent_proposals ADD COLUMN outcome_at     TIMESTAMPTZ;
UPDATE agent_proposals SET executed_at = updated_at WHERE status = 20;
CREATE INDEX idx_agent_proposals_outcome_due ON agent_proposals(merchant_id, outcome_due_at)
    WHERE status = 20 AND outcome_at IS NULL AND outcome_due_at IS NOT NULL;

-- +goose Down
DROP INDEX idx_agent_proposals_outcome_due;
ALTER TABLE agent_proposals DROP COLUMN outcome_at;
ALTER TABLE agent_proposals DROP COLUMN outcome;
ALTER TABLE agent_proposals DROP COLUMN outcome_due_at;
ALTER TABLE agent_proposals DROP COLUMN executed_at;
