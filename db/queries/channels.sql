-- 渠道适配层（00301）。编排在 service/channel*.go；表结构与取舍见数据模型文档第十七节。
-- secrets 列的值只有 GetChannelBindingSecrets / SetChannelBindingSecrets 两条碰它：其余读路径只选「配过没有」
-- （has_secrets = secrets <> '{}'，后台显示「已配置 / 未配置」），接口不回显值。

-- name: CreateChannelBinding :one
INSERT INTO channel_bindings (channel, external_account, name, roles, status, config)
VALUES (@channel::text, @external_account::text, @name::text, @roles::smallint, @status::smallint, @config::jsonb)
RETURNING id, channel, external_account, name, roles, status, config, (secrets <> '{}'::jsonb)::boolean AS has_secrets, created_at, updated_at;

-- name: GetChannelBinding :one
SELECT id, channel, external_account, name, roles, status, config, (secrets <> '{}'::jsonb)::boolean AS has_secrets, created_at, updated_at
  FROM channel_bindings WHERE id = @id::bigint;

-- name: ListChannelBindings :many
SELECT id, channel, external_account, name, roles, status, config, (secrets <> '{}'::jsonb)::boolean AS has_secrets, created_at, updated_at
  FROM channel_bindings ORDER BY id;

-- name: UpdateChannelBinding :one
-- 只改传了的字段（sqlc.narg 为空即不改）。secrets 不经这里。
UPDATE channel_bindings
   SET name   = COALESCE(sqlc.narg(name)::text, name),
       roles  = COALESCE(sqlc.narg(roles)::smallint, roles),
       status = COALESCE(sqlc.narg(status)::smallint, status),
       config = COALESCE(sqlc.narg(config)::jsonb, config)
 WHERE id = @id::bigint
RETURNING id, channel, external_account, name, roles, status, config, (secrets <> '{}'::jsonb)::boolean AS has_secrets, created_at, updated_at;

-- name: GetChannelBindingSecrets :one
SELECT secrets FROM channel_bindings WHERE id = @id::bigint;

-- name: SetChannelBindingSecrets :execrows
UPDATE channel_bindings SET secrets = @secrets::jsonb WHERE id = @id::bigint;

-- name: CountActiveOutletBindings :one
-- 这家店启用中的销售渠道 binding 数（roles 位 4）。决定库存服务要不要为它发 stock.changed。
SELECT count(*) FROM channel_bindings WHERE status = 1 AND (roles & 4) <> 0;

-- name: ListActiveOutletBindingsForStore :many
-- 这家门店映射过、启用中的销售渠道 binding，连同渠道门店 ID。
SELECT b.id, b.channel, b.external_account, b.name, b.roles, b.status, b.config,
       (b.secrets <> '{}'::jsonb)::boolean AS has_secrets, b.created_at, b.updated_at,
       l.external_store_id
  FROM channel_bindings b
  JOIN channel_store_links l ON l.binding_id = b.id
 WHERE l.store_id = @store_id::bigint AND b.status = 1 AND (b.roles & 4) <> 0
 ORDER BY b.id;

-- name: UpsertChannelStoreLink :exec
INSERT INTO channel_store_links (binding_id, store_id, external_store_id)
VALUES (@binding_id::bigint, @store_id::bigint, @external_store_id::text)
ON CONFLICT ON CONSTRAINT channel_store_links_pkey DO UPDATE SET external_store_id = EXCLUDED.external_store_id;

-- name: DeleteChannelStoreLink :execrows
DELETE FROM channel_store_links WHERE binding_id = @binding_id::bigint AND store_id = @store_id::bigint;

-- name: ListChannelStoreLinks :many
SELECT binding_id, store_id, external_store_id, created_at
  FROM channel_store_links WHERE binding_id = @binding_id::bigint ORDER BY store_id;

-- name: UpsertChannelStockRule :one
INSERT INTO channel_stock_rules (binding_id, store_id, sku_id, ratio_bp, safety_qty, cap_qty)
VALUES (@binding_id::bigint, sqlc.narg(store_id)::bigint, sqlc.narg(sku_id)::bigint,
        @ratio_bp::int, @safety_qty::int, sqlc.narg(cap_qty)::int)
ON CONFLICT ON CONSTRAINT uk_channel_stock_rules_scope
DO UPDATE SET ratio_bp = EXCLUDED.ratio_bp, safety_qty = EXCLUDED.safety_qty, cap_qty = EXCLUDED.cap_qty
RETURNING id, binding_id, store_id, sku_id, ratio_bp, safety_qty, cap_qty, updated_at;

