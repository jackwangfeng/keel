# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

> **Why this file is in English while the rest of the repository is in Chinese.**
> Commit messages, design documents and code comments here are Chinese, and that
> is not going to change — they are written for the people maintaining this.
> A changelog is different: it is the text that shows up on a GitHub Releases
> page, in a dependency bot's diff, and in the first search result someone hits
> when deciding whether to try this at all. It is read by people who have not
> opened the repository yet.
>
> There is deliberately **no translated mirror of this file**. Two copies of one
> truth diverge — that is the reasoning behind almost every "single source" rule
> in this codebase, and it does not stop applying at the changelog. The two
> READMEs are the exception, and they carry a consistency gate
> (`scripts/check_promises.py`) precisely because a mirror without one rots.

## Versioning, concretely

Until `1.0.0`, **the API contract may break in a minor release.** What that means
in practice:

- `0.x.y` → `0.x.(y+1)` — bug fixes, no contract change. The generated clients
  in `internal/api/`, `web/src/api/` and `flutter_app/lib/api/` stay compatible.
- `0.x.y` → `0.(x+1).0` — may add, rename or remove operations in
  `docs/电商系统-OpenAPI.yaml`. Regenerate your client.
- Database migrations are **forward-only**. Every release states which migration
  number it lands on; downgrading is not supported and `goose down` past a
  release boundary is not tested.

A release's version is compiled into the binary and served at `GET /version`,
so "which one is running?" never depends on anyone's memory.

---

## [Unreleased]

Core migrations `00300` (`channel_merchants`, also in the inventory database), `00301` (channel tables),
`00302` (`uploads.channel_binding_id`: an upload's owner may now be a channel account), and `00320`–`00326`
(channel orders: `orders.source`/`channel_order_id`, `channel_orders`/`channel_order_requests`, nullable
`refunds.user_id`/`payment_id`, a `channel_orders` notification target, and the per-order keel-build basis),
and `00330`–`00332` (AI channel allocation: `channel_listing_zero_spans`, `agent_ro` channel views and
`orders.source`, `agent_auto_policies.max_ratio_step_bp`).

### Added

- **Request trace id.** Each public and internal HTTP request carries `X-Trace-Id`
  (an incoming 32-hex id or W3C `traceparent` is kept, otherwise one is generated)
  and the response echoes it. Structured logs add `trace_id`. Internal RPC forwards
  the header. Saga branch URLs store it as a `trace` query parameter so a replayed
  inventory branch logs the same id. This is separate from the search-attribution
  `trace_id` on `/search`.
- **AI employee: channel stock allocation.** A new MCP compute tool, `channel_allocation_review`, gives the agent,
  per store × SKU × sales channel, the rule in force, the published quantity, units sold, daily velocity while in
  stock, hours the channel sat at zero (split into "keel had stock but the rule published 0" and "keel was out"),
  stock-out rejections and net revenue per unit (channel price minus `commission_bp` minus SKU cost), plus
  rule-based baseline suggestions. A new proposal kind, `channel_stock_rule`, changes a channel's ratio / safety
  stock / cap for a store or store × SKU: the agent must quote the current rule (`prev`), checked again under a
  per-binding lock at execution so a human edit is never overwritten; proposals carry a preview of the published
  quantity before and after; auto-execution is capped by `max_ratio_step_bp` and never zeroes a channel; a 7-day
  before/after review lands in the scorecard. Zero periods are recorded from push results
  (`channel_listing_zero_spans`). Playbook: `agent/skills/渠道库存分配.md`. Design:
  `docs/superpowers/specs/2026-10-03-ai-channel-allocation-design.md`.
- Channels whose callbacks carry the full order state are ingested straight from the payload (no read-back);
  a demo-only simulated takeout channel (`demo_takeout`, registered only with `KEEL_CHANNEL_DEMO=on`) drives the
  allocation loop on the demo site. It is a simulation, not a Meituan or Ele.me integration.

- **Sales-channel adapter layer, phase 1: the skeleton** (`KEEL_CHANNELS=on`, off by default). One interface
  designed for the union of what Shopify, Meituan Shangou and Ele.me Retail need (and, later, a supermarket ERP),
  split by role — catalog source, stock source, sales outlet, sales sink — with capability flags so the generic
  code never branches on a channel's name. Design: `docs/superpowers/specs/2026-10-02-channel-adapter-design.md`.
  Phase 1 shipped no real adapter; Shopify arrives in phase 2 below.
- One stock, per-channel views: each channel publishes `clamp(floor(available × ratio) − safety stock, 0, cap)`,
  configurable per channel, per store and per SKU, plus per-channel pricing (markup or a fixed SKU price) on top
  of the store's effective price. When a SKU's sellable quantity changes, the inventory service sends a dtmrs
  two-phase message (`stock.changed`, keys only); core recomputes and pushes absolute quantities, coalescing bursts,
  retrying with backoff, and overwriting the channel (keel is authoritative) when a compare-and-set reports that
  someone changed the number on the platform.
- `POST /webhooks/channels/{binding_id}`: one callback address per channel account, verified by the channel's
  adapter, deduplicated by the platform's event id, processed asynchronously. Unknown binding, bad signature and
  missing secret are the same empty 401.
- Admin API under `/admin/channel-kinds` and `/admin/channel-bindings`: accounts, write-only credentials, store and
  SKU mappings, stock allocation and price rules, and the last values pushed. Reads need a shop-wide role, writes
  need an administrator.

- **Sales-channel adapter layer, phase 2: Shopify catalog and stock.** A Shopify adapter (Admin GraphQL
  `2026-10`, client-credentials tokens renewed before their 24-hour expiry) acting as both catalog source and sales
  outlet. Enabling a Shopify account pulls its products into keel (new products are created as drafts under the
  account's `default_category_id`; variants whose SKU code already exists in keel are adopted instead of
  duplicated), takes each new SKU's initial stock from Shopify so the first push is a no-op rather than a wipe,
  downloads product images from Shopify's CDN, installs the webhooks it needs, and follows `products/*` callbacks.
  From then on keel is authoritative for stock and price: quantities go out through `inventorySetQuantities` with
  compare-and-set (a conflicting row is re-read and overwritten; the rest of its batch is resubmitted in the same
  call), prices through `productVariantsBulkUpdate` from one designated store per account.
- Changing a store, region or base price, disabling a SKU, or publishing / unpublishing / deleting a product now
  re-evaluates what every sales channel shows (previously only channel rules did).
- Rate limiting by a platform no longer counts towards a channel job's retry budget, and adapter error text is
  scrubbed of the account's credentials before it is stored or logged.
- A test-only Shopify simulator (`internal/channel/shopify/shopifytest`) and opt-in tests against a real
  development store (`KEEL_SHOPIFY_LIVE=1`).

- **Channel admin pages.** The merchant console gets a "Channels" section (shown only when `KEEL_CHANNELS` is on and
  the operator is shop-wide): channel accounts with write-only credentials, store mappings, stock allocation and
  price rules, the last values pushed to each channel (filterable to errors), and a manual "re-sync products".
  Products whose catalog comes from a channel are marked "managed by Shopify"; their title, description, images and
  variant specs are locked — `PATCH`/`PUT` that would change them returns `409` with the new problem type
  `https://keel.dev/problems/managed-by-channel` (re-sending the current values is still accepted), and the lock
  lifts when the channel account is disabled. New contract fields: `ChannelBinding.has_secrets`,
  `ChannelListing.sku_code` / `product_title`, `AdminProduct.managed_by`; new operation
  `POST /admin/channel-bindings/{binding_id}/catalog-pulls`.

- **Sales-channel adapter layer, phase 3: channel orders.** `orders.user_id` is nullable now
  (a new `source` column marks self-run vs. channel, 0/1, migrations `00320`–`00322`); a channel order first lands
  as a normalized platform snapshot (`channel_orders`, one row per platform order) and only becomes a paid keel
  order — deducting the same shared stock — once accepted. Shopify orders arrive paid-only (Shopify's `PENDING` /
  `AUTHORIZED` / `PARTIALLY_PAID` are treated as not-yet-payable) and accept automatically, building the keel
  order against each line's *remaining* quantity (`qty − refunded_qty`) so refunds taken on the platform before
  accept are respected; a missing store or SKU mapping, or insufficient stock, marks the channel order an
  exception with a specific reason instead of silently failing, and a store operator can retry once it is fixed.
  Shipping in keel pushes the tracking number back to Shopify (`fulfillmentCreate`, idempotent); a cancel, refund
  or fulfillment made on Shopify's side is translated into the matching keel action — platform refunds post
  directly as settled refund rows against the matching keel order line, no payment channel involved
  (migrations `00323`/`00324` let `refunds.user_id`/`payment_id` be null for these); a whole-order cancel before
  shipment refunds the order and restocks, a cancel after shipment is marked an exception instead of
  auto-refunding. Platform-initiated requests (`channel_order_requests`) get a generic accept/decline flow with a
  decision-deadline scan, ready for Meituan's accept-or-timeout model in phase 4. A fourth notification target
  (`channel_orders`, migration `00325`) lets a store be reminded about an unaccepted order or a pending platform
  request before a keel order number even exists. Migration `00326` records, per channel order, the basis a keel
  order was built on (platform line → keel order line, and which platform refunds were already folded into the
  then-remaining quantity) so later platform refunds land on the right line and are not double-counted.
  New admin pages: a channel-order list and detail (exception flag, retry, accept / reject, request decisions),
  filtered to a store-scoped operator's own stores (reading another store's order returns `404`, not `403`, to
  avoid leaking existence); the admin order list marks each order's `source`. With `KEEL_CHANNELS` off, the
  self-run order, refund and fulfillment write paths stay branch-free, same as phases 1–2.

### Fixed

- Shopify stores with tax-inclusive pricing (`taxesIncluded`) now record keel's paid amount as the customer's
  full payment; tax-exclusive stores keep tax out of the keel order and log it on the channel order instead.
- A refund-only request (no cancellation) now books freight before goods and strips tax from the refunded
  amount; a whole-order cancel's line-level refunds are capped at what was actually received per line.
- Channel order list and detail responses carry `retryable`; the admin "retry" button follows that one field
  instead of a separate, possibly inconsistent, check.
- Retrying a channel order now recovers correctly after a SAGA commit failed partway through, or when two
  platform updates landed in the same second; "retry" can rescue an order stuck at draft or swept by cleanup.
- An action's response now checks the same store-scope permission the action itself used, so a store admin no
  longer sees an action succeed and then get `403` reading it back.
- Migration `00325` commits statement-by-statement so a failed run can be resumed instead of left half-applied.
- Fulfillment push-back only counts an order as "already sent" once every non-cancelled fulfillment order is
  fully `CLOSED`; `ON_HOLD` / `SCHEDULED` / `INCOMPLETE` are retried (and counted toward the dead-letter budget)
  instead of being treated as done.

### Known limitations

- No currency conversion: amounts are recorded in cents as given, so a channel's currency should match keel's.
  A negative quantity on Shopify (oversold) is still overwritten with keel's non-negative number. An order Shopify
  splits across more than one fulfillment location is marked an exception rather than split into multiple keel
  orders. An edit made to an existing Shopify order (adding or removing line items) is only logged, not applied.

### Invariant

- **With `KEEL_CHANNELS` off nothing changes**: no channel route, branch or background task exists, and the
  inventory service never queries `channel_merchants`. With it on, only merchants that have an active sales
  channel send `stock.changed`; for everyone else the stock write path is unchanged. Both are pinned by tests.
  In split deployments core and the inventory service must use the same value.

## [0.7.0] - 2026-10-02

Core migrations `00170`–`00173` (`orders.receiver_phone` and its backfill, keyword search as a definer function,
`inventory_logs` created-at index), `00180` (`promotion_skus.quota_qty`), `00190` (store fence geometry index),
`00200` (product listing category and in-stock indexes), `00210` and `00220` (`search_logs` keyword recall
columns), `00230` (`search_relevance_judgments`) and `00240` (promotion quota revision); the core database lands
on `00240`. The inventory database (split deployment) gains `00173` and `00240` (`activity_sync_revs`) and lands
on `00240`.

**Highlights.** The split deployment grows up: the transaction coordinator runs as its own service and the
inventory service no longer calls back into core — every cross-service effect is a dtmrs two-phase message.
Uploads can live in any S3-compatible bucket, which removes the last thing pinning the app to one machine.
Search learns to tell relevant from merely similar: an offline evaluation showed a cosine floor cannot separate
the two (precision 0.13 at `0.40`), so hot queries now get their vector-only candidates judged in the
background by a discriminative model. A load-testing round found and fixed nine bottlenecks (small-category
listing 57×, admin reads 5.7–24×, mixed-load ceiling from ~400 to 1150+ RPS). The frozen uni-app x client is
gone; Flutter is the only buyer app.

### Added

- **Search relevance pre-judging with a discriminative model** (`KEEL_SYSTEMONE_ENDPOINT` +
  `KEEL_SYSTEMONE_TOKEN`, any engine speaking `/v1/systemone`, such as Kev, Jev or infero). Once an hour (one
  elected instance), queries searched at least 3 times in the last 7 days have their vector-only candidates
  judged for relevance; results land in `search_relevance_judgments` (migration 00230). Search then keeps a judged
  candidate when P(relevant) ≥ 0.3 and drops it otherwise; unjudged candidates still go through the cosine floor.
  On the demo site the first pass judged 528 pairs for 23 hot queries in 5 seconds: "dress" stops pulling in
  cardigans, coats and jeans, and a query for something the shop does not sell ("vacuum flask") now gets the
  "you might want" fallback instead of six coffee accessories posing as hits. Off unless configured.
  Online judging is not used: at ~25 ms + 14 ms per candidate it cannot sit in front of a search request.
- **Offline search evaluation** (`keel-searcheval sample → label → calibrate`): samples candidate pairs from
  real search logs, labels them with the same discriminative model (47/50 agreement with a human reference),
  and calibrates the relevance floor against the labels. This is how the cosine floor was shown not to work.
- Keyword recall is recorded per search (`search_logs`, migrations 00210 / 00220) and exposed in the explain
  diagnostics header: AND only, AND then OR, or AND plus vector.
