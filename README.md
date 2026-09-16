# searchkit

`searchkit` is a Go library for:

- **Keyword search and typeahead** over structured titles, aliases and keywords, using PostgreSQL exact indexes, PGroonga native-script prefix matching, and indexed `pg_trgm` typo candidates.
- **Semantic search** (language-specific embeddings) via pgvector `halfvec` stored in `embedding_vectors`.
- A **single, host-run worker loop** that:
  - consumes `search_dirty` notifications (changed/deleted entities),
  - runs resumable cursor-based backfill (no “insert 10M dirty rows”),
  - and drains `embedding_tasks` to compute/store embeddings.

This README is a **manual** for host applications. Design notes live in `agents/NOTES.md`.

## Keyword-first installation

The normal search path is keyword-only. An omitted `SearchOptions.Mode` uses
lexical retrieval. No embedder, provider credentials, vector extension or
ClickHouse connection is needed. Semantic/dual modes require explicit opt-in;
semantic search and recommendation work remain separate capabilities.

**Fresh keyword installations:** load `migrations.KeywordPostgres` into a new
`searchkit-keyword` migration group scoped to the host schema. This profile owns
three tables / 25 columns: documents (11), dirty queue (8), and backfill cursor (6).
It requires `pg_trgm` and PGroonga. Apply the complete profile, including its
additive keyword-fields migration, before using the current client.

**Existing combined installations:** keep `migrations.Postgres`, the original
`searchkit` migration group, and its original migration checksums. Run keyword
mode with no embedders and no `SemanticEntityTypes`; optional semantic tables and
data remain intact. Their eight-table footprint is not reduced by changing runtime
configuration. Do not switch a populated migration ledger between profiles or
mark a different baseline applied. Enabling semantic storage on a fresh keyword
installation needs a separate future provisioning/migration step; it is not a
profile toggle. No automatic table drop or semantic-data conversion is performed.

```go
client, err := searchkit.NewClient(searchkit.ClientConfig{
    Pool: pool, Schema: "doujins", DefaultLanguage: "en",
})
hits, err := client.Search(ctx, query, searchkit.SearchOptions{
    EntityTypes: []string{"gallery"}, // defaults to lexical
})
```

For indexing, construct `runtime.New` with `Pool`, `Schema` and
`BuildKeywordDocuments`, returning `pg.KeywordDocument{Title, Aliases, Keywords}`, then run `worker.SyncOnce` with lexical entity types and a
bounded ID-listing callback. A missing requested ID in a successful builder result
means the source entity no longer exists and deletes its old document; transient
failures must return an error. Explicit deletion also works without semantic
tables. Existing dirty revisions, writer serialization and retry rules apply.
The existing `BuildLexicalString` callback remains an adapter for hosts upgrading:
its entire string becomes the title until the host provides structured inputs.
Reindex through the dirty queue after adopting the structured callback; do not
rewrite an applied baseline or guess alias boundaries in concatenated old text.

### Keyword matching contract

Search and Typeahead share the same matcher for every language. Database-side
Unicode compatibility normalization, Latin accent folding, lowercasing, and whitespace folding are identical for index
and query; display strings and native characters remain intact. Japanese dakuten and
Hangul composition are preserved; `café`/`cafe` and `résumé`/`resume` match. Canonical exact
names rank above exact aliases, then literal token/prefix matches, then one-edit
matches. All query tokens are required. `not`, `OR`, hyphens and quotes are name
text/punctuation, not Boolean query syntax. Native CJK substrings can match inside
an unspaced name; Latin prefixes start at token boundaries.

Typos accept one Unicode insertion, deletion, substitution or adjacent
transposition per token. One/two-character terms require literal matches. This
is not transliteration, keyboard-layout correction, or a promise of complete
edit-distance recall: fuzzy retrieval first selects trigram candidates, so a typo
with no shared trigram can be missed. In particular very short native names may
need explicit aliases. Exact names and aliases have their own indexed candidate
route and do not compete with the fuzzy candidate limit.