-- name: DeleteChannelStockRule :execrows
DELETE FROM channel_stock_rules WHERE binding_id = @binding_id::bigint AND id = @id::bigint;

-- name: ListChannelStockRules :many
SELECT id, binding_id, store_id, sku_id, ratio_bp, safety_qty, cap_qty, updated_at
  FROM channel_stock_rules WHERE binding_id = @binding_id::bigint ORDER BY id;

-- name: ListChannelStockRulesForStore :many
-- 算对外可售数用：渠道级 + 这家门店的（门店级与门店 × SKU 级）。
SELECT id, binding_id, store_id, sku_id, ratio_bp, safety_qty, cap_qty, updated_at
  FROM channel_stock_rules
 WHERE binding_id = @binding_id::bigint AND (store_id IS NULL OR store_id = @store_id::bigint);

-- name: UpsertChannelPriceRule :one
INSERT INTO channel_price_rules (binding_id, sku_id, markup_bp, fixed_cents)
VALUES (@binding_id::bigint, sqlc.narg(sku_id)::bigint, @markup_bp::int, sqlc.narg(fixed_cents)::bigint)
ON CONFLICT ON CONSTRAINT uk_channel_price_rules_scope
DO UPDATE SET markup_bp = EXCLUDED.markup_bp, fixed_cents = EXCLUDED.fixed_cents
RETURNING id, binding_id, sku_id, markup_bp, fixed_cents, updated_at;

-- name: DeleteChannelPriceRule :execrows
DELETE FROM channel_price_rules WHERE binding_id = @binding_id::bigint AND id = @id::bigint;

-- name: ListChannelPriceRules :many
SELECT id, binding_id, sku_id, markup_bp, fixed_cents, updated_at
  FROM channel_price_rules WHERE binding_id = @binding_id::bigint ORDER BY id;

-- name: GetChannelListings :many
SELECT binding_id, store_id, sku_id, published_qty, published_cents, version, pushed_at, last_error
  FROM channel_listings
 WHERE binding_id = @binding_id::bigint AND store_id = @store_id::bigint AND sku_id = ANY(@sku_ids::bigint[]);

-- name: UpsertChannelListing :one
-- 推送成功后记下推出去的值；version 每次成功加一（幂等键 binding:store:sku:version）。
INSERT INTO channel_listings (binding_id, store_id, sku_id, published_qty, published_cents)
VALUES (@binding_id::bigint, @store_id::bigint, @sku_id::bigint, @published_qty::int, @published_cents::bigint)
ON CONFLICT ON CONSTRAINT channel_listings_pkey
DO UPDATE SET published_qty = EXCLUDED.published_qty, published_cents = EXCLUDED.published_cents,
              version = channel_listings.version + 1, pushed_at = now(), last_error = NULL
RETURNING version;

-- name: SetChannelListingError :exec
UPDATE channel_listings SET last_error = @last_error::text
 WHERE binding_id = @binding_id::bigint AND store_id = @store_id::bigint AND sku_id = @sku_id::bigint;

-- name: ListChannelListingsPage :many
-- 后台的推送状态：带 SKU 货号与商品名（删了的 SKU 也照列，行还在就说明推过）；errors_only 只看出错的。
SELECT l.binding_id, l.store_id, l.sku_id, l.published_qty, l.published_cents, l.version, l.pushed_at, l.last_error,
       s.sku_code, p.title AS product_title
  FROM channel_listings l
  JOIN skus s     ON s.id = l.sku_id
  JOIN products p ON p.id = s.product_id
 WHERE l.binding_id = @binding_id::bigint
   AND (sqlc.narg(store_id)::bigint IS NULL OR l.store_id = sqlc.narg(store_id)::bigint)
   AND (NOT @errors_only::boolean OR l.last_error IS NOT NULL)
 ORDER BY l.store_id, l.sku_id
 LIMIT @lim::int OFFSET @off::int;

-- name: InsertChannelInboundEvent :one
-- 重复投递（同一外部事件 ID）不插、不报错：没有行返回即重复（调用方当 inserted=false）。
INSERT INTO channel_inbound_events (binding_id, external_event_id, topic, payload, status)
VALUES (@binding_id::bigint, @external_event_id::text, @topic::text, @payload::jsonb, @status::smallint)
ON CONFLICT ON CONSTRAINT uk_channel_inbound_events_external DO NOTHING
RETURNING id;