- **Map picker in the buyer app** (`flutter_map`): `GET /geo/map` says whether a basemap is configured and which
  layers to stack; `GET /geo/tiles/{layer}/{z}/{x}/{y}` proxies Tianditu or OSM tiles (`KEEL_TILE_PROXY` for an
  egress proxy where OSM is unreachable). The admin fence editor also shows the shop's other stores and fences.
- The address book knows the store: `GET /addresses?store_id=` marks each address `in_service_area`, and
  checkout picks the default address inside the delivery fence automatically.
- Retention cleanup service (expired idempotency records and other aged rows), run by one elected instance.
- Graceful shutdown on SIGTERM / SIGINT, `/readyz`, and pool-level `statement_timeout` (15 s) /
  `idle_in_transaction_session_timeout` defaults.
- Load-testing kit: `cmd/keel-loadtest`, seed scripts in `scripts/loadtest`, and the report in
  `docs/性能压测-2026-10.md`.
- `compose.infero-cpu.yaml`: semantic search without a GPU (CPU build of infero; plan for 4–8 dedicated cores).
- **Uploads can live in S3-compatible object storage** (`KEEL_UPLOAD_DRIVER=s3`, `KEEL_S3_*`): AWS S3,
- **Uploads can live in S3-compatible object storage** (`KEEL_UPLOAD_DRIVER=s3`, `KEEL_S3_*`): AWS S3,
  Alibaba Cloud OSS, Tencent COS, or self-hosted SeaweedFS (`compose.s3.yaml`). Required for multi-instance,
  Kubernetes and serverless deployments. Local disk stays the default.
- Reads, deletes, thumbnails and orphan cleanup follow each row's `uploads.driver`, so switching storage needs
  no downtime: new files go to the bucket while existing ones keep being served from disk.
- `keel-uploads migrate` (shipped in the app image) moves existing files to the bucket, verifying sha256 and
  size before repointing each row; resumable, with `-dry-run` and `-delete-source`.
- Optional presigned redirects (`KEEL_S3_PRESIGN_ENDPOINT`): image requests 302 straight to the object store,
  so bytes no longer pass through the app.

### Changed (split deployment)

- **The transaction coordinator now runs as its own service in the split (tier C) deployment.**
  `core` and `inventory` no longer embed dtmrs; they talk to a standalone dtmrs 0.12 cluster
  (`KEEL_DTM_SERVER` + `KEEL_DTM_TOKEN`), which calls their branches back over the internal port
  (`KEEL_SELF_URL`). `compose.split.yaml` gains a `dtmrs` service and its own `postgres-dtm`.
  The monolith (`KEEL_ROLE=all`) still embeds the coordinator (`KEEL_DTM_DSN`).
- **Inventory no longer calls core.** Zero-crossing stock notifications are published to the topic
  `stock.zero_crossing` and core subscribes; activity quota sync carries the definition plus a
  monotonic version in the message payload (migration 00240) instead of having inventory read it back.
  `KEEL_CORE_URL` is retired and refused at startup.
- Upgrading a running tier-C deployment: stop taking orders, wait until both embedded coordinators have
  no unfinished transactions, then switch the configuration. In-flight transactions are not migrated.

### Changed

- **In-stock ordering follows stock changes immediately instead of once a minute.** When a
  SKU's sellable quantity in a store crosses zero (an order drains it, a cancellation or refund
  restocks it, an admin sets or adjusts it), the inventory service registers a dtmrs two-phase
  message in the same local transaction; the core re-reads live stock for the affected products
  and rewrites `product_store_stock`. Changes that do not cross zero send nothing. The full pass
  becomes a safety net: `KEEL_STOCK_FLAG_INTERVAL` now defaults to `1h`. In the split deployment
  the message goes through the standalone coordinator (topic `stock.zero_crossing`, see above;
  `compose.split.yaml` wires it); without a coordinator, ordering falls back to the hourly pass.
- **Promotion quotas sync through a two-phase message instead of a call before the write.** The
  quota an operator configures is now stored in the core (`promotion_skus.quota_qty`, migration
  00180); creating a promotion or replacing its SKUs registers a dtmrs message in the same
  transaction, and the inventory service re-reads the current definition and applies it
  (idempotent, order-independent). Saving works while the inventory service is down; the quota
  catches up when it returns. The "sold SKUs cannot be removed / quota cannot go below sold" rules
  are still checked before the write (same 422) whenever the inventory service is reachable;
  violations discovered only at delivery are clamped (the sold SKU stays, the quota is raised to
  what was sold). Going live still syncs directly first, so a promotion never goes live on a
  stale quota.

- Lock and statement timeouts return `503 busy` with `Retry-After` instead of a bare 500, and only when the
  operation definitely did not take effect. An open inventory circuit breaker fails the order immediately with
  503 instead of a 409 after 15 seconds.
- Request bodies that fail to decode distinguish a syntax error from a wrong field type; 422 names the field.
- Contract: coordinates, fence points and distances are `format: double`; `POST /search` rejects `size`
  outside 1–100 with 422; product subtitles and the shop's service phone are validated.
- Fence checks use planar geometry (`ST_Intersects(fence::geometry, point)`), matching the straight edges the
  admin map draws; fences read back bit-for-bit (`ST_AsGeoJSON(..., 24)`).
- The admin console is usable on a phone (≤768 px: drawer menu, single-column forms).
- Compose overlays (`compose.split.yaml`, `compose.split-b.yaml`, `compose.multi.yaml`) prefix their volume
  names with the project name, so stacks started with different `-p` no longer share volumes. Under the default
  project name the volume names are unchanged.

### Performance

- Product listing: small categories page through a category index instead of filtering the whole catalogue
  (57–58×); deep pages pick the page before computing prices; totals are cached in-process for 30 s per
  (merchant, store, category, in-stock-only); the in-stock count starts from a partial index (00200).
- Keyword search truncates by relevance before loading prices and images; multi-word queries try AND first and
  only fall back to OR when a page is not filled; vector hits that fill a page skip OR entirely; the OR hit set
  is capped at 1000. Without an embedding engine, p95 at 16 / 32 / 64 concurrent is 51 / 99 / 170 ms.
- Admin sessions write `last_seen_at` at most once a minute instead of on every request (admin reads 5.7–24×).
- Notifications with no external channel configured no longer enqueue delivery jobs (in-app messages only).
- The application pool starts with `jit=off` (`KEEL_DB_JIT` turns it back on): deep listing queries were spending
  ~200 ms compiling for a 12 ms execution.
- Overdue-order closing keeps running while a pass is full (up to 20 passes per wake-up) instead of a fixed 500
  orders per minute.

### Removed

- **The uni-app x buyer client (`app/`)** and its tooling (`generate-uts`, `app-*` make targets, UTS contract
  checks). It had been frozen since Flutter took over; the last state is tagged `uniapp-final`.
- `KEEL_CORE_URL` (split deployment; see above). Startup refuses it.

### Fixed

- Category listing computed the inner `LIMIT` as `offset + limit` in int4 and could overflow; now bigint.
- Three RLS predicates that were not leakproof stopped using indexes (order receiver phone among them); the
  phone moved to a redundant indexed column (00170 / 00171).
- Refund review, receipt and over-collection returns read the callback secret on the transaction's own
  connection instead of a second pooled one (a test now fails on any such second connection).
- Activating a promotion always registers a quota sync message in the same transaction, closing a race with a
  concurrent SKU change.
- Missing in-stock flag rows are treated as out of stock, and flags are filled in right after creating a store,
  a SKU or an import.
- Leader election detects a frozen or partitioned leader (server-side `idle_session_timeout`).
- Buyer app: errors shown to buyers no longer expose HTTP details.

## [0.6.0] - 2026-09-28

Core migrations `00140` (`search_logs.fallback`, and the column on the `agent_ro.search_logs` view), `00141`
(`agent_briefs.corrects_id`), `00142` (price upper bounds, `NOT VALID`) and `00150` (`payment_intents`,
`payment_returns`) and `00151` (`shop_preferences.after_sale_days`); the core database lands on `00151`.

**Highlights.** Hardening. AI operations were accepted end to end on the demo site, and three rounds of destructive
testing (money and concurrency, input validation, state machines and AI tools, cross-shop isolation) found real
bugs that are fixed here. The biggest: a payment the order does not accept is now refunded automatically instead of
kept with only a log line. Search gets a relevance floor, so the zero-result metric and the AI staff's search-gap
playbook finally mean something.

### Added

- `ProductSummary.promo_min_price_cents` (on `GET /products` and `POST /search`): the lowest limited-time / flash-sale
  unit price in the resolved store, present only when it undercuts `min_price_cents`. The Flutter product card shows
  it with the store price struck through.
- `GET /admin/orders` and `GET /admin/refunds` accept `created_date_from` / `created_date_to` (`YYYY-MM-DD`, both
  inclusive), cut into days in the **shop's time zone** — the same rule as the reports. Combining them with
  `created_from` / `created_to` is a 422. The admin console's date filter now sends these instead of converting in
  the browser's time zone.

- `POST /search` has a relevance floor. A product found only by vector recall counts as a match when its cosine
  similarity is at least `KEEL_SEARCH_VECTOR_FLOOR` (default `0.40`, calibrated for Qwen3-Embedding-0.6B). If
  anything matches, only matches are returned; if nothing does, the low-similarity results come back as suggestions
  with `fallback: true`, and the search is counted as a zero-result search in reports, `search_insights` and the
  `search_zero_spike` event. Before this, vector recall always filled the page, so the zero-result rate was always 0.
- `AdminRefund.order_shipped_at` (also on the MCP `list_refunds` tool).
- AI staff can correct a brief they already posted: `post_brief` takes `corrects_brief_id`; the original is kept
  verbatim and marked corrected (`AgentBrief.corrected_by_brief_id`, also on the public AI log).
- `propose_coupon` accepts a fixed validity window (`valid_start_at` + `valid_end_at`) as an alternative to
  `valid_days`.
- `search_insights` and `GET /admin/reports/search`: per-term `click_count` / `order_count`, and a
  `low_click_queries` list (searched at least 3 times, always with results, lowest click-through first).
- `promotion_review` on a coupon reports `refunded_order_count`.
- Proposal outcome reviews wait for the measurement window to close; a review that comes due early is deferred.
- MCP tool output reports times in the shop's time zone (RFC 3339 with offset) instead of UTC.

- **Over-collected payments are returned automatically.** An order accepts exactly one payment. Starting a payment
  again on the same channel now reuses the same channel transaction (switching channels supersedes the old one), and
  any payment the order does not accept — a duplicate, one that arrives after the order was cancelled or closed, or
  one whose amount does not match — gets a payment return that refunds it to the original channel (immediately in
  sandbox mode; real channels settle through `/webhooks/refunds/{channel}`). A sweep every minute backfills anything
  missed and retries failed submissions. New `GET /admin/payment-returns`; `OrderDetail.payment_returns` for buyers.
  Previously the money was kept with only an error log.

- **After-sale window**: `ShopSettings.after_sale_days` (default 15). A finished order can be refunded only within that
  many days of completion; after that `POST /orders/{no}/refunds` is a 409 `after-sale-window-closed`.
  `OrderDetail.after_sale_deadline` tells clients when it ends; the Flutter app hides the refund entry after it.
- `OrderPreviewItem.available_qty`: the store's current stock for each line, so checkout can flag a shortage before
  submitting (the Flutter checkout does, and disables submit).
- Searching for a product's exact title puts that product first (when in stock).
- A platform session switched into a shop with `X-Keel-Merchant` now manages that shop's staff (create, list,
  update, reissue); without the header it still manages platform staff.

### Fixed

- A pending order at a closed store (or a disabled region) can no longer start a payment (409
  `store-unavailable`); paid orders there can still be shipped.
- Refunding an unshipped line bought at a promotion price releases its promotion quota (the per-buyer limit is kept).
- Unit prices had no upper bound; a huge price overflowed order totals (500) or produced an absurd order. Capped at
  100 million yuan everywhere prices are written.
- An unshipped order refunded line by line (a second request while the first was pending) never refunded the freight
  and could still be shipped. That request is now refused in favour of a whole-order refund; shipping an order whose
  every item is refunded or being refunded is refused.
- After-sales refunds now go back to the payment that settled the order, not the earliest successful one.
- Approving an AI proposal whose target had changed returned 500 and left it stuck "executing"; it is now recorded as
  failed with the reason. `propose_coupon` validates exactly like the admin coupon form. The auto-execution daily
  limit held under concurrency. `query_sql` results are capped at 1 MB (4 KB per value).
- Search results never carried `promotion_tags`; they now carry the same tags as the product list.
- The Flutter checkout page no longer shows the idempotency key ("技术信息") in release builds.

## [0.5.0] - 2026-09-28

Core migrations `00120`–`00122`, `00130`–`00132`; the core database lands on `00132`. Migration `00131` creates the
cluster role `keel_agent_ro` (NOLOGIN) and grants it to `keel_app`.

**Highlights.** AI operations M10 and M11. AI staff can now propose limited-time discounts, coupons, product copy
changes and after-sales decisions, not just restocks; they are woken by events (pull over MCP, or a signed
webhook for your own harness); every executed proposal is measured after the fact and rolled into a scorecard; a
shop admin can let proposals of a kind execute automatically under caps; a read-only SQL tool answers ad-hoc
questions over curated views; and a shop can publish its AI operations log. 24 MCP tools, each with an output
schema.

### Added

