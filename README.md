<div align="center">

# Keel

**An AI-native commerce platform with a built-in distributed transaction engine.**
*Runs on a single machine. Scales without a rewrite. No external AI APIs.*

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
Keel is not trying to be the fiftieth. It exists for three things none of them have:

### 1. A distributed transaction engine, built in

Transactional consistency is handled by [dtmrs](https://github.com/jackwangfeng/dtmrs) —
a Rust transaction coordinator supporting SAGA, TCC, two-phase messaging, XA and workflow.

Checkout deducts stock, redeems a coupon and creates an order across three branches.
If any step fails, the rest are compensated — correctly, idempotently, and with
sub-transaction barriers that survive process crashes.

Most open-source commerce projects either avoid the problem (one big local transaction)
or bolt on an external coordinator. Keel treats it as a first-class concern.

### 2. AI across the whole commerce lifecycle — all running locally

Not one AI feature bolted on. AI at every stage, on your own hardware:

| Stage | Capabilities |
|---|---|
| **Supply** | Extract SKUs and attributes from messy supplier files · auto-categorize · detect duplicate listings across suppliers · block prohibited advertising claims before publish |
| **Demand** | Hybrid semantic search · conversational shopping · visual search · intent clarification when nothing matches |
| **Operations** | Demand forecasting · replenishment suggestions · ask your data in plain language |
| **Feedback** | Cluster review complaints into actionable product fixes · classify return reasons |

**No external API calls. No data leaving your network. No per-token billing.**

Supplier prices, cost structure and customer conversations stay on your machine.

This matters more than it sounds. Other platforms' AI is either a wrapper around a
third-party API, or only available on their hosted cloud — Medusa's semantic search,
for instance, runs on Medusa Cloud. If you self-host, you are back to wiring up
Algolia yourself. Keel assumes you want to own your stack.

And almost all of it sits on the demand side. **Nobody is applying AI to the supply
side or to operations** — which is exactly where merchants spend their hours.

### 3. One codebase, from a laptop to a cluster

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

### Four — one deployment, many merchants

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

---

## Live demo

**<https://eshop.zzss.fun>** — open to everyone, no sign-up or login needed.

| Path | What you get |
|---|---|
| [`/`](https://eshop.zzss.fun/) | Buyer app (H5 build of `app/`) |
| [`/admin/`](https://eshop.zzss.fun/admin/) | Merchant console, signed in as the demo merchant's admin |

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

- **two after-sales timers and two buyer-app screens.** Auto-confirming receipt
  N days after shipping, buyer-entered return tracking numbers and private refund
  evidence uploads (visible only to the buyer and to staff) are there, but closing
  a return-and-refund whose goods never come back, and cleaning up uploads left
  unreferenced for 24 hours, have no background job yet; the buyer app has not
  wired the evidence upload and return-shipment endpoints (the server and the
  contract are ready)
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

  **That is the only shape a GPU-less deployment has today.** infero has CUDA
  and Metal backends and no CPU backend yet, and this repo does not keep a
  second engine implementation around as a stand-in — the M3 Python service
  (BGE-M3 on CPU) has been retired. The fix is a CPU backend inside infero
  itself, which is in progress there; until it lands this is an intention, not a feature
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
shipping-fee templates (per piece or by weight, priced per province, free over
an amount or a quantity after discounts, undeliverable regions, per store or
shop-wide) · promotions (tiered spend/quantity discounts, limited-time prices, flash
sales, new-buyer gifts; allocated per line, coupons apply to the
post-promotion amount) · order state
machine · multi-store with delivery fences · tiered staff roles · in-app
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
- **Conversational shopping** — understands intent, never invents products.
  Every item shown comes from a real retrieval result.
- **Visual search** — find the same product from a photo.
- **Product understanding** — extract structured attributes from messy supplier
  spreadsheets, auto-classify categories, detect duplicate listings across suppliers.
  What ships today is the zero-shot half of auto-classification: bulk import
  suggests categories from title embeddings (Top-3 95.9% on the offline set) and
  leaves low-confidence ones to a human; attribute extraction waits for the
  inference engine's generate endpoint.
- **Compliance checks** — catch prohibited advertising claims before publish.

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
| **Storefront** | uni-app x (UTS compiled to native Kotlin / Swift) | Android · iOS · H5 · WeChat Mini Program — one codebase (the Mini Program runs in the WeChat devtools simulator; not yet previewed on a device or published) |
| **Admin console** | Vue 3 + Element Plus | Desktop web |

Every client is generated from the same OpenAPI spec, so a contract change
breaks the build rather than silently breaking production.
Design tokens (color, spacing, typography, radius) are shared across platforms;
component implementations are per-platform, because a mini program cannot render
what a browser can.

**WeChat Mini Program support is a deliberate choice.** In China most commerce
happens inside WeChat, and no major open-source commerce platform targets it.
If you are selling in that market, a web-only storefront is not a storefront.

> v0.1.0 ships the admin console and the buyer app (an Android apk can be
> built locally; the H5 build deploys as-is). The WeChat Mini Program now builds
> with `make app-build-mp-weixin` and runs browse, login, cart and checkout in
> the WeChat devtools simulator; on-device preview and publishing need an HTTPS
> domain. A desktop-optimized web storefront follows.

---

## Buyer app — what's actually in `app/` today

The table above is the plan. This section is the part that exists, so that
nothing above reads as if the rest already ships.

[`app/`](./app) is the buyer storefront, written in
[uni-app x](https://doc.dcloud.net.cn/uni-app-x/) (UTS compiled to native
Kotlin/Swift — not a webview). Product list (filterable by category) / search →
product detail → login → checkout (preview, best coupon picked automatically,
then submit) → my orders → order detail → pay; "Me" holds the coupon claim
center and my coupons. Search uses `POST /search`, hybrid semantic + keyword.

**Its types are generated from the same OpenAPI spec, but not from the same
artifact as `web/`.** UTS is not TypeScript — its type system has to land on
Kotlin and Swift, so the conditional/mapped types in `web/src/api/client.mts`
have nothing to compile to. A second generator
(`scripts/gen_uts_schema.py`) emits `app/src/api/schema.uts` from the same
`docs/电商系统-OpenAPI.yaml`, the artifact is committed, and two gates hold it
in place: `scripts/check_uts_contract.py` (regenerate to a temp dir and diff)
and `scripts/check_app_types.py` (`tsc --strict` over every `.uts`).
Rename a contract field and both go red — the mutation transcript is in
[`app/README.md`](./app/README.md).

**Command-line builds reach H5, Kotlin, and a local apk.**
`uni build --platform h5` produces a deployable web bundle;
`uni build --platform app-android` produces Kotlin source;
`make app-apk` turns that into an installable apk with the offline SDK and
Gradle, no HBuilderX needed. CI runs the first two steps.
`app/README.md` records what was measured at each step, including the things
that had to be worked around to get a CLI project to build at all.

Building and running it: see [`app/README.md`](./app/README.md).
The H5 form needs to be served same-origin with the API (the server sends no
CORS headers), which is what the dev-server proxy in `app/vite.config.js` does.

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
[Data model](./docs/电商系统-数据模型设计.md) ·
[Search layer](./docs/电商系统-语义检索层设计.md) ·
[Product understanding](./docs/电商系统-商品理解服务设计.md)

---

## Is Keel right for you?

**Use Keel if you want**
- Transactional correctness you can actually reason about
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
- [ ] **M5 Measurable search quality** — business re-ranking, search logs,
  click-back events and metrics ✅; cross-encoder reranking (waiting on the
  inference engine's rerank endpoint) and an offline evaluation set to do
- [ ] **M6 Ready to open a shop** — in-app notifications and console to-dos ✅
  (written in the same transaction as the state change, outbound channels
  pluggable); shipping-fee templates and free-shipping coupons ✅; real
  payments, WeChat login and SMS codes need business qualifications and will
  be wired in once those are in hand
- [ ] **M7 Ready to do business** — promotions (tiered discounts, flash
  prices, new-buyer gifts) ✅ (group buying not done); business reports, Excel
  bulk import with AI category suggestions (in progress)
- [ ] **M8 Visual search** — image embeddings, a differentiator

**Later, if real demand shows up:** conversational shopping, cross-supplier
duplicate merging, attribute extraction and review attribution (need the
inference engine's generate endpoint), natural-language analytics, an MCP
server, sales forecasting (needs months of orders). These AI features demo
well but do little for a shop that just opened, so they come after "can open a
shop" and "can do business".

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
