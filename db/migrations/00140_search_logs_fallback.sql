-- 检索的相关度下限（2026-09-28，演示站 AI 经营实跑发现）：向量召回没有下限，店里没有的词（瑜伽垫、登山鞋）
-- 也凑满 10 条，于是 ranked_ids 恒不为空、无结果统计恒 0、search_zero_spike 永远不触发。
--
-- 现在 POST /search 只把「关键词命中、或向量相似度 ≥ 下限」算作可信命中（service/search.go 的 applyFloor）：
-- 有可信命中就只回可信的；一条都没有时回低于下限的结果作「猜你想要」，响应 fallback = true。
-- fallback 记在这一列：无结果的口径从「ranked_ids 为空」改为「ranked_ids 为空或 fallback」
-- （报表、search_metrics.sql、search_zero_spike、agent_ro.search_logs 同一个口径）。
-- 旧行默认 false：之前没有下限，历史上的「凑满 10 条」无从追认。
-- +goose Up
ALTER TABLE search_logs ADD COLUMN fallback BOOLEAN NOT NULL DEFAULT FALSE;

CREATE OR REPLACE VIEW agent_ro.search_logs WITH (security_barrier) AS
SELECT id, query, strategy, cardinality(ranked_ids) AS result_count, clicked_id, carted_id, ordered_id, latency_ms,
       created_at, fallback
  FROM public.search_logs WHERE merchant_id = current_merchant();

-- +goose Down
-- 视图删不了列（CREATE OR REPLACE 只能在末尾加），先删再按 00131 的原样建回，并补回 00131 的授权。
DROP VIEW agent_ro.search_logs;
CREATE VIEW agent_ro.search_logs WITH (security_barrier) AS
SELECT id, query, strategy, cardinality(ranked_ids) AS result_count, clicked_id, carted_id, ordered_id, latency_ms,
       created_at
  FROM public.search_logs WHERE merchant_id = current_merchant();
GRANT SELECT ON agent_ro.search_logs TO keel_agent_ro;
ALTER TABLE search_logs DROP COLUMN fallback;
