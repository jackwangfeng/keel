-- AI 调渠道库存分配（docs/superpowers/specs/2026-10-03-ai-channel-allocation-design.md §4.2–4.4）：
-- 新提案种类 channel_stock_rule（改一个销售渠道在一家门店的库存分配规则）。
--
-- 一、agent_proposals.kind 放进 channel_stock_rule。它是全店类（store_id 为空）：改渠道分配要全店范围的 AI 员工提、
--     全店范围的人批（门店写在 payload 里，chk_agent_proposal_store 不用动）。
-- 二、自动执行策略加一列 max_ratio_step_bp：每条改动的比例变化绝对值不超过它（万分比）；默认 0 = 不自动执行这种提案。
--     安全库存的变化沿用 max_units；「把比例调到 0」（等于下架这个渠道）永远不自动执行 —— 那条写在
--     service/agent_auto_policy.go 的 withinPolicy 里，这里只管取值范围。
-- +goose Up
ALTER TABLE agent_proposals DROP CONSTRAINT chk_agent_proposal_kind;
ALTER TABLE agent_proposals ADD CONSTRAINT chk_agent_proposal_kind
    CHECK (kind IN ('inventory_adjust', 'flash_price', 'coupon', 'product_copy', 'refund_decision', 'channel_stock_rule'));

ALTER TABLE agent_auto_policies ADD COLUMN max_ratio_step_bp INT NOT NULL DEFAULT 0;
ALTER TABLE agent_auto_policies ADD CONSTRAINT chk_auto_policy_ratio_step CHECK (max_ratio_step_bp BETWEEN 0 AND 10000);
ALTER TABLE agent_auto_policies DROP CONSTRAINT chk_auto_policy_kind;
ALTER TABLE agent_auto_policies ADD CONSTRAINT chk_auto_policy_kind
    CHECK (kind IN ('inventory_adjust', 'flash_price', 'coupon', 'product_copy', 'channel_stock_rule'));

-- +goose Down
DELETE FROM agent_auto_policies WHERE kind = 'channel_stock_rule';
ALTER TABLE agent_auto_policies DROP CONSTRAINT chk_auto_policy_kind;
ALTER TABLE agent_auto_policies ADD CONSTRAINT chk_auto_policy_kind
    CHECK (kind IN ('inventory_adjust', 'flash_price', 'coupon', 'product_copy'));
ALTER TABLE agent_auto_policies DROP CONSTRAINT chk_auto_policy_ratio_step;
ALTER TABLE agent_auto_policies DROP COLUMN max_ratio_step_bp;

DELETE FROM agent_proposals WHERE kind = 'channel_stock_rule';
ALTER TABLE agent_proposals DROP CONSTRAINT chk_agent_proposal_kind;
ALTER TABLE agent_proposals ADD CONSTRAINT chk_agent_proposal_kind
    CHECK (kind IN ('inventory_adjust', 'flash_price', 'coupon', 'product_copy', 'refund_decision'));
