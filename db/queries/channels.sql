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

-- name: LockChannelBindingRules :one
-- 库存规则按 binding 串行（后台改 / 删规则与执行 AI 的调分配提案）：拿 binding 这一行的 FOR NO KEY UPDATE，
-- 拿到之后再读规则。NO KEY：不挡子表（规则、挂零时段……）插入时外键检查要的 KEY SHARE。
SELECT id FROM channel_bindings WHERE id = @id::bigint FOR NO KEY UPDATE;

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

-- name: CloseChannelZeroSpan :exec
-- 关掉这个格子还挂着的那段挂零时段。only_held_not 非空时只关 held 与它不同的那段（挂零但 held 变了：关旧开新）。
-- GREATEST：时钟回拨也不违反 ended_at >= started_at。
UPDATE channel_listing_zero_spans SET ended_at = GREATEST(@at::timestamptz, started_at)
 WHERE binding_id = @binding_id::bigint AND store_id = @store_id::bigint AND sku_id = @sku_id::bigint
   AND ended_at IS NULL
   AND (sqlc.narg(only_held_not)::boolean IS NULL OR held <> sqlc.narg(only_held_not)::boolean);

-- name: OpenChannelZeroSpan :exec
-- 开一段挂零时段；已有一段还挂着（held 相同）时什么都不做：撞上部分唯一索引 uk_channel_listing_zero_spans_open
-- 就不插（表上唯一的另一条唯一约束是自增主键），并发的两次开段也只留一段。
INSERT INTO channel_listing_zero_spans (binding_id, store_id, sku_id, held, started_at)
VALUES (@binding_id::bigint, @store_id::bigint, @sku_id::bigint, @held::boolean, @at::timestamptz)
ON CONFLICT DO NOTHING;

-- name: OpenChannelZeroSpansHeld :many
-- 这个 binding 在这家门店这批 SKU 上还挂着的挂零时段的 held（重算时判断「一直是 0 但 held 变了」）。
SELECT sku_id, held FROM channel_listing_zero_spans
 WHERE binding_id = @binding_id::bigint AND store_id = @store_id::bigint AND sku_id = ANY(@sku_ids::bigint[])
   AND ended_at IS NULL;

-- name: SumChannelZeroHours :many
-- 挂零时段与 [from, to) 的交集小时数，按 (binding, 门店, SKU) 分 held / 非 held 汇总；还挂着的段截到 to。
SELECT binding_id, store_id, sku_id,
       (COALESCE(SUM(EXTRACT(EPOCH FROM LEAST(COALESCE(ended_at, @to_at::timestamptz), @to_at::timestamptz)
                                     - GREATEST(started_at, @from_at::timestamptz))) FILTER (WHERE held), 0) / 3600)::float8 AS held_hours,
       (COALESCE(SUM(EXTRACT(EPOCH FROM LEAST(COALESCE(ended_at, @to_at::timestamptz), @to_at::timestamptz)
                                     - GREATEST(started_at, @from_at::timestamptz))) FILTER (WHERE NOT held), 0) / 3600)::float8 AS empty_hours
  FROM channel_listing_zero_spans
 WHERE store_id = @store_id::bigint AND sku_id = ANY(@sku_ids::bigint[])
   AND started_at < @to_at::timestamptz AND (ended_at IS NULL OR ended_at > @from_at::timestamptz)
 GROUP BY binding_id, store_id, sku_id
 ORDER BY binding_id, sku_id;

-- name: ChannelZeroSpanSKUs :many
-- 这家门店 [from, …) 里在任一渠道挂过零的 SKU（channel_allocation_review 自动挑 SKU 用）。
SELECT DISTINCT sku_id FROM channel_listing_zero_spans
 WHERE store_id = @store_id::bigint AND (ended_at IS NULL OR ended_at > @from_at::timestamptz)
 ORDER BY sku_id;

