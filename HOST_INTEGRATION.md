# SearchKit Host Integration Guide

This guide defines the host-facing contract for embedding SearchKit in apps such as:

- `hentai0` (video search)
- `doujins` (gallery search)

SearchKit is a library. Hosts own HTTP routes, auth, and response shaping.

## Contract

Hosts should only use:

- `client.Search(ctx, query, searchkit.SearchOptions{...})`
- `client.SearchWithTrace(ctx, query, searchkit.SearchOptions{...})` for offline evaluation/debugging
- `client.Typeahead(ctx, query, searchkit.TypeaheadOptions{...})`

An omitted `SearchOptions.Mode` defaults to lexical keyword search. Semantic and
dual modes require explicit opt-in and optional storage/provider configuration:


- `searchkit.SearchModeLexical`
- `searchkit.SearchModeSemantic`
- `searchkit.SearchModeDual`

Do not call low-level `searchkit/search` package APIs from host app code for normal request paths.

## Filter Policy (Host-Owned)

Hosts inject trusted SQL via:

- `FilterSQL string`
- `FilterArgs map[string]any`

Rules:

- Never concatenate raw user input into `FilterSQL`.
- Use named args in `FilterArgs`.
- Apply auth/business policy in host code and pass the resulting filter into SearchKit.
- For `SearchModeDual`, the same fragment runs in lexical (`sd`) and semantic (`ev`) SQL scopes. Use only a backend-neutral fragment. If a filter must reference those aliases, run explicit lexical and semantic calls with their respective fragments.

SearchKit applies filters in retrieval queries before ranking/pagination.

## Candidate Depth and Semantic Confidence

`SearchOptions` separates three controls:

- `Limit`: maximum final RRF-fused hits returned to the host.
- `CandidateLimit`: maximum candidates requested from each lexical/semantic source before RRF. It defaults to `Limit` and is clamped to at least `Limit`.
- `SemanticMinSimilarity`: positive raw cosine-similarity floor applied to semantic candidates before RRF. Values `<= 0` disable the additional floor unless `SemanticMinSimilarityEnabled` is true; NaN and infinities are rejected. Enable an explicit zero floor to retain candidates with cosine similarity exactly zero while dropping negative candidates.

`OversampleFactor` is independent: with two-stage semantic retrieval it controls the approximate stage-one width (`CandidateLimit * OversampleFactor`) before exact cosine rescore. Values `<= 1` use effective factor `5`.

Do not interpret `SearchHit.Score` as semantic confidence. It is an RRF rank score. Use `SearchWithTrace` to inspect raw source scores and their explicit score kinds. Trace collection is opt-in and returns partial provenance alongside errors.

Search traces include normalized query text and candidate IDs. Store them only in access-controlled evaluation artifacts; do not emit them wholesale to routine logs or user responses.

An explicitly configured `CandidateLimit` is capped at 10000. Existing `Limit`, `RRFK`, and `OversampleFactor` behavior remains compatible; two-stage multiplication and RRF arithmetic are checked or computed without integer overflow.

## Evaluation Ownership

SearchKit's `eval` package owns generic cases, metrics, reports, comparisons, and score-floor sweeps. Hosts own:

- immutable corpus/dataset identity;
- business-specific judgments and visibility policy;
- executing SearchKit and end-to-end host pipelines;
- cache isolation and failure policy;
- selecting production thresholds from measured sweeps.

Keep score domains separate when sweeping. In particular, do not combine cosine similarity, FTS rank, trigram similarity, PGroonga score, and RRF score.

## Language Strictness

Hosts pass request language explicitly (`Language`) and choose `LanguageMode`:

- `searchkit.LanguageModeExact` (default): requested language only.
- `searchkit.LanguageModeFallbackEnglish`: requested language + English.

For strict language behavior, set `LanguageModeExact` and keep host hydration/read-model queries strict as well (no implicit English fallback).

## Example: hentai0 (video search)

```go
const videoFilterSQL = `
EXISTS (
  SELECT 1
  FROM hentai0.videos v
  JOIN hentai0.video_versions vv ON vv.video_id = v.id
  WHERE v.id::text = sd.entity_id
    AND v.deleted_at IS NULL
    AND vv.deleted_at IS NULL
    AND (vv.live_at IS NULL OR vv.live_at <= NOW())
    AND v.default_version_id::uuid = vv.id::uuid
)`

hits, err := searchkitClient.Search(ctx, query, searchkit.SearchOptions{
  Language:     language,
  LanguageMode: searchkit.LanguageModeExact,
  Mode:         searchkit.SearchModeLexical,
  EntityTypes:  []string{"video"},
  Limit:        100,
  FilterSQL:    videoFilterSQL,
})
```

Host then hydrates `EntityID` values into API response cards.

## Example: doujins (gallery typeahead)

