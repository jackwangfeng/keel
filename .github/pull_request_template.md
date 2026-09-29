<!-- Describe what changes and, more importantly, why. Commit messages in this
     repository explain reasoning rather than restating the diff; a PR
     description works the same way. -->

## What and why

## Checklist

The two hard rules from [CONTRIBUTING.md](../CONTRIBUTING.md):

- [ ] **No SQL or transactions in `handler/`.** Business logic lives in
      `service`, data access in `repository`. This is the rule the "evolve to
      microservices without rewriting business code" claim rests on.
- [ ] **Contract first.** If this changes the API: the spec changed before the
      handler did, and `make generate generate-uts flutter-generate` has been
      run and its four outputs committed.

And:

- [ ] `./scripts/check-all.sh` passes.
- [ ] `make test-db` passes (needs a local Postgres — see CONTRIBUTING.md).
- [ ] New tables are classified in `db/tenancy.json` and documented in
      `docs/电商系统-数据模型设计.md`, with a reason for any exemption.
- [ ] If this adds a gate or an assertion: it was made to fail on purpose once,
      and **the thing that went red was that assertion** rather than something
      else failing first.

## Anything you decided yourself

<!-- Judgement calls made without asking, and what you rejected. This section
     being empty on a non-trivial change is usually a sign something went
     unexamined. -->
