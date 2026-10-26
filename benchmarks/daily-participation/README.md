# Daily participation scalar experiment

At 100,000 items over 365 days, combining **already reduced daily counts** into
one complete ranking took **60–889 ms of ClickHouse server time** in this
experiment, depending on the window. Each result contained all 100,000 ordered
item IDs. Three overlapping synthetic scopes and four buckets per scope were
built and persisted in 4.665 seconds of cumulative server time.

This establishes the cost of the scalar combination stage on this machine. It
**does not measure or validate** producing those counts from canonical receipts,
receipt replacement/deduplication, per-person qualification, historical access,
erasure, or the production ContentKit algorithm. The earlier reported 107-second
30-day result covered a different algorithm and fixture; these results are not
a matched speedup comparison.

## Workload and metric

- Full baseline: 100,000 items × 365 UTC days = 36,500,000 daily rows, one scope.
- Source-volume assumption: 60,000 active users × 10 distinct items per user per
  day = 600,000 qualified item/user-days per day, or **219,000,000 per year**.
  The benchmark directly generates their daily scalar totals, not user records.
- Deterministic skew puts 50% of participation on the top 1% of items. The hot
  item ordering rotates weekly. Daily per-item U tiers are 1,200, 200, 20, 2, 1
  across 0.1%, 0.9%, 9%, 30%, and 60% of items, respectively.
- `repeat_visits` ranges from one to four per qualified contribution; the full
  baseline contains 547,366,641 such visits. They are deliberately **excluded**
  from the score, illustrating the intended scalar boundary without pretending
  to execute person-level deduplication.
- A separate daily population table supplies N. Scope 0 has 60,000 eligible
  active people per day. The three-scope sensitivity uses 60k, 30k, and 15k
  synthetic overlapping audiences, scaling U down by 1, 1/2, and 1/4 with integer
  flooring. Those extra scopes are assumptions, not proven eligibility cohorts.
- Score: `round(avg(U / (N + 20.0)), 12)`, with descending score and ascending item
  ID as the stable tie breaker. Every synthetic item is available every day, so
  the mean denominator is the covered window length. New-publication and
  partial-day eligibility semantics are outside this experiment.
- Rounding to 12 decimal places makes the prototype's score/tie precision
  explicit. An initial unrounded Float64 experiment produced differing result
  hashes across identical runs. This is not a proposed production scoring rule;
  the initial run and later byte comparisons remain in `.task/`.
- `all` means all history in this fixture: initially 365 days, then 366 days in
  the next-day refresh. It is not a multi-year growth measurement.

## One-scope results

ClickHouse query-log times include aggregation and sorting; separate wall time
also includes Docker CLI startup and transporting the full result. Four runs,
no ClickHouse query cache. The first observed run followed insertion.

| Bucket | Server time, four runs | Wall time | Logical rows read | Logical bytes read | Peak memory |
|---|---:|---:|---:|---:|---:|
| 7d | 71, 72, 60, 60 ms | 0.208–0.246 s | 704,877 | 7,047,427 | 25.6 MiB |
| 30d | 121, 110, 103, 93 ms | 0.243–0.301 s | 3,002,061 | 30,019,573 | 29.8 MiB |
| 365d | 889, 780, 762, 769 ms | 0.927–1.055 s | 36,500,365 | 365,007,320 | 33.4 MiB |
| all | 869, 859, 774, 802 ms | 0.932–1.048 s | 36,500,365 | 365,007,320 | 25.0 MiB |

The small population table and index granularity explain reads slightly above
exact item/day counts. One query computes each bucket; no per-item or per-reader
query loop runs.

Writing all 100,000 ranks to the prototype's prepared table took **93, 164, 891,
860 ms** for 7d, 30d, 365d and all. Reading 24 prepared IDs took **4–14 ms** server
time (0.154–0.183 seconds including a fresh Docker CLI call). This is not an HTTP
page latency measurement and does not hydrate catalog cards or apply visibility.

Generating and inserting the 36.5m synthetic scalar rows took **1.934 seconds**
server time, **2.101 seconds** wall time, and 118.3 MiB peak memory. This is **not a
raw engagement backfill benchmark**. The daily table occupied about 219.4 MiB
compressed at its recorded snapshot; 400,000 stored ranking rows took 3.1 MiB.

## UUID key sensitivity

The baseline uses UInt32 item surrogates. A second real table copied all 36.5m
rows to **UUID item keys**, grouped and sorted by those UUIDs, and returned full
UUID strings plus scores as JSON. This avoids hiding the grouping/key-width cost
behind integer IDs.