- **Public AI operations log (M11, 00132).** A shop admin can publish the AI staff's work: `PUT
  /admin/ai-log/settings` (off by default) makes `GET /api/v1/ai-log` return, without login, the latest briefs
  (title, dates, first 300 characters), the latest proposals (kind, title, status, auto-executed or not, review
  verdict) and 30-day totals — never the evidence, execution parameters or rejection reasons. A static page
  `web/ai-log/` renders it; the demo site serves it at `/ai-log/`.
- **Playbooks and an event-driven runner (M10).** `agent/skills/` gains 滞销清仓 (clearance), 搜索缺口 (search
  gaps), 售后审核 (after-sales review), 活动复盘 (promotion review) and 事件处理 (event dispatch);
  `agent/runner/claude-events.sh` checks for new events with one plain JSON-RPC call and only starts the agent
  when there are some (cron every 10 minutes). `agent/AGENTS.md` and `docs/AI接口.md` cover all tools.
- **Read-only SQL for AI staff (M11, 00131).** The MCP tool `query_sql` answers questions the dedicated tools
  can't. It runs one `SELECT` / `WITH` as the role `keel_agent_ro`, which can only read a set of views in schema
  `agent_ro` (orders, order items, products, SKUs, categories, stores, regions, refunds, search logs, coupons,
  promotions) that leave out phone numbers, addresses, buyers' own words, payment callbacks, tokens and keys and
  filter by tenant explicitly. Functions that change session settings or touch the server are rejected up front,
  the tenant setting is re-checked after the query (results are discarded if it changed), 3-second timeout, 500
  rows. Merchant-wide AI staff only.
- **Auto-execution policies (M11, 00130).** A shop admin can let an AI staff member's proposals of one kind execute
  immediately, within a per-proposal cap (restock units, minimum discount rate, coupon face value) and a 24-hour
  count. Matching proposals run through the same execution path as a human approval (`auto_approved = true`, no
  `decided_by`); anything over a limit waits in the queue as before. After-sales decisions can never be
  auto-executed. `GET /admin/agents/{staff_id}/auto-policies`, `PUT /admin/agents/{staff_id}/auto-policies/{kind}`.
- **Post-execution review and the AI staff scorecard (M10, 00122).** Keel measures every executed proposal after a
  while — restocks after 7 days (units sold, stock-out days), limited-time discounts 3 days after they end (units vs
  the equal period before), coupons after 7 days (claimed, used, use rate), copy changes after 7 days (units vs the
  7 days before) — and stores an `outcome` with a fixed, explainable `verdict` (positive / neutral / negative).
  `GET /admin/agents/{staff_id}/scorecard` counts proposals by kind and status plus the verdicts over 30 days; the
  agent reads its own with the MCP tool `my_scorecard`. `AgentProposal` gains `executed_at`, `outcome`,
  `outcome_at`.
- **AI staff can propose marketing, copy and after-sales actions (M10, 00120).** Four new proposal kinds, each an
  MCP tool that only proposes; a human approves in the console and Keel executes as the agent, with the same checks
  as the admin API and the proposal id as the idempotency key: `propose_flash_price` (a limited-time discount on up
  to 20 SKUs, at most 50% off, at most 14 days — created and put online on approval), `propose_coupon` (a coupon
  template, face value ≤ ¥100, ≤ 10,000 issued), `propose_product_copy` (new title / subtitle; before and after are
  kept), `propose_refund_decision` (approve / reject a pending after-sales request, with a buyer-facing reason
  for rejections). Marketing and product proposals are shop-wide: only merchant-wide AI staff can file them and only
  merchant-wide staff can approve them. `AgentProposal.store_id` is now optional; the proposal list filters by
  `kind`.
- **AI staff events (00121, AI operations M10).** Keel records an event when a (store, SKU) drops to or below its
  warning line (`stock_low`, same basis as the inventory-alerts report, at most once per pair per 24 hours), a buyer
  files an after-sales request (`refund_created`, written in the same transaction), a zero-result search term is seen
  5+ times in an hour (`search_zero_spike`, shop-wide, once per term per day), or a proposal is executed / fails /
  is rejected / expires (`proposal_decided`). A background sweep every 5 minutes emits the first and third kind; it
  reads stock through the inventory service, so it works with inventory in its own database. New MCP tools
  `list_events` (filtered by the agent's scope: store events only for stores it can operate, shop-wide events only
  for merchant-wide agents) and `ack_events` (a per-agent cursor that only moves forward).
- **Event webhooks for AI staff.** `GET/PUT/DELETE /admin/agents/{staff_id}/webhook` (shop admins only): one https
  URL per AI staff member; the signing secret is returned only on creation or with `rotate_secret: true`. Events are
  delivered through the jobs queue as `POST` JSON with `X-Keel-Event`, `X-Keel-Event-Id` and
  `X-Keel-Signature: sha256=<hex HMAC-SHA256 of the body>`, 5 s timeout, up to 6 attempts with exponential backoff,
  only for events within the agent's scope at delivery time; every attempt is logged and the last 20 are shown by
  `GET`. See `docs/AI接口.md` 「事件」.
- **Two more read-only MCP compute tools (AI operations M10).** `slow_movers`: for each (store, SKU) with
  available stock at or above `min_available`, the inventory turnover in days (available ÷ daily average,
  same denominator convention as `restock_plan` — stockout days excluded); a null turnover means nothing sold
  in the lookback window, sorted first as the most worth clearing. `promotion_review`: given exactly one of
  `promotion_id` or `coupon_template_id` — for a promotion, sales/orders/AOV and participating-SKU units sold
  in its window (`starts_at`..`min(ends_at, now)`) versus the previous window of equal length (limited-time
  discounts and flash sales scope to their `promotion_skus`; full-reduction/full-discount promotions are
  shop-wide); for a coupon template, claimed/used counts, use rate, and the sales and total discount from the
  orders that used it. No migrations. 16 MCP tools total; see `docs/AI接口-工具清单.json` and
  `docs/AI接口.md`.

## [0.4.0] - 2026-09-28

Migration `00111`; the database lands on `00111`.

### Added

- **Same-city delivery templates (00111).** Save a delivery rule (minimum order, free-over amount, distance tiers)
  as a named template and mark one as the default. A fenced store with no configuration of its own now follows the
  default template, so opening a new store needs no delivery setup; a store can reference another template or keep
  a custom rule, and editing a template takes effect for every store using it. `GET/POST
  /admin/local-delivery-templates`, `PUT/DELETE /admin/local-delivery-templates/{template_id}` (writes need
  merchant-wide staff; a template in use, or the default one, cannot be deleted — 409
  `local-delivery-template-in-use`); `DELETE /admin/stores/{store_id}/local-delivery` switches a store back to
  following the default. The store response gains `source` (`custom` / `template` / `default_template` / `none`),
  `template_id`, `template_name` and `custom`; the v0.3 request and response shapes still work. The console gets
  a 「同城配送模板」 section and the store tab chooses follow-default / template / custom.

## [0.3.1] - 2026-09-28

No migrations; the database stays on `00110`.

### Fixed

- **AI staff list showed no scope.** `GET /admin/agents` returned empty `region_ids` / `store_ids` for every AI staff
  member, so the console's 「管辖范围」 column showed 「—」 for region and store managers and it looked as if the
  scope had not taken effect. Only the list was wrong: the scope was stored and enforced on every tool call all
  along (and `GET /admin/agents/{staff_id}` returned it).

## [0.3.0] - 2026-09-28

Core migrations `00075`–`00076`, `00085`–`00087`, `00090`–`00093`, `00100`, `00110`; the core database lands on
`00110`. The inventory service's own database (split tier C) starts its migration line at `00001`.

**Highlights.** Keel is now location-first: a store is a geofence, the buyer's location picks the store, the
shipping address must be inside that store's fence, and fenced stores charge same-city delivery by distance with a
minimum order. POI place search and reverse geocoding replace typing addresses. AI staff (M9) ship: plug in your own
agent over MCP — reads, deterministic restock computation, proposals a human approves, briefs, audit — with an
integrator-facing interface contract that any harness can build on. The Flutter storefront becomes the main client.

### Added

- **Flutter storefront (`flutter_app/`) becomes the main buyer client.** One codebase for Android, iOS, Web and
  the WeChat Mini Program (via mp-flutter), generated from the same OpenAPI contract (`make flutter-generate` →
  `lib/api/schema.g.dart`, committed and checked; pages read view models, a check enforces it), with unit tests
  and a headless Web e2e suite. The demo site's `/` now serves its Web build (CanvasKit and fallback fonts are
  self-hosted, so it loads without Google's CDN). The uni-app x client (`app/`) is frozen: bug fixes only.
- **Shipping addresses carry coordinates (00100).** `Address` / `AddressInput` gain optional `lat` / `lng`
  (WGS-84, both or neither — one alone is 422; `PUT` without them clears them). Filled by place search or map
  picking; the fence check at checkout and same-city delivery distance use them.
- **Admin 「AI 员工」 section (M9, task 6).** Proposals (filter by status, evidence / payload / result shown as
  plain text, approve with confirmation and outcome-aware feedback, reject with a required reason), briefs, and
  AI staff with access keys (create, edit role and scope, disable, issue a key shown once with a ready-to-paste
  MCP config, revoke). The overview page shows a banner when proposals are waiting.
- **POI backend: `/geo/reverse` and `/geo/suggest`.** Server-side proxy to the map provider (AMap for now,
  `KEEL_GEO_PROVIDER` / `KEEL_GEO_KEY`); coordinates in and out are WGS-84 (GCJ-02 converted server-side),
  results cached, rate-limited per IP; 501 when not configured so clients fall back to manual entry.
  `street` is the sub-district (township, the fourth level of a shipping address), not the road name;
  coordinates are rounded to 6 decimals. The admin console's store editor gained a place search above the map
  that fills province / city / district / address from the chosen point.
- **AI interface for third-party harnesses (`docs/AI接口.md`).** An integrator-facing reference for connecting any
  MCP client (Claude Code, Codex, Cursor, custom scripts, chat bots) as AI staff. Every MCP tool now declares an
  `outputSchema` (validated before returning); tool errors carry a machine-readable `_meta["keel/problem"]`
  (`type`, `title`, `status`, `detail`, same problem types as the admin API). The tool list is snapshotted in
  `docs/AI接口-工具清单.json` and guarded by a test: changes must be additive, breaking changes get a new tool name.
- **Same-city delivery for geofenced stores (00110).** A store with a fence (and not the default store) now charges
  delivery by distance instead of by freight template: fee tiers by straight-line distance from the store to the
  shipping address (`within_m` → `fee_cents`, beyond the last tier or without coordinates the last tier applies),
  free delivery over an amount (after promotions and coupons, like free shipping), and a minimum order (after
  promotions, before coupons) — below it, preview and checkout return 422 `below-minimum-order` and the cart shows
  the shortfall. Freight templates, including templates attached to a product, no longer apply at fenced stores:
  a product's nationwide express template used to override the store's own delivery fee. The default store and
  stores without a fence keep using templates. `FreightBreakdown` gains `mode` (`express` / `local`) and `local`;
  configured under `GET/PUT /admin/stores/{store_id}/local-delivery`.
  **Upgrade note:** fenced stores charge 0 delivery until configured.
- **Orders must ship inside the store's fence.** `POST /orders` and `POST /orders/preview` return 422
  `address-out-of-range` when the shipping address has coordinates outside the chosen store's fence. Addresses
  without coordinates (manual entry, pre-00100) are not checked; the default store (nationwide fallback) and
  stores without a fence accept any address.
- **Briefs (M9, task 5).** AI staff write operating briefs (daily store-walk reports and the like) with the MCP
  tool `post_brief` (markdown, up to 8 KB, with the covered date range); shop-wide staff read them under
  `/admin/agent-briefs` (migration 00092). Bodies are treated as untrusted input by clients.
- **Proposals (M9, task 4): AI staff propose, humans approve, Keel executes as the agent.** New MCP tools
  `propose_inventory_adjust` (same authorization as the admin inventory adjustment; one open proposal per
  store and SKU) and `list_my_proposals` (results and rejection reasons). Admin endpoints under
  `/admin/agent-proposals` list, show, approve and reject (migration 00091). Approval checks the approver,
  then executes the existing inventory adjustment **as the agent** with the proposal id as idempotency key,
  so a narrowed scope or a disabled agent makes execution fail (status 40, reason recorded) and a retry
  after a crash never adds stock twice. Unhandled proposals expire after 48 hours.
- **`restock_plan` MCP tool (M9, task 3): deterministic restock suggestions.** Per (store, SKU): paid units
  over the lookback window divided by *in-stock* days (days whose closing level was ≤ 0 are excluded —
  otherwise items that ran out get under-ordered), days of cover, projected sell-out date in the shop's time
  zone, suggested quantity to cover N days (rounded up to a multiple of 5, capped at 1000), and a `low`
  confidence flag when fewer than 5 days of data exist. Stockout days come from the inventory service
  (new `StockoutDays`, local and HTTP), since inventory logs live there in split deployments. Same scope rules
  as the store inventory list.
- **MCP endpoint for AI staff (M9, task 2): `/api/v1/mcp`.** Streamable HTTP (official Go SDK, stateless:
  every request re-authenticates the `kagt_` key), tenant from Host. Ten read tools — `shop_overview`,
  `sales_trend`, `product_ranking`, `store_comparison`, `inventory_alerts`, `search_insights`, `list_stores`,
  `list_products`, `get_product`, `list_refunds` — call the same services as the admin API (same scope
  checks: a store-manager AI sees only its stores), return the same contract types, and translate errors
  through the same error writers into the same problem types. Every call is audited in `agent_tool_calls`
  (migration 00093); each key is limited to 120 requests per minute (HTTP 429 with Retry-After).
- **AI staff accounts and access keys (AI operations M9, task 1).** An AI staff member is a staff row
  with `kind = 2` (migration 00090): it reuses roles and scopes, so every tool it will call goes through
  the same authorization as a human; it cannot be an admin or platform-level (database CHECKs), cannot
  obtain an admin session (the login-token and session queries only accept `kind = 1`), and does not
  appear in or get modified through `/admin/staff`. Shop admins manage them under `/admin/agents`
  and issue `kagt_…` access keys (shown once, stored as sha256, optional expiry, revocable, instantly
  effective). `GET /agent/whoami` lets an agent check its key. Keys are looked up inside the tenant
  resolved from Host, so one shop's key is unknown on another shop's domain.

- **Thumbnails: `GET /uploads/{upload_id}?w=`.** Four widths (160 / 320 / 480 / 640); any
  other value snaps up to the next tier, capped at 640; absent means the original, unchanged.
  Aspect-preserving, never upscales, always JPEG quality 80 with transparency flattened to
  white. Applies to product images and avatars only (refund evidence ignores it). Thumbnails
  are cached per (file, width) next to the original and removed with it. Motivation: the
  Flutter mini-program decodes images in wasm without JIT on iOS, and list cards display
  ~173 px of an 800 px image.

- **Out-of-stock SKUs sort last and are clearly marked.** On the product page and in the
  quick-add sheet, SKUs with no stock move after the in-stock ones (stable partition, server
  order kept within each group) and render with a dashed grey chip, struck-through label and a
  「无货」 tag instead of just being faded.
- **Stores must have a location, picked on a map, and sit inside their own fence.**
  `POST /admin/stores` now requires `lat` / `lng` (422 without them); `PATCH` can move a store
  but not clear its location. Setting a fence, or moving a store that has one, checks
  `ST_Covers(fence, location)` in the same transaction and rejects with 422
  `store-outside-fence` (points on the boundary count as inside); fencing a legacy store that
  has no location is 422 `store-location-required`. The admin console replaces the two
  number inputs with a Leaflet map picker (same OSM / WGS-84 map as the fence editor) in both
  the create dialog and the store page, draws the saved fence as a reference, and tags
  stores without a location as 「未定位」.

- **Split deployment tiers B and C (phase 2 of `docs/电商系统-微服务拆分方案.md`).**
  Tier B keeps one Postgres but puts inventory in its own `inventory` schema owned by a
  `keel_inventory` role that `keel_app` cannot read. Tier C is `compose.split.yaml`: two
  keel processes (`core` + `inventory`) and two Postgres instances. The inventory migration
  directory now builds into whatever schema the connection's `search_path` points at, and
  grants to `KEEL_INVENTORY_ROLE` (default `keel_app`, so the single database is unchanged).
  Tier B also starts with a single command: `compose.split-b.yaml` runs one process, with
  schema, role, migration and data move all done by the stack.
  `scripts/split-migrate.sh` moves a monolith's inventory tables across
  (`prepare-b` / `copy` / `verify` / `cutover` / `rollback`). All steps can be re-run, and
  `cutover` revokes `keel_app` on the old copies so a process still on the monolith
  configuration fails loudly instead of using stale stock.
- **Hourly inventory reconciliation** in the `all` / `core` roles. It is read-only and only
  reports what it finds: inventory rows whose SKU or store no longer exists in core,
  mismatches between the quota rows of live promotions and `promotion_skus`, and
  dead-lettered `inventory.release` jobs. Findings are logged at WARN.
- **Payment waits for the order SAGA to finish (migration 00085, `orders.placed_at`).** An
  order shows as pending payment as soon as its first SAGA step commits. When the inventory
  service is down, that state can last for minutes in a split deployment. Paying during that
  window and then losing the stock deduction left a paid order that compensation could not
  close. `POST /orders/{order_no}/payments` now returns the existing 409
  `order-status-not-payable` until the finish branch has run. No contract change.
- **Groundwork for splitting inventory into its own service (phase 0 of
  `docs/电商系统-微服务拆分方案.md`).** No behaviour change in the default deployment.
  New, all optional: `KEEL_ROLE` (`all` default / `core` / `inventory`; unknown values
  refuse to start), `KEEL_INVENTORY_DSN` (inventory database, defaults to the main pool),
  `KEEL_INTERNAL_ADDR` + `KEEL_INTERNAL_SECRET` (a separate internal HTTP listener under
  `/internal/v1`, HMAC-signed with the tenant header covered by the signature), and
  `KEEL_INVENTORY_URL` (validated but not used until phase 1). `KEEL_ROLE=inventory`
  currently serves only `/healthz`, `/version` and `/readyz` on the internal port.
  Internally: a signed service-to-service client that separates "definitely failed" from
  "outcome unknown", SAGA steps with per-step payloads, and an HTTP adapter so a saga
  branch can run in another process. No migration.
- **Inventory reads and back-office inventory writes go through an inventory service
  interface (phase 1a).** Internal change; the default single-process deployment behaves
  as before, except where noted below. Everything that reads or writes store stock
  (product detail, cart, search `in_stock`, back-office product / SKU / store-inventory
  pages, the low-stock report, back-office stock edits, initial stock of new SKUs and
  imports) now asks an `inventory.Service` — in-process by default, over the internal
  HTTP API when `KEEL_ROLE=core`. **`KEEL_ROLE=core` now requires `KEEL_INVENTORY_URL`**
  (it refuses to start without it); `KEEL_ROLE=inventory` serves the inventory API under
  `/internal/v1/inventory/`. Order stock deduction, close/cancel release and refund restock
  still run in-process until phase 1b. No migration.
  Behaviour differences: `POST /search` with `in_stock_only: true` now filters after recall
  (the recall window is doubled for it), so a page can come back shorter than `size` when
  many recalled products are out of stock. When the inventory service is unreachable
  (split deployment only): browsing and search keep working with `in_stock` omitted; product
  detail, cart, back-office stock pages/edits and the low-stock report return
  `503 inventory-unavailable` with `Retry-After` (additive contract change); a retried
  back-office stock adjustment with the same `Idempotency-Key` never applies twice.
- **Core and inventory can run on two databases (phase 1b).** Core code no longer touches
  the inventory tables and the inventory service no longer touches core tables.
  Migrations **00075–00076** (core) and a new directory **`db/migrations-inventory`**
  (`make migrate-inventory`, own version table `goose_db_version_inventory`; a no-op on a
  single database that already ran `make migrate`, a full build on an empty one).
  00075 moves flash-sale / limited-price quota and sold counts to a new inventory-owned
  table `activity_stocks` (backfilled; `promotion_skus.stock_qty` / `sold_qty` are no
  longer read or written and will be dropped next release); 00076 drops the foreign keys
  from `inventories` / `inventory_logs` to `skus`, `stores` and `merchants`.
  The order saga is now four steps: create (+ per-user limit), coupon, **inventory**
  (store stock + campaign quota + stock log in one inventory-local transaction, barrier in
  the inventory database, addressed `local://` in one process or `http://` to the
  inventory service when `KEEL_ROLE=core`), and a core **finish** step (refuses an order
  that was cancelled meanwhile, maps an inventory rejection to the usual 409, emits the
  low-stock alert from the inventory log). Buyer cancel, timeout close and refund restock
  commit the core side and enqueue an outbox job (`inventory.release`) in the same
  transaction; it runs right after commit and a worker retries it until the inventory
  service answers. Contract: `503 inventory-unavailable` added (additive) to order
  preview/create, `POST /coupons/applicable` and the back-office promotion endpoints,
  for split deployments only.
  Behaviour differences: a per-user-limit violation now leaves the draft order in status 0
  (it is invisible and closed by the orphan sweep) instead of closing it immediately;
  a rejected stock deduction leaves one zero-quantity log row (`biz_type = 7`); closing an
  order writes a zero-quantity "checked" row for lines that had nothing to release;
  when the inventory service is down (split only), product lists omit flash-sale /
  limited-price tags and stock released by cancel/timeout/refund comes back once it is up.