Each exact, prefix and fuzzy route returns at most `min(MaxCandidateLimit,
max(100, Limit * 8))` documents before Go validates and ranks them. Host eligibility
filters execute inside every SQL route before its limit. Query limits are 256
characters and 16 tokens. Each request has a two-second ceiling, including SQL
and candidate scoring, or an earlier caller context deadline; transactions also
set a local server statement timeout. This is a resource bound, not a latency
promise. Structured input limits are 512 characters per name,
64 aliases, and 256 keywords; these also apply to legacy string adapters. Supply
structured fields instead of concatenating a long description. No full-catalog Go scan is used. Typeahead's
`MinSimilarity` now filters the documented match-tier score (exact title 1, alias
0.9, token/prefix 0.75–0.77, typo 0.5–0.52), not PGroonga's raw score. Search scores
remain RRF scores; opt-in traces show the `keyword` source and `keyword_match`
score. Quality/recall should be evaluated on the host's catalog before changing
candidate bounds or typo rules.

## The embedded hub (signal + discovery planes)

Beyond search, searchkit can run as an in-process **discovery hub**: it records per-user
interaction **signals** in ClickHouse and answers id-returning discovery queries — history, unseen,
view-context annotation, engagement, popularity/trending, personalized search, and
recommendations. The host hydrates ids → cards from its own DB; searchkit stores no presentation
data.

**Mechanism vs meaning:** searchkit owns storage/aggregation/queries. Entity types, signal types,
scoring weights, and completion rules are host-defined data — no business noun appears in the
schema.

### Setup

The hub needs the existing Postgres content plane plus a **dedicated ClickHouse database**:

```go
import (
	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/open-rails/searchkit"
	"github.com/open-rails/searchkit/signal"
)

ch, _ := clickhouse.Open(&clickhouse.Options{Addr: []string{"localhost:9000"}})

// Once at startup (DDL privileges required). Idempotent. Set Cluster for
// replicated/ON CLUSTER deployments.
_ = signal.EnsureSchema(ctx, ch, signal.SchemaOptions{Database: "hub"})

hub, _ := searchkit.NewEmbedded(searchkit.EmbeddedConfig{
	PG:           pgPool,
	PGSchema:     "hub",          // dedicated schema (NOT the host app schema)
	Embedder:     embedder,
	DefaultModel: "qwen3-embedding",
	CH:           ch,
	CHDatabase:   "hub",          // dedicated ClickHouse database
	Tenant:       "myapp",        // single implicit tenant embedded
	Scorers: map[string]signal.Scorer{
		// Host-defined: map a raw session to score/progress/completed.
		"blog_post": blogScorer, // e.g. read-time + scroll depth, completed >= 90%
	},
	Catalogs: map[string]searchkit.EntityCatalog{
		// Host-defined: the "universe" for Unseen, read from YOUR tables
		// with YOUR visibility/premium gating, newest first.
		"blog_post": blogCatalog,
	},
})
```

Omitting `CH` runs content-only: search/typeahead work, signal/discovery methods return
`ErrSignalPlaneDisabled`.

### Recording signals

One summarized event per session/interaction (exit-beacon style — never one row per scroll tick):

```go
_ = hub.RecordSignal(ctx, signal.Signal{
	EntityRef:   signal.EntityRef{EntityType: "blog_post", EntityID: "42"},
	Subject:     signal.Subject{UserID: userID},   // or AnonKey for anonymous
	Type:        "view",                            // host-defined
	DurationS:   180,
	Progress:    95, ProgressMax: 100,              // generic consumption units
	Resume:      "scroll:95",                       // opaque resume pointer
})
```

The registered `Scorer` for the entity type fills `Score`/`Progress`/`ProgressMax`/`Completed`.
Replayed events (same content or same `EventID`) deduplicate instead of double-counting. The default
`event_id` hashes `occurred_at` at nanosecond precision, so genuinely distinct same-second interactions
stay distinct; set an explicit `EventID` for idempotency independent of timing.

**Impression + attribution logging (learned-ranking training data).** Log one row per SERP/shelf render so
clicks can be attributed to what was shown. Call once per render (exit-beacon style, **never per item**);
clicks then carry that render's `query_id` + position via `WithAttribution`:

```go
qid := newQueryID() // unique per render
_ = hub.RecordImpressions(ctx, []signal.Impression{{
  QueryID: qid, Surface: signal.SurfaceSearch, NormalizedQuery: "two factor", Language: "en", Subject: user,
  Shown: []signal.EntityRef{ // in rank order; positions derived as StartPosition + index (default 1)
    {EntityType: "gallery", EntityID: "g1"},
    {EntityType: "gallery", EntityID: "g2"},
  },
}})

// On click, attach the render context to the click signal:
_ = hub.RecordSignal(ctx, signal.Signal{
  EntityRef: signal.EntityRef{EntityType: "gallery", EntityID: "g1"}, Subject: user, Type: "click",
}.WithAttribution(signal.Attribution{QueryID: qid, Surface: signal.SurfaceSearch, Position: 1}))
```