-- name: ChannelSoldBySource :many
-- 这家门店 [since, until) 里各渠道卖出的件数，按（binding, SKU）：自营（source 0）binding_id 记 0。
-- 「卖出」与 StoreSKUSales（restock.sql）同一口径：20 / 30 / 40 / 50 算，10 待支付、90 已关闭、60 整单退款不算。
SELECT (CASE WHEN o.source = 0 THEN 0 ELSE co.binding_id END)::bigint AS binding_id, oi.sku_id,
       sum(oi.quantity)::bigint AS qty
  FROM order_items oi
  JOIN orders o ON o.id = oi.order_id
  LEFT JOIN channel_orders co ON co.id = o.channel_order_id
 WHERE o.store_id = @store_id::bigint
   AND o.status IN (20, 30, 40, 50)
   AND o.paid_at >= @since::timestamptz
   AND o.paid_at < @until::timestamptz
   AND (o.source = 0 OR co.id IS NOT NULL)
 GROUP BY 1, 2;

-- name: ChannelStockoutRejects :many
-- 这家门店 [since, until) 里建的渠道单因缺货没接成的件数，按（binding, SKU）。判据三条同时成立：
--   有一张来源 1、已关闭（90）的 keel 订单指着它（接单 SAGA 走了补偿）；
--   现在没有挂着 keel 订单（order_no 为空 = 最终没接成；补货后重试成功的不算拒单）；
--   异常是「缺货：…」（channel_order_open_undo 写的）或已拒单（7，AcceptRequired 渠道补偿时入队拒单）。
--   异常被人处理清空、又不是拒单的那种会漏算——只低估不高估。
-- 件数取那张 90 订单的行（渠道单行里的 sku_id 不回写，映射在建 keel 订单时才解析）；一张渠道单重试过多次
-- 也只算最后那张 90 的订单。
WITH rej AS (
    SELECT DISTINCT ON (co.id) co.binding_id, o.id AS order_id
      FROM channel_orders co
      JOIN orders o ON o.channel_order_id = co.id AND o.source = 1 AND o.status = 90
     WHERE co.store_id = @store_id::bigint
       AND co.created_at >= @since::timestamptz
       AND co.created_at < @until::timestamptz
       AND co.order_no IS NULL
       AND (co.exception LIKE '缺货%' OR co.status = 7)
     ORDER BY co.id, o.id DESC
)
SELECT rej.binding_id, oi.sku_id, sum(oi.quantity)::bigint AS qty
  FROM rej
  JOIN order_items oi ON oi.order_id = rej.order_id
 GROUP BY 1, 2;

-- name: ChannelAllocationSKUs :many
-- 渠道分配要的 SKU 信息：人读的名字、成本价、上架时间（断货天数的窗口不早于它）。删了的不列。
SELECT s.id, p.title AS product_title, s.sku_code, COALESCE(s.spec_values::text, '{}')::text AS spec_values,
       s.cost_cents, s.created_at
  FROM skus s
  JOIN products p ON p.id = s.product_id
 WHERE s.id = ANY(@sku_ids::bigint[]) AND s.deleted_at IS NULL AND p.deleted_at IS NULL
 ORDER BY s.id;

-- name: PurgeChannelZeroSpans :execrows
-- 保留期清理：删掉 before 之前就结束了的段，一次至多 lim 条（有界 DELETE，同 PurgeExpiredNotifications）。
DELETE FROM channel_listing_zero_spans
 WHERE id IN (SELECT id FROM channel_listing_zero_spans
               WHERE ended_at < @before::timestamptz
               LIMIT @lim::int);

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

-- name: ChannelManagedProducts :many
-- 这批商品里由启用中的商品源管理的那些（商品级映射 kind 1），连同渠道。后台据此标「由 … 管理」并锁字段；
-- 停用 / 凭据失效的 binding 不算（不再同步，字段放开）。挂在多个商品源上时取 binding id 最小的那个。
SELECT DISTINCT ON (l.keel_id) l.keel_id AS product_id, b.channel
  FROM channel_item_links l
  JOIN channel_bindings b ON b.id = l.binding_id
 WHERE l.kind = 1 AND l.keel_id = ANY(@product_ids::bigint[])
   AND b.status = 1 AND (b.roles & 1) <> 0
 ORDER BY l.keel_id, b.id;

-- name: ChannelSKUsByCodes :many
-- 按货号找 keel 的 SKU（商品源拉商品时认领同货号的已有 SKU）。删了的也列出来：uk_skus_code 不分删没删，
-- 撞上一个删了的货号也建不出新的。
SELECT id, product_id, sku_code, (deleted_at IS NOT NULL)::boolean AS deleted
  FROM skus
 WHERE sku_code = ANY(@codes::text[]);

