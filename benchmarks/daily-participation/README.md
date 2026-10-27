# Daily participation experiments

At 100,000 items over 365 days, combining **already reduced daily counts** into
one complete ranking took **60–889 ms of ClickHouse server time** in this
experiment, depending on the window. Each result contained all 100,000 ordered
item IDs. Three overlapping synthetic scopes and four buckets per scope were
built and persisted in 4.665 seconds of cumulative server time.

That first experiment isolates a prototype scalar combination stage. Later
sections separately measure the actual Store's daily receipt reduction and its
String-key, `FINAL` ranking query. None is an end-to-end HTTP/catalog benchmark.
The earlier reported 107-second
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
  hashes across identical runs. Decoding twelve additional unrounded 7-day
  runs found **identical ordered IDs in all twelve**; seven runs differed only
  in score low bits, by at most 4.34e-19. The original first two outputs retain
  hashes only, so their ID ordering cannot be reconstructed. There is no
  observed ordering flip in that UInt32 experiment. The later actual String-key
  Store experiment below did reproduce ordering changes and motivated the
  implemented precision rule. These are separate observations; original
  comparisons remain in `.task/` and
  `receipts/unrounded-repeat-comparison.json`.
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

## Actual Store ranking over 365 days

This experiment calls **`signal.Store.DailyPopular`**, using the exact migration
0006 `ReplicatedReplacingMergeTree` schema: UUID-formatted **String** content IDs,
UInt64 U/N counts and replacement versions, with N on each item/day row. It
contains 100,000 items × 365 days, the same skew as the scalar prototype, and
100,000 replacement rows for the latest day. Those replacements add five readers
per item. A `FINAL` truth check confirms **36,500,000 canonical rows** and
**219,500,000 total readers**; physical rows at the first measurement were
36,616,384. Every query returns the complete 100,000-item order, not a short
top-k sample.

Only this newly created benchmark table's background merges were paused during
the initial overlap measurement, then resumed. Shared tables and caches were
untouched. The initial query is the unrounded implementation at `2a480be9`,
unchanged at `3772fe78`.

| Actual Store window | Server time, three runs | Driver wall time | Logical bytes read | Peak memory |
|---|---:|---:|---:|---:|
| 7d | 167, 175, 192 ms | 0.634–1.206 s | 55.73 MB | 74.5 MiB |
| 30d | 299, 292, 339 ms | 0.754–1.345 s | 207.55 MB | 76.9 MiB |
| 365d | 1,835, 2,189, 2,330 ms | 2.295–3.386 s | 2,418.42 MB | 75.2 MiB |
| all (365d) | 1,805, 2,111, 2,292 ms | 2.335–3.318 s | 2,345.62 MB | 75.0 MiB |

The original driver's wall timer includes writing its full JSON result and
checking sample scores. Server time comes from `system.query_log` and excludes
that local work. The 365-day queries consumed 3.55–4.55 CPU-user seconds plus
0.07–0.10 CPU-system seconds under **two query threads**. `FINAL` and String keys
cost more than the earlier UInt32/UUID prototype; the prototype's subsecond
yearly number should not be attributed to this implementation.

### Repeated ordering and score precision

Sixteen further actual Store calls used the **same unchanged logical dataset**
in `ck261_actual_20261026_212751`, with ordinary background merges enabled. Three
unrounded runs changed **100, 41 and 100 ID positions** relative to the first
run. For example, position 7,217 changed from item suffix `016bde` to `016c20`.
Both have an independently calculated exact score of **10,355 / 21,907,300**.
Both initially returned `0.0004726734923975113`; a later run returned
`0.00047267349239751144` and `0.0004726734923975115`, respectively. These low-bit
differences altered the ID order of mathematically tied items. This is a real
ordering change, unlike the earlier UInt32 hash-only variation.

The implementation change at **`1e8f332b`** applies
`round(avg(readers / (eligible + 20.0)), 12)` before the existing ID tie breaker.
Repeating the actual method another **16 times on the same database** produced
all **100,000 unique, correctly sorted IDs and scores identically in every
run**. The ordered-ID SHA256 was
`e71bd9bf9a03b4ea823812409d4e249c72dfabfc84bc0e12ec29d86e468e2e01`.

| Same-data repeated query | Server time | Driver wall time | Method-only wall time | Peak memory | CPU user / system |
|---|---:|---:|---:|---:|---:|
| Unrounded, 16 runs | 1.997–3.502 s | 2.681–4.305 s | not separately captured | 49.4 MiB | 3.87–6.80 / 0.08–0.18 s |
| Rounded, 16 runs | 1.588–2.165 s | 1.889–2.528 s | 1.638–2.216 s | 49.4 MiB | 3.08–4.17 / 0.06–0.11 s |

Each read 36.5m rows and **2.336 GB logical bytes**. Background merges had
already removed physical duplicates in this repeated-query phase; the earlier
table above separately exercises overlapping replacements. These timings were
observed under varying concurrent host load, so the lower rounded timings are
**not evidence of a rounding speedup**. Rounded runs reported zero physical
`OSReadBytes`; unrounded runs reported 0–86,016 bytes. Both are cache-assisted
measurements, not controlled cold-disk tests.

