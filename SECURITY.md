# Security Policy

## Reporting a vulnerability

**Do not open a public issue.** Use GitHub's private reporting:
the **Security** tab → **Report a vulnerability**. That opens a draft advisory
visible only to you and the maintainers.

There is no response-time commitment here, because one that nobody measures is
worth nothing. This is a small project; reports are read.

---

## What this project treats as a vulnerability

Keel is a **multi-tenant system on a single database**. That shapes the list:
the class of bug that matters most here is one that lets one tenant reach
another's data, and it does not have to look like a classic exploit to qualify.

In rough order of how seriously each is taken:

1. **Tenant isolation bypass.** Any path by which merchant A reads or writes
   merchant B's rows. A missing row-level-security policy, a policy that is
   `ENABLE`d but not `FORCE`d, a view that evaluates with owner privileges
   instead of `security_invoker`, a query that reaches the pool outside a
   tenant-scoped transaction — all of these are the same bug wearing different
   clothes, and all of them count.
2. **Privilege escalation across the staff scope.** `staff.merchant_id` is
   nullable, and `NULL` means a platform-level operator. Any route by which a
   merchant-level administrator creates, becomes, or acts as one is a serious
   bug. (The obvious form of this was found and closed during design — a
   predicate of `merchant_id = current_merchant() OR merchant_id IS NULL` would
   double as the write check and hand out platform admin. If you find another
   shape of it, that is exactly the kind of report wanted.)
3. **Authentication and token handling.** Forging a session, reusing a revoked
   one, or anything in the hand-rolled HS256 implementation in
   `internal/auth/token.go`. It is deliberately hand-rolled and deliberately
   narrow — it rejects any `alg` other than `HS256` and never reads the
   algorithm out of the token to decide how to verify — but hand-rolled crypto
   invites review, which is the point of saying so here.
4. **Reaching the database outside the intended grant surface.** The
   application connects as `keel_app`, which is not the table owner and does not
   bypass RLS. Anything that lets application code act as `keel` — or that gives
   `keel_app` more than `db/tenancy.json` says it should have — is in scope.
5. **Injection of any kind**, including into the semantic search path.

## What is *not* a vulnerability

Each of these is a deliberate development default. They are listed so a report
about them can be answered with a link instead of a discussion — and so that if
one of them is wrong, the argument is on the record and can be attacked.

- **`POSTGRES_PASSWORD` defaults to `keel` in `compose.yaml`.** It is a
  development default, and the Postgres service publishes no host port — the
  database is reachable only from inside the compose network. Do not run that
  compose file as-is on a public host; it is a demo stack, and says so.
- **The seeded demo accounts and their passwords.** They are in
  `db/seed/` in plain text on purpose.
- **`KEEL_AUTH_SECRET` unset means a random per-process signing key.** The
  process logs a warning at startup and every restart invalidates every token.
  That is the documented behaviour, not a weak key.
- **Sandbox payments accept synthetic callbacks** when `KEEL_PAYMENT_SANDBOX` is
  not set to `off`. It warns loudly at startup. Turning it off is a deployment
  decision, and one nobody can make on your behalf.
- **Version and commit are exposed at `GET /version`.** The source is public;
  the commit hash is not a secret, and knowing which version is running is worth
  more than the nothing it gives away.

## Scope

Only this repository. The inference engine
([infero](https://github.com/jackwangfeng/infero)) and the transaction
coordinator (`dtmrs`) are separate projects with their own reporting channels.

There is **no hosted deployment to test against**, so please do not go looking
for one. Everything here is reproducible locally with `docker compose up`.

## Supported versions

Pre-1.0. Only the latest release gets fixes; there are no maintained branches,
and the database migrations are forward-only. See `CHANGELOG.md` for what that
means for upgrades.
