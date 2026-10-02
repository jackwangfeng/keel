-- AI 调渠道库存分配（docs/superpowers/specs/2026-10-03-ai-channel-allocation-design.md §3.3）：query_sql 能读到订单来源与渠道表。
--
-- 一、agent_ro.orders 重建：原列顺序不变，末尾加 source（0 自营 / 1 渠道单）、channel_order_id。
--     CREATE OR REPLACE 只能在末尾加列，这里照样先 DROP 再 CREATE（与 00140 Down 同一写法），授权照 00131 补回。
-- 二、新视图：channel_bindings / channel_orders / channel_stock_rules / channel_listing_zero_spans。
--     **不含** channel_bindings.config / secrets（凭据与渠道配置）、channel_orders.receiver（收货人）/ last_payload
--     （平台原文，内含收货人）/ lines（行里只要标题以外的数字，汇总走 order_items）/ rider（骑手手机号）。
--     每个视图显式 WHERE merchant_id = current_merchant()（视图以属主身份读底表，不能指望底表的 RLS，见 00131）。
-- +goose Up
DROP VIEW agent_ro.orders;
CREATE VIEW agent_ro.orders WITH (security_barrier) AS
SELECT id, order_no, user_id, status, refund_status, store_id, region_id, goods_amount_cents, freight_cents,
       discount_cents, promotion_discount_cents, freight_discount_cents, payable_cents, paid_cents, refunded_cents,
       coupon_name, created_at, paid_at, shipped_at, finished_at, source, channel_order_id
  FROM public.orders WHERE merchant_id = current_merchant();
GRANT SELECT ON agent_ro.orders TO keel_agent_ro;

CREATE VIEW agent_ro.channel_bindings WITH (security_barrier) AS
SELECT id, channel, name, status, roles
  FROM public.channel_bindings WHERE merchant_id = current_merchant();

CREATE VIEW agent_ro.channel_orders WITH (security_barrier) AS
SELECT id, binding_id, store_id, order_no, status, (exception IS NOT NULL) AS has_exception,
       COALESCE((amounts->>'goods')::bigint, 0)      AS goods_cents,
       COALESCE((amounts->>'freight')::bigint, 0)    AS freight_cents,
       COALESCE((amounts->>'commission')::bigint, 0) AS commission_cents,
       COALESCE((amounts->>'buyer_paid')::bigint, 0) AS buyer_paid_cents,
       created_at
  FROM public.channel_orders WHERE merchant_id = current_merchant();

CREATE VIEW agent_ro.channel_stock_rules WITH (security_barrier) AS
SELECT id, binding_id, store_id, sku_id, ratio_bp, safety_qty, cap_qty, updated_at
  FROM public.channel_stock_rules WHERE merchant_id = current_merchant();

CREATE VIEW agent_ro.channel_listing_zero_spans WITH (security_barrier) AS
SELECT id, binding_id, store_id, sku_id, held, started_at, ended_at
  FROM public.channel_listing_zero_spans WHERE merchant_id = current_merchant();

GRANT SELECT ON agent_ro.channel_bindings, agent_ro.channel_orders, agent_ro.channel_stock_rules,
                agent_ro.channel_listing_zero_spans TO keel_agent_ro;

-- +goose Down
DROP VIEW agent_ro.channel_listing_zero_spans;
DROP VIEW agent_ro.channel_stock_rules;
DROP VIEW agent_ro.channel_orders;
DROP VIEW agent_ro.channel_bindings;
DROP VIEW agent_ro.orders;
CREATE VIEW agent_ro.orders WITH (security_barrier) AS
SELECT id, order_no, user_id, status, refund_status, store_id, region_id, goods_amount_cents, freight_cents,
       discount_cents, promotion_discount_cents, freight_discount_cents, payable_cents, paid_cents, refunded_cents,
       coupon_name, created_at, paid_at, shipped_at, finished_at
  FROM public.orders WHERE merchant_id = current_merchant();
GRANT SELECT ON agent_ro.orders TO keel_agent_ro;