The versioned `receipts/actual-*` files retain every query ID, server timing,
CPU/read/memory counter, changed ID/score pair and full-result hashes. The large
full ordered result files remain in `.task/actual-rank-365-v2/`,
`.task/actual-unrounded-repeats-v2/` and `.task/actual-rounded-repeats/`.

```sh
GOWORK=off go build -p 1 -o .task/actual-rank benchmarks/daily-participation/actual_rank/main.go
GOWORK=off go build -p 1 -o .task/repeat-rank benchmarks/daily-participation/repeat_rank/main.go
. /path/to/owned/fixtures.env
.task/actual-rank --days 365 --output .task/actual-rank-new
# Use the newly created ck261_actual_* database printed above:
.task/repeat-rank --database ck261_actual_YYYYMMDD_HHMMSS --output .task/repeats-new --repeats 16
```

Record `git rev-parse HEAD` with a new run. The current checkout's actual Store
method is compiled into these drivers; they do not carry a replacement ranking
SQL implementation. This phase generates reduced daily rows directly. It does
not ingest a year of receipts, hydrate catalog cards or establish HTTP latency.

## Actual source receipts to daily preparation

A second experiment calls the implementation's **actual**
`signal.Store.PrepareDailyPopularity` and `DailyPopular` methods at source
`d5e6dc73` plus `2a480be9`. It does not substitute a benchmark SQL formulation.
It uses the exact relevant table definitions from that source's migrations,
with a new isolated database on the same ClickHouse/Keeper fixture.
The later `f6761101` source change moves immutable access/domain/language
filters before receipt aggregation. The raw timings below predate that change
and do not measure its performance or the complete multi-scope host pipeline.

One historical UTC day contains:

- 60,000 people, ten distinct works per person: **600,000 person/work-days**.
- Five reads per person are drawn from 1,000 hot items, five from the remaining
  99,000. IDs are UUID-shaped strings matching the actual schema's String keys.
- Three stable receipt identities per visit, two cumulative revisions each:
  **3,600,000 delivered source rows**, **1,800,000 canonical receipts**.
  Revision 2 arrives before revision 1 initially. Full replay inserts them in
  the opposite order using the same identities.
- 90% of people have 400 ms per final receipt, totaling 1,200 ms per edition;
  10% have only 300 ms total and do not qualify. Every receipt has a nonempty
  coverage array. This intentionally exercises qualification, not opening.
- Half the available items are public; half require Gold. Independent audience
  observations cover all 60,000 people. Half gain Gold in their second same-day
  observation. Each of the two observations is delivered twice: **240,000
  population rows**. Public N is 60,000; Gold N is 30,000.

All **100,000 daily item U/N pairs** were checked against an independently
calculated Go oracle before and after replay/rebuild. The resulting eligible,
qualified participation totals **360,000** person/item-days; 60,000 items have a
positive count. The later raw `FINAL` read confirms 1.8m receipts, 600k distinct
person/item pairs, 60k people, and 666m active milliseconds. Population `FINAL`
contains 120k distinct observations for 60k people. Replaying the same sources
changed none of those canonical values or prepared counts.

| Actual method phase | Wall time | Main SQL time | Logical rows / bytes read | Query peak memory |
|---|---:|---:|---:|---:|
| First daily preparation | **34.860 s** | 34.083 s | 2.34m / 780.26 MB | 357.4 MiB |
| Reuse finalized day | **14 ms** | 12 ms | 1 / 103 bytes | 0.86 MiB |
| Reuse after full reordered replay | **25 ms** | 21 ms | 1 / 103 bytes | 0.86 MiB |
| Explicit rebuild after replay | **47.113 s** | 45.377 s | 4.14m / 1,529.06 MB | 370.5 MiB |
| Rank that prepared day, 60k positive IDs | **233 ms** | 182 ms | 100k / 6.60 MB | 29.4 MiB |

Initial preparation's main query consumed **58.85 CPU-user seconds + 2.34
CPU-system seconds**, consistent with two parallel threads over about 34 wall
seconds. Rebuild consumed 79.47 + 3.05 CPU seconds. The initial method issued
three statements: checkpoint read, actual projection INSERT, checkpoint write.
Closed reuse issued **one checkpoint read and no raw-source query**. Explicit
rebuild added a synchronous day deletion (85 ms), as intended; ordinary first
preparation did not delete existing rows.

`ReplacingMergeTree` background merges run normally. Consequently query-log
read counts can be below delivered source counts, and the replayed rebuild saw
more unmerged rows than the initial preparation. No merges or caches were
stopped or flushed to manufacture either timing. These measurements are a
single full-day run plus replay/rebuild, not a distribution of repeated daily
loads. They show a roughly 35-second once-per-day preparation cost for these
inputs, followed by cheap finalized reuse. They do not establish that 365-day
initial raw backfill is inexpensive or that all possible audience cohorts have
this cost.