`NormalizedQuery` must be normalized text only (no raw referrers/PII). `WithAttribution` writes the
standardized `query_id`/`surface`/`position` payload keys training jobs join on. Paginated renders set
`StartPosition` to the page's first absolute position.

### Discovery reads (all id-returning)

```go
hist, _ := hub.History(ctx, user, signal.HistoryOptions{EntityType: "blog_post", Status: signal.HistoryInProgress})
fresh, _ := hub.Unseen(ctx, user, searchkit.UnseenOptions{EntityType: "blog_post", Limit: 20})
states, _ := hub.States(ctx, user, refs)          // bulk "seen? % read? resume?" for a page of cards
eng, _   := hub.Engagement(ctx, ref)              // unique subjects, completion rate, avg score
top, _   := hub.Popular(ctx, "blog_post", signal.PopularOptions{Window: signal.LastDays(30)})
slice, _ := hub.Popular(ctx, "blog_post", signal.PopularOptions{Window: signal.Between(a, b)}) // arbitrary date slices
recs, _  := hub.Recommend(ctx, user, searchkit.RecommendOptions{EntityTypes: []string{"blog_post"}})
```

- `Popular` merges a tiny daily rollup (`entity_daily`) for day-aligned windows and scans raw
  events for sub-day slices. Default ranking: log-scaled unique subjects × Bayesian-smoothed
  engagement; tune via `RankWeights` (equal time weight inside each window) or replace with a trusted
  `RankExpr`.
