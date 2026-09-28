-- 只读 SQL 工具（AI 经营 M11，docs/AI经营-M10M11设计.md §7）：MCP 工具 query_sql 让 AI 员工用 SQL 查经营数据，
-- 但只能读这里的一组视图。
--
-- # 三道闸
--
-- 1. **角色**：查询在 `SET LOCAL ROLE keel_agent_ro` 之后跑。这个角色 NOLOGIN、只对 schema agent_ro 里的视图有 SELECT，
--    对 public 里的表什么权限都没有 —— 写不了任何东西，也读不到视图之外的列。keel_app 是它的成员（才能 SET ROLE）。
-- 2. **视图只放经营数据**：不含买家手机号 / 邮箱 / 口令哈希、收货地址快照、订单备注、售后的买家原话与凭证、
--    支付渠道回调、令牌、密钥。每个视图显式 `WHERE merchant_id = current_merchant()` —— 视图以属主（迁移角色）的
--    身份读底表，不能指望底表的 RLS。
-- 3. **租户不许在查询里被改**：current_merchant() 读的是会话设置 app.merchant_id，而 set_config() 对 PUBLIC 可执行。
--    service 层（agent_sql.go）静态拒掉含 set_config 等函数名的 SQL，并在查询之后复核 app.merchant_id 没变，
--    变了就丢弃结果并报错。
--
-- 另外：单条语句（扩展协议）、只接受 SELECT / WITH、statement_timeout 3 秒、结果至多 500 行。
-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'keel_agent_ro') THEN
        CREATE ROLE keel_agent_ro NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
    END IF;
END $$;
-- +goose StatementEnd
GRANT keel_agent_ro TO keel_app;

CREATE SCHEMA agent_ro;
REVOKE ALL ON SCHEMA agent_ro FROM PUBLIC;
GRANT USAGE ON SCHEMA agent_ro TO keel_agent_ro;

CREATE VIEW agent_ro.orders WITH (security_barrier) AS
SELECT id, order_no, user_id, status, refund_status, store_id, region_id, goods_amount_cents, freight_cents,
       discount_cents, promotion_discount_cents, freight_discount_cents, payable_cents, paid_cents, refunded_cents,
       coupon_name, created_at, paid_at, shipped_at, finished_at
  FROM public.orders WHERE merchant_id = current_merchant();

CREATE VIEW agent_ro.order_items WITH (security_barrier) AS
SELECT id, order_id, sku_id, product_id, title_snapshot, spec_snapshot, list_price_cents, price_cents, quantity,
       amount_cents, discount_cents, promotion_discount_cents, price_promotion_id, refunded_qty, refunded_cents
  FROM public.order_items WHERE merchant_id = current_merchant();

CREATE VIEW agent_ro.products WITH (security_barrier) AS
SELECT id, category_id, brand_id, title, subtitle, total_stock, sales_count, status, published_at, created_at, updated_at
  FROM public.products WHERE merchant_id = current_merchant() AND deleted_at IS NULL;

CREATE VIEW agent_ro.skus WITH (security_barrier) AS
SELECT id, product_id, sku_code, spec_values, price_cents, cost_cents, weight_gram, status, created_at
  FROM public.skus WHERE merchant_id = current_merchant() AND deleted_at IS NULL;

CREATE VIEW agent_ro.categories WITH (security_barrier) AS
SELECT id, parent_id, name, path, level, status
  FROM public.categories WHERE merchant_id = current_merchant() AND deleted_at IS NULL;

CREATE VIEW agent_ro.stores WITH (security_barrier) AS
SELECT id, region_id, code, name, province, city, district, is_default, status,
       ST_Y(location::geometry) AS lat, ST_X(location::geometry) AS lng, created_at
  FROM public.stores WHERE merchant_id = current_merchant() AND deleted_at IS NULL;

CREATE VIEW agent_ro.regions WITH (security_barrier) AS
SELECT id, code, name, status
  FROM public.regions WHERE merchant_id = current_merchant() AND deleted_at IS NULL;

CREATE VIEW agent_ro.refunds WITH (security_barrier) AS
SELECT id, refund_no, order_id, refund_type, reason_code, goods_amount_cents, freight_cents, amount_cents, status,
       audited_at, refunded_at, created_at
  FROM public.refunds WHERE merchant_id = current_merchant();

CREATE VIEW agent_ro.search_logs WITH (security_barrier) AS
SELECT id, query, strategy, cardinality(ranked_ids) AS result_count, clicked_id, carted_id, ordered_id, latency_ms,
       created_at
  FROM public.search_logs WHERE merchant_id = current_merchant();

CREATE VIEW agent_ro.coupon_templates WITH (security_barrier) AS
SELECT id, name, coupon_type, threshold_cents, discount_cents, discount_rate, max_discount_cents, valid_mode,
       valid_start_at, valid_end_at, valid_days, total_count, issued_count, per_user_limit, claimable, status, created_at
  FROM public.coupon_templates WHERE merchant_id = current_merchant();

CREATE VIEW agent_ro.user_coupons WITH (security_barrier) AS
SELECT id, template_id, user_id, source, status, order_id, created_at, used_at
  FROM public.user_coupons WHERE merchant_id = current_merchant();

CREATE VIEW agent_ro.promotions WITH (security_barrier) AS
SELECT id, name, promo_type, threshold_unit, stack_with_coupon, starts_at, ends_at, status, created_at
  FROM public.promotions WHERE merchant_id = current_merchant();

CREATE VIEW agent_ro.promotion_skus WITH (security_barrier) AS
SELECT id, promotion_id, sku_id, promo_price_cents, discount_rate, per_user_limit, stock_qty, sold_qty
  FROM public.promotion_skus WHERE merchant_id = current_merchant();

GRANT SELECT ON ALL TABLES IN SCHEMA agent_ro TO keel_agent_ro;

-- +goose Down
DROP SCHEMA agent_ro CASCADE;
REVOKE keel_agent_ro FROM keel_app;