-- name: GetChannelInboundEvent :one
SELECT id, binding_id, external_event_id, topic, payload, status, error, received_at, processed_at
  FROM channel_inbound_events WHERE id = @id::bigint;

-- name: MarkChannelInboundEvent :exec
UPDATE channel_inbound_events
   SET status = @status::smallint, error = sqlc.narg(error)::text, processed_at = now()
 WHERE id = @id::bigint;

-- name: ChannelSyncRev :one
-- 「开关渠道」消息的版本：数据库时钟的微秒数。只用一台数据库的时钟，多个 core 实例之间没有时钟偏差。
SELECT (extract(epoch FROM clock_timestamp()) * 1000000)::bigint AS rev;

-- name: ChannelSKUOffers :many
-- 推给渠道的基准价（门店就近生效价，sku_prices_by_store 是唯一实现）与「能不能卖」的三个因素：
-- SKU 启用且没删、商品没删、商品已上架。不能卖的 SKU 对外可售按 0 推；「上架」这一条对商品源 binding
-- 不看（上下架归商品源管，见 repository.SKUOffer.Sellable）。
SELECT v.sku_id, v.price_cents,
       (s.status = 1 AND s.deleted_at IS NULL)::boolean AS sku_active,
       (p.deleted_at IS NULL)::boolean                  AS product_live,
       (p.status = 1)::boolean                          AS product_published
  FROM sku_prices_by_store v
  JOIN skus s     ON s.id = v.sku_id
  JOIN products p ON p.id = s.product_id
 WHERE v.store_id = @store_id::bigint AND v.sku_id = ANY(@sku_ids::bigint[]);

-- name: UpsertChannelItemLink :exec
INSERT INTO channel_item_links (binding_id, kind, keel_id, external_id, extra)
VALUES (@binding_id::bigint, @kind::smallint, @keel_id::bigint, @external_id::text, @extra::jsonb)
ON CONFLICT ON CONSTRAINT channel_item_links_pkey
DO UPDATE SET external_id = EXCLUDED.external_id, extra = EXCLUDED.extra, synced_at = now();

-- name: ListChannelItemLinks :many
SELECT binding_id, kind, keel_id, external_id, extra, synced_at
  FROM channel_item_links
 WHERE binding_id = @binding_id::bigint AND kind = @kind::smallint AND keel_id = ANY(@keel_ids::bigint[]);

-- name: ListLinkedSKUIDsPage :many
-- 这个 binding 映射过的 SKU，按 id 键集分页（整店重算用）。
SELECT keel_id FROM channel_item_links
 WHERE binding_id = @binding_id::bigint AND kind = 2 AND keel_id > @after::bigint
 ORDER BY keel_id LIMIT @lim::int;

-- name: LockChannelMerchant :exec
-- 这家店的渠道启停串行化（事务级 advisory lock，提交 / 回滚即释放）。拿着它再数启用中的销售渠道、取版本，
-- 版本的先后就等于提交的先后（并发启停 A、停用 B 时不会停在错误的「关」）。第一段键见 service.ChannelMerchantLockKey。
SELECT pg_advisory_xact_lock(7340301, current_merchant()::int);

-- name: ChannelSKUExists :one
SELECT EXISTS (SELECT 1 FROM skus WHERE id = @sku_id::bigint AND deleted_at IS NULL);

-- name: GetChannelItemLinkByExternal :one
-- 按外部 ID 反查映射（商品源拉进来的商品 / 规格，找它在 keel 里是哪一个）。
SELECT binding_id, kind, keel_id, external_id, extra, synced_at
  FROM channel_item_links
 WHERE binding_id = @binding_id::bigint AND kind = @kind::smallint AND external_id = @external_id::text;

-- name: DeleteChannelItemLink :execrows
DELETE FROM channel_item_links
 WHERE binding_id = @binding_id::bigint AND kind = @kind::smallint AND keel_id = @keel_id::bigint;

-- name: ChannelSKUsByCodes :many
-- 按货号找 keel 的 SKU（商品源拉商品时认领同货号的已有 SKU）。删了的也列出来：uk_skus_code 不分删没删，
-- 撞上一个删了的货号也建不出新的。
SELECT id, product_id, sku_code, (deleted_at IS NOT NULL)::boolean AS deleted
  FROM skus
 WHERE sku_code = ANY(@codes::text[]);