Synthetic source insertion itself is separate: twelve bounded 300k-row INSERTs
took 20.411 seconds wall and delivered 3.6m rows; population insertion took
1.068 seconds. A first attempted single INSERT exceeded the **1 GiB** query
limit during fixture generation, before the projection method ran. Its failed
receipt/database remain retained. The successful driver bounds setup and does
not increase the production query limit. The earlier 1k-person pilot also
verified all 100k available-item rows and took 1.619 seconds to prepare.

Run the actual-method phase after integrating the cited source commits:

```sh
GOWORK=off go build -p 1 -o .task/raw-daily benchmarks/daily-participation/raw/main.go
. /path/to/owned/fixtures.env
.task/raw-daily --users 1000 --output .task/raw-pilot-new
.task/raw-daily --users 60000 --output .task/raw-full-new
```

The driver uses `CONTENTKIT_CRITICAL_CH_ADDR`, `_USER`, and `_PASSWORD` without
printing them. It creates a unique database and retains it, records every real
query ID/SQL and phase result, and obtains server metrics from `system.query_log`.
It performs direct synthetic table inserts; it does **not** benchmark the HTTP
receipt ingestion path, authorization, media serving or a production migration.
Versioned `receipts/raw-*` preserve the full successful run; `.task/raw-*` also
contains the pilot and failed setup attempt.

### Updated projection on the retained replayed dataset

After the immutable access/domain/language filter change at **`f6761101`**, one
actual explicit rebuild reused database `ck261_raw_20261026_211659`. It inserted
no source or population rows and created no replacement fixture. The driver
checked **all 100,000 U/N pairs before and after** the rebuild: 360,000 qualified
item/person-days, public population 60,000 and Gold population 30,000 remained
exactly correct. The resulting rounded ranking also matched the count oracle
for all 60,000 positive items.

| Actual method phase | Method wall time | Main SQL time | Logical rows / bytes read | Query peak memory |
|---|---:|---:|---:|---:|
| Explicit rebuild at `f6761101` | **13.325 s** | **13.065 s** | 4.14m / 1,529.06 MB | 357.4 MiB |
| Reuse finalized day | **7.55 ms** | 6 ms | 3 / 309 bytes | 1.45 MiB |
| Rank prepared day, 60k IDs | **111 ms** | 93 ms | 100k / 6.60 MB | 29.4 MiB |

The projection's logical row and byte counts **exactly matched the earlier
47.113-second replay rebuild**. Background merging did not reduce the recorded
input volume in this comparison. CPU time was **24.225 user + 1.013 system
seconds**, versus the earlier 79.47 + 3.05 seconds. The explicit day deletion
took 68 ms; reuse issued only the checkpoint query. Its three checkpoint rows
are replacing versions of the same completed day, not three raw scans.

This is one observed before/after result on a shared host, not a throughput
guarantee. The new projection reported 79.3 MB physical `OSReadBytes` versus
1.529 GB logical reads; no cache was dropped, so it is not a controlled cold
measurement. The fixture's receipts all belong to the selected `main/en` scope:
this verifies the changed actual method and qualification result, but does not
measure a 23-scope host refresh or benefit from excluding out-of-scope traffic.
It also omits PostgreSQL history loading and full host/catalog preparation.

```sh
GOWORK=off go build -p 1 -o .task/raw-daily benchmarks/daily-participation/raw/main.go
. /path/to/owned/fixtures.env
.task/raw-daily --existing-database ck261_raw_20261026_211659 --users 60000 \
  --implementation f67611014860792bc07670f5155f21370dc197ce \
  --output .task/raw-existing-new
```

`--existing-database` skips schema creation, source insertion and replay. It
performs a deliberate rebuild of that owned synthetic database and then verifies
finalized reuse; use the original fixture's user count. The versioned
`receipts/raw-f676-*` preserve this run, including exact SQL and query IDs.

## Environment and measurement limits

Measured 2026-10-26 in the task-owned `ck254-contracts-ch` fixture, ClickHouse
**25.8.33.6**, UTC, Linux x86-64, Intel **Core i9-13950HX**, 32 logical CPUs.
The scalar experiment used **max_threads=2**, **max_insert_threads=2**, and
max_memory_usage=2 GiB. The actual-source daily method and its driver used two
threads and a **1 GiB** query limit, with external grouping/sort thresholds at
128 MiB. The host had **62.5 GiB physical RAM** (67,097,427,968 bytes); the recorded
post-run snapshot had about 39 GiB available. Container CPU/memory were otherwise
uncapped. Other
workspace work shared the host, so these are observed timings, not an isolated
capacity guarantee.

No cache dropping, service restart, data-volume reset, or production mutation
occurred. **These are not controlled cold-disk measurements.** The first run
followed inserts and therefore benefited from filesystem cache; 365-day ranking
query logs reported zero physical `OSReadBytes` while still reading roughly
184 MB through filesystem buffers. Subsequent runs are warm. Query-log read
bytes, memory, CPU counters, cache counters, result counts, and client wall time
are retained rather than inferred from a stopwatch alone.

The prototype's final rounded result sets were checked for 100,000 unique IDs, score-descending /
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
