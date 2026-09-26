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
  and GitHub-hosted runners do not have one, so there is no CI job that would go
  red if the engine integration broke.
- **No hosted demo and no documentation site.**

[Unreleased]: https://github.com/jackwangfeng/keel/commits/main
