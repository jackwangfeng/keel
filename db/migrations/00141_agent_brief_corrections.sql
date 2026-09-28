-- 简报更正（2026-09-28 演示站 AI 经营实跑发现）：简报发出后改不了，AI 店长两次自己发现写错了数字 / 时间
-- （把 UTC 07:57 当北京时间、把 9 条待批数成 8 条），只能在对话里说「需要的话再发一份」。
--
-- 做法：post_brief 可带 corrects_brief_id，发一份新简报更正旧的那份。旧的**不改写**（它曾经被人看到过，
-- 审计要留原样），后台与公开日志上标「已更正 → #新」。只能更正自己写的；一份简报至多被直接更正一次
-- （再错就更正那份更正），所以 corrects_id 在租户内唯一。
-- +goose Up
ALTER TABLE agent_briefs ADD CONSTRAINT uk_agent_briefs_tenant_id UNIQUE (merchant_id, id);
ALTER TABLE agent_briefs ADD COLUMN corrects_id BIGINT;
ALTER TABLE agent_briefs ADD CONSTRAINT fk_agent_briefs_corrects
    FOREIGN KEY (merchant_id, corrects_id) REFERENCES agent_briefs(merchant_id, id);
CREATE UNIQUE INDEX uk_agent_briefs_corrects ON agent_briefs(merchant_id, corrects_id) WHERE corrects_id IS NOT NULL;
ALTER TABLE agent_briefs ADD CONSTRAINT chk_agent_brief_not_self CHECK (corrects_id IS NULL OR corrects_id <> id);

-- +goose Down
ALTER TABLE agent_briefs DROP CONSTRAINT chk_agent_brief_not_self;
DROP INDEX uk_agent_briefs_corrects;
ALTER TABLE agent_briefs DROP CONSTRAINT fk_agent_briefs_corrects;
ALTER TABLE agent_briefs DROP COLUMN corrects_id;
ALTER TABLE agent_briefs DROP CONSTRAINT uk_agent_briefs_tenant_id;
