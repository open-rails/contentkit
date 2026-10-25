# Critical ContentKit integration suite

This suite exercises the public HTTP, Go and TypeScript contracts against real
AuthKit, PostgreSQL, object storage, media workers, browsers and ClickHouse.
Tenant and app fixtures share database tables so separate schemas cannot hide
missing scope checks. Controlled boundaries include clocks, storage failures
and connection cuts; authentication and access decisions use the real services.

The old test corpus was retired when this suite was built. The scenario and
binding inventories describe what must be verified. They are not themselves
proof that an execution passed.

## Full qualification

From the repository root:

```sh
python3 tests/critical/run.py --race
```

The runner requires every reviewed scenario, HTTP operation and public Go/TS
binding to have executable case mappings. It then requires every selected case
to run and pass exactly once. Failures, skipped tests, missing cases, unfinished
packages and empty executions all fail qualification. The HTTP inventory is
checked against the declared API, and both operation and binding inventories
are checked against the ledger.

The scenario, HTTP-operation and binding inventories describe the contracts;
`coverage.json` maps them to executable cases. `results/go.jsonl` records actual
execution. A successful full runner is the qualification result, not the review
status of an individual ledger entry.

The runner reports named Go test results, including parent functions and their
subtests. One contract can produce many results: the HTTP authentication matrix
runs each route under several credential and permission profiles. The reported
total is therefore not a count of independent end-to-end journeys. Use the
inventories to inspect behavioral coverage and the execution log to inspect what
actually ran.

CI builds and checks the shipped SDK, public Go modules and native commands
before running the suite with race detection. It uploads execution results and
browser screenshots even when the run fails.

## Owned fixtures

Provide disposable services through:

- `CONTENTKIT_CRITICAL_PG`: PostgreSQL 18 with PGroonga 4.1+ and pg_trgm.
- `CONTENTKIT_CRITICAL_S3_ENDPOINT`, `CONTENTKIT_CRITICAL_S3_USER`,
  `CONTENTKIT_CRITICAL_S3_PASSWORD`: S3-compatible storage supporting signed
  checksums, multipart and conditional writes. Browser uploads also need CORS.
- `CONTENTKIT_CRITICAL_CH_ADDR`, `CONTENTKIT_CRITICAL_CH_USER`,
  `CONTENTKIT_CRITICAL_CH_PASSWORD`, optionally `CONTENTKIT_CRITICAL_CH_CLUSTER`:
  ClickHouse with Keeper and replica macros. Each fixture owns a fresh database.

Native tests need libvips and the media worker's compatible FFmpeg runtime;
browser tests need Node, pnpm and Playwright Chromium. CI provisions these in
`.github/workflows/go.yaml`. `.github/scripts/critical-media-tools.sh` exposes
the pinned media image's FFmpeg tools to host tests and removes only its owned
container. It does not replace the worker under test with a released worker.

Build the shipped SDK and browser harness before browser tests:

```sh
pnpm --dir sdk/ui build
pnpm --dir sdk/ui exec vite build --config ../../tests/critical/browser/vite.config.mjs
```

Missing required fixtures fail rather than skip. Never point destructive
fixture tests at application or production databases.

## Focused development

Run whole test owners to preserve their setup and actor profiles:

```sh
python3 tests/critical/run.py --focus '^TestDeletedContentPurgeOwnsMedia$' --race
python3 tests/critical/run.py --focus '^TestMediaSSEUpload$'
```

Focused execution is prohibited in CI and prints
`LOCAL PARTIAL EXECUTION; NOT QUALIFICATION`. Pure public-value checks can run
without database fixtures after building the SDK; integration fixtures still
require their actual services.

The fixed CI selection `--suite sdk` runs all reviewed TypeScript binding test
owners in full. It applies the same failure, omission and duplicate-case checks
to that selection; it does not qualify the complete backend suite.

`CK_NEGATIVE_CONTROL=foreign-app-grant` deliberately widens one fixture actor's
authority. Its cross-app assertion must fail. This demonstrates the assertion's
sensitivity and is never a normal passing mode.

## Media acceptance

The real ZIP/browser journey verifies upload byte progress, partial extraction,
loaded thumbnails, reopening the editor, credential refresh, and a fully
processed draft that remains unpublished. Go streaming cases cover snapshots,
replay/resync, expiry, revocation, ordered callbacks and cancellation.

Video checks inspect actual AV1/H264 output and audio streams alongside HLS
metadata. Other cases cover image dimensions, interrupted work,
immutable upload admission, multipart handling, scoped delivery, legacy storage
migration and the existing content-purge sweep's storage retry and restore race.

Scope and value tests retain their actual public boundaries: organization keys
exercise root/tenant/app authority; unsupported personal-token admission is not
claimed. TypeScript errors expose the fields declared by that client; a Go-only
error field is not treated as a missing TypeScript contract.