| Bucket | Server time, three runs | Wall time | Logical bytes read | Peak memory |
|---|---:|---:|---:|---:|
| 7d | 82, 78, 89 ms | 0.297–0.316 s | 15,501,661 | 25.0 MiB |
| 30d | 165, 150, 181 ms | 0.351–0.415 s | 66,220,563 | 26.8 MiB |
| 365d | 1,373, 1,402, 1,387 ms | 1.603–1.606 s | 803,012,252 | 25.3 MiB |

Each result still had 100,000 distinct, correctly sorted IDs. Output was about
7.4 MB per query because JSON includes textual UUIDs and column names. This
sensitivity changes key width and output format; it does not qualify the raw
receipt or eligibility projection. The fixture UUIDs are synthetic.

## Three scopes and daily refresh

Three scopes contain **109,500,000 daily rows** and 1,095 population rows. The
same scope-0 buckets took 50–64 ms (7d), 92–108 ms (30d), and 697–702 ms (365d).
365-day runs for scopes 1 and 2 took 710–767 ms and 717–792 ms. Peak memory across
those 365-day queries stayed below 34 MiB.

- Initial build and persistence of all **12 rankings**, 100k IDs each:
  **4.665 seconds server**, **6.324 seconds wall** across sequential commands.
- Append one day of **300k already reduced daily scalar rows**: **25 ms server**;
  append three population rows: 6 ms.
- Refresh and persist all **12 rankings** after that append:
  **4.646 seconds server**, **6.370 seconds wall**.
- Prepared reads of 24 IDs: **3–5 ms server** across the 12 buckets.
- Daily storage after append: 109.8m rows, **543.3 MiB compressed**. Each set of
  1.2m prepared ranking rows occupied about 9.3 MiB.

Scopes are processed sequentially here. Additional independently stored scopes
multiply aggregate input and preparation work; there is no claim of free
cross-scope reuse.

## Environment and measurement limits

Measured 2026-10-26 in the task-owned `ck254-contracts-ch` fixture, ClickHouse
**25.8.33.6**, UTC, Linux x86-64, Intel **Core i9-13950HX**, 32 logical CPUs.
Every query used **max_threads=2**, inserts used **max_insert_threads=2**, and
max_memory_usage was 2 GiB. Container CPU/memory were otherwise uncapped. Other
workspace work shared the host, so these are observed timings, not an isolated
capacity guarantee.

No cache dropping, service restart, data-volume reset, or production mutation
occurred. **These are not controlled cold-disk measurements.** The first run
followed inserts and therefore benefited from filesystem cache; 365-day ranking
query logs reported zero physical `OSReadBytes` while still reading roughly
184 MB through filesystem buffers. Subsequent runs are warm. Query-log read
bytes, memory, CPU counters, cache counters, result counts, and client wall time
are retained rather than inferred from a stopwatch alone.

All final result sets were checked for 100,000 unique IDs, score-descending /
ID-ascending order, identical repeated outputs, and eight independently
calculated sample scores per bucket. This checks the synthetic formula only.

## Reproduce

The script uses an existing disposable ClickHouse container whose
`CLICKHOUSE_USER` and `CLICKHOUSE_PASSWORD` environment variables identify its
fixture account. It prints neither credential. It creates a unique `ck261_*`
database and leaves it intact for review. Use a task-owned fixture, not an
application database.

```sh
python3 benchmarks/daily-participation/benchmark.py \
  --items 10000 --days 30 --repeats 2 --output .task/pilot-new
python3 benchmarks/daily-participation/benchmark.py \
  --items 100000 --days 365 --scopes 1 --repeats 4 --output .task/full-new
python3 benchmarks/daily-participation/benchmark.py \
  --items 100000 --days 365 --scopes 3 --repeats 3 --output .task/scopes-new
python3 benchmarks/daily-participation/uuid_sensitivity.py \
  --metadata .task/full-new/metadata.json --output .task/uuid-new
```

`--container` selects the fixture for the main script. The smaller pilot scales
U tiers with item count and is a functional check, not the full 60k-user sizing
case. Each output directory must be new. Scripts use only Python's standard
library and the container's ClickHouse client. `SYSTEM FLUSH LOGS` persists
query diagnostics; it does not flush database or operating-system caches.

Versioned [receipts](receipts/) contain server/client measurements, metadata,
row/storage counts and validation. Full executed SQL and binary ordered IDs are
retained under this worktree's `.task/full-100k-365-rounded/` and
`.task/full-100k-365-three-scopes/`; the original unrounded and pilot runs are
also retained. UUID SQL and returned IDs are in `.task/uuid-sensitivity/`. No database or artifact is released for deletion by this report.
