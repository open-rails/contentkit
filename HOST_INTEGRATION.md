# SearchKit Host Integration Guide

This guide defines the host-facing contract for embedding SearchKit in apps such as:

- `hentai0` (video search)
- `doujins` (gallery search)

SearchKit is a library. Hosts own HTTP routes, auth, and response shaping.

## Contract

Hosts should only use:

- `client.Search(ctx, query, searchkit.SearchOptions{...})` → `SearchResult{Hits, HasMore, Truncated}`
- `client.SearchWithTrace(ctx, query, searchkit.SearchOptions{...})` for offline evaluation/debugging
- `client.Typeahead(ctx, query, searchkit.TypeaheadOptions{...})`

An omitted `SearchOptions.Mode` defaults to lexical keyword search. Semantic and
dual modes require explicit opt-in and optional storage/provider configuration:

- `searchkit.SearchModeLexical`
- `searchkit.SearchModeSemantic`
- `searchkit.SearchModeDual`

Do not call low-level `searchkit/search` package APIs from host app code for normal request paths.

## Documents: one per version and language

Index the unit that owns language and traits, not the parent item. A gallery
with an English original, an English colored edition and a Spanish original is
three documents of entity type `gallery_version`, keyed by version id, each in
its own language, each carrying `Title`/`Aliases` of that version and
`Keywords` = work tags ∪ that version's tags (never a sibling's). Mark every
version's `(entity_type, entity_id, language)` dirty when the version, its
parent, or any contributing tag/alias changes. Ungrouped entities (tags,
artists, series) stay one document per language.

## Eligibility join (host-owned, trusted)

`SearchOptions.Eligibility` / `TypeaheadOptions.Eligibility` is trusted host SQL
joined laterally to each candidate document (`sd.entity_type`, `sd.entity_id`,
`sd.language`) inside every retrieval route, before any limit:

- return **no row** when the document is not eligible for this request;
- return **one row** with `parent_id` (text, the content item) and `priority`
  (integer; lower is preferred among the item's equal matches, e.g. `0` for
  the language default);
- ownership, access, publication **and every requested version trait** must hold
  on that one row. Never OR across sibling versions.

Searchkit groups documents per `(entity_type, parent_id)` across the searched
languages before `Offset`/`Limit`. Each `SearchHit` returns `ParentID` (the
item), `EntityID` (the matched version) and `Language`; `Score` ranks the item
by its best document in any searched language. `HasMore` is true when items
follow the page or when `Truncated` (a candidate window filled). Hosts re-check
authorization while hydrating. `Eligibility` requires lexical mode.

## Filter Policy (Host-Owned)

Hosts inject trusted SQL via:

- `FilterSQL string` / `FilterArgs map[string]any`: a WHERE fragment on `sd` (all modes).
- `Eligibility{SQL, Args}`: the per-document join above (lexical).

Rules:

- Never concatenate raw user input into either. Use named args.
- Named args must not reuse the reserved names `language`, `q`, `prefix`, `limit`, `types`, and must be unique across `FilterArgs` and `Eligibility.Args`.
- Apply auth/business policy in host code and pass the resulting SQL into SearchKit.
- For `SearchModeDual`, the same `FilterSQL` fragment runs in lexical (`sd`) and semantic (`ev`) SQL scopes. Use only a backend-neutral fragment.

SearchKit applies filters and the join in retrieval queries before ranking/pagination.

## Pages, Candidate Depth and Semantic Confidence

`SearchOptions` separates:

- `Limit`: page size in content items; `Offset`: items skipped. Both apply after grouping.
- `CandidateLimit`: the document window requested per language source before grouping. Defaults to `max(100, 2*(Offset+Limit))`, clamped to at least `Offset+Limit`, capped at 10000. Pass the same value on every page when a `Truncated` window must stay identical across pages.
- `SemanticMinSimilarity`: positive raw cosine-similarity floor applied to semantic candidates before RRF. Values `<= 0` disable the additional floor unless `SemanticMinSimilarityEnabled` is true; NaN and infinities are rejected.

`OversampleFactor` is independent: with two-stage semantic retrieval it controls the approximate stage-one width (`CandidateLimit * OversampleFactor`) before exact cosine rescore. Values `<= 1` use effective factor `5`.

`SearchHit.Score` is the keyword match tier in lexical mode (exact title 1, alias 0.9, token/prefix 0.75–0.77, typo 0.5–0.52) and an RRF rank score in dual/semantic mode; it is never semantic confidence. Use `SearchWithTrace` to inspect raw source scores and their explicit score kinds. Trace collection is opt-in and returns partial provenance alongside errors.

Search traces include normalized query text and candidate IDs. Store them only in access-controlled evaluation artifacts; do not emit them wholesale to routine logs or user responses.

## Evaluation Ownership

SearchKit's `eval` package owns generic cases, metrics, reports, comparisons, and score-floor sweeps. Hosts own:

- immutable corpus/dataset identity;
- business-specific judgments and visibility policy;
- executing SearchKit and end-to-end host pipelines;
- cache isolation and failure policy;
- selecting production thresholds from measured sweeps.

Keep score domains separate when sweeping. In particular, do not combine cosine similarity, FTS rank, trigram similarity, PGroonga score, and RRF score. Golden keys are content items (`ParentID`).

## Language Strictness

Hosts pass request language explicitly (`Language`) and choose `LanguageMode`:

- `searchkit.LanguageModeExact` (default): requested language only.
- `searchkit.LanguageModeFallbackEnglish`: requested language + English. An item matched in both is returned once, represented by its requested-language document; ties between items never prefer English.

For strict language behavior, set `LanguageModeExact` and keep host hydration/read-model queries strict as well (no implicit English fallback).

## Example: hentai0 (video versions)

Documents: entity type `video_version`, id = `video_versions.id`, one per
metadata language, keywords from `effective_video_tags(version_id, tag_id)`
(work `video_tags` ∪ `video_version_tags`).

```go
const videoEligibilitySQL = `
SELECT vv.video_id AS parent_id, (vv.id <> v.default_version_id)::int AS priority
FROM hentai0.video_versions vv
JOIN hentai0.videos v ON v.id = vv.video_id
WHERE vv.id::text = sd.entity_id
  AND v.deleted_at IS NULL AND vv.deleted_at IS NULL
  AND (vv.live_at IS NULL OR vv.live_at <= NOW())
  AND (@include_unlisted OR v.is_unlisted = FALSE)
  -- every requested tag must be effective on this very version
  AND (SELECT count(DISTINCT vt.tag_id) FROM hentai0.effective_video_tags vt
       WHERE vt.version_id = vv.id AND vt.tag_id = ANY(@tag_ids::bigint[])) = cardinality(@tag_ids::bigint[])`

page, err := searchkitClient.Search(ctx, query, searchkit.SearchOptions{
  Language:     language,
  LanguageMode: searchkit.LanguageModeExact,
  EntityTypes:  []string{"video_version"},
  Limit:        pageSize,
  Offset:       (page - 1) * pageSize,
  Eligibility:  &searchkit.Eligibility{SQL: videoEligibilitySQL, Args: map[string]any{"include_unlisted": includeUnlisted, "tag_ids": requiredTagIDs}},
})
```

Host then hydrates `ParentID` (video) and links `EntityID` (the matched
version); `page.HasMore` drives the next-page control.

## Example: doujins (gallery versions)

Documents: entity type `gallery_version`, id = `gallery_versions.id`, language
= `page_language`, keywords from `gallery_tags` ∪ `gallery_version_tags`.

```go
eligibilitySQL := `
SELECT gv.gallery_id AS parent_id, (NOT gv.is_language_default)::int AS priority
FROM doujins.galleries g
JOIN doujins.gallery_versions gv ON gv.gallery_id = g.id
WHERE gv.id::text = sd.entity_id
  AND gv.page_language = sd.language
  AND (@show_soft_deleted OR (g.deleted_at IS NULL AND gv.deleted_at IS NULL))
  AND (@show_drafts OR gv.live_at IS NOT NULL)
  AND (@show_future OR gv.live_at <= NOW())
  -- every requested slug must be a work tag or a tag of this very version
  AND (SELECT count(DISTINCT t.slug) FROM doujins.tags t
       WHERE t.slug = ANY(@only_slugs::text[])
         AND (EXISTS (SELECT 1 FROM doujins.gallery_tags gt WHERE gt.gallery_id = g.id AND gt.tag_id = t.id)
           OR EXISTS (SELECT 1 FROM doujins.gallery_version_tags vt WHERE vt.version_id = gv.id AND vt.tag_id = t.id)))
      = cardinality(@only_slugs::text[])`

hits, err := searchkitClient.Typeahead(ctx, query, searchkit.TypeaheadOptions{
  Language:     language,
  LanguageMode: searchkit.LanguageModeExact,
  EntityTypes:  []string{"gallery_version"},
  Limit:        12,
  Eligibility:  &searchkit.Eligibility{SQL: eligibilitySQL, Args: map[string]any{
    "show_soft_deleted": permissionOptions.ShowSoftDeleted,
    "show_drafts":       permissionOptions.ShowDrafts,
    "show_future":       permissionOptions.ShowFuture,
    "only_slugs":        onlyTagSlugs,
  }},
})
```

Host then resolves `ParentID` to gallery payloads with the matched version and applies response formatting.

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
- Index per-version documents and move visibility/trait policy into the `Eligibility` join; keep `FilterSQL` for ungrouped entities.
- Page with `Offset`/`Limit` and `HasMore`; never fetch N documents and deduplicate afterwards.
- Keep app code to request validation, policy mapping, and hydration.
