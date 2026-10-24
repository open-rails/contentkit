# Critical ContentKit integration suite

This replacement is in progress in draft PR390. It must not be used as passing
replacement evidence until every selected critical behavior is implemented and
verified. The old test corpus was deleted first from this isolated candidate,
as requested; the production checkout and other worktrees are unchanged.

The first slices use real AuthKit, PostgreSQL, the scoped ContentKit engine and
its public HTTP/Go clients. All apps share tables: separate schemas must not
hide missing tenant/app predicates. The only controlled boundaries are the
clock, an actual TCP connection cut, and legitimately signed expired issuer
input. No internal authentication, access, store or billing collaborator is
mocked.

Run a focused implemented slice against an owned disposable PostgreSQL18 with
PGroonga4.1+ and pg_trgm:

```sh
cd tests/critical
CONTENTKIT_CRITICAL_PG='postgres://user:password@127.0.0.1:port/disposable?sslmode=disable' \
  GOWORK=off go test -count=1 -run '^TestPublicationWithoutWorkers$' ./...
```

Missing PostgreSQL fails the suite; it never skips. The broader selected-case
runner, real object-store/ClickHouse/commercial-grant fixtures and browser cases
are being implemented. `coverage.json` records obligations rather than turning
scenario titles or counts into tests. Existing pure fixtures may be reused;
legacy tests are not retained under new names.

`CK_NEGATIVE_CONTROL=foreign-app-grant` deliberately widens one actor's authority
in the test fixture. Its cross-app403 assertion must fail. This opt-in fault is
for demonstrating sensitivity, never a normal passing CI mode.

The new publication slice exposed an injected-clock inconsistency: catalog
queries used Deps.Clock while access decisions used time.Now. The separate
clock-wiring fix keeps ordinary nil-clock behavior and is covered by real HTTP
release-boundary and wall-clock controls. It is not a claim that deployed
wall-clock publication was broken.
