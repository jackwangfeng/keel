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

[Documentation](./docs) · [Changelog](./CHANGELOG.md) · [Security](./SECURITY.md) · [中文文档](./README.zh-CN.md)

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

## Quick start

```bash
git clone https://github.com/jackwangfeng/keel && cd keel
docker compose up -d --build
./scripts/smoke.sh                        # exit 0 means the chain works
curl http://localhost:8080/api/v1/products
```

One command brings up PostgreSQL, runs the migrations, loads a seed shop and
starts the API. That `curl` comes back with the shop's products, and nothing in
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

For the multi-merchant shape, where the `Host` header picks the shop:
`docker compose -f compose.yaml -f compose.multi.yaml up -d --build`.

No Elasticsearch. No MongoDB. No RabbitMQ. No Redis.
**One database.** Vector search lives in `pgvector`, full-text in `tsvector`,
the job queue in a table.

> Files (product images, avatars, refund evidence) go to a local disk volume by
> default — not another service. Switching to the S3 driver is what adds a component.

### Not in the box yet

The three services above — PostgreSQL, the migrations plus seed, the API — are
all `docker compose up` brings up today. The rest of this README describes the
system being built; these parts are on the roadmap and are listed here so that
nothing above reads as if it already ships:

- **cross-encoder reranking and business re-ranking.** `POST /search` today is
  two-stage — vector recall and keyword recall, fused with RRF. The contract
  describes four stages; the last two land in M5. `explain: true` names the
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
  itself; **it is not written yet**, so this is an intention, not a feature
- the merchant admin surface. The contract has it; the implementation does not —
  a merchant can process orders but cannot list a product yet (M4)
- the storefront and admin UI — there is no page on port 3000 yet

---

## What it does

**Commerce core**
Products & SKUs · inventory · cart · checkout · payments · refunds ·
coupons · order state machine

**AI-native capabilities**
- **Semantic search** — hybrid vector + keyword retrieval fused with RRF.
  Cross-encoder reranking and *business re-ranking* (stock, promotions, quality)
  are designed and contracted, and land in M5 — semantically relevant is not the
  same as worth selling, and that distinction is the point of the last two stages.
- **Conversational shopping** — understands intent, never invents products.
  Every item shown comes from a real retrieval result.
- **Visual search** — find the same product from a photo.
- **Product understanding** — extract structured attributes from messy supplier
  spreadsheets, auto-classify categories, detect duplicate listings across suppliers.
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
| **Storefront** | uni-app (Vue 3) | H5 · WeChat Mini Program · iOS · Android — one codebase |
| **Admin console** | React + Tailwind + shadcn/ui | Desktop web |

Every client is generated from the same OpenAPI spec, so a contract change
breaks the build rather than silently breaking production.
Design tokens (color, spacing, typography, radius) are shared across platforms;
component implementations are per-platform, because a mini program cannot render
what a browser can.

**WeChat Mini Program support is a deliberate choice.** In China most commerce
happens inside WeChat, and no major open-source commerce platform targets it.
If you are selling in that market, a web-only storefront is not a storefront.

> First release ships the admin console and the H5 / Mini Program storefront.
> Native app builds and a desktop-optimized web storefront follow.

---

## Buyer app — what's actually in `app/` today

The table above is the plan. This section is the part that exists, so that
nothing above reads as if the rest already ships.

[`app/`](./app) is the buyer storefront, written in
[uni-app x](https://doc.dcloud.net.cn/uni-app-x/) (UTS compiled to native
Kotlin/Swift — not a webview). Product list → product detail → login →
checkout (preview then submit) → my orders → order detail → pay.
Search is a disabled placeholder; the endpoint it needs is `POST /search`.

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

**Command-line builds reach H5 and Kotlin, not an apk.**
`uni build --platform h5` produces a deployable web bundle;
`uni build --platform app-android` produces Kotlin source and stops there —
turning that into an installable app needs HBuilderX or DCloud's cloud build,
and there is no CLI for it. CI runs exactly those two steps and says so.
`app/README.md` records what was measured, including the five things that had
to be worked around to get a CLI project to build at all.

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
- [ ] **M2** — Catalog → cart → checkout → payment, orchestrated by dtmrs
- [ ] **M3** — Text embeddings + hybrid search → *first public release*
- [ ] **M4** — Compliance checks + product understanding service
- [ ] **M5** — Reranking + business re-ranking + search analytics
- [ ] **M6** — Image embeddings → visual search
- [ ] **M7** — Conversational shopping assistant
- [ ] **M8** — Cross-supplier duplicate detection

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
