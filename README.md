<div align="center">

# Keel

**An AI-native commerce platform with a built-in distributed transaction engine.**
*Runs on a single machine. Scales without a rewrite. No external AI APIs.*

[![CI](https://img.shields.io/badge/CI-passing-brightgreen)](#)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.23+-00ADD8)](#)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-16+-336791)](#)

[Documentation](./docs) · [Live Demo](#) · [中文文档](./README.zh-CN.md)

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

---

## Quick start

```bash
git clone https://github.com/<org>/keel && cd keel
docker compose up
```

That's it. One command brings up the API, PostgreSQL (with pgvector),
the inference engine and seed data. Open <http://localhost:3000>.

No Elasticsearch. No MongoDB. No RabbitMQ. No Redis.
**One database.** Vector search lives in `pgvector`, full-text in `tsvector`,
the job queue in a table.

> Works without a GPU — the inference engine falls back to small CPU models.
> Search quality degrades gracefully; nothing breaks.

---

## What it does

**Commerce core**
Products & SKUs · inventory · cart · checkout · payments · refunds ·
coupons · order state machine

**AI-native capabilities**
- **Semantic search** — hybrid vector + keyword retrieval, RRF fusion,
  cross-encoder reranking, then *business re-ranking* (stock, promotions, quality).
  Semantically relevant is not the same as worth selling.
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
