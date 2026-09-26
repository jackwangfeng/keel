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

First release will be cut at the end of M4. Everything below has landed on
`main` and is covered by the gates described in `CONTRIBUTING.md`; this section
becomes `0.1.0` when the remaining M4 work is in.

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

### Added — merchant self-service (M4, in progress)

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
  rotated once the server has definitively rejected it. Stores and regions are
  menu placeholders — that contract has not landed — and the orders page states
  plainly that no admin order-list operation exists rather than drawing a fake
  table.

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

[Unreleased]: https://github.com/jackwangfeng/keel/commits/main
