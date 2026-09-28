-- 提案种类扩到五种（AI 经营 M10，docs/AI经营-M10M11设计.md §1）：
--   inventory_adjust（M9）/ flash_price 限时折扣 / coupon 券模板 / product_copy 改标题副标题 / refund_decision 售后审核。
--
-- 营销、商品是全店的，没有门店：store_id 改为可空（售后审核按订单的履约门店填上）。
-- 去重不再是（kind，门店，SKU）：新增 target_key（这条提案作用在什么上，如 store:1:sku:3、product:12、
-- refund:RF…、flash:3,5、coupon:<名称>），同一个（kind，target_key）同时至多一条待处理 / 执行中。
-- +goose Up
ALTER TABLE agent_proposals ALTER COLUMN store_id DROP NOT NULL;
ALTER TABLE agent_proposals ADD COLUMN target_key TEXT;
UPDATE agent_proposals SET target_key = 'store:' || store_id || ':sku:' || COALESCE(sku_id::text, '');
ALTER TABLE agent_proposals ALTER COLUMN target_key SET NOT NULL;
ALTER TABLE agent_proposals ADD CONSTRAINT chk_agent_proposal_target CHECK (length(target_key) BETWEEN 1 AND 200);

ALTER TABLE agent_proposals DROP CONSTRAINT chk_agent_proposal_kind;
ALTER TABLE agent_proposals ADD CONSTRAINT chk_agent_proposal_kind
    CHECK (kind IN ('inventory_adjust', 'flash_price', 'coupon', 'product_copy', 'refund_decision'));
-- 门店类必须有门店；全店类（营销、商品）没有门店。
ALTER TABLE agent_proposals ADD CONSTRAINT chk_agent_proposal_store
    CHECK ((kind IN ('inventory_adjust', 'refund_decision')) = (store_id IS NOT NULL));

DROP INDEX uk_agent_proposals_open;
CREATE UNIQUE INDEX uk_agent_proposals_open ON agent_proposals(merchant_id, kind, target_key)
    WHERE status IN (10, 15);

-- +goose Down
DROP INDEX uk_agent_proposals_open;
CREATE UNIQUE INDEX uk_agent_proposals_open ON agent_proposals(merchant_id, kind, store_id, sku_id)
    WHERE status IN (10, 15);
ALTER TABLE agent_proposals DROP CONSTRAINT chk_agent_proposal_store;
ALTER TABLE agent_proposals DROP CONSTRAINT chk_agent_proposal_kind;
ALTER TABLE agent_proposals ADD CONSTRAINT chk_agent_proposal_kind CHECK (kind IN ('inventory_adjust'));
ALTER TABLE agent_proposals DROP CONSTRAINT chk_agent_proposal_target;
ALTER TABLE agent_proposals DROP COLUMN target_key;
ALTER TABLE agent_proposals ALTER COLUMN store_id SET NOT NULL;