-- ---------------------------------------------------------------------------
-- 渠道订单（00320，第三期）。编排在 service/channel_order.go / channel_order_saga.go。
-- ---------------------------------------------------------------------------

-- name: InsertChannelOrder :one
-- 第一次见到这张平台单。并发的第二条（同一张单的另一条回调）撞 uk_channel_orders_external、等第一条提交后什么都不做
-- （没有行返回），调用方转去 LockChannelOrderByExternal —— 于是只有一个事务「新建」了它。
INSERT INTO channel_orders (binding_id, external_order_id, external_order_name, store_id, platform_status, status,
                            accept_deadline, delivery_mode, amounts, lines, receiver, version, last_payload, test)
VALUES (@binding_id::bigint, @external_order_id::text, @external_order_name::text, sqlc.narg(store_id)::bigint,
        @platform_status::text, @status::smallint, sqlc.narg(accept_deadline)::timestamptz, @delivery_mode::smallint,
        @amounts::jsonb, @lines::jsonb, @receiver::jsonb, @version::bigint, @last_payload::jsonb, @test::boolean)
ON CONFLICT ON CONSTRAINT uk_channel_orders_external DO NOTHING
RETURNING id, binding_id, external_order_id, external_order_name, store_id, order_no, platform_status, status, exception,
          accept_deadline, delivery_mode, amounts, lines, receiver, version, last_payload, test, created_at, updated_at;

-- name: LockChannelOrderByExternal :one
SELECT id, binding_id, external_order_id, external_order_name, store_id, order_no, platform_status, status, exception,
       accept_deadline, delivery_mode, amounts, lines, receiver, version, last_payload, test, created_at, updated_at
  FROM channel_orders
 WHERE binding_id = @binding_id::bigint AND external_order_id = @external_order_id::text
   FOR UPDATE;

-- name: LockChannelOrder :one
SELECT id, binding_id, external_order_id, external_order_name, store_id, order_no, platform_status, status, exception,
       accept_deadline, delivery_mode, amounts, lines, receiver, version, last_payload, test, created_at, updated_at
  FROM channel_orders WHERE id = @id::bigint
   FOR UPDATE;

-- name: GetChannelOrder :one
SELECT id, binding_id, external_order_id, external_order_name, store_id, order_no, platform_status, status, exception,
       accept_deadline, delivery_mode, amounts, lines, receiver, version, last_payload, test, created_at, updated_at
  FROM channel_orders WHERE id = @id::bigint;

-- name: UpdateChannelOrderSnapshot :exec
-- 新版本的平台快照（版本守卫在调用方：只在 version 更大、或「重试」强制时调）。
UPDATE channel_orders
   SET external_order_name = @external_order_name::text, store_id = sqlc.narg(store_id)::bigint,
       platform_status = @platform_status::text, status = @status::smallint,
       accept_deadline = sqlc.narg(accept_deadline)::timestamptz, delivery_mode = @delivery_mode::smallint,
       amounts = @amounts::jsonb, lines = @lines::jsonb, receiver = @receiver::jsonb,
       version = GREATEST(version, @version::bigint), last_payload = @last_payload::jsonb, test = @test::boolean
 WHERE id = @id::bigint;

-- name: TouchChannelOrderPayload :exec
-- 旧版本（或同版本）的快照只留档、不改状态（Review Focus 2）。
UPDATE channel_orders SET last_payload = @last_payload::jsonb WHERE id = @id::bigint;

-- name: SetChannelOrderKeelBasis :exec
-- 建 keel 草稿时写下依据（00326：平台行 → keel 订单行、当时已吸收的平台退款）。
UPDATE channel_orders SET keel_basis = @keel_basis::jsonb WHERE id = @id::bigint;

-- name: GetChannelOrderKeelBasis :one
SELECT keel_basis FROM channel_orders WHERE id = @id::bigint;

-- name: SetChannelOrderState :exec
-- keel 这一侧对渠道单的处置：规整状态、关联的 keel 订单、异常、接单截止。四列一起写（调用方给全值）。
UPDATE channel_orders
   SET status = @status::smallint, order_no = sqlc.narg(order_no)::text, exception = sqlc.narg(exception)::text,
       accept_deadline = sqlc.narg(accept_deadline)::timestamptz
 WHERE id = @id::bigint;