- **`KEEL_INTERNAL_SECRET` can be rotated without draining in-flight SAGA transactions.**
  New, optional `KEEL_INTERNAL_SECRET_PREVIOUS` (comma-separated for more than one):
  during rotation, verification of internal-request HMAC signatures and SAGA branch
  tokens (`?bt=`) accepts the current secret or any listed previous one; signing always
  uses the current secret. Split deployments only; no effect when unset. See the
  deployment guide's environment variable table for the safe rotation order (expand
  accepted secrets first, then flip which one is current, then drop the old one once
  in-flight transactions have drained).

### Fixed

- **Product list puts in-stock items first**, globally (pagination stays correct). Stock lives
  in the inventory service, so the list query cannot join it; migration 00087 adds
  `product_store_stock`, a per-store "has stock" flag used only for ordering. It is refreshed
  right after every admin inventory write, and by a full pass every `KEEL_STOCK_FLAG_INTERVAL`
  (default 1m) that catches order deductions and restocks. The `in_stock` shown on each item
  is still read live, so display is always current; ordering can lag by at most one pass.
- **Product list showed an add button on out-of-stock items.** `GET /products` never read
  inventory, so `in_stock` was always absent and the buyer app could not tell. The list now
  asks the inventory service once per page for the resolved store and fills `in_stock` (absent
  only when the inventory service is down, as with search). Product cards grey out the cover
  with a centred 「无货」, dim the price, and replace the 「＋」 with a 「无货」 tag.
- **Disabling a region had no effect on its stores**, and a closed store could still take
  orders. `regions.status` was never read. A disabled region now takes all its stores offline:
  they drop out of fence resolution, `GET /stores` and the default-store fallback, cannot be
  made the default, and `POST /orders`, `/orders/preview` and `/coupons/applicable` answer
  409 `store-unavailable` for them — as they now also do for a store that is itself closed.
  The admin region dialog warns how many stores are affected before disabling.
- **Fence editor appended to a closed polygon.** Once a fence was closed, every map click still
  appended a vertex, wiring it between the last and first points. The editor now has a drawing
  state (polyline; click the first vertex or 「闭合」 to close) and a closed state (drag
  vertices, click or drag edge midpoints to insert, right-click or select + Delete to remove),
  with full undo.
- **Zero-payable orders could not be paid.** A coupon or promotion that brings the payable
  amount to 0 left the order stuck in pending (the payment webhook rightly rejects
  `amount_cents <= 0`) until the timeout sweep closed it and released the coupon. The SAGA's
  finish branch now settles such orders in the same transaction that marks them placed:
  status 20, `paid_cents = 0`, no `payments` row, coupon consumed, "order paid" notification
  sent. `POST /orders` returns them as status 20; paying again is 409 and refunds report
  nothing refundable.
- **Single-character search returned nothing** (「杯」, 「咖」). The keyword index only held
  CJK bigrams, so a one-character query never matched a lexeme. `search_text` now appends each
  distinct ideograph after the bigrams (`search.IndexTerms`); bigram positions are unchanged, so
  multi-character recall and `ts_rank_cd` ordering are unaffected. The search-text fingerprint
  has its own version (`SearchTextVersion = "bigram-v2"`), and migration 00086 rewinds
  `product_understanding.updated_at` so the indexing job re-judges every product: `search_text`
  is rewritten, embeddings are not recomputed. Stacks without `KEEL_EMBED_ENDPOINT` keep their
  old `search_text` until reseeded.
- **The seeded flash-price promotion had no effect on fresh installs.** Since 00075 the quota
  lives only in `activity_stocks`. That migration's backfill runs before the seed on a new
  database, so the seed's "挂耳咖啡限时特价" had no quota row, and pricing skipped it. The seed
  now writes the quota row, and `scripts/smoke.sh` checks that the promotion price is in effect.
- **Cart lines and order lines show the product image.** When a SKU has no image of its own,
  `GET /cart` items and new order lines fall back to the product's main image (order lines
  snapshot it at order time, so existing orders are unchanged).
- **Buyer API returns product images.** `GET /products`, `POST /search` and
  `GET /products/{id}` now fill `image_url` with the main image (the `product_images` row
  with the lowest `sort_order`, as managed by `PUT /admin/products/{id}/images`), and the
  detail's `images` lists every image URL in display order. Both are omitted — not empty —
  for products without images. URLs are `/api/v1/uploads/{id}`, readable anonymously.
  No migration.

## [0.2.0] - 2026-09-27

Migrations `00027`–`00038`, `00053`–`00065`. The database lands on `00065`.


### Added

- **Buyer app: adding to cart from product lists.** Product cards on the home list and
  in search results have a "+" button: single-spec products go straight into the cart,
  multi-spec products open a spec/quantity sheet; either way the list stays put, a toast
  confirms, and the cart tab badge updates (it counts items, not lines). The product
  detail page gains a cart button with a badge, and "已加入购物车，去结算 ›" is tappable.
- **Buyer app: sessions renew themselves.** A 401 while logged in triggers one shared
  `POST /auth/refresh` (single-flight, because refresh tokens rotate) and replays the
  waiting requests; only when refresh fails is the session cleared and the login page
  opened ("登录已过期，请重新登录"), returning to the previous page after login.
- **Password login lockout**: five wrong passwords for the same phone in one shop within
  15 minutes lock that phone for 15 minutes (`429 rate-limited` with `Retry-After`),
  counted per phone rather than per IP; unknown phones are locked the same way.
- **Relative inventory adjustments** (`POST /admin/stores/{store_id}/skus/{sku_id}/inventory/adjustments`,
  plus the single-store shortcut `POST /admin/skus/{sku_id}/inventory/adjustments`;
  migration `00063`). The body is just `delta` (non-zero, |delta| ≤ 1,000,000) and an
  optional `reason` (≤ 200 characters). Restocking 100 units no longer means
  read-then-compare-and-set-then-retry-on-409: the server adds the delta to the live
  value in one conditional statement, so concurrent restocks and concurrent order
  deductions all land. A missing inventory row counts as 0 — a positive delta creates
  it, even when many requests race to be first. Going below zero is `409
  inventory-insufficient` with the current stock in `current` (unlike the CAS `409`,
  retrying as-is will not help). Because a delta is not naturally idempotent,
  `Idempotency-Key` is required; a replay returns the first result without applying
  it twice. Every adjustment writes an `inventory_logs` row (`biz_type = 5`, before /
  after, `biz_id = adj:<staff id>:<key>`, and the new `reason` column) in the same
  transaction. Staff adjustments do not raise low-stock notifications, same as the
  CAS endpoint. The admin console's inventory dialog now defaults to "add / subtract",
  keeping "set to" for stock-take results and the warning threshold.
