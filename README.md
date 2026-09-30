<div align="center">

# Keel

**Location-first commerce with geofenced stores, AI staff, and an open interface for any agent harness.**
*Built-in distributed transactions. Runs on a single machine, scales without a rewrite.*

Open-source multi-tenant e-commerce · multi-store O2O and local delivery · geofence-based store routing · Go + PostgreSQL + Flutter + Vue 3

<!-- The badge and the clone URL in the quick start point at the same repository.
     scripts/check_promises.py guards two things: a build badge is a false claim when
     .github/workflows/ does not exist; and every GitHub URL pointing at this repository
     must agree — either all still placeholders, or all filled in with the same owner.
     Replacing only half is both the easiest mistake to make and the hardest to spot. -->
[![CI](https://github.com/jackwangfeng/keel/actions/workflows/ci.yml/badge.svg)](https://github.com/jackwangfeng/keel/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](./LICENSE)
![Go](https://img.shields.io/badge/Go-1.26+-00ADD8)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-16+-336791)

[Documentation](./docs/README.md) (Chinese) · [Changelog](./CHANGELOG.md) · [Security](./SECURITY.md) · [中文文档](./README.zh-CN.md)

</div>

---

## Why another commerce platform?

There are already good open-source commerce systems — Saleor, Medusa, Shopware, mall.
Keel is not trying to be the fiftieth. Three things are what it is about, and three more
are the foundation that makes them safe:

### 1. Location-first: every store is a geofence

Keel is built for shops that deliver from physical stores — convenience chains, fresh
groceries, pharmacies, a coffee brand with twenty branches. A store is a point on the map
plus a **delivery fence** (a polygon drawn on the map in the console), and the whole flow
is decided by where the buyer is:

- **Open the app, get the right store.** `GET /stores/resolve?lat&lng` returns every open
  store whose fence contains the buyer, nearest first; outside every fence, the default
  store takes over as the nationwide fallback. Fences may overlap (shared trading areas) —
  the server never silently picks one for you.
- **Per-store everything.** Stock, visibility and price are per store (base → region →
  store), and a sold-out item sorts to the end *of that store's* list.
- **Orders must ship inside the fence.** A shipping address with coordinates outside the
  chosen store's fence is rejected at preview and at checkout (`422 address-out-of-range`),
  with the client offering "change address" or "switch to the store that covers it".
- **The console keeps the map honest.** A store must have coordinates, and must sit inside
  its own fence; disabling a region takes every store in it offline.
- **Same-city delivery, not express shipping.** A fenced store charges delivery by distance tiers (straight-line
  from the store to the address), with a minimum order and free delivery over an amount — the cart shows
  "¥x more to reach the minimum" before checkout. Freight templates (by province) stay for the default store's
  nationwide express shipping; a product's express template can no longer override a store's delivery fee.
- **POI, not typing.** `/geo/reverse` and `/geo/suggest` proxy a map provider (AMap today)
  on the server: the home page shows "deliver to …" with the street address, buyers pick an
  address by searching or dropping a pin, and the console fills a store's address from a
  place search. The key never leaves the server; everything is stored in WGS-84 (GCJ-02 is
  converted in one place, with tests against known points). No key configured → 501, and
  clients fall back to manual entry plus map picking.

### 2. AI staff: an agent on the team, not a chatbot on the page

Keel does not bundle a model. It gives **your** agent a job: a staff account with the same
role and store/region scope as a human, MCP tools to read the business and compute, and a
**proposal queue** — every write is a proposal with evidence and expected impact, and a
human approves before Keel executes it (idempotently, as the agent, fully audited).

This is running on the demo site today (M9–M11, shipped): every morning, and whenever stock runs low or a
buyer files an after-sales request, an AI store manager
walks the shop, posts a daily brief (sales vs. the day before, anomalies worth a second
look, what is about to sell out) and files restock proposals computed by Keel — daily
sales with stock-out days removed from the denominator, days of cover, suggested quantity,
a confidence flag. Approve one in the console and the stock goes up; reject it with a
reason and the agent reads that reason next time.

Most platforms put their AI on the demand side (search, recommendations, chat). **Almost
nobody applies it to replenishment, pricing, promotions or after-sales** — where merchants
spend their hours. The hard part is not wiring up a model, it is being willing to hand
over the keys. What makes that safe here is infrastructure Keel already had: idempotent
write APIs, tiered roles and scopes (an agent that oversteps is rejected exactly like a
human), row-level security, preview endpoints, distributed transactions and audit.
Plan: [AI Operations: Plan](./docs/AI经营-规划.md); M9 design:
[AI Operations M9 Design](./docs/AI经营-M9设计.md) (Chinese).

### 3. Open to any agent harness

The AI interface is a documented, versioned contract, so you bring the agent:
Claude Code, Codex, Cursor, the official MCP SDKs in a twenty-line script, or the bot behind
your Feishu / DingTalk / WeCom group. Keel speaks **MCP over streamable HTTP**, stateless,
one `kagt_…` access key per agent:

```json
{ "mcpServers": { "keel": { "type": "http", "url": "https://<your-shop>/api/v1/mcp",
    "headers": { "Authorization": "Bearer ${KEEL_AGENT_KEY}" } } } }
```

Every tool declares its input **and output** JSON Schema (validated before returning);
tool errors carry a machine-readable problem type identical to the admin API's; the full
tool list is snapshotted in the repo and a test holds it — changes are additive only, a
breaking change gets a new tool name. stdio-only clients use the bundled `cmd/keel-mcp`
bridge. Integrator guide: [AI Interface](./docs/AI接口.md) (Chinese); tool list with
schemas: [`docs/AI接口-工具清单.json`](./docs/AI接口-工具清单.json).

### 4. A distributed transaction engine, built in

Transactional consistency is handled by [dtmrs](https://github.com/jackwangfeng/dtmrs) —
a Rust transaction coordinator supporting SAGA, TCC, two-phase messaging, XA and workflow.

Checkout deducts stock, redeems a coupon and creates an order across three branches.
If any step fails, the rest are compensated — correctly, idempotently, and with
sub-transaction barriers that survive process crashes.

Most open-source commerce projects either avoid the problem (one big local transaction)
or bolt on an external coordinator. Keel treats it as a first-class concern.

### 5. One codebase, from a laptop to a cluster

Thanks to dtmrs's embeddable coordinator, transaction orchestration code is
**identical** whether branches are in-process function calls or remote services.

```diff
  // Single process
- tc.saga(gid).step("local://deduct_stock", "local://restore_stock").submit()
  // Microservices — the only line that changes
+ tc.saga(gid).step("http://inventory/deduct", "http://inventory/restore").submit()
```

Traditional systems force a choice: a monolith that's easy to deploy but hard to scale,
or microservices that scale but are a deployment nightmare. Keel doesn't require choosing.

This is running code, not a slogan: inventory already ships as a separate service. See
[Deployment shapes](#deployment-shapes-monolith-and-microservices).

### 6. One deployment, many merchants

Each merchant gets their own storefront, and orders never span merchants.

**This does not cost you the single-machine story.** A small shop runs
`docker compose up` and gets a deployment with exactly one tenant; nothing about
multi-tenancy is visible to them, and opening a second shop later does not mean
changing architecture.

Cross-tenant isolation does not rest on remembering a `WHERE` clause — that kind of
bug is invisible against single-tenant test data. It rests on every table carrying
`merchant_id`, parent-child rows pinned by composite foreign keys, and PostgreSQL
row-level security underneath. All three are checked mechanically: the
`merchant_id` rule against **the DDL in the design doc**, the other two against
**the live database's catalog** in the test suite.

### 7. Search and product understanding with local inference

| Stage | Capability |
|---|---|
| **Demand** | Hybrid vector + keyword search, fused with RRF, then business re-ranking (out-of-stock demotion) |
| **Supply** | Category suggestions from title embeddings during bulk import (Top-3 95.9% offline) · block prohibited advertising claims before publish |

**No external API calls. No data leaving your network. No per-token billing.**
Search's vector inference runs on our own [infero](https://github.com/jackwangfeng/infero)
engine (details under "Not in the box yet" below). Other platforms' semantic search is
either a wrapper around a third-party API, or only available on their hosted cloud —
Medusa's semantic search, for instance, runs on Medusa Cloud. If you self-host, you are
back to wiring up Algolia yourself. Keel assumes you want to own your stack.

---

## Live demo

**<https://eshop.zzss.fun>** — open to everyone, no sign-up or login needed.

| Path | What you get |
|---|---|
| [`/`](https://eshop.zzss.fun/) | Buyer app (Flutter Web build of `flutter_app/`) — allow location, or pick an address, to see the geofenced store |
| [`/admin/`](https://eshop.zzss.fun/admin/) | Merchant console, signed in as the demo merchant's admin — see **AI 员工** for the AI store manager's briefs, proposals, their measured outcomes and its scorecard (it walks the shop every morning and wakes on events, on simulated orders) |
| [`/ai-log/`](https://eshop.zzss.fun/ai-log/) | The public AI operations log: what the AI staff proposed, what was approved or auto-executed, and how it turned out |

> This is a demo environment shared by all visitors. Data is reset from time to time without notice, and anything you enter may be seen by others — **do not enter real personal information** (names, phone numbers, addresses, payment details).

---

## Quick start

```bash
git clone https://github.com/jackwangfeng/keel && cd keel
docker compose up -d --build
./scripts/smoke.sh                        # exit 0 means the chain works
curl http://localhost:8080/api/v1/products
```

One command brings up PostgreSQL, runs the migrations, loads a seed shop and
starts the API and the merchant console. That `curl` comes back with the shop's products, and nothing in
it says which shop — the deployment has exactly one tenant, which is the
small-shop shape promised above.

If 8080 is taken, move the whole group — the four commands above read the same
variable, but the port in that `curl` is hard-coded, so don't change only the first:

```bash
export KEEL_HTTP_PORT=18080
docker compose up -d --build
./scripts/smoke.sh
curl "http://localhost:$KEEL_HTTP_PORT/api/v1/products"
```

> When something else holds 8080, the `curl` copied verbatim gets a 404 from
> **that** service. It reads like "Keel failed to start" when in fact the request
> never reached Keel.

### Merchant console

The same `docker compose up` also brings up the **merchant admin console** at
<http://localhost:8081> (Vue 3 + Element Plus, Chinese UI, served by nginx which
reverse-proxies `/api` to the API — same origin, so no CORS).

Getting in the first time needs a bootstrap token. The process mints one at
startup and **prints it to the log in the clear**; it lasts 24 hours and is
consumed on first use:

```bash
docker compose logs app | grep bootstrap_token
```

Exchange it under "first time in" on the login page and you can create
categories and products, upload images, add SKUs, set stock, publish, issue
coupons and add staff.
If 8081 is taken: `KEEL_CONSOLE_PORT=18081 docker compose up -d --build`.

> The console is **not** a separately deployed thing: it lives in the same
> compose file as the API and on the same contract — there is not one
> hand-written request/response type in it, they all come from
> `web/src/api/schema.d.ts`. Rename a field in the contract and the console's
> type check goes red on the spot (`make admin-type-check`, wired into
> `./scripts/check-all.sh`).
>
> Today it covers products, SKUs, stock, categories, uploads, **bulk product
> import** (xlsx / csv; a dry-run preview that flags every bad cell and
> prohibited claim and suggests categories from title embeddings, then a
> confirm step that creates drafts), **coupons**
> (amount-off / percent-off / no-threshold, a claim center plus targeted grants,
> scoped by category, product, region or store), **promotions** (tiered
> discounts, limited-time prices, flash-sale quotas with per-buyer limits,
> new-buyer gifts, online / offline), **staff with tiered roles**
> (admin, operator, region manager, store manager — the last two carry scopes,
> checked by the server on every call), **merchant management** (platform-level: list, open, disable / enable, and a
> "currently managing" switcher), plus **regions and stores**: per-region and per-store product
> visibility and pricing, per-store stock, and delivery fences drawn on
> OpenStreetMap (WGS-84 — the same datum as the `GEOGRAPHY(POLYGON, 4326)`
> column and the buyer app's location, with no conversion on the way), and
> **orders and after-sales**: find orders by status, store, date, order number
> or phone, ship them, review refunds (approve / reject, set the return freight)
> and confirm returned goods, plus a **to-do bell** in the top bar (new paid
> orders, refunds awaiting review, returns shipped back, low stock — narrowed to
> the staff member's store scope, read state kept per person, each item jumps
> straight to the order, refund or store stock it is about), and the **business
> overview** dashboard on the home page: net sales (paid minus refunded), orders,
> paying buyers, average order value and refund rate against the previous period,
> an hourly / daily trend line, top products, store and region comparison,
> low-stock alerts and a search summary (top queries and zero-result queries) —
> fixed definitions, days cut in the shop's time zone, scoped by role, no AI involved.

For the multi-merchant shape, where the `Host` header picks the shop:
`docker compose -f compose.yaml -f compose.multi.yaml up -d --build`.

The seed includes a buyer you can log in as: phone `13800000000`, password
`keel-demo-2026` (a development seed for local demos). Step-by-step guides —
quick start, deployment and configuration, the console manual, API conventions,
FAQ — are in [`docs/`](./docs/README.md) (Chinese). Read the deployment guide's
"must change before going live" list before serving real customers.

No Elasticsearch. No MongoDB. No RabbitMQ. No Redis.
**One database.** Vector search lives in `pgvector`, full-text in `tsvector`,
the job queue in a table.

> Files (product images, avatars, refund evidence) go to a local disk volume by
> default — not another service. Switching to the S3 driver is what adds a component.

### Not in the box yet

What `docker compose up` brings up today is PostgreSQL, the migrations plus
seed, the API and the merchant console. Buyers can browse, filter by category,
search, claim coupons, check out and pay (sandbox); merchants can list
products, run regions and stores, issue coupons, manage staff, ship orders and
handle after-sales. These parts
are not there yet, and are listed so that nothing above reads as if it ships:

- **avatar upload in the buyer app.** The server accepts it (only the buyer's own
  upload); neither client has wired the screen yet. Refund evidence, return tracking
  numbers and the auto-confirm / return deadlines are wired in both clients
- **a map provider key.** POI search and reverse geocoding (`/geo/*`) need
  `KEEL_GEO_PROVIDER=amap` and an AMap *Web service* key (`KEEL_GEO_KEY`); an individual
  developer's free quota is small and commercial use needs a verified business account.
  Without it those endpoints answer 501 and clients fall back to manual entry plus map
  picking — geofenced store resolution and the fence check at checkout work either way
- **cross-encoder reranking.** `POST /search` today is three-stage — vector
  recall and keyword recall fused with RRF, then business re-ranking
  (out-of-stock products are demoted multiplicatively below everything in
  stock). The contract's fourth stage, reranking, is missing: the inference
  engine has no `/v1/rerank` yet. Business re-ranking itself has only the stock
  factor; promotions and quality have no data to read. `explain: true` names the
  stages that actually ran, so the response never claims more than it did
- **the inference engine is not part of `docker compose up`, and it needs an
  NVIDIA GPU.** Since M4 the engine is our own
  [infero](https://github.com/jackwangfeng/infero) running
  Qwen3-Embedding-0.6B (1024-dim, exactly the `vector(1024)` already in the
  schema). It runs as a **host process outside compose**:

      ./scripts/infero-up.sh                                       # engine first
      docker compose -f compose.yaml -f compose.infero.yaml up -d   # then the stack

  Why not a compose service: infero is GPU-only and one card holds exactly one
  of them at a time, so containerising it buys only the single command and
  costs a 2–3 GB CUDA base image plus GPU passthrough. The measured reasoning
  is in the header of `scripts/infero-up.sh`.

  **Without a GPU the stack still runs, and search still answers.** Leave
  `KEEL_EMBED_ENDPOINT` unset and `/search` takes the keyword-only path
  (bigram recall plus business re-ranking): HTTP 200, no error, and
  `explain: true` reports exactly which stages ran. That degradation is a
  designed path with tests behind it, not a failure mode. What you lose is the
  semantic half — a query like "something slimming for summer" shares no
  characters with the product titles it should match, and recall drops
  visibly. The derived-data indexer does not start either; the startup log
  carries one WARN spelling that out.

  **A GPU-less deployment can now have semantic search too: run infero's CPU build.**
  infero gained a CPU backend on 2026-09-26 (`--features cpu`); the first cut was
  single-threaded and too slow for Keel. `cb0ccbd` (2026-09-29 — parallel GEMM via
  the `gemm` crate, a resident F32 weight cache, parallel attention) fixed that.
  Measured here on an idle 20-core machine: 117 ms median / 124 ms p90 for a short
  query, 359/384 ms for a 60-character query, 2.68 s median for a batch of 64 —
  all three under Keel's budgets (250 ms / 430 ms / 5 s), cosine similarity to the
  GPU build ≥ 0.9999995, same model id. Under load it slows down a lot (450–1,000 ms
  per query at load 17–30), so **the CPU shape needs cores reserved for it**
  (4–8+ dedicated cores recommended). Measurements and how to reproduce them are in
  the architecture doc (§1, "那个缺口"); build and startup steps are in
  [部署与配置](./docs/指南/部署与配置.md) ("没有 GPU" section, Chinese only for now).
  Or bring the engine up with the rest of the stack in one command:
  `docker compose -f compose.yaml -f compose.infero-cpu.yaml up -d --build`
  (builds the image locally from infero's source and its `Dockerfile.cpu`).
  This repo does not keep a second engine implementation around as a stand-in — the
  M3 Python service (BGE-M3 on CPU) has been retired; the CPU path is the same infero
  binary built with a different feature flag.
- **SMS, WeChat and e-mail.** SMS-code login, WeChat login and the console's
  e-mail login link all need an outside service that is not wired up; those
  endpoints answer 501 on purpose. Buyers log in with phone + password; staff
  get in with a one-time login token. Notifications are in the same position:
  **in-app notifications ship** (buyer message center, console bell), and the
  outbound-channel interface and delivery log for WeChat subscribe messages,
  SMS and e-mail are in place, but no real channel is wired up — every
  delivery is recorded as "not configured, skipped"
- **real payment channels.** Payments run in a sandbox whose callback path is
  the real one (signature check, amount check, de-duplication), but no WeChat
  Pay or Alipay merchant account is wired in

---

## What it does

**Commerce core**
Products & SKUs · category tree · per-store inventory · three-tier pricing
(base → region → store) · cart · address book · checkout · payments · cancel ·
shipping · confirm receipt · after-sales refunds · coupons (amount-off /
percent-off / no-threshold / free-shipping, claim center and targeted grants) ·
same-city delivery for fenced stores (distance tiers, minimum order, free over an amount) ·
shipping-fee templates (per piece or by weight, priced per province, free over
an amount or a quantity after discounts, undeliverable regions, per store or
shop-wide) · promotions (tiered spend/quantity discounts, limited-time prices, flash
sales, new-buyer gifts; allocated per line, coupons apply to the
post-promotion amount) · order state
machine · multi-store with delivery fences (store resolution by location, the
shipping address must be inside the store's fence) · POI place search and reverse
geocoding · tiered staff roles · in-app
notifications (buyer message center and console to-do bell, written in the same
transaction as the state change) · business reports (overview vs. previous
period, trend, top products, store comparison, low-stock alerts, search summary)

Cart, address book, profile, cancel, confirm-receipt, shipping and after-sales
refunds (partial refunds allocated to the cent, discounts included) are in;
the remaining gaps are under "Not in the box yet" above.

**AI-native capabilities**
- **Semantic search** — hybrid vector + keyword retrieval fused with RRF, then
  *business re-ranking* (since M5: out-of-stock demotion; the promotion and
  quality factors have no data yet). Cross-encoder reranking is designed and
  contracted but not built — semantically relevant is not the same as worth
  selling, and that distinction is the point of the last two stages. Every
  search writes a `search_logs` row (strategy, stages that actually ran, model
  version); clients report the clicks, add-to-carts and orders that follow via
  `POST /search/events`, and `make search-metrics` turns them into CTR@10,
  search→cart and search→order rates per strategy.
- **Product understanding** — what ships today is auto-classification: bulk import
  suggests categories from title embeddings (Top-3 95.9% on the offline set) and
  leaves low-confidence ones to a human. Cross-supplier duplicate detection and
  attribute extraction are not built yet — attribute extraction waits for the
  inference engine's generate endpoint; see "Later" in the roadmap.
- **Compliance checks** — catch prohibited advertising claims before publish.
- **AI staff** — shipped (M9–M11): staff accounts and `kagt_` access keys; 24 MCP tools
  (reports, inventory, search, catalog, after-sales reads; `restock_plan`, `slow_movers`,
  `promotion_review`; read-only SQL over curated views; proposals for restocks, limited-time
  discounts, coupons, product copy and after-sales decisions; events; briefs; scorecard); a
  proposal queue approved in the console, with per-kind auto-execution policies under caps;
  events by pull or signed webhook; every executed proposal reviewed after the fact;
  per-call audit; playbooks and reference runners. See "AI staff" and "Open to any agent
  harness" above.

**Correctness, taken seriously**
- Money is `BIGINT` cents. Never a float.
- Order totals enforced by a database `CHECK` constraint — a miscalculation
  is rejected at write time, not discovered during reconciliation.
- Payment callbacks made idempotent by a unique index on `(channel, txn_id)`.
- Stock deduction via conditional atomic update, backed by a `CHECK` constraint.
- Discounts allocated per line item, so partial refunds compute correctly.

---

## Clients

Keel ships with its clients, not just an API.

| Client | Stack | Targets |
|---|---|---|
| **Storefront** (main) | Flutter ([`flutter_app/`](./flutter_app)) | Android · iOS · Web · WeChat Mini Program (via mp-flutter) — one codebase; the demo site serves the Web build. The Mini Program runs in the WeChat devtools and on-device debugging; not yet published |
| **Admin console** | Vue 3 + Element Plus | Desktop web |

Every client is generated from the same OpenAPI spec, so a contract change
breaks the build rather than silently breaking production.
Design tokens (color, spacing, typography, radius) are shared across platforms;
component implementations are per-platform, because a mini program cannot render
what a browser can.

**WeChat Mini Program support is a deliberate choice.** In China most commerce
happens inside WeChat, and no major open-source commerce platform targets it.
If you are selling in that market, a web-only storefront is not a storefront.

> The WeChat Mini Program is now built from Flutter via mp-flutter
> (`make flutter-build-mp`), and runs browse, login, cart and checkout in both
> the WeChat devtools and on-device debugging; not yet published — consistent
> with the table above.

---

## Buyer apps — what's actually there today

**[`flutter_app/`](./flutter_app) is the storefront that gets new features.** Home with
the geofenced store and "deliver to …" (change it by place search, a map pin, a saved
address or the current location), category and search, product detail, cart, checkout
(best coupon picked automatically, every failure explained — undeliverable, out of the
store's fence, sold out, price changed), orders and payment (sandbox), after-sales with
evidence and return tracking, address book with place search, coupons and messages.
Its types come from the same OpenAPI spec (`make flutter-generate` →
`lib/api/schema.g.dart`, committed and checked), pages read view models rather than the
generated types (a check enforces that), and it has unit tests plus a headless Web e2e
suite. How to build and run it: [`flutter_app/README.md`](./flutter_app/README.md).

The original client, `app/`, written in
[uni-app x](https://doc.dcloud.net.cn/uni-app-x/) (UTS compiled to native
Kotlin/Swift), was retired on 2026-09-30 once the Flutter storefront covered
the same ground. Its last state, including the measured-results log, is
preserved at the `uniapp-final` git tag.

---

## Architecture

```
Clients (Web · Mini Program · App · Admin)
             │  OpenAPI 3.1 — both sides generated from one spec
      ┌──────▼───────┐
      │   Go / Gin   │  handlers: validation only, no SQL, no transactions
      └──────┬───────┘
      ┌──────▼───────────────────────────────┐
      │  Services: catalog · inventory ·      │
      │  order · payment · promotion · search │
      └───┬───────────────┬──────────────┬───┘
          │               │              │
    ┌─────▼────┐   ┌──────▼──────┐  ┌────▼─────┐
    │  dtmrs   │   │   Product   │  │ sqlc     │
    │  (Rust)  │   │Understanding│  │          │
    └─────┬────┘   └──────┬──────┘  └────┬─────┘
          │        ┌──────▼──────┐       │
          │        │  Inference  │       │
          │        │   engine    │       │
          │        └─────────────┘       │
          └───────────────┬──────────────┘
                   ┌──────▼──────┐
                   │ PostgreSQL  │  business tables + barrier + pgvector
                   └─────────────┘
```

Full details: [Architecture](./docs/电商系统-总体架构.md) ·
[Design principles and a hardening pass](./docs/架构-设计与加固.md) (Chinese) ·
[Data model](./docs/电商系统-数据模型设计.md) ·
[Search layer](./docs/电商系统-语义检索层设计.md) ·
[Product understanding](./docs/电商系统-商品理解服务设计.md)

---

## Deployment shapes: monolith and microservices

**One codebase, two ways to deploy it.** `KEEL_ROLE` decides which service a process
plays: `all` (the default, today's `docker compose up`), `core`, or `inventory`. The
monolith is not a degraded mode of the split. In both shapes inventory is read and written
only through the inventory service's interface and only touches inventory's own tables. The
interface is an in-process Go call in one shape and signed internal HTTP in the other. The
consistency protocol is the same, so the full test suite on the monolith also covers the
split's business logic. A separate two-database test suite covers the transport.

**What is split today, and why only that.** Inventory (per-store stock plus flash-sale and
limited-offer quotas) is its own service. Everything else stays in `core`:

| Candidate | Decision | Why |
|---|---|---|
| Inventory | **Split** | Clear boundary (quantities per SKU × store). Order placement's hottest write lands here, and flash sales pile onto single rows. |
| Coupons | Keep in core | Thresholds, scopes and stacking are all computed in core's pricing. A coupon service would be storage plus six cross-service calls per order, for very little load. |
| Orders / payments / refunds | Keep in core | They share one money state machine. Splitting them would only create distributed transactions. |
| Catalog and search (read-only) | Next candidate | The measured pressure is on reads: listing at 6,300 req/s used about 10 Postgres cores, versus about 5.5 cores for placing 650 orders/s (all on one 20-core box). |

**Clients don't change.** There is one public entry point and one contract (OpenAPI).
How the backend is split is invisible behind it. When a split needs the contract to say
something new, it only adds. The only addition so far is a `503 inventory-unavailable` on
the few endpoints that can't answer without stock levels. Those can only happen in a split
deployment.

**How consistency holds across the split.**

- **Placing an order** is a SAGA in core's embedded coordinator. The inventory step's address
  is `local://inventory_deduct` in the monolith and
  `http://inventory:8090/internal/v1/saga/inventory_deduct` when split. The step body is the
  same function, and a subtransaction barrier in inventory's own database makes retries and
  compensations safe.
- **Closing an order, a buyer cancelling, or a refund landing** commits core's part together with an
  outbox job (`inventory.release`), and a worker calls inventory, which is idempotent by
  order or refund number.
- **The trade-off is "under-sell, never over-sell".** After an order closes, stock looks held
  until the release lands: milliseconds normally, longer while inventory is down.
- **An order can't be paid until its SAGA has finished.** While inventory is down it waits in
  your order list, and payment answers 409 until the stock is actually deducted.

**How to run it.**

```bash
# Monolith (tier A)
docker compose up -d

# Split, one Postgres: inventory in its own schema under its own role (tier B)
docker compose -f compose.yaml -f compose.split-b.yaml up -d --build

# Split, two processes and two databases (tier C)
export KEEL_INTERNAL_SECRET=$(openssl rand -base64 48)
docker compose -f compose.yaml -f compose.split.yaml up -d --build

./scripts/smoke.sh
```

In tier B, core's database role cannot even see the `inventory` schema, so a cross-module
JOIN fails at the permission check. Moving an existing monolith to B or C means
`scripts/split-migrate.sh` (`copy` → `verify` → `cutover`, re-runnable, with `rollback`) plus
changing environment variables. No code changes. Step by step:
[deployment guide](./docs/指南/部署与配置.md).

**Availability.**

- Every role can run several instances behind one address. All state lives in the database:
  row locks, advisory locks and outbox jobs. Health checks are `/healthz` and `/readyz` on the
  internal port. Several `core` instances also need the coordinator on Postgres instead of
  the default SQLite (see the deployment guide).
- When inventory is down:
  - browsing and search still work (the in-stock flag is omitted);
  - stock-dependent endpoints return 503;
  - an order placed in the meantime finishes on the coordinator's next retry after inventory
    returns (backoff caps at 5 minutes);
  - release jobs retry until it's back.
- You don't need service discovery or a config center to start. `KEEL_INVENTORY_URL` should
  be a stable name (a DNS name, Kubernetes Service or load balancer), not an instance address,
  because in-flight SAGAs have their branch addresses persisted.
- Add discovery once you're running many services whose addresses change dynamically. The
  change is small because every address is resolved in one place (`dtm.BranchResolver` and
  `rpc.Client`), and configuration is read once at startup.
- The internal secret rotates without draining (`KEEL_INTERNAL_SECRET_PREVIOUS`).

**Splitting the next service** reuses the same machinery: roles, the signed internal client,
configurable branch addresses, the outbox, a per-service migration directory and the
two-database test harness. The work is in the boundary:

1. List every JOIN that crosses it and every local transaction that writes both sides.
2. Replace each JOIN with "fetch your own rows, batch-ask the other service, merge in Go".
3. Replace each transaction with a SAGA branch or an outbox job.
4. Drop the cross-boundary foreign keys, and let reconciliation report orphans.
5. Add a test that the two sides' query files never touch each other's tables.

The inventory split is the worked example:
[microservice split plan](./docs/电商系统-微服务拆分方案.md).

---

## Is Keel right for you?

**Use Keel if you want**
- Transactional correctness you can actually reason about
- Stores that deliver within a geofence, with the store picked by where the buyer is
- An AI staff member you can plug your own agent into, with a human approving every write
- AI features without sending your catalog to a third party
- A system that runs on one box today and splits into services later
- A modern Go/PostgreSQL stack

**Use something else if you want**
- A complete, batteries-included storefront today —
  [mall](https://github.com/macrozheng/mall) has far broader feature coverage
  and an excellent tutorial ecosystem
- A hosted SaaS — use Shopify
- A mature plugin marketplace — use Magento or WooCommerce

Keel is young. It has fewer features than the projects above and has not been
battle-tested at scale. What it has is a stronger core.

---

## Roadmap

- [x] Data model, OpenAPI contract, architecture
- [x] **M2** — Catalog → checkout → payment, orchestrated by dtmrs
- [x] **M3** — Text embeddings + hybrid search
- [x] **M4** — Merchant self-service + multi-store and regions + compliance
  checks + product-understanding skeleton + coupons + tiered roles
  → **v0.1.0, first public release**
- [x] **Transaction flow completed** — cart, address book, cancel, shipping,
  confirm and auto-confirm receipt, after-sales refunds with return tracking,
  admin orders and after-sales pages
  → **v0.2.0** (together with the finished parts of M5–M7 below)
- [ ] **M5 Measurable search quality** — business re-ranking, search logs,
  click-back events and metrics ✅; cross-encoder reranking (waiting on the
  inference engine's rerank endpoint) and an offline evaluation set to do
- [ ] **M6 Ready to open a shop** — in-app notifications and console to-dos ✅
  (written in the same transaction as the state change, outbound channels
  pluggable); shipping-fee templates and free-shipping coupons ✅; real
  payments, WeChat login and SMS codes need business qualifications and will
  be wired in once those are in hand
- [ ] **M7 Ready to do business** — promotions (tiered discounts, flash
  prices, new-buyer gifts) ✅ (group buying not done); business reports with
  export, Excel bulk import with AI category suggestions ✅
- [x] **M9 AI operations: staff can plug in** — AI staff accounts and access
  keys, an MCP service (read / compute / proposal / brief tools), a proposal
  queue with admin approval, call auditing, two playbooks, demo-site simulated
  commerce data and a scheduled AI staff run — **running on the demo site**, see
  [AI Operations: Plan](./docs/AI经营-规划.md) and
  [AI Operations M9 Design](./docs/AI经营-M9设计.md) (Chinese)
- [x] **Location & POI** — geofenced store resolution, the fence check at checkout,
  shipping addresses with coordinates, place search and reverse geocoding (AMap,
  server-side), the Flutter storefront, same-city delivery for fenced stores
  → **v0.3.0** (together with M9 above)
- [x] **M10 AI operations: staff can run the shop** — events (stock low, new
  after-sales request, zero-result search spike, proposal decided) pulled over MCP or
  pushed by signed webhook; proposals for limited-time discounts, coupons, product
  copy and after-sales decisions; `slow_movers` and `promotion_review`; automatic
  before/after review of every executed proposal and a scorecard; five new playbooks
- [x] **M11 AI operations: staff can be trusted with more** — per-kind auto-execution
  policies with caps, a read-only SQL tool over curated views, a public "AI operations
  log" page, the interface re-tested with the official Python SDK (Gemini CLI and
  opencode connect; see the compatibility record in the AI Interface doc)
  → **v0.5.0**
- [x] **Hardening** — AI operations accepted end to end on the demo site (all five proposal kinds executed,
  events, webhooks, auto-execution, reviews); three rounds of destructive testing fixed: over-collected payments
  (duplicates, payments after cancellation, amount mismatches) are refunded automatically, unit prices are capped,
  line-by-line refunds of unshipped orders no longer skip the freight, the auto-execution cap holds under
  concurrency; a search relevance floor (queries for things the shop doesn't sell no longer come back padded);
  an after-sale window; stock in checkout previews; brief corrections
  → **v0.6.0**
- [ ] **M8 Visual search** — image embeddings, a differentiator; pushed after
  AI operations

**Later, if real demand shows up:** conversational shopping, cross-supplier
duplicate merging, attribute extraction and review attribution (need the
inference engine's generate endpoint), sales forecasting (needs months of
orders). These AI features demo well but do little for a shop that just
opened, so they come after "can open a shop", "can do business" and AI
operations.

---

## Contributing

Contributions are welcome. Please read [CONTRIBUTING.md](./CONTRIBUTING.md).

Two rules that matter most:

1. **Handlers contain no SQL and no transactions.** Business logic lives in services,
   data access in repositories. This is what makes the monolith-to-services path work.
2. **The OpenAPI spec is the source of truth.** Change the contract first,
   regenerate, then implement.

---

## License

Apache-2.0