- `Recommend` fuses content similarity (seeded from the subject's high-signal entities) with
  co-engagement, excludes seen, and falls back to popularity on cold start. **Negative feedback
  demotes**: signals with `Value < 0` (e.g. a dislike) exclude an entity from seeds and results,
  count *against* co-engagement strength, and (in personalized search) apply a strong
  `DislikePenalty`. Co-engagement reads the precomputed `item_pairs` rollup when present — refresh
  it periodically via `hub.RefreshCoEngagement(...)` — and falls back to a query-time scan.
  Optional MMR diversity via `DiversityLambda` (uses stored embeddings; best-effort).
- `hub.Search(..., HubSearchOptions{Personalize: &searchkit.Personalization{Subject: user, DemoteSeen: true}})`
  blends candidate popularity into the ranking and demotes seen/completed entities — recall
  unchanged, ranking only, per-request toggle.
- `hub.SimilarTo(..., HubSimilarOptions{CoEngagement: true})` fuses vector neighbours with
  "subjects who engaged with X also engaged with Y".

**Maintenance (host-scheduled).** Two `Hub` maintenance methods run periodically
(cron-style): `hub.RefreshCoEngagement(...)` rebuilds the `item_pairs` rollup, and
`hub.ReprojectStaleStates(ctx, signal.StaleStateOptions{})` re-derives any current-state row that fell
behind the event stream after a crashed reprojection — idempotent, and a no-op when everything is current.

## Host app integration (manual)

### 1) Apply Postgres migrations (required)

searchkit migrations are applied/tracked with migratekit (`public.migrations`) under `app=searchkit`,
and are scoped to the host schema via `SET LOCAL search_path = <schema>, public`.

Note on PGroonga (CJK/Korean support):

- You must install the PGroonga extension package in your Postgres image for your Postgres major version (package names vary by distro).
  - Example (Debian/Ubuntu images): install `postgresql-<MAJOR>-pgroonga` from the PGDG/APT repo, then restart Postgres.
- The baseline migration runs `CREATE EXTENSION pgroonga`, which typically requires superuser (or elevated) privileges.
- If your environment can’t run `CREATE EXTENSION` from app migrations, install/enable PGroonga out-of-band, then apply the complete baseline to create its tables, functions and indexes.
- PGroonga is required for the current keyword matcher for every language. Provision it and apply additive migrations before upgrading the client.

```go
import (
	"context"
	"database/sql"

	"github.com/doujins-org/migratekit"
	"github.com/open-rails/searchkit/migrations"
)

func applySearchkitMigrations(ctx context.Context, sqlDB *sql.DB, schema string) error {
	migs, err := migratekit.LoadFromFS(migrations.Postgres)
	if err != nil {
		return err
	}
	m := migratekit.NewPostgres(sqlDB, "searchkit").WithSchema(schema)
	if err := m.ApplyMigrations(ctx, migs); err != nil {
		return err
	}
	return m.ValidateAllApplied(ctx, migs)
}
```

### 2) Optional semantic setup: create embedders

Use `embedder.NewOpenAICompatible(...)` with your provider’s OpenAI-compatible base URL + API key + model name.

For VL, the contract is URL-only (the host app provides presigned/public URLs).

**Instruction-trained query embeddings (optional).** Instruction models like Qwen3-Embedding expect queries as `Instruct: {task}\nQuery: {query}` while documents stay bare. Set a per-model task string on `runtime.Options.QueryInstructions` (model name → instruction); it is applied in `EmbedQueryText` **only** — documents are never prefixed, so no reindex is needed and `SimilarTo` (stored doc vectors) is unaffected. A missing or blank entry preserves current behavior.

```go
rt, _ := runtime.NewWithContext(ctx, runtime.Options{
  // ...pool, schema, embedders, callbacks...
  QueryInstructions: map[string]string{
    "qwen3-embedding": "Given a search query on an adult gallery site, retrieve matching galleries",
  },
})
```

### 3) Wire host callbacks (batch-first)

> ⚠️ **Changing (new design).** Pull-callbacks (`BuildLexicalString` / `BuildSemanticDocument` /
> `ListAssetURLs`) are being replaced by **push ingestion**: the host will call `UpsertEntity(...)`
> with the lexical/semantic text + asset URLs. Pull-callbacks only work *embedded*; push makes the
> embedded and server modes symmetric. See [`docs/api-surface.md`](docs/api-surface.md). The callback
> model below is how it works **today**.

Host apps provide:

- `runtime.BuildSemanticDocument(ctx, entity_type, language, []entity_id) -> map[id]string` (required only with semantic embedders)
  - Used to generate embeddings.
- `runtime.BuildKeywordDocuments(ctx, entity_type, language, []entity_id) -> map[id]pg.KeywordDocument`
  - Supplies canonical titles, aliases and contextual keywords. Searchkit owns all derived indexes.
  - `BuildLexicalString` remains a whole-title adapter for existing hosts.
- `vl.ListAssetURLs(ctx, entity_type, []entity_id) -> map[id][]AssetURL` (required only if VL models are enabled)

### 4) Mark changes (host writes `search_dirty`)

> ⚠️ **Changing (new design).** With push ingestion the host calls `UpsertEntity` and searchkit marks
> `search_dirty` internally — hosts will no longer write `search_dirty` directly. Current behavior
> below.

The host does **not** enqueue per-model tasks directly.
Instead, it upserts into `<schema>.search_dirty`:

- `(entity_type, entity_id, language, is_deleted, reason, updated_at)`

searchkit decides what to rebuild based on worker config + active model set.

### 5) Run one worker loop (host-owned, searchkit-provided)

Run a background worker (River/cron/goroutine) that calls:

- `worker.SyncOnce(ctx, rt, worker.SearchkitOptions{...})`

This single entrypoint:

1) processes `search_dirty`,
2) runs bounded backfill for missing docs/embeddings,
3) drains `embedding_tasks` (does provider calls and writes `embedding_vectors`).

Apply migration `0002_search_dirty_revision.up.sql` before deploying the worker.
Its trigger assigns a new sequence-backed revision on every dirty insert/update,
including equal-timestamp UPSERTs. Hosts continue writing the existing columns.

`SyncOnce` permits one lexical writer per database/schema. A competing tick
returns without work. Documents and queue acknowledgements commit together;
changed generations are skipped after the callback and remain queued. Backfill
adds IDs to this same queue, so new backfill documents appear on a subsequent tick.

The pool must have at least two connections. Document/list callbacks must be
bounded, read-only and respect cancellation; one transaction/connection remains
open across the tick. Hosts must mark catalog changes dirty in the same transaction
as the catalog write. Direct document writers must not race this worker. These
changes do not fence semantic embedding provider workers; that lifecycle remains
separate.


### 6) Query candidates (lexical + semantic)

Recommended entrypoint:

- Create a SearchKit client once and reuse it:

```go
client, err := searchkit.NewClient(searchkit.ClientConfig{
  Pool:            pool,
  Schema:          "doujins",
  DefaultLanguage: "en",
})
```

Then per request:

```go
hits, err := client.Search(ctx, userQuery, searchkit.SearchOptions{
  Language: "en",
  LanguageMode: searchkit.LanguageModeExact, // exact|fallback_en (default exact)
  Mode:     searchkit.SearchModeLexical, // default; semantic/dual are opt-in
  EntityTypes: []string{"gallery"},
  Limit:          20,  // final fused results
  CandidateLimit: 100, // per-source candidates before RRF; defaults to Limit
})
```

Search scores and semantic confidence are different domains:

- `SearchHit.Score` is an RRF score derived from source ranks. It is not cosine similarity.
- `SemanticMinSimilarity` filters raw cosine similarity before RRF.
- `SemanticMinSimilarityEnabled` makes an explicit zero floor inclusive, retaining zero-similarity candidates while dropping negative candidates.
- `SimilarOptions.MinSimilarityEnabled` provides the same explicit zero-floor behavior for nearest-neighbor recommendations.
- `CandidateLimit` controls each source's retrieval depth; `Limit` controls the final response size.
- `OversampleFactor` controls only the binary-quantized first stage when `TwoStage=true`. Values `<=1` use the effective default `5`.

For offline evaluation and diagnostics, use the opt-in traced call:

```go
hits, trace, err := client.SearchWithTrace(ctx, userQuery, opts)
```

The trace records effective routing/configuration, source candidates and score domains, and exact RRF contributions. On failure, it contains the work completed before the error. Ordinary `Search` does not collect trace candidates.

Traces contain normalized query text and candidate entity IDs. Treat them as potentially sensitive evaluation artifacts: do not log them indiscriminately, attach them to user-visible errors, or expose them from public APIs.

Typeahead suggestions while typing:

```go
hits, err := client.Typeahead(ctx, userQuery, searchkit.TypeaheadOptions{
  Language: "en",
  LanguageMode: searchkit.LanguageModeExact, // exact|fallback_en (default exact)
  EntityTypes: []string{"tag", "artist", "series"},
  Limit:    10,
  MinSimilarity: 0.3,
  FilterSQL:  "EXISTS (SELECT 1 FROM app.entities e WHERE e.id::text = sd.entity_id AND e.deleted_at IS NULL)",
  FilterArgs: map[string]any{},
})
```

Host-injected filters:

- `FilterSQL` and `FilterArgs` are supported on both `SearchOptions` and `TypeaheadOptions`.
- SearchKit applies these filters inside retrieval queries (before ranking/pagination) for lexical and semantic search paths.
- Treat `FilterSQL` as trusted host SQL only. Never concatenate raw user input into it; pass values through `FilterArgs`.
- This keeps SearchKit schema-agnostic: each host can enforce visibility/business constraints with host-specific SQL (including joins/EXISTS).
- In dual mode, one `FilterSQL` fragment is applied to both lexical (`sd`) and semantic (`ev`) queries. The fragment must therefore be valid in both SQL scopes. If policy needs backend-qualified aliases, issue explicit lexical/semantic calls with the matching fragment until channel-specific filter options are available.

Language strictness:

- `LanguageModeExact` (default): query only requested language.
- `LanguageModeFallbackEnglish`: query requested language and English in one call.
- Language mode is applied inside SearchKit retrieval (before ranking/pagination), not as post-filtering in host app code.

Language-specific routing (handled inside the client):

- Search and Typeahead use the shared keyword matcher described above, regardless of language. Low-level FTS, trigram and PGroonga APIs remain available for explicit specialist callers.

Query syntax notes:

- SearchKit does **not** treat leading `-term` as an operator. Leading `-` is treated as punctuation (so `-factor` behaves like `factor`).
- For Postgres FTS (`websearch_to_tsquery`), SearchKit normalizes intra-token hyphens to spaces so tokens like `two-factor` behave like `two factor`.
- Natural-language negation: for FTS only, `not X` is rewritten to `-X` before it reaches Postgres. This is a convenience for users typing normal phrases like `X not Y`.

## Offline search evaluation

Package `searchkit/eval` provides host-neutral golden-query evaluation:

- graded judgments (`0..3`) and exact-empty cases;
- recall@K, success@K, MRR@K, nDCG@K, and result-count metrics;
- versioned reports with dataset, suite, and candidate identities;
- caller-configured baseline tolerances;
- exact score-floor candidate generation and sweeps isolated by score domain.

Hosts still own corpus snapshots, business-policy fixtures, query execution, and production acceptance thresholds. Execution failures must be represented with `eval.Failed`; they are never treated as successful empty results.