- **`KEEL_TRUSTED_PROXIES`**: a comma-separated list of reverse-proxy IPs / CIDRs.
  The `/search` and `/search/events` rate limits now key on the real client IP —
  `X-Forwarded-For` / `X-Real-IP` are honoured only on connections from a listed
  proxy (gin's "trust everyone" default is always replaced). Unset means trust no
  one, which is the old behaviour; before this, every visitor behind a reverse proxy
  shared the proxy's single bucket. An invalid entry refuses to start.
- **Bulk product import** (`/admin/product-imports`, migration `00054`). Download an
  xlsx or csv template, upload it for a **dry-run preview** that writes nothing —
  per-row errors (required cells, prices parsed as decimal strings with no
  floating point, duplicate SKU codes within the file and against the shop,
  multi-spec rows merged by title), prohibited-claim hits located to the code
  point, and a category decision per product — then **confirm** by re-sending the
  same file with the chosen categories. The confirm step re-validates everything,
  runs in one transaction, creates drafts only, and is idempotent twice over: the
  `Idempotency-Key`, and a per-shop unique index on the file's sha256 so the same
  file is never imported twice. Limits are 5 MB / 2000 rows. Excel pitfalls are
  handled explicitly: formulas are rejected, merged cells are filled down (merged
  headers are rejected), cells Excel turned into dates are flagged, and GBK-encoded
  csv is read. Image URLs are recorded, not downloaded (server-side fetching is an
  SSRF surface that needs its own allow-list work). New dependency:
  `github.com/xuri/excelize/v2`.
- **Category suggestions from embeddings.** When the category column is empty or
  does not match, the preview ranks leaf categories by cosine between the
  title+subtitle embedding and each category path's embedding, and returns the top
  three. It auto-selects only when the top score is at least 0.50 *and* beats the
  runner-up by 0.03; otherwise it asks a human. With no inference engine (or when it
  is down) the preview degrades to "pick manually" instead of failing. Offline
  evaluation (123 hand-labelled titles over 41 leaf categories, Qwen3-Embedding-0.6B
  on infero): Top-1 83.7%, Top-3 95.9%, 96.7% precision on the 74% it
  auto-selects. The set lives in `internal/understanding/testdata/category_eval/`;
  `make category-eval` reruns it against a live engine.
- **Admin console: "Bulk import" page** — template download, preview table with
  error rows in red and prohibited words highlighted, editable category per
  product, confirm, and a result page listing created drafts and skipped rows.
- **Business re-ranking in `POST /search`** (semantic search design §6). After
  RRF fusion and before truncating to `size`, every candidate's score is
  multiplied by a business factor — multiplicative decay, not an additive
  penalty. Only the stock factor exists so far: out-of-stock products get
  `0.05`, which is small enough to sink the best out-of-stock candidate below
  the worst in-stock one across the whole recall window, while keeping relevance
  order among out-of-stock items. The promotion and quality factors have no data
  to read (promotions landed later in this release but are not wired into
  search yet, and there are no reviews); freshness and sales are
  deliberately left out until reranking gives scores a real dynamic range and an
  offline evaluation set exists; margin weighting is not implemented, per the
  design's own advice to keep it off.
- **Two named strategies.** The default is now `rrf-biz-v1` (hybrid recall +
  RRF + business re-ranking). `rrf-v1`, the previous pipeline without business
  re-ranking, can still be requested explicitly and does exactly what its name
  says, so the two can be compared. Any other value still falls back to the
  default without an error, and the response echoes the strategy that ran.
- **`search_logs`** (data model §8, migration `00027`). Every successful search
  writes one row: the query, the resolved strategy, the stages that actually ran
  (a new `stages` column — a degraded keyword-only search no longer hides under
  the same strategy as a hybrid one), the embedding model and version, the fused
  candidate ids and the returned ids, server-side latency and a random 128-bit
  `trace_id`. A failed log write is reported as an `ERROR` and never fails the
  search. Tenant-isolated with `ENABLE` + `FORCE` row-level security.
- **Search feedback: `/search` returns `trace_id`, and `POST /search/events`
  is implemented** (semantic search design §9.2). Clients send
  `{trace_id, event, product_id}` with `event` one of `click` / `add_cart` /
  `order`; the server fills `clicked_id` / `carted_id` / `ordered_id` on that
  search's log row. Public like `/search`. `product_id` must be one of the
  products that search actually returned (`ranked_ids`), otherwise `422` and
  nothing is written — an open endpoint accepting any id would let anyone
  inflate a product's click-through. Each column keeps its first value; a
  repeated event returns `204` without overwriting, so retries are safe. An
  unknown `trace_id` and another shop's `trace_id` are the same `404`.
  `trace_id` is omitted from the `/search` response when the log row could not
  be written, since every event sent with it would 404. Rate-limited per IP in
  its own bucket, separate from `/search` (default 36/s, burst 36 — three times
  the search quota, one per behaviour column); override with
  `KEEL_SEARCH_EVENT_RATE_PER_SEC` / `KEEL_SEARCH_EVENT_RATE_BURST`.
- **`make search-metrics`** prints zero-result rate, CTR@10, search→add-to-cart
  rate, search→order rate and the mean reciprocal rank of the first click, per
  shop × strategy × stages that ran, over the last `PERIOD` (default `7 days`).
  The query lives in `scripts/search_metrics.sql` and is tested against a
  hand-computed dataset. Needs an admin connection (the table has RLS).
- **Orders carry the name of the coupon they were placed with** (`coupon_name` on
  `Order`, so on the order detail, the order list and the create response). It
  is a snapshot taken by the same statement that writes `user_coupon_id`: renaming
  the coupon template later does not change what past orders show, the same rule
  as the item title and store snapshots. The field is absent when no coupon was
  used; a `CHECK` keeps the two columns present or absent together. Migration
  `00029` backfills existing orders from the template's name at migration time —
  the name at order time was never recorded, so an order placed before a rename
  that happened before this migration shows the newer name. Coupons shipped the
  same day as 0.1.0, so that window is hours wide.

- **A staff member whose session expired can be let back in without being
  deleted and re-created.** `POST /admin/staff/{staff_id}/login-token` issues a
  fresh one-time login token — the same kind a new staff member gets (15 minutes,
  single use, exchanged at `POST /admin/auth/session`) — and revokes that
  person's earlier unused ones, so calling it twice leaves exactly one valid
  token (which is why it takes no `Idempotency-Key`: replaying an archived
  response would put the token in the database). Until now a 7-day session was
  the end of the road: the e-mail link answers `501` because there is no mail
  service, and the only other one-time token was the one printed when the account
  was created. Who may issue for whom is exactly who may edit whom
  (`PATCH /admin/staff/{staff_id}`); disabled staff get `409 staff-disabled`.
  **The token is returned in the response body** as well as logged — without a
  mail service the admin has to hand it over some other way. The issuer can
  therefore log in once as that person; they could already change that person's
  role and status. The back office's staff page has a button for it.
- **Buyer address book** (`/addresses`, six operations). At most one default
  address per buyer, enforced by the partial unique index *and* serialized on
  the server (the buyer's `users` row is locked while the default is switched),
  so two concurrent "set as default" taps never surface a 500. `PUT` replaces
  every field except `is_default`; switching the default goes through
  `PUT /addresses/{id}/default`, as the contract says. Deletion is a soft delete
  and never touches existing orders, which hold a snapshot. `POST` is idempotent.
- **Shopping cart** (`/cart`, seven operations) on two new tables
  (`carts`, `cart_items`, migration 00031). Line prices come from the very same
  query `/orders/preview` uses, so for a given store the cart's selected total
  equals the preview's goods amount to the cent. Delisted, not-sold-here,
  out-of-stock and short lines stay in the cart and are flagged with a `status`
  instead of being dropped. Adding past 999 is a 422 `cart-quantity-exceeded`,
  never a silent truncation. `POST /cart/items` and the batch delete are
  idempotent.
- **Profile** (`GET`/`PATCH /me`, `GET /me/identities`,
  `DELETE /me/identities/{provider}` with the last-credential guard).
  `POST /me/identities/wechat` and `POST /me/phone` answer **501** with an
  explicit reason: they need WeChat Open Platform and an SMS provider, neither
  of which this project integrates yet.
- Other buyers' addresses and cart lines are **404, not 403**, for every
  operation — within a shop and across shops — and every one of the 19 new
  routes requires a buyer token (both checked by tests).
- **In-app notifications** (data model §16, migration `00053`). Key order and
  after-sales state changes now notify someone: payment received, shipped (with
  carrier and tracking number), auto-confirm due within a day, auto-confirmed,
  closed for non-payment, refund approved / rejected (with the reason), refund
  paid out — for the buyer; new paid order to ship, new refund to review, return
  shipped back, stock dropping to the warning line — for the store. Titles and
  bodies are rendered on the server (Chinese); clients display them verbatim.
- **Written in the same transaction as the state change (outbox).** The
  `notifications` row *is* the in-app delivery, and a `notification.deliver` job
  for outbound channels is enqueued in the same transaction on the existing
  `jobs` queue — a state change that commits always has its notification, a
  rolled-back one never leaves one behind (tested with commit-time failing
  triggers on both sides). A `dedupe_key` unique index keeps one event to one
  notification.
- **Buyer message center**: `GET /me/notifications` (paged, `unread_only`),
  `GET /me/notifications/unread-count`, `POST /me/notifications/{id}/read`,
  `POST /me/notifications/read-all`. Buyers only ever see their own.
- **Console to-do bell**: the same four operations under `/admin/notifications`,
  narrowed to the caller's store scope with the same rule as the order list,
  read state kept per staff member (`notification_reads`). A bell in the console
  top bar shows the unread count and a drop-down that jumps to the order, refund
  or store stock.
- **Pluggable outbound channels** (WeChat subscribe messages, SMS, e-mail) behind
  a `NotificationChannel` interface. All three default to "not configured": every
  attempt is recorded as skipped in `notification_deliveries`, nothing errors or
  retries. Failures back off and retry only the channel that failed; exhausted
  jobs go to the dead-letter state. A draft integration guide is in
  `docs/指南/消息通知外发渠道接入.md`.
- **90-day retention**: expired notifications are deleted per tenant in bounded
  batches; reads and delivery records cascade.
- **A source-level test enumerates every state transition** — every statement in
  `db/queries` that changes an order status, touches a refund or moves stock is
  traced to each service call site, which must either send a named notification
  or state why it deliberately does not (buyers' own actions, SAGA
  create/compensate, amount-mismatch callbacks, manual stock edits…). Every edge
  of both state machines must be accounted for too.
- **Promotions** (data model §7·二): tiered spend or quantity
  discounts (amount-off and percent-off), limited-time prices, flash sales and
  new-buyer gifts. The pricing order is fixed and shared with the shipping-fee
  work: store price → limited-time / flash price (`min(store price, special
  price)`, so a special never costs more than the store's own price) → tiered
  discounts, allocated per line with the coupon's remainder rule → coupon →
  shipping. A line takes part in at most one tiered promotion; overlapping
  promotions are resolved greedily, biggest discount first. One implementation
  (`service/promotion_calc.go`) serves preview, checkout, the applicable-coupon
  list, the cart and product tags, so they cannot disagree.
- **Promotion details everywhere a buyer looks**: `promotion_tags` on products,
  `promo_price_cents` on SKUs, activity prices, `promotion_discount_cents` and
  `promotions` (applied discounts and "spend ¥X more" hints) on the cart,
  `/orders/preview` and orders; orders keep a snapshot of the promotions they
  hit. Order lines record the store price (`list_price_cents`), the promotion
  that set the price, and the promotion share of the discount.
- **Flash-sale quotas and per-buyer limits** are enforced with conditional
  updates in the same transaction as the stock deduction (the SAGA stock
  branch), released by compensation, timeout close and buyer cancel. A quota is
  a cap on units sold at the flash price, not separate stock. `409
  promotion-sold-out` and `409 promotion-limit-exceeded`.
- **New-buyer gifts** grant a coupon from a chosen template when a buyer who has
  never placed an order signs in (there is no sign-up endpoint), reusing
  targeted grants; one per buyer per promotion. Such coupons have `source = 3`.
- **Console promotions page and `/admin/promotions`** (list, create, detail,
  PATCH, online/offline), same permission row as coupons. Promotions are created
  offline; a live promotion can only be renamed or taken offline. The demo seed
  ships a store-wide tiered discount and a limited-time price with rolling dates.

- **Business reports** (`GET /admin/reports/overview`, `/trend`, `/products`,
  `/stores`, `/inventory-alerts`, `/search`; read-only, no AI). The overview
  gives paid amount, refunds, net sales (paid minus refunded), paid orders,
  paying buyers, average order value and refund rate for the window and for the
  immediately preceding window of the same length (today compares with the same
  hours of yesterday). Sales are attributed by payment time, refunds by the time
  the money actually went back (status `40`); drafts, unpaid and closed orders
  never count, fully refunded orders still count as paid. Days and hours are cut
  in the shop's time zone (shop settings' `timezone`, falling back to
  `Asia/Shanghai`, echoed in the response); `last_7_days` / `last_30_days` are
  complete days excluding today; a custom window is capped at 366 days, since
  every report aggregates the raw rows on the spot. Scope follows the order list:
  region and store managers only see their own stores' numbers; the search
  summary (query counts, zero-result rate, top and zero-result queries) is for
  admins and operators only, because search logs have no store dimension. Four
  partial / covering indexes back the windows (migration `00057`); measured on
  1.3M orders and 1M search logs, a 30-day overview is ~20 ms and a 366-day one
  under 200 ms. No materialized views or rollup tables.
- **CSV export of the product ranking and the store comparison**
  (`GET /admin/reports/products.csv`, `/admin/reports/stores.csv`). Same parameters,
  same scope and the same service call as the JSON reports — the file is exactly the
  table on screen. UTF-8 with a BOM (so Chinese Excel opens it without mojibake),
  CRLF, RFC 4180 quoting, amounts in yuan with two decimals, and text cells starting
  with `=` `+` `-` `@` prefixed with `'` so a product title cannot become a formula in
  someone's spreadsheet. The file name carries the window's dates. The dashboard's
  ranking and store-comparison cards gain an "Export" button.
- **Shop settings** (`GET` / `PUT /admin/shop-settings`, migration `00059`; admins
  only, including a platform admin switched in with `X-Keel-Merchant`): time zone
  (validated as a loadable IANA name — `Local`, empty strings and abbreviations are
  rejected; reports switch on the next request), automatic delivery-confirmation days
  (1–365), return-shipment deadline days (1–365, new) and a customer-service phone.
  The shop name is echoed read-only (the merchant directory stays platform-managed).
  `PUT` replaces the whole document and is naturally idempotent. The editable half
  lives in a new tenant table, `shop_preferences`; `timezone` and `auto_confirm_days`
  **moved** there from `shop_settings` (not copied), because granting the app role
  `UPDATE` on `shop_settings` — which has no row-level security, since tenant
  resolution reads it — would let one tenant rewrite another's custom domain or
  payment-callback secret. The back office gains a "Shop settings" page.
- **`OrderDetail.auto_confirm_at`** (shipped orders only): shipped time plus the
  shop's current auto-confirm days, so clients no longer guess seven days.
  **`Refund.return_deadline_at`** (return-and-refund requests waiting for the buyer
  to ship): audit time plus the shop's return-shipment deadline.
- **Returns not shipped in time are closed automatically** (`20 → 60`, the edge the
  refund state machine always had). A background job — same per-tenant fair scheduler
  as auto-confirmation, every ten minutes — closes return-and-refund requests whose
  approval (`audited_at`) is older than the shop's return-shipment deadline (default 7
  days) and that still have no return tracking number. Requests with a tracking number
  are never closed. Closing runs the same order-side wrap-up as a buyer withdrawal
  (in-flight quantities are released, a whole-order refund returns the order to paid)
  and notifies the buyer with the new `refund_return_expired` kind. The check is
  repeated under the order and refund row locks, in the same lock order as submitting
  a tracking number, so a buyer who ships at the last second is never closed. Partial
  index `idx_refunds_return_due` (migration `00060`).
- **Orphaned uploads are cleaned up** (data model §13's 24-hour rule finally has an
  executor). Uploads that are not referenced and older than 24 hours — unsubmitted
  refund evidence, unused product images, replaced avatars — are deleted, row first
  and then the stored file, hourly, per tenant. The delete re-checks `NOT referenced`
  under the row lock, so it cannot race a refund that is marking the same evidence as
  referenced: whichever takes the lock first wins, and the loser either skips the file
  or fails the refund with the same `422` as a missing upload.
- **The back office opens on a business-overview dashboard**: metric cards with
  period-over-period change and the definition of each metric on hover, a trend
  line (hourly for a single day, daily otherwise) with a crosshair tooltip, top
  products by revenue or quantity with a category filter, store / region
  comparison bars, low-stock alerts and the search summary. The charts are
  hand-written SVG — no chart library, zero bundle-size increase.

### Added — shipping fees and free-shipping coupons (migrations 00055–00056)

- **Shipping-fee templates** (data model §7). A template is a charge mode (per
  piece, or by weight using each SKU's `weight_gram`), a set of rules keyed by
  province-level division code — first unit + fee, each additional unit + fee,
  free over an amount and/or a quantity — plus a list of undeliverable
  provinces. Exactly one rule is the "everywhere else" default; a province may
  appear in only one rule. Templates are either shop-wide (products can be
  pinned to one; one of them can be the shop default) or per store (at most one
  per store). A line uses the product's pinned template, else its fulfilling
  store's template, else the shop default, else ships free (`no_template`).
  Lines on different templates are priced separately and summed.
- **Admin API**: `GET/POST /admin/freight-templates`,
  `GET/PUT/DELETE /admin/freight-templates/{template_id}` (`POST` needs an
  `Idempotency-Key`; `PUT` replaces the whole template; `DELETE` is a soft
  delete refused with `409 freight-template-in-use` while products are pinned to
  it). Shop-wide templates are writable by admins and operators; store templates
  follow the store-price rule (region managers for their regions, store managers
  for their own store). `AdminProduct.freight_template_id` pins a product.
- **Freight is priced by `POST /orders/preview` and written by `POST /orders`**,
  always in this order: list price → promotions → coupon (threshold judged after
  promotions) → shipping (free-over-amount judged on the goods total **after**
  discounts) → free-shipping coupon. Orders store `freight_cents`, the new
  `freight_discount_cents`, and a `freight` snapshot of the rules used, so later
  template edits never rewrite history. The amount identity
  `payable = goods + freight − discount` is unchanged (`discount` includes the
  freight a coupon covered) and a new `chk_freight_discount` keeps the coupon
  from covering more than the freight.
- **Undeliverable addresses are refused per line**: preview and order return
  `422 region-not-deliverable` with `undeliverable_items`
  (`sku_id`, `reason_code`, `reason`).
- **Free-shipping coupons (`coupon_type = 4`) can be created.** They cover the
  freight, capped by `max_discount_cents` (0 means all of it), never below zero,
  and are not applicable to an order whose freight is already zero.
- **Cart shows estimated freight**: every cart operation returning a `Cart`
  accepts an optional `address_id` (default: the buyer's default address) and
  returns `freight`, `address_id` and a per-line `undeliverable` marker.
- **After-sales refunds use the freight actually paid** (`freight_cents −
  freight_discount_cents`) both for the full refund of an unshipped order and as
  the cap for return-freight decided at audit.
- **Console**: a new "运费模板" page (province picker, per-rule free-shipping
  conditions, undeliverable provinces), a template picker on the product page,
  and the free-shipping option in the coupon dialog.
- **Demo seed** gives the `demo` shop a default template (¥8 first piece, ¥2 each
  additional, free over ¥99; remote provinces ¥15 + ¥5 with no free shipping;
  Hong Kong, Macao and Taiwan undeliverable).

### Added — order fulfillment (migration 00033)

- **Buyer cancellation** (`POST /orders/{order_no}/cancel`): closes a pending
  order and, in the same local transaction, returns its stock to the store it
  was deducted from and unlocks its coupon. It reuses the exact release path of
  the order-timeout sweep, so the two can never drift; stock movements are
  logged with a new `biz_type = 6` so "changed their mind" and "forgot to pay"
  stay distinguishable.
- **Shipping** (`POST /admin/orders/{order_no}/shipments`) and **delivery
  confirmation** (`POST /orders/{order_no}/confirm`). Whole-order shipment only;
  shipping never touches stock, is refused while a whole-order refund is pending,
  and a duplicated tracking number is a `409`, not a second parcel. Who may ship
  follows the store-inventory row of the role matrix: admins and operators
  anywhere, region and store managers only within their scope.
- **The order state machine is now enforced by the database.** A trigger checks
  every `orders.status` change against `order_status_transitions` and rejects
  anything else with `23514 order_status_transition`; a new
  `chk_fulfillment_timestamps` ties `paid_at` / `shipped_at` / `finished_at` to
  the states that imply them. Service-level conditional updates remain the first
  line and map to the contract's `409`s.
- All three write endpoints honour `Idempotency-Key` in a single transaction
  (claim → business → archive), shared with the admin write path.
- **Automatic delivery confirmation** (migration 00036). A background job moves
  orders that have been shipped for the shop's `auto_confirm_days` days
  (default 7 — the column existed since 00001, guarded to 1–365; it lives in
  `shop_preferences` since 00059) from `30` to `40`, through the very same
  conditional update as the buyer's own confirmation. It runs like the order
  timeout sweep — per tenant, with the same fairness scheduler, now shared as
  `fairRound` — every ten minutes. Orders with an open refund (`10`/`20`/`30`)
  are paused rather than confirmed, re-checked under the order row lock that
  refund requests also take; once the refund ends they are confirmed on the
  next round. A shop that never changed its settings falls back to the column
  default.

### Added — refunds and after-sales (migration 00034)

- **Refund requests with partial refunds that add up to the cent**
  (`POST /orders/{order_no}/refunds`). The client never sends an amount: each
  line refunds `floor(net × k / quantity)` of its net amount — the line total
  minus the coupon discount allocated to it at checkout — and the last unit takes
  the remainder, so a fully refunded line always refunds exactly what was paid
  for it. In-flight over-refunds (which no `CHECK` can catch) are prevented by
  re-checking under an order row lock and allowing at most one in-flight refund
  per order line.
- **Refund lifecycle**: buyer withdrawal (`POST /refunds/{refund_no}/cancel`),
  back-office audit (`POST /admin/refunds/{refund_no}/audit`) and a new
  **confirm-returned-goods** step (`POST /admin/refunds/{refund_no}/receipt`) —
  the contract's state machine always had the `20 → 30` edge but no endpoint
  walked it. Buyer reads: `GET /refunds`, `GET /refunds/{refund_no}`,
  `GET /orders/{order_no}/refunds`; order detail now carries `refunds` and
  per-line `refunding_qty`.
- **Refund webhook** (`POST /webhooks/refunds/{channel}`), isomorphic to the
  payment webhook: per-tenant HMAC signature (a shop without a secret is always
  rejected), amount check, and idempotency on the channel refund id via
  `uk_refunds_channel_txn`. Settlement writes back order lines, order totals and
  `refund_status` in one local transaction; an unshipped whole-order refund moves
  the order `50 → 60`. Stock is restocked only if the order was never shipped;
  the coupon is returned only once every line is fully refunded (and not expired).
- **Sandbox refunds** follow the payment sandbox switch (`KEEL_PAYMENT_SANDBOX`):
  when a refund enters "refunding", the server plays the channel inside the same
  transaction — builds and signs a canonical callback and runs it through the
  exact webhook settlement path.
- The refund state machine is enforced by a trigger over
  `refund_status_transitions` (`23514 refund_status_transition`), and
  `chk_refund_state` ties "refunded" to the channel refund id and timestamp.
- Who may audit or confirm receipt follows the same store-scoped rule as
  shipping.
- **Return shipment** (`POST /refunds/{refund_no}/return-shipment`, migration
  00037). While a return-and-refund waits for the goods (`20`), the buyer enters
  the carrier and tracking number of the parcel sent back; the refund stays at
  `20` until the merchant confirms receipt, and the entry can be corrected until
  then. `Refund.return_shipment` carries it on buyer and admin views alike, and
  the console's refund drawer shows it. `chk_refund_return_shipment` keeps the
  three columns all-or-nothing and on return-and-refunds only. Receipt does not
  require it (goods may come back in person).
- **Buyer uploads** (`POST /uploads`) for avatars (`2`) and refund evidence
  (`3`), with the same storage, size and media-type limits and the same
  store-bytes-then-claim-key idempotency as `POST /admin/uploads`; product
  images (`1`) are refused with a `422` and stay on the admin path.
- **Refund evidence is private, as the contract always said.**
  `GET /uploads/{upload_id}` now takes an optional buyer token: evidence is
  readable only by its uploader with their own token (a bad token is a `401`,
  never a silent downgrade to anonymous). Staff read files through the new
  `GET /admin/uploads/{upload_id}`, where evidence is authorized through the
  refunds that reference it (same store-scoped rule as the refund detail) and
  evidence referenced by no refund is readable by nobody. Time-limited links
  for private files are signed in a separate domain the second hop requires
  (migration 00038 indexes `refunds.evidence_urls` for that lookup). The
  console loads evidence images through the admin endpoint.
- **Back-office order and refund lists** (migration 00035):
  `GET /admin/orders` (status, store, created-at range, exact order number or
  phone — receiver's or the buyer account's), `GET /admin/orders/{order_no}`
  (lines with discount allocation and refunded / in-flight quantities, payments,
  shipments, refunds), `GET /admin/refunds` (status, store, date range) and
  `GET /admin/refunds/{refund_no}` (lines, evidence, audit trail, order summary).
  Same store-scoped rule as shipping: lists only return what is in the caller's
  scope (filters intersect with it, so an out-of-scope `store_id` is an empty
  page), details outside it are `403 out-of-scope`. A region is resolved from the
  store's *current* region, soft-deleted stores included. A malformed or inverted
  date range is a `422`, not a silently unfiltered page. New indexes back the
  merchant-wide, by-status and by-receiver-phone paths.
- **Refund audit trail**: `refunds.audited_by`, `received_at` and `received_by`
  record who approved / rejected a refund and who confirmed the returned goods
  (single-column staff foreign keys, like `shipments.created_by`). Exposed on the
  admin views only; refunds audited before 00035 show timestamps without a name.
- **Console: orders and after-sales pages.** The orders page (previously a text
  placeholder) lists orders with filters and pagination, opens a detail drawer
  and ships paid orders with an `Idempotency-Key`. A new after-sales page lists
  refunds and lets staff approve or reject pending ones (return freight editable
  for return-and-refund only) and confirm returned goods. Per-line refund amounts
  are shown as computed by the server and are never editable. Buttons are greyed
  out by role; the server stays authoritative.

### Changed

- **Setting inventory with `PUT .../inventory` now leaves an `inventory_logs` row**
  (`biz_type = 5`, `biz_id = set:<staff_id>:<random>`) when the quantity actually changes,
  so every manual stock change is traceable — not only relative adjustments. A successful
  compare-and-set proves the old value was `expected_available_qty`, so no extra read is
  needed. Changing only the warning threshold writes nothing.
- **`products.total_stock` is no longer read or written by any code** (it had been
  always 0 since `00019`); `AdminProduct.total_stock` in the API was already computed from
  `inventories` and is unchanged. The column itself stays for one release (migration
  `00062` is intentionally a no-op) so that old app instances still running during a rolling
  deploy keep working; it will be dropped in a later migration.
- **Coupon thresholds and percentages now apply to the post-promotion amount**
  of each line (`amount_cents − promotion_discount_cents`), not the store price.
  Without promotions nothing changes. A promotion can be marked as not stackable
  with coupons; orders hitting it get `409 coupon-not-applicable` when a coupon
  is supplied, and an empty `applicable_coupons`.
- **`discount_cents` on orders and order lines now means promotions + coupon.**
  The promotion share is in `promotion_discount_cents`. Refunds keep using
  `amount_cents − discount_cents`, so fully refunding a line still returns
  exactly what was paid for it. The database constraint that tied any discount
  to a coupon now requires a coupon-less order's discount to equal its promotion
  discount.
- **`OrderPreview.items` is a named schema (`OrderPreviewItem`)** with
  `price_cents`, `list_price_cents`, `price_promotion_id` and
  `promotion_discount_cents`; the fields it had are now required.
- **Contract (breaking for generated clients):** `OrderPreview.freight_cents`,
  `freight_discount_cents` and `freight` are now required — freight used to be
  absent ("not computed"), it is now always computed, `0` with
  `free_reason = no_template` when the shop has no template. `Order` gains
  `freight_discount_cents`; `OrderDetail` / `AdminOrderDetail` gain `freight`;
  `Problem` gains `undeliverable_items`; the five `Cart` operations take
  `address_id`; `CouponApplicableRequest` takes an optional `address_id`
  (free-shipping coupons are only listed when it is given). `POST
  /orders/preview` now looks up `address_id` and answers `422` for an unknown
  address, as `POST /orders` always did. Regenerate your client.
- **`explain: true` lists exactly the stages that ran.** `scores.business`
  appears when business re-ranking ran; `scores.vector` is absent when the
  vector route did not run (engine down or not configured); `scores.final` is
  the score the ordering actually used. `scores.rerank` never appears —
  cross-encoder reranking is still not implemented, because the inference
  engine has no `/v1/rerank` endpoint yet.
- **`filters.in_stock_only` now defaults to `false`, as the contract has said
  since M3.** The handler had kept defaulting to `true`, which silently turned
  the contract's "out-of-stock products are demoted" into "out-of-stock products
  are dropped". Out-of-stock products now appear, ranked last; pass
  `in_stock_only: true` to drop them.
- When the caller is outside every store's service area, `strategy` now echoes
  the resolved strategy instead of the raw request string.
- **Contract (breaking for generated clients):** every cart operation that
  returns a `Cart` takes an optional `store_id` query parameter;
  `Cart.store` (`StoreContext`) and `CartItem.status` (`CartItemStatus`) are
  now required; `CartItem.price_cents` is nullable (null for lines that are
  off the shelf or not sold at that store); `CartItem.available` is required
  and always equals `status == available`. Regenerate your client.
- **Contract (breaking for generated clients):** `POST /search/events` is now
  `security: []`, no longer takes an `Idempotency-Key` (it is naturally
  idempotent, and a public endpoint usually has no `user_id` to scope a key to),
  and `product_id` is required. `trace_id` on the `/search` response documents
  its shape (`^[0-9a-f]{32}$`) and when it is absent.
- Migration 00030 gives `user_addresses.merchant_id` a
  `DEFAULT current_merchant()`, now that the application writes that table.
- **Contract (breaking for clients): `PATCH /me` only accepts the buyer's own avatar
  upload in `avatar_url`.** It must be exactly an `Upload.url` from `POST /uploads`
  with `purpose = 2` by the same buyer; external links (including WeChat avatar URLs),
  someone else's upload, refund evidence and query-string variants are a `422` naming
  `avatar_url`. An empty string still clears the avatar. The new avatar is marked
  referenced in the same transaction; the previous one, if it was the buyer's upload,
  is unmarked and removed by orphan cleanup after 24 hours. Avatars stored before this
  change are kept as they are until the buyer changes them. Clients must upload first
  (`POST /uploads`, `purpose=2`) and then send the returned `url`.
- **`shop_settings.timezone` and `shop_settings.auto_confirm_days` moved to
  `shop_preferences`** (migration `00059`, values carried over). Anything that wrote
  those columns directly — seed scripts, manual SQL — must write `shop_preferences`
  instead.
- **Product ranking is faster on large shops.** The query now bounds order lines by
  the range of order ids in the window (order ids grow with payment time, so a 30-day
  window scans the last slice of order lines instead of all of them) and aggregates per
  (product, order) first so the order count no longer needs a `count(DISTINCT)` sort;
  `idx_order_items_order` became a covering index with the same key (migration `00061`).
  On 1.3M orders: 7 days 108 → 61 ms, 30 days 557 → 244 ms, 90 days 1321 → 842 ms;
  the 365-day window is about 12% slower (3.2 → 3.6 s). The index is about three times
  larger. Results are identical to the old query across 72 parameter combinations.
- **Contract: `RefundCreateRequest.evidence_urls` only accepts the buyer's own
  refund-evidence uploads** — each entry must be exactly an `Upload.url` from
  `POST /uploads` with `purpose = 3` by the same buyer, no duplicates; anything
  else (external links, someone else's file, product images) is a `422`. The
  files are marked referenced in the refund's transaction, so orphan cleanup can
  never delete them.

### Fixed

- Buyer app product cards (home, category and search results now share one component):
  long price ranges no longer push over the "+" button — the price shrinks, then
  truncates, and grid cards show "¥9.90 起" for multi-spec products; titles clamp at two
  lines and cards in a row line up. The product page's bottom bar gives its hint its own
  line (it wrapped into four lines on an iPhone) and keeps the cart badge off the label.
- **Concurrent refreshes with the same refresh token all succeeded**, rotating the session
  once per request so every client but the last held an already-dead token (found by
  firing 5 concurrent refreshes through 3 instances: 5 × 200). Rotation now requires the
  old token hash, so exactly one succeeds and the rest get 401.
- **The login lockout counted per process**: behind 3 instances a phone locked only after
  15 failures, and any restart unlocked everyone. Counts now live in Postgres
  (`login_failures`, migration `00065`), shared by all instances and kept across restarts.
  `KEEL_LOGIN_LOCK_EXEMPT_PHONES` exempts phones whose password is public (demo buyers).
- **Product list and search were unusably slow on large catalogs**: `skus` had no index on
  `product_id` (the data model declared `idx_skus_product`, no migration created it), so
  every per-product price/stock lookup scanned all of a shop's SKUs — about 55 s per page
  at 20k products. Migration `00064` adds it, a listing index matching the list's sort
  (104 ms → 0.15 ms), and the two category indexes the data model also declared.
- **Closing an order could restore stock that was never deducted.** Between the order
  being moved to "pending payment" and the stock branch finishing (a retried branch),
  a buyer cancel or timeout close restored every line unconditionally. Closing now restores
  only what the order's inventory log shows as still deducted, and the stock branch locks
  the order row and refuses an order that is already closed.
- Refund restocking takes inventory row locks in `sku_id` order, like every other stock
  path, so it can no longer deadlock against an order deduction.
- Migration `00063` adds its CHECK as `NOT VALID` (no full scan of `inventory_logs` under
  an exclusive lock), and `00062` no longer drops `products.total_stock` in this release
  (old app instances still read it during a rolling deploy).
- Coupon "not applicable" reasons shown to buyers used fen and UTC timestamps
  ("还差 1000 分", "2026-09-30T16:00:00Z 才开始可用"); they now read "还差 ¥10" and
  Beijing time.
- Buyer app: coupon end dates showed the exclusive end instant's date (a coupon ending
  "10-07 00:00" read "至 10-07"); discount rates were rounded to one decimal ("9.95折"
  showed as "10折"); the order detail address dropped the street; zero "已付" / "优惠合计"
  rows were shown on unpaid orders.
- Buyer app: placing an order now opens the order for payment directly, and in sandbox
  mode the bottom button becomes "模拟支付完成（沙箱）" after starting payment — before, the
  result and the settle button were below the fold and nothing visibly happened.
- Back office: coupon validity and promotion date ranges picked by day now end at
  23:59:59 on the last day instead of 00:00 (which silently dropped that day).
- **Buyer app times were 8 hours early** (order, payment, refund, coupon, notification
  times and the new auto-confirm / return deadlines). The API sends UTC RFC3339 as the
  contract says; the app sliced the string instead of parsing it, so a Beijing buyer saw
  UTC clock time — and coupon validity dates could be off by a day. Times are now parsed
  as instants (fractional seconds normalised to milliseconds first) and shown in the
  device's local time zone. The back office already parsed them; reports already cut days
  in the shop's time zone.

- **`PATCH /admin/products/{product_id}` with `"brand_id": null` now clears the
  brand.** `encoding/json` turns a JSON `null` into a nil pointer without calling
  `UnmarshalJSON`, so an explicit `null` was indistinguishable from an omitted
  field and was ignored. Presence is now read from the raw object (the same fix
  covers the new `freight_template_id`).
- **Referencing an upload a second time no longer fails.** `MarkUploadReferenced`
  only matched rows not yet referenced, so re-using an image (the same picture on
  a second SKU, the same evidence on a re-submitted refund) affected zero rows and
  was reported as "upload not found".
- **Shipping, refund audit and "return received" no longer fail with a 500 for
  a region manager when the order's store has been soft-deleted.** The store-scope
  check looked the store up among live stores only and the resulting not-found
  was never mapped; order operations now resolve the store's region including
  soft-deleted stores, the same rule the new admin order lists use.
- **The embedded coordinator's storage is fully closed when `Close` returns.**
  Bumped dtmrs to v0.11.1. Under v0.11.0, `dtmrs_close` dropped its runtime
  before its SQLite connection pool and never closed the pool, so SQLite's
  per-connection threads were still checkpointing and deleting `-wal` / `-shm`
  after the call had returned — anything that removed or reopened the data
  directory right after shutdown could race them (it showed up as a flaky
  `directory not empty` in `TestCloseIsIdempotent`). The test helper that used
  to wait for those threads now asserts instead: after `Close`, only `dtm.db`
  may remain. Against v0.11.0 that assertion fails 600 times out of 600; against
  v0.11.1, 0.
- **Opening a shop (`POST /admin/merchants`) and adding staff (`POST /admin/staff`)
  are now idempotent**, closing the first item under 0.1.0's Known gaps. Sending
  the same `Idempotency-Key` with the same body replays the original `201` with
  `Idempotency-Replayed: true` — no second shop, no second staff member, and no
  second one-time login link. The same key with a different body is a `422`
  `idempotency-key-reused`, as on every other back-office write. Adding staff is
  idempotent for both kinds of caller (a platform admin adding platform
  operators, a shop admin adding shop staff), so the endpoint has one meaning.
  A request without the header is now rejected with `422`, as the contract
  always required.

  The fix is the one the Known gaps entry described: `idempotency_keys.merchant_id`
  became nullable (NULL = a platform-scope record) and its row-level-security
  policy became the one `staff` already uses —
  `merchant_id IS NOT DISTINCT FROM staff_scope_merchant()`, for both reading and
  writing. Inside a shop's scope that is row-for-row the old policy, so buyers
  and shop staff cannot read or write platform records; inside the platform
  scope only the NULL rows are visible, so a platform session cannot pick up a
  shop's record either. The privilege-escalation shape
  (`... OR merchant_id IS NULL`) is exactly what the tenancy gate's verbatim
  policy check rejects. Migration `00028`; the reasoning for not using a
  separate platform table is in its header.
- **Uploaded files did not survive a container rebuild under the stock
  `compose.yaml`.** `KEEL_UPLOAD_ROOT` was never set, so the API fell back to a
  temporary directory inside the container (it logged a WARN saying exactly
  that). Recreating the `app` container wiped every product image while the
  `uploads` rows still pointed at them. Uploads now live on their own named
  volume, `uploads`, mounted at `/var/lib/keel-uploads` (created and owned by
  the non-root user in the image). Existing deployments: images uploaded before
  this change are already gone after the next recreate and must be re-uploaded.
- The 0.1.0 notes list the cart among the M2 features, but the server never
  routed any `/cart` operation (they returned 404 "接口不存在"). It does now.

### Not yet

- Group buying (needs its own state machine for forming groups and refunding
  failed ones), repeating "every ¥100 off ¥10" discounts, and a promotion
  performance report are not implemented. Flash-sale quotas and per-buyer
  limits are not returned on refunds.
- Shipping-fee regions stop at the province level (no city or county rules),
  and lines on different templates are summed rather than merged the way a
  single parcel would be. Who pays return freight is still decided by staff at
  audit.
- The shop's customer-service phone is stored and editable but not yet shown to
  buyers anywhere. Replaced product images and withdrawn refunds' evidence still stay
  referenced forever — only avatars are unmarked on replacement. If deleting a file
  fails after its upload row was removed, the file is left on disk (logged as an error).
- No outbound notification channel is wired up (WeChat subscribe messages, SMS
  and e-mail all need credentials this project does not have); buyers have no
  notification preferences yet.
- The second-search rate (semantic search design §9.2) still cannot be computed:
  `/search` is public and `search_logs.session_id` is always `NULL`, so there is
  no way to tell that two searches came from the same person.

## [0.1.0] - 2026-09-26

The first release. It closes milestone M4: a merchant can open a shop, publish
products, run it as a chain of regions and stores with their own prices and
stock, and sell with coupons — from a back office, not just from `curl`.
Everything below is covered by the gates described in `CONTRIBUTING.md`.

### Added — foundation (M1)

- **Multi-tenant on a single database.** Row-level security with both `ENABLE`
  and `FORCE`, a `STABLE current_merchant()`, and `SET LOCAL` inside every
  transaction. Tenant resolution runs before the security context exists, which
  is why the tenant directory tables are protected by the grant surface rather
  than by RLS.
- **A machine-checked tenancy manifest.** `db/tenancy.json` classifies every
  table into one of seven classes and is read by two independent gates — a Go
  one against the live catalog (`pg_class`) and a Python one against the design
  document. A table that exists in neither, or is classified in a way its actual
  DDL contradicts, fails the build.
- **OpenAPI 3.1 as the single source of truth**, with three generated clients
  (Go, TypeScript, UTS) committed to the tree and a drift gate that fails when
  they disagree with the spec.
- **`docker compose up` brings up the whole stack**, seeded, in one command.

### Added — transactions (M2)

- **Product catalog, cart, orders and sandbox payments** end to end.
- **An embedded distributed transaction coordinator.** `dtmrs` (Rust) is linked
  into the process over its C ABI; order placement runs as a SAGA with
  in-process branches. The sub-transaction barrier table is owned by this
  repository's own migration and is written through `repository.WithSagaBranch`,
  which never hands a `pgx.Tx` to a caller.
- **Order state machine and inventory deduction** with the compensation path
  covered by tests, including the case where a barrier insert affects zero rows.

### Added — semantic search (M3)

- **Hybrid retrieval**: pgvector HNSW over text embeddings fused with bigram
  keyword recall by Reciprocal Rank Fusion.
- **The HNSW post-filter trap is handled explicitly.** Under a tenant predicate
  the planner will happily choose a plan that filters after the index scan and
  silently returns fewer rows than asked for; `hnsw.iterative_scan`,
  `max_scan_tuples` and `ef_search` are therefore set unconditionally on every
  tenant transaction rather than tuned per query.
- **Staleness is decided by a fingerprint, not a timestamp.** Per-capability
  input hashes live in `product_understanding.input_hashes`, so changing a price
  does not re-embed anything and renaming a category re-embeds exactly the
  products whose text actually contains it.

### Added — merchant self-service and chain stores (M4)

- **Back-office identity**: email-link sessions, platform-level and
  merchant-level scopes, and a `staff` table whose nullable `merchant_id` is
  guarded by `IS NOT DISTINCT FROM staff_scope_merchant()` rather than an
  `OR merchant_id IS NULL` predicate that would double as a privilege-escalation
  hole on the write side.
- **16 catalog write operations** — products, SKUs, categories, images,
  publication, inventory compare-and-set, uploads.
- **Build information**: `GET /version` and a startup log line reporting
  version, commit, build date and Go version.
- **Stores, regions and geofencing** (migration 00020, 23 operations). A
  merchant groups stores into regions; each store carries a
  `GEOGRAPHY(POLYGON, 4326)` fence. `GET /stores/resolve` answers "which store
  serves this buyer": inside a fence it returns every match ordered by spherical
  distance, outside every fence — **or with no coordinate at all, which is the
  same code path** — it falls back to the merchant's single default store, and a
  merchant with no default store returns `match_type = none` with an empty array
  and **HTTP 200**, because "we do not deliver there" is a normal query result
  and not an error.
- **Three-layer pricing**: SKU base price, region override, store override. The
  `COALESCE` lives in exactly one place, the `sku_prices_by_store` view, and a
  gate (`scripts/check_query_tenancy.py`) fails the build if any query in
  `db/queries/` touches the two override tables directly. Product lists,
  product detail, search results and order pricing all read through that one
  view, so the price shown in a list cannot diverge from the price charged.
- **Two-layer visibility**: `region_product_overrides` and
  `store_product_overrides` are both **exclusion** tables — a missing row means
  "on sale", so a newly opened store sells everything from day one. The two
  layers combine with AND, not OR: what a region delists, a store cannot
  relist.
- **Inventory is per store.** `inventories` is keyed on `(sku_id, store_id)`;
  `orders` carries `store_id`, `region_id` and a display snapshot of the store
  taken at checkout. A missing inventory row means **zero available**, not
  "this store does not sell it" — the distinction is what keeps a new store
  from appearing to sell nothing.
- **Views are `security_invoker`.** A new gate asserts it for every non-extension
  view in `public`. PostgreSQL evaluates a view with the owner's privileges by
  default, so the underlying RLS policies are checked against the owner:
  measured with two merchants, one store and one SKU each, the correct answer is
  4 rows and a default view returns 5. Not an error, not an empty set — one
  extra row belonging to someone else.
- **Advertising-law term screening before a product goes public.** Publishing
  (`POST /admin/products/{id}/publication`) and editing the copy of an
  already-published product both run a synchronous check over `title`,
  `subtitle` and `description`. A hit is a `422` whose `errors[]` carries the
  offending field and the **offset and length in Unicode code points**, because
  "rejected" on its own does not tell a merchant what to change. Drafts are not
  screened: nothing a buyer can see has changed yet.
- **A queue that stays fair across tenants.** The job table enforces a
  per-tenant in-flight cap, so one merchant importing a catalogue cannot occupy
  the whole worker pool. `priority` alone does not achieve this — it only
  orders jobs *within* a tenant.
- **A merchant admin console** (`web/admin/`, Vue 3 + Vite + Element Plus) that
  comes up with the same `docker compose up`, on port 8081, behind an nginx that
  reverse-proxies `/api` to the API — same origin, no CORS. There is not one
  hand-written request or response type in it: every shape comes from the
  committed contract artifact `web/src/api/schema.d.ts`, and it reuses the
  committed TypeScript SDK rather than growing a second client. A gate
  (`make admin-type-check`, wired into `scripts/check-all.sh`) compiles every
  `.ts` and `.vue` under `--strict` and asserts the contract artifact is really
  in scope; renaming a contract field makes it fail immediately, which was
  verified by mutation rather than assumed. The console surfaces the three
  things the API deliberately designed for: the inventory compare-and-set `409`
  shows the server's `current` and offers a one-click retry with it, the
  advertising-law `422` highlights the offending characters in the copy using
  the `errors[]` code-point offsets, and every idempotent POST carries an
  `Idempotency-Key` that is held across a retry of the same submission and
  rotated once the server has definitively rejected it. The orders page states
  plainly that no admin order-list operation exists rather than drawing a fake
  table.
- **Merchant management for platform operators.** `GET /admin/merchants`
  (including disabled shops, so there is a way to re-enable them),
  `GET/PATCH /admin/merchants/{id}` for renaming and disabling / enabling, and an
  `X-Keel-Merchant` header that lets a **platform-level** session choose which
  shop a request manages. The header is read only after the staff session has
  been verified; a merchant-level session sending it gets `403
  tenant-switch-forbidden` rather than having it silently ignored, an unknown
  code is `422 unknown-merchant` rather than a fallback to the Host's shop, and
  buyer and public endpoints never read it. Renames and status changes do not
  grant the application role `UPDATE` on `merchants`: they append to
  `merchant_revisions` (migration 00024), whose insert policy only admits the
  platform scope, so a tenant-scoped transaction cannot disable another shop
  even through a bug. A single-merchant deployment (`KEEL_DEFAULT_MERCHANT`)
  refuses to open a second shop with `409 single-merchant-mode` instead of
  creating one that would make the next start fail its preflight check. The
  admin console gains a merchant list and a "currently managing" switcher with
  a banner on every page.
- **Regions and stores in the admin console** — all 21 admin operations of the
  contract's Store tag. Delivery fences are drawn with Leaflet on OpenStreetMap
  tiles, which are WGS-84 like the `GEOGRAPHY(POLYGON, 4326)` column, so the
  vertices clicked are the vertices stored; coordinates pasted from Chinese map
  providers (GCJ-02, BD-09) are converted only when the operator says that is
  where they came from, and that conversion has tests (`make admin-test`, wired
  into `scripts/check-all.sh`). A fence PostGIS rejects shows its
  `ST_IsValidReason` verbatim and circles the reported point on the map. `409`s
  are handled by `type`, not status: the old single-store inventory endpoint's
  `store-ambiguous` now routes the operator to a per-store stock page instead
  of reading as a compare-and-set conflict to retry. A missing default store is
  flagged on every page, a non-default store without a fence is shown as
  "incomplete", and a product a region has delisted says so on the store's
  page instead of looking like a switch that does nothing.

- **Coupons** (migration 00026, data model §7). Fixed-amount-over-threshold,
  percentage (with an optional cap) and no-threshold coupons, scoped by
  category (descendants included, the same semantics as the product list's
  `category_id` filter), product, brand, **region and store** — exclusions win
  over inclusions. Buyers claim from a coupon centre (total and per-buyer limits
  are enforced by a conditional `UPDATE` on the template row, so forty people
  racing for the last five get exactly five — there is a test that does
  exactly that); merchants grant by phone number, all-or-nothing per batch.
  The calculation exists **once**, as a pure function fed the rows priced at the
  ordering store's effective price, and quote, order and "coupons usable on this
  cart" all call it. Percentage discounts round **down** to the cent, and
  allocation onto order lines never gives a line more than its own amount.
  Using a coupon is a branch of the order saga with a compensation: the coupon
  is locked when the order is placed, marked used in the same transaction as the
  payment callback, and released when stock deduction fails or the unpaid order
  times out. A coupon the order cannot use is a `409`, never a silently ignored
  field. The admin console has a coupon page (templates, a scope picker,
  claim toggle, grant dialog, issuance and redemption counts); for now only
  merchant admins and operators may use it.

- **Tiered back-office permissions.** Two new roles on top of merchant admin
  and operator: **region manager** and **store manager**, each scoped to one or
  more regions or stores (`staff_scopes`, migration 00025). Region managers run
  their regions' prices, listings and stores; store managers run their own
  stores' prices, listings and inventory; product master data and base prices
  stay merchant-wide. Authorization lives in one place
  (`internal/service/authz.go`) and every admin write calls exactly one check.
  The executor is a matrix test — every `/admin/` route × every role × in and out
  of scope, 408 cells — that also fails the build when a new admin route is
  registered without a row in it. Nobody can change their own role or scope.
- **Buyer-side categories**: `GET /categories` returns the enabled category tree
  (a disabled category hides its whole subtree), and `GET /products?category_id=`
  filters by a category **including its descendants**. An unknown category is an
  empty list, not every product.
- **Buyer app** (`app/`, uni-app x): resolves the serving store once from the
  device location (WGS-84; denied location falls back to the default store) and
  uses the same store for browsing, search and checkout, so the price in a list
  is the price charged; category chips on the home page; a coupon centre, *My
  coupons*, coupon selection at checkout, and the coupon on the order page.
  Verified on an Android device; iOS is compiled but was not run on a device for
  this release.

### Fixed

- **A deployment could lock itself out of the back office for good.** The
  one-time bootstrap token is printed once and valid for 24 hours; if it expired
  unused, the unredeemed placeholder admin still counted as "an admin exists", so
  no new token was ever issued — and e-mail login answers 501 without SMTP. A
  restart now re-issues a token for the same placeholder when nobody has ever
  redeemed one and none is still live.
- **Order detail dropped the coupon.** `GET /orders/{order_no}` omitted
  `user_coupon_id` although the contract declares it. The test meant to catch
  this compared three hand-picked fields; it now compares every field of `Order`
  by reflection.
- **An invalid geofence reported its reason in the wrong field.** PostGIS's
  `ST_IsValidReason` was sent as the problem `title`; it is now in `detail`, as
  the contract says, and `title` is fixed per problem type.
- **The database image was built on an end-of-life OS.** `keel-postgres:16` was
  based on Debian 11, whose support ended in June 2026, and its older glibc made
  PostgreSQL report a collation mismatch on volumes created by the previous image
  — the warning that precedes silently wrong text-index lookups. It is now built
  on `postgres:16-bookworm` with PostGIS and pgvector from PGDG.

### Changed

- **Embeddings now run on [infero](https://github.com/jackwangfeng/infero)**, a
  self-hosted Rust inference engine, replacing a throwaway Python service:
  7.6 ms per text against 62 ms, and a 1 second cold start against 75.
- `products.min_price_cents` / `max_price_cents` were **removed** (migration
  00019) and are computed on read. With per-region and per-store pricing, "what
  does this product cost" depends on which store is serving the request, and a
  scalar on the product row cannot answer that.

### Known gaps

Listed because a changelog that only lists wins is an advertisement.

- **Two platform-level operations are not idempotent: opening a shop
  (`POST /admin/merchants`) and adding staff (`POST /admin/staff`).** Every other
  back-office write replays the first response when the same `Idempotency-Key` is
  sent twice. These two answer the retry with `409` instead. Nothing is created
  twice — unique constraints on the shop code and the e-mail hold — but a client
  that retried after a dropped response sees a conflict where it should see the
  original result. The cause is structural: idempotency records are keyed by
  tenant, and a platform-level session has none. Fixing it means a nullable tenant
  column plus a matching change to its row-level-security policy — the same shape
  that, on the `staff` table, took deliberate care to keep from becoming a
  privilege-escalation path — so it was deferred to M5 rather than rushed before
  the first release. `TestPlatformScopedWritesAreNotYetIdempotent` asserts today's
  `409` and will go red when the fix lands.

- **Free-shipping coupons are rejected.** There is no freight in this system yet
  (`orders.freight_cents` is always 0), so a free-shipping coupon would always
  take off nothing while telling the buyer it had been applied. The type is
  reserved in the enum; a `CHECK` constraint and the admin API refuse to create
  one until freight exists. Coupon stacking (`stackable` / `priority`) is modelled
  but not enabled — one coupon per order. A used coupon has no way back to
  unused yet, because refunds have not landed.

- **No linter.** There is no golangci-lint configuration in the repository.
- **`trace_id` appears nowhere in business code**, despite structured logging
  being in place everywhere.
- **The three inference acceptance criteria only run by hand.** They need a GPU,
  and the CI runner now has one, so this is no longer "impossible" — it is
  "not written yet". The reasoning and the constraint (the job must connect to
  the engine already running on the host, never start a second copy) are in the
  header of `.github/workflows/ci.yml`.
- **Geofence overlap and gaps are not validated.** Two stores may have identical
  fences (that is intentional — the nearest one wins) and nothing detects a
  region of the map that no fence covers. Such a buyer silently falls back to
  the default store, which is correct behaviour and also indistinguishable from
  a misconfiguration.
- **`GET /products?in_stock_only=` is still ignored.** Inventory is per store
  now, but that list query does not join `inventories`. The debt is registered
  in `internal/handler/contract_test.go`, which fails if the parameter
  disappears from the contract or starts being read without the entry going
  away.
- **No hosted demo and no documentation site.**
- **The advertising-law screen is a word list, with no model fallback for
  variants.** The design calls for "rule list plus a small model to catch
  variants". Only the first half exists. The three variants named in the design
  ("蕞", "No.1", "巅峰之作") are caught because they are *enumerated in the
  list* — anything not enumerated (character splitting, homophones, a term
  broken up by punctuation) passes. The second half is not simply unfinished:
  the fast path has a 200 ms budget, and under the "a check that cannot answer
  rejects the publish" rule, putting a generative call there would turn "engine
  busy" into "merchant cannot ship". It needs a design that does not sit on the
  synchronous path.
- **Category-licence screening is not implemented, because the data does not
  exist.** No table records merchant qualifications, so there is nothing to
  judge against. This is why medical and health claims are deliberately absent
  from the word list: whether "for treating hypertension" is lawful depends on
  the category and the licence, and listing those terms without the licence data
  would permanently block a legitimate blood-pressure-monitor shop with no way out.
- **Compliance verdicts are not persisted.** `product_understanding` carries an
  `updated_at` touch trigger that doubles as the indexing watermark, so writing
  a verdict there would silently drop a freshly published product out of the
  index queue. The consequence is accepted and stated: the back office cannot
  show why a product was rejected last time; the merchant only ever saw it in
  that one response.

[Unreleased]: https://github.com/jackwangfeng/keel/compare/v0.7.0...HEAD
[0.7.0]: https://github.com/jackwangfeng/keel/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/jackwangfeng/keel/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/jackwangfeng/keel/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/jackwangfeng/keel/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/jackwangfeng/keel/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/jackwangfeng/keel/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/jackwangfeng/keel/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/jackwangfeng/keel/releases/tag/v0.1.0
