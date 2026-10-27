# Host-scale performance qualification

Owner: Codex `/root/verify_host_jobs`. Branch `bench/261-host-performance`.
Base: `d0a9f52060c452537750cc5e55b625f58f95b3af`, ContentKit v0.131.0.
Status: harness preparation; no full host-scale result yet.

The previous Store measurements exclude PostgreSQL access-history loading,
all configured scope groups, stored preset writes and HTTP card hydration.
This experiment measures those paths on disposable local services.

## Actual entry points

```text
ContentKit popularRefreshWorker.Work
  discovery.Service.RefreshPopularRankings
    discovery.Service.Availability (PostgreSQL immutable history)
    popularity.Prepare -> Store.PrepareDailyPopularity (ClickHouse)
    popularity.Rank -> Store.DailyPopular (ClickHouse)
    PostgreSQL content_popular_rankings writes
HTTP GET /v1/contents
  discovery.Service.List -> configured ranking or explicit prepared date range
  visibility/filter/page selection -> catalog cards
```

The benchmark uses the real engine/worker and HTTP handler. Fixture generation
is measured separately and may bulk insert synthetic catalog/source records;
it is not an ingestion or publication throughput claim. It does not run host
billing, ingress, media transfer or frontend rendering.

## Workload

Re-read the active host adoption model and example config before the final run.
Checked earlier host model builders declare five presets (7d, 30d, 90d, 365d,
all): Doujins three domains x (six languages + language-empty) = 21 groups /105
snapshots; Hentai0 two domains x (five languages + language-empty) =12 /60.

Each app independently receives 60,000 synthetic daily users x ten distinct
items =600,000 person/item-days TOTAL, distributed across its domains/languages.
Three receipt identities per visit x two cumulative revisions =3.6m delivered
rows per app/day. Use skew, repeats, mixed qualification, audience eligibility
and a known count oracle. Do not multiply these people/receipts by scope count.

Start with 1,000 items and one day to validate the actual worker, HTTP response,
filter and pagination oracles. Then use 100,000 works per app. Gallery editions
have one language each, using one primary edition per work in this baseline; translated/multilingual
edition sensitivity is explicitly outside the measured input. Historical daily rows follow available
editions/scopes rather than multiplying every item into every language. Available
zero-reader days must still contribute zero. Exact distributions and generated
row counts will be recorded with the fixture.

Separate phases:

1. Raw day: real worker with PostgreSQL access history and one day's receipts,
   all configured scope groups; verify prepared U/N and all preset snapshots.
2. Finalized reuse: the same worker reads closed checkpoints, combines counts
   and writes due snapshots without reducing raw receipts again.
3. Historical scalars: generate 365 days of prepared item/day counts directly,
   run the actual worker across all presets; this is not a year of raw ingestion.
4. HTTP: preset and explicit 7/30/365/all-history ranges, representative
   domain/language/entity filters, first and subsequent pages, actual card bodies.
   Measure serial requests and low concurrency (1,4,8) alone and during a due
   worker refresh. Record every error and validate response scope/order/metadata.

## Resources and receipts

Use existing owned ck254 PostgreSQL18/PGroonga and ClickHouse25.8/Keeper, with
unique benchmark schemas/databases. Root confirmed the other qualification owner
uses a separate stack. Verify labels/ports before each heavy phase. No production
requests, shared restart, cache flush, or service reconfiguration.

ClickHouse queries use two threads and the implementation's existing1GiB
projection limit. Bound fixture inserts. Run the apps sequentially. Start small,
record disk growth, and only then load the365-day fixture; do not manufacture a
1.2b-row dense33-scope dataset when edition languages make it sparse.

Retain phase wall/CPU time, ClickHouse query IDs/read rows/bytes/peak memory,
PostgreSQL/HTTP query timing, full snapshot counts, fixture/oracle counts,
HTTP latency distributions, errors and machine/concurrent-load observations.
Record cache state as observed; these are not controlled cold-disk timings.
No invented SLA or production-capacity extrapolation. Keep the previous Store
benchmark results distinct from this host-path experiment.