-- name: ListChannelOrders :many
-- 后台的渠道单列表：可按 binding、门店（通知跳过来只带门店）、状态筛，可只看异常；新的在前。
-- only_region_ids / only_store_ids 是员工的范围（service 的 orderListScope，同后台订单列表）：NULL 不限，
-- 空数组一个都不给；没映射门店的渠道单（store_id 空）只有不限范围的人看得见。
-- 渠道单表是一家店的渠道单（量远小于 orders），可空筛选在这里不构成 generic plan 的问题。
SELECT id, binding_id, external_order_id, external_order_name, store_id, order_no, platform_status, status, exception,
       accept_deadline, delivery_mode, amounts, lines, receiver, version, last_payload, test, created_at, updated_at
  FROM channel_orders
 WHERE (sqlc.narg(binding_id)::bigint IS NULL OR binding_id = sqlc.narg(binding_id)::bigint)
   AND (sqlc.narg(store_id)::bigint IS NULL OR store_id = sqlc.narg(store_id)::bigint)
   AND (sqlc.narg(status)::smallint IS NULL OR status = sqlc.narg(status)::smallint)
   AND (NOT @exception_only::boolean OR exception IS NOT NULL)
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR store_id IN (SELECT st.id FROM stores st
                         WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR store_id = ANY(sqlc.narg(only_store_ids)::bigint[]))
 ORDER BY id DESC
 LIMIT @lim::int OFFSET @off::int;

-- name: CountChannelOrders :one
-- 与 ListChannelOrders 同一组筛选的总条数（后台分页）。
SELECT count(*)::bigint
  FROM channel_orders
 WHERE (sqlc.narg(binding_id)::bigint IS NULL OR binding_id = sqlc.narg(binding_id)::bigint)
   AND (sqlc.narg(store_id)::bigint IS NULL OR store_id = sqlc.narg(store_id)::bigint)
   AND (sqlc.narg(status)::smallint IS NULL OR status = sqlc.narg(status)::smallint)
   AND (NOT @exception_only::boolean OR exception IS NOT NULL)
   AND (sqlc.narg(only_region_ids)::bigint[] IS NULL
        OR store_id IN (SELECT st.id FROM stores st
                         WHERE st.region_id = ANY(sqlc.narg(only_region_ids)::bigint[])))
   AND (sqlc.narg(only_store_ids)::bigint[] IS NULL
        OR store_id = ANY(sqlc.narg(only_store_ids)::bigint[]));

-- name: ChannelOrderKeelStatuses :many
-- 后台渠道单列表 / 详情的 retryable：这一页渠道单指着的 keel 订单此刻的状态（一次查询）。
SELECT order_no, status FROM orders WHERE order_no = ANY(@order_nos::text[]);

-- name: ChannelOrderRefs :many
-- 后台订单列表 / 详情的「来自 Shopify #1001」：这一页有渠道单（source = 1）时按 channel_order_id 补查一次
-- （订单列表不 JOIN，见第三期计划 Task 1 的执行中修正）。
SELECT co.id, co.external_order_name, b.channel, b.name AS binding_name
  FROM channel_orders co
  JOIN channel_bindings b ON b.id = co.binding_id
 WHERE co.id = ANY(@ids::bigint[]);

-- name: DecrementChannelListingBaseline :exec
-- 接单成功：平台卖出时已经自己减了平台上的数，推送基线跟着减（下限 0），下一次推送的 CAS 才对得上
-- （Review Focus 3）。version 加一：基线变了，下一次推送换一个幂等键。没推过的格子没有行，不建。
UPDATE channel_listings
   SET published_qty = GREATEST(published_qty - @qty::int, 0), version = version + 1
 WHERE binding_id = @binding_id::bigint AND store_id = @store_id::bigint AND sku_id = @sku_id::bigint;

-- name: ChannelOrderSKUs :many
-- 渠道单建 keel 订单行要的 SKU 快照（商品名、规格、图）。删了的 SKU / 商品不列：映射指向它时按「没映射」处理。
SELECT s.id, s.product_id, s.spec_values, s.image_url, p.title
  FROM skus s
  JOIN products p ON p.id = s.product_id
 WHERE s.id = ANY(@sku_ids::bigint[]) AND s.deleted_at IS NULL AND p.deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- 平台发起的申请（channel_order_requests，00320；第三期 Task 6）。编排在 service/channel_order_request.go。
