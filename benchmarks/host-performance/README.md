# Configured host popularity and HTTP experiment

Status: the isolated 1,000-work, 600-person, 365-day pilot passed for both host
models (215.703 seconds together). Full 100,000-work / 60,000-person results are
pending. This is a disposable synthetic benchmark, not production load or an SLA.

Source: ContentKit v0.131.0 (`d0a9f520`). Model snapshots under `models/` retain
only public content structure, domain filters and language configuration from the
named host commits. The Hentai0 model/filter bytes are unchanged at `1c52fbd86`.
The worker is the normal `contentkit_popular_refresh` River worker. It loads
PostgreSQL access history, prepares ClickHouse counts, combines the host's five
presets and persists their ordered IDs. HTTP requests use the real scoped engine
and explicit Host headers against a loopback server.

## Workload

- Each app has 100,000 works, one primary-language edition per work, balanced
  across its configured languages. Translated/multilingual-edition cardinality
  is not measured. Publication and immutable access history align with the
  synthetic historical first day.
- Doujins has three domains x seven language selections =21 groups /105 preset
  snapshots. Hentai0 has two domains x six selections =12 groups /60 snapshots.
- Per app/day: 60,000 synthetic people x ten distinct works =600,000 person/work
  pairs. Half the choices favor a hot subset. Three stable receipts per visit,
  each delivered as revision2 then revision1, produce3.6m source rows. Revisions
  and scope overlap do not create additional people or work-day contributions.
- Qualification is1,200ms for90% of people and300ms for the remainder.20% of
  works need premium; half of each scope's people gain premium during their
  second same-day observation. Every observation is duplicated. Cohort choice
  uses a per-scope ordinal so language/domain modulo does not erase a cohort.
- An independent Go enumeration checks **every** prepared U/N row, including
  available zero-reader rows. Raw synthetic people are not60,000 AuthKit accounts.
  A separate real AuthKit reader exercises the signed-in HTTP path with empty
  preferences and progress.
- Earlier history is generated directly from the measured raw day's daily counts
  with deterministic weekly80–120% variation, bounded by eligible population.
  It is **not a year of raw receipts**. Available rows total219m for Doujins and
  146m for Hentai0, with one global-language and one specific-language row per
  available work/domain/day. Exact row counts and UTC bounds are verified.

## Measurements

1. Catalog generation/publication fixture and source insertion are separate from
   the timed production worker.
2. One raw-day refresh across every configured group and preset.
3. Finalized-day reuse, including PostgreSQL history and all preset writes.
4.365-day prepared-data refresh and HTTP preset/explicit range reads.
5. Filtered first pages, mandatory cursor and numbered second pages, correct
   version/kind/language, covered dates and independently selected ordered IDs.
6. Anonymous requests at concurrency1/4/8 across every configured selection;
   signed-in account requests are reported separately. Every response is checked.
7. The same unchanged-count refresh under HTTP load. Load begins only after
   River reports Running. Request/worker timestamps count actual overlap;
   requests after worker completion remain explicitly mixed-overlap samples.
8. One combined **raw-day recomputation + yearly ranking** cycle, alone and
   under concurrency8. Only the owned last-day checkpoint is aged to before its
   finalization deadline, so the ordinary worker recomputes it. This is one full
   raw day's pressure, not proof of two full provisional days.

Query-log CPU/read bytes/peak memory, PostgreSQL query times, HTTP p50/p95/p99,
errors, full snapshot counts and the exact fixture/oracle data are retained.
Percentiles describe these finite samples; no pass/fail latency threshold is
invented. All-time covers this fixture's365 days, not unbounded future growth.

## Run

Use an owned disposable PostgreSQL18/PGroonga and ClickHouse25.8/Keeper stack.
The harness creates and retains a unique logical PostgreSQL database and
ClickHouse database for each app. It never restarts services or clears caches.
The caller supplies credentials through the existing critical fixture variables;
no credential is stored in reports. The supplied environment must permit owned
scratch database creation. Native ContentKit dependencies must already exist.

```sh
# From the repository root, with CONTENTKIT_CRITICAL_PG and CH_* already exported:
export HOST_PERF_OUTPUT="$PWD/.task/host-performance-new"
export HOST_PERF_ITEMS=1000 HOST_PERF_USERS=600 HOST_PERF_DAYS=365
export HOST_PERF_APPS=doujins,hentai0 AUTHKIT_TEST_KEEP_DB=1 GOMAXPROCS=2
cd tests/critical
GOWORK=off go test -tags hostperformance -run '^TestHostPerformance$' \
  -count=1 -timeout=4h -v ./hostperformance
```

For the full workload use `HOST_PERF_ITEMS=100000` and `HOST_PERF_USERS=60000`.
`HOST_PERF_REQUESTS` defaults to48; each anonymous load executes at least one
request per configured case, even when that is more than48. Apps run sequentially.
The host-owned benchmark fleet uses normal workers and disables unrelated periodic
schedules; refreshes are explicit River jobs, not an alternative SQL algorithm.

## Limits and retained pilot evidence

This measures embedded ContentKit's host-model worker/API path, including real
PostgreSQL filtering and card hydration. It excludes deployed host ingress,
billing calls, frontend rendering and media bytes/storage. Access-context JSON,
coverage and catalog text are synthetic; immutable history contains one state per
edition. Source insertion bypasses the HTTP receipt authorization path and is not
an ingestion throughput measurement. There are no production requests.

Reads follow fixture insertion and functional requests. They are cache-assisted,
not controlled cold-disk measurements. Other work shares the host; record machine,
load and resource observations before interpreting a capacity result.

The first shared-database pilot encountered a PGroonga `Sources... nonexistent
object` error during fixture ANALYZE. Its schema and logs are retained. Both apps
then passed using their own logical databases on the same unchanged container;
no shared PGroonga objects were repaired or removed. This bounds the observed
fixture issue without claiming a proven PGroonga root cause or product defect.

Full pilot receipts remain in the owned worktree's `.task/pilot-isolated-v7/`.
The combined recomputation orchestration was added after that pilot, reviewed and
compiled; its execution will be part of the frozen full workload. All datasets,
failed pilot receipts and prior single-Store benchmarks remain retained.
