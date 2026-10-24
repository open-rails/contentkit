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

Missing PostgreSQL fails the suite; it never skips. The strict selected-case runner, real object-store/commercial fixtures and built-package browser cases are present. ClickHouse recovery checks and the full public-operation mapping remain in progress. `coverage.json` records obligations rather than turning
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


The full runner is `python3 tests/critical/run.py --race` from the repository
root. It deliberately fails until the ledger has reviewed executable cases and
the selected public-operation mapping. For local development only:

```sh
python3 tests/critical/run.py --focus '^TestMultipartUpload$'
```

This prints `LOCAL PARTIAL EXECUTION; NOT QUALIFICATION`. It rejects selected
skips and zero completed tests; focused execution is prohibited in CI.
`results/go.jsonl` records every Go test action. Full qualification also checks
every exact reviewed scenario, operation and public Go/TS binding case name actually passed.
The HTTP ledger is also checked against the current declared route inventory,
so missing, duplicate and changed operation records cannot qualify.

Additional required owned-fixture settings:

- `CONTENTKIT_CRITICAL_S3_ENDPOINT`, `CONTENTKIT_CRITICAL_S3_USER`,
  `CONTENTKIT_CRITICAL_S3_PASSWORD`: a real S3-compatible service with signed
  checksums, multipart and conditional writes. Browser uploads also need CORS.
- `CONTENTKIT_CRITICAL_CH_ADDR`, `CONTENTKIT_CRITICAL_CH_USER`,
  `CONTENTKIT_CRITICAL_CH_PASSWORD`, optionally `CONTENTKIT_CRITICAL_CH_CLUSTER`:
  ClickHouse with Keeper and replica macros. Each fixture owns a fresh database.
- Native libvips, ffmpeg, Node, pnpm and Playwright Chromium. Build the package
  and harness with `pnpm --dir sdk/ui build` followed by
  `pnpm --dir sdk/ui exec vite build --config ../../tests/critical/browser/vite.config.mjs`.

Current additions exercise actual image output dimensions and published cover
URLs, draft reopen/status reads, rejected uploads, immutable edits, uncertain
commit replay, real multipart resume/abort, gateway capabilities and real React
identity changes. Their scenario IDs are still obligations under semantic
review; for example file-count coverage does not establish owner quota coverage,
foreign multipart tickets do not establish expiry coverage, and HTTP range
coverage does not establish HLS/archive authorization.

The TypeScript `ContentKitError` contract exposes status/code, retryAfter,
details, blobs, ban, action, reason, availableAt and unknownCode. RequestID is a
Go error field; adding it to TypeScript is outside the current public contract.
