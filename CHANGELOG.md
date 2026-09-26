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
  in `internal/api/`, `web/src/api/` and `app/src/api/` stay compatible.
- `0.x.y` → `0.(x+1).0` — may add, rename or remove operations in
  `docs/电商系统-OpenAPI.yaml`. Regenerate your client.
- Database migrations are **forward-only**. Every release states which migration
  number it lands on; downgrading is not supported and `goose down` past a
  release boundary is not tested.

A release's version is compiled into the binary and served at `GET /version`,
so "which one is running?" never depends on anyone's memory.

---

## [Unreleased]

Migrations `00027`–`00035`.

### Added

- **Business re-ranking in `POST /search`** (semantic search design §6). After
  RRF fusion and before truncating to `size`, every candidate's score is
  multiplied by a business factor — multiplicative decay, not an additive
  penalty. Only the stock factor exists so far: out-of-stock products get
  `0.05`, which is small enough to sink the best out-of-stock candidate below
  the worst in-stock one across the whole recall window, while keeping relevance
  order among out-of-stock items. The promotion and quality factors have no data
  to read (there is no promotions table and no reviews); freshness and sales are
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
- Migration 00030 gives `user_addresses.merchant_id` a
  `DEFAULT current_merchant()`, now that the application writes that table.

### Fixed

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

- `trace_id` is generated and stored but still not returned by `/search`: its
  only consumer, `POST /search/events`, is not implemented.

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

[Unreleased]: https://github.com/jackwangfeng/keel/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/jackwangfeng/keel/releases/tag/v0.1.0