-- ---------------------------------------------------------------------------

-- name: InsertChannelOrderRequest :one
-- 第一次见到这个申请。同一个申请的重复回调撞 uk_channel_order_requests_external、没有行返回，
-- 调用方转去 LockChannelOrderRequestByExternal（调用方已经锁着渠道单行，不会有并发的第二条插入）。
INSERT INTO channel_order_requests (channel_order_id, external_request_id, kind, lines, amount_cents, reason,
                                    status, deadline)
VALUES (@channel_order_id::bigint, @external_request_id::text, @kind::smallint, @lines::jsonb, @amount_cents::bigint,
        @reason::text, @status::smallint, sqlc.narg(deadline)::timestamptz)
ON CONFLICT ON CONSTRAINT uk_channel_order_requests_external DO NOTHING
RETURNING id, channel_order_id, external_request_id, kind, lines, amount_cents, reason, status, deadline,
          decided_by, decided_at, created_at, updated_at;

-- name: LockChannelOrderRequestByExternal :one
SELECT id, channel_order_id, external_request_id, kind, lines, amount_cents, reason, status, deadline,
       decided_by, decided_at, created_at, updated_at
  FROM channel_order_requests
 WHERE channel_order_id = @channel_order_id::bigint AND external_request_id = @external_request_id::text
   FOR UPDATE;

-- name: LockChannelOrderRequest :one
SELECT id, channel_order_id, external_request_id, kind, lines, amount_cents, reason, status, deadline,
       decided_by, decided_at, created_at, updated_at
  FROM channel_order_requests WHERE id = @id::bigint
   FOR UPDATE;

-- name: ListChannelOrderRequests :many
-- 一张渠道单上的申请（后台详情）：新的在前。
SELECT id, channel_order_id, external_request_id, kind, lines, amount_cents, reason, status, deadline,
       decided_by, decided_at, created_at, updated_at
  FROM channel_order_requests WHERE channel_order_id = @channel_order_id::bigint
 ORDER BY id DESC;

-- name: DecideChannelOrderRequest :execrows
-- 处置一个待处理（1）的申请：2 同意 / 3 拒绝 / 4 超时自动同意 / 5 平台已撤销。decided_by 是员工（自动策略、超时、撤销为空）。
-- 只改待处理的：返回 0 = 已经处置过了。
UPDATE channel_order_requests
   SET status = @status::smallint, decided_by = sqlc.narg(decided_by)::bigint, decided_at = now()
 WHERE id = @id::bigint AND status = 1;

-- name: ExpiredChannelOrderRequests :many
-- 过了平台截止还没处置的申请（截止扫描把它们记成 4 超时自动同意）。走 idx_channel_order_requests_pending。
SELECT r.id, r.channel_order_id, r.kind, r.amount_cents, co.store_id, co.order_no, co.external_order_name
  FROM channel_order_requests r
  JOIN channel_orders co ON co.id = r.channel_order_id
 WHERE r.status = 1 AND r.deadline <= now()
 ORDER BY r.deadline
 LIMIT @lim::int;

-- name: DueAcceptReminders :many
-- 等人接单、离接单截止不到 accept_remind_minutes（binding config，缺省 3）分钟、还没提醒过的渠道单。
-- 「提醒过」就是通知表里有那一条（去重键与 service/channel_order_request.go 的 acceptRemindDedupe 逐字一致），
-- 不另记一列。没映射到 keel 门店的单（store_id 空）发不了门店通知、也接不了单，不列。
SELECT co.id, co.store_id::bigint AS store_id, co.external_order_name, co.accept_deadline
  FROM channel_orders co
  JOIN channel_bindings b ON b.id = co.binding_id
 WHERE co.status = 2 AND co.order_no IS NULL AND co.exception IS NULL AND co.store_id IS NOT NULL
   AND co.accept_deadline > now()
   AND co.accept_deadline <= now() + make_interval(mins => COALESCE(
         CASE WHEN jsonb_typeof(b.config -> 'accept_remind_minutes') = 'number'
              THEN (b.config ->> 'accept_remind_minutes')::numeric::int END, 3))
   AND NOT EXISTS (SELECT 1 FROM notifications n
                    WHERE n.dedupe_key = 'merchant_channel_order_pending:accept_remind:' || co.id)
 ORDER BY co.accept_deadline
 LIMIT @lim::int;