```go
filterSQL := `
EXISTS (
  SELECT 1
  FROM doujins.galleries g
  JOIN doujins.gallery_i18n gi
    ON gi.gallery_id = g.id
   AND gi.language = @language
  LEFT JOIN doujins.gallery_i18n_versions giv
    ON giv.id = gi.default_version_id
  WHERE g.id::text = sd.entity_id
    AND (@show_soft_deleted OR g.deleted_at IS NULL)
    AND (@show_soft_deleted OR gi.deleted_at IS NULL)
    AND (@show_soft_deleted OR giv.deleted_at IS NULL OR giv.id IS NULL)
    AND (@show_drafts OR giv.live_at IS NOT NULL OR giv.id IS NULL)
    AND (@show_future OR giv.live_at <= NOW() OR giv.id IS NULL)
)`

hits, err := searchkitClient.Typeahead(ctx, query, searchkit.TypeaheadOptions{
  Language:     language,
  LanguageMode: searchkit.LanguageModeExact,
  EntityTypes:  []string{"gallery"},
  Limit:        12,
  FilterSQL:    filterSQL,
  FilterArgs: map[string]any{
    "language":          language,
    "show_soft_deleted": permissionOptions.ShowSoftDeleted,
    "show_drafts":       permissionOptions.ShowDrafts,
    "show_future":       permissionOptions.ShowFuture,
  },
})
```

Host then resolves IDs to gallery payloads and applies response formatting.

## Attribution Export (paged evaluation data)

`hub.Attribution(ctx, signal.AttributionOptions{Stage, Window, Surface, Limit, ClickLimit, After})`
exports one deterministic sequence per (stage, window, surface):

1. renders at the stage in `RenderID` order, each with its canonical clicks in
   `(OccurredAt, entity, subject, EventID)` order;
2. then the `Unattributed` clicks (render has no exposure at that stage) in
   `(render id, OccurredAt, entity, subject, EventID)` order.

Cursor contract:

- Every page holds at most `Limit` renders (default 500) and `ClickLimit` click rows, attributed and
  unattributed together (default 5000). Both bounds may change between pages.
- `Next` is an opaque token positioned exactly after the last row emitted. Pass it back unchanged as
  `After` with the same `Stage`, `Window` and `Surface`; a token from another export or a malformed
  token is rejected with an error. Empty `Next` means the export is complete; a non-empty `Next`
  always leads to a non-empty page.
- A render whose clicks do not fit continues on the next page: its header repeats with
  `Continued: true` and only the remaining clicks. Merge by `RenderID`. Zero-click renders are
  emitted once, complete.
- Once a page carries `Unattributed` rows no later page carries renders.
- Concatenating all pages at any bounds yields exactly the single-page result: no row is lost or
  duplicated. Rows written or revised during the walk may fall before or after the cursor; export a
  closed `Window` for reproducible datasets.
- Clicks are signals of type `signal.TypeClick` carrying `render_id`/`position`
  (`Signal.WithAttribution`); clicks without a render id are not exported. Erased subjects are
  excluded through the erasure ledger at read time, not only by deletion.

## Subject Erasure Completion Contract

`hub.EraseSubjects(ctx, subjects)` (account deletion) returns an `ErasureReport`; `Complete()` is
true only when all of the following hold, and the guarantee then survives restarts, other
processes, delayed jobs and restores:

1. The erasure is recorded in the `erasures` ledger with `insert_quorum` = every replica of the
   ledger table. If a replica is down the call fails fast (`TOO_FEW_LIVE_REPLICAS`), records
   nothing and deletes nothing; retry later. Never treat an error as deletion.
2. Every row of the subjects that existed when the call ran is deleted from events, compact state,
   daily contributions, exposures and legacy raw tables on every replica (`mutations_sync = 2`),
   re-counted as zero (up to three delete-and-verify passes), and co-engagement pairs touching
   entities they contributed to are removed.
3. From the moment the ledger row is durable, every write, read, projection and export evaluates
   the ledger inside its own ClickHouse statement: `RecordSignals`/`RecordExposures` drop the
   subjects' rows server-side; `States`, `History`, `SeenIDs`, `NegativeIDs`, `TopStates`,
   `Metrics`, `Popular`, `PopularityFor`, `CoEngaged`, `Attribution`, `Inventory` never return them;
   `RecordSignals`' projections, `RepairProjections` and `RefreshCoEngagement` never derive from
   them (a co-engagement build re-verifies the ledger around itself and rebuilds if an erasure
   landed meanwhile).

The subject key is dead in that tenant forever: later signals and exposures for it are dropped, and
writes are never accepted under a "new epoch". User ids are never reused; rotate anonymous keys
instead of reusing one after erasing it. A shared account exists in every tenant: each host erases
its own tenant.

Residue (rows that landed physically after the fence, or rows restored from a backup) is
unreadable by (3). `hub.EnforceErasures(ctx)` deletes it for every recorded erasure of the tenant;
there is no cursor, so nothing is skipped. Schedule it (daily is enough) and run it after every
restore, after re-erasing the subjects deleted since the backup (the host's deletion ledger is
authoritative; the `erasures` table is restored with the backup). A non-`Complete()` report from it
means rows remained after three passes: investigate, do not ignore.

Host wiring: call `EraseSubjects` from account deletion and treat only `Complete()` as done;
schedule `EnforceErasures`, `RepairProjections` and `RefreshCoEngagement`; keep the host deletion
ledger for restores. Multi-replica behaviour is qualified on two replicas sharing one Keeper; a
production cluster must be qualified in place.

## Migration Checklist

- Create and reuse a single `searchkit.Client`.
- Remove app-local lexical backend branching (FTS/PGroonga selection).
- Move app business filtering to host filter-builders (`FilterSQL/FilterArgs`).
- Keep app code to request validation, policy mapping, and hydration.