```go
suite, err := eval.ParseSuite(fixture)
if err != nil {
  return err
}

outcome, err := eval.Evaluate(suite.Cases[0], []eval.Result{
  {Key: eval.GoldenKey{EntityType: "gallery", EntityID: "42"}, Score: 0.81},
})
if err != nil {
  return err
}

report, err := eval.BuildReport(eval.ReportIdentity{
  DatasetID: "corpus-snapshot-sha256",
  SuiteID: "suite-sha256",
  CandidateID: "searchkit-and-config-sha256",
}, []eval.Outcome{outcome}, "suite")
```

**Running a suite against a live client.** `eval` is dependency-free; wire your client behind `eval.CaseRunner` and let `eval.RunSuite` execute every case and build one report. `searchkit.NewEvalRunner` adapts a `*searchkit.Client` (each case's query → `client.Search` → results):

```go
runner := searchkit.NewEvalRunner(client, searchkit.SearchOptions{
  Mode: searchkit.SearchModeDual, Language: "en", SemanticMinSimilarity: 0.5,
})
report, err := eval.RunSuite(ctx, suite, runner, eval.ReportIdentity{
  DatasetID: "corpus-sha", SuiteID: suite.ID, CandidateID: "dual+floor0.5",
}, "query_type") // optional group-by labels → per-label breakdowns
```

**Regression gate.** Commit a baseline report as a golden file, then fail CI when quality drops beyond tolerance:

```go
comparison, err := eval.Compare(baseline, report, eval.Tolerances{
  RecallAtKDrop: 0, SuccessAtKDrop: 0, NDCGAtKDrop: 0.05, ExactEmptyRateDrop: 0,
})
if comparison.Regressed() { /* fail the test */ }
```

**Config diffing.** Run the suite under two `SearchOptions` (e.g. floor on vs off) with different `CandidateID`s and `Compare` the reports to pick a setting from measured nDCG/recall rather than by feel; `eval.CandidateFloors` + `eval.SweepResultFloors` sweep a semantic-score floor per score domain.

Quality metrics exclude execution failures from their denominators, while `FailedCases` remains explicit. `QualityStatus` distinguishes hits, misses, exact emptiness, unexpected results, and unjudged cases. Pass only stable lowercase identifier categories such as `timeout` or `semantic_search` to `eval.Failed`; unsafe categories normalize to `unspecified`. Reports include a deterministic content identity covering report identity, outcomes, ordered results, and scores. Baseline comparison validates that identity, report aggregates, and exact case definitions before comparing metrics. Floor sweeps require every outcome, including failures, to carry one matching `score_domain` label and accept `context.Context` for cancellation.

Before releasing changes to search retrieval or evaluation, run the PostgreSQL integration gate in addition to normal tests. Point it at a disposable test database: the test creates extensions and an isolated uniquely named schema, then removes that schema.

```bash
SEARCHKIT_TEST_URL='postgres://...' go test -count=1 -run TestClientSearch_Integration_LexicalAndSemantic .
```

Host integration details (contract, filter-builder patterns, hentai0/doujins examples):

- See `HOST_INTEGRATION.md`.

## Language → Postgres FTS config mapping

FTS uses a schema-local function created by migrations:

- `<schema>.searchkit_regconfig_for_language(language)`

It maps common codes like `en/es/fr/de/...` to built-in configs and falls back to `simple`.

## Model registry + ANN indexes

Construct the runtime via `runtime.NewWithContext(...)` to:

- upsert the configured model set into `<schema>.embedding_models`, and
- ensure per-model cosine + binary HNSW indexes exist (via `CREATE INDEX CONCURRENTLY`).

### Popularity window semantics

Popularity and card viewer counts use only `view` events. Clicks and reactions
remain stored as separate signals. All qualifying views inside the requested
window have equal time weight; `RankWeights.HalfLifeDays` has been removed in
this pre-v1 change. No current Doujins/Hentai0 host call used it. A zero engagement
score is a valid view observation, not a missing score.

Until the daily projection has view-only, retry-safe aggregates, these reads use
`signal_events FINAL`. This fixes duplicate-delivery and mixed-signal correctness
but scans more data than a daily rollup; qualify production query cost before
large-scale rollout. Do not expire these source events before the replacement
projection and durable compact-history design can answer the same queries.
Retries must still preserve both event ID and occurrence time: changing either
can represent a different storage key. This patch does not implement session
revision reconciliation, compaction, or the future explicit-feedback formula.
