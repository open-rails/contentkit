# ContentKit Host Integration Guide

ContentKit is a library. Hosts own HTTP routes, auth, visibility, hydration
and response shaping. Every call is scoped to one tenant; references of
another tenant are rejected.

## Contract

One migrate call, one constructor, one HTTP mount per tenant:

```go
_ = contentkit.Migrate(ctx, contentkit.MigrateConfig{DB: sqlDB, Schema: "doujins", SearchSchema: "doujins_searchkit", ClickHouse: &chmigrate.Config{...}})
rt, _ := contentkit.NewRuntime(ctx, contentkit.RuntimeConfig{
	EmbeddedConfig: contentkit.EmbeddedConfig{PG: pool, PGSchema: "doujins_searchkit", Tenant: "doujins", CH: ch, CHDatabase: "hub"},
	Content: content.Options{Schema: "doujins", Identity: identity, Authz: authz, Resolver: resolver, Users: users,
		Storage: storage, Processor: sanitizer, Perms: content.Perms{...}, ContentKinds: []string{"gallery", "post", "tag"}},
})
mux.Handle("/api/social/", http.StripPrefix("/api/social", rt.Handler()))
```

Hosts use:

- `rt.Search(ctx, query, contentkit.HubSearchOptions{...})` → `SearchResult{Hits, HasMore, Truncated, Degraded}`
- `rt.Client().SearchWithTrace(...)` for offline evaluation/debugging
- `rt.Typeahead(ctx, query, contentkit.TypeaheadOptions{...})`
- `worker.SyncOnce(ctx, rt.WorkerOptions(hostOptions))` on a schedule, `search.MarkDirty` in content transactions
- the `Hub` methods for the signal and discovery planes
- `rt.Content` (package `content`) for interactions: `Counts`, `MyReactions`, `IsFavorited`, `ListFavorites`, `LatestComments`, `ReactionsByActor`

Do not call `search` package SQL helpers from request paths.

## Interactions (`content`)

The content module owns posts, comments, reactions, favorites and polls in the
host schema's `social_*` tables. Every row is keyed by the `ContentRef` of
the host-owned work: `(tenant_id, content_kind, content_id,
content_version_id)`; comment threading is `reply_to_id`. Routes are
`/{kind}/{id}/comments|like|dislike|neutral|reaction|favorite`,
`/comments/{cid}/...`, `/comments/latest`, `/comments/admin?content_kind=`,
`/favorites`, `/polls...`, `/posts...`; `kind` must be in `ContentKinds`.

Ports (all in `content`):

| Port | Required | Contract |
|---|---|---|
| `Identity` | yes | reads the already-authenticated `Actor` from context; ContentKit never authenticates |
| `Authorizer` | yes | `Can(actor, perm)` for `Perms{PostWrite, PollWrite, CommentModerate}`; fail-closed on error and on an unset perm |
| `ContentResolver` | yes | `Resolve(ref, actor) → Resolution{Ref, Visible, Accessible}`: the whole gating surface. `Ref` is the canonical reference rows are stored under (an alias or per-language route resolves to it); zero keeps the request; another tenant is an error. React/comment need `Accessible`, favorite needs `Visible` |
| `UserEnricher` | no | display data for author ids |
| `MediaStore` / `Storage` | no | poll/post images; `Storage` is the built-in public-bucket S3 store |
| `ContentProcessor` | no | rich-text sanitizer for comment/post bodies (default strips tags) |

There is no `Recorder` and no `Moderation` port: reactions and favorites feed
the signal plane through ContentKit's own preference outbox (C3), and
`ContentModerator` (C4) decides comment/post publication at the `screen` seam
in `comments.go`. A policy rejection answers 422 (`content.RejectedError`).

Posts are ContentKit's own keyword documents (kind `post`, the post's
language): every post write queues its `DocumentKey` in the keyword schema's
dirty queue inside the write transaction; `rt.WorkerOptions` routes kind
`post` to the module's builder and lister, so one worker tick maintains host
documents and posts.

The tenant is pinned at construction and stamped on every row; a `ContentRef`
of another tenant passed to any read is `content.ErrTenant`, never remapped.
Existing single-tenant rows convert under `tenant_id = ''` and are adopted
once with `content.AssignTenant` ([docs/migration.md](docs/migration.md)).

## Documents: one per content reference and language

Index the unit that owns language and traits. A gallery with an English
original, an English colored edition and a Spanish original is three
documents referenced as `ContentRef{gallery, g1, version}` in that version's
language, each carrying `Title`/`Aliases` of that version and `Keywords` =
work tags ∪ that version's tags (never a sibling's). Mark every version's
`DocumentKey` dirty when the version, its work, or any contributing tag or
alias changes. Ungrouped records (tags, artists, series) are one document per
language referenced by their taxonomy id as `ContentRef{tag, <taxonomy id>}`.

## Eligibility join (host-owned, trusted)

`Eligibility{SQL, Args}` is trusted host SQL joined laterally to each
candidate document (`sd.tenant_id`, `sd.content_kind`, `sd.content_id`,
`sd.content_version_id`, `sd.language`) inside every retrieval route, before
any limit:

- return **no row** when the document is not eligible for this request;
- return **one row** with `priority` (integer; lower is preferred among a
  work's equal matches, e.g. `0` for the language default);
- ownership, access, publication **and every requested version trait** must
  hold on that one row. Never OR across sibling versions.

ContentKit groups documents per work `(tenant, kind, content_id)` across the
searched languages before `Offset`/`Limit`. Each hit returns the matched
document's reference (`ContentID` = the work, `Version()` = the matched
version or `""`) and `Language`; `Score` ranks the work by its best document in
any searched language. Hosts re-check authorization while hydrating. Semantic
candidates from a `SemanticRanker` pass the same join before fusion.

## Filter policy (host-owned)

- `FilterSQL`/`FilterArgs`: a WHERE fragment on `sd`, named args only.
- Reserved arg names: `tenant`, `language`, `q`, `prefix`, `limit`, `kinds`,
  `candidates`; names must be unique across `FilterArgs` and `Eligibility.Args`.
- Never concatenate user input into either.

## Pages and candidate depth

- `Limit`: page size in works; `Offset`: works skipped; both apply after grouping.
- `CandidateLimit`: the document window per language source before grouping.
  Defaults to `max(100, 2*(Offset+Limit))`, capped at 10000. Pass the same
  value on every page when a `Truncated` window must stay identical.
- `HasMore` is true when works follow the page or when `Truncated`.

## Language strictness

- `LanguageModeExact` (default): requested language only.
- `LanguageModeFallbackEnglish`: requested language + English; a work matched
  in both is returned once, represented by its requested-language document;
  ties never prefer English.

## Semantic ranking (optional)

Register a `SemanticRanker` on `ClientConfig`/`EmbeddedConfig` and set
`SearchOptions.Semantic`. The ranker sees the normalized query, language,
kinds, window and the host constraints; ContentKit re-verifies its candidates
through the eligibility join, RRF-fuses them with the keyword list
(`SemanticWeight`, `RRFK`), then groups and pages. Absent ranker, timeout
(`SemanticTimeout`, default 2s) or failure: keyword-only with
`SearchResult.Degraded = true`. Never surface that as an error.

## Worker

`worker.SyncOnce` permits one writer per schema and tenant; a competing tick
returns without work. Documents, queue acknowledgements and sink deliveries
commit together; a document whose sink delivery failed stays queued under a
new revision while its keyword row commits. The pool needs two connections;
callbacks must be bounded, read-only and respect cancellation.

## Example: hentai0 (video versions)

```go
const videoEligibilitySQL = `
SELECT (vv.id <> v.default_version_id)::int AS priority
FROM hentai0.video_versions vv JOIN hentai0.videos v ON v.id = vv.video_id
WHERE vv.id::text = sd.content_version_id AND v.id::text = sd.content_id
  AND v.deleted_at IS NULL AND vv.deleted_at IS NULL
  AND (vv.live_at IS NULL OR vv.live_at <= NOW())
  AND (@include_unlisted OR v.is_unlisted = FALSE)
  -- every requested tag must be effective on this very version
  AND (SELECT count(DISTINCT vt.tag_id) FROM hentai0.effective_video_tags vt
       WHERE vt.version_id = vv.id AND vt.tag_id = ANY(@tag_ids::bigint[])) = cardinality(@tag_ids::bigint[])`

page, err := client.Search(ctx, query, contentkit.SearchOptions{
  Language: language, ContentKinds: []string{"video"}, Limit: pageSize, Offset: (page - 1) * pageSize,
  Eligibility: &contentkit.Eligibility{SQL: videoEligibilitySQL, Args: map[string]any{"include_unlisted": includeUnlisted, "tag_ids": requiredTagIDs}},
})
```

## Example: doujins (gallery versions)

```go
eligibilitySQL := `
SELECT (NOT gv.is_language_default)::int AS priority
FROM doujins.galleries g JOIN doujins.gallery_versions gv ON gv.gallery_id = g.id
WHERE gv.id::text = sd.content_version_id AND g.id::text = sd.content_id AND gv.page_language = sd.language
  AND (@show_soft_deleted OR (g.deleted_at IS NULL AND gv.deleted_at IS NULL))
  AND (@show_drafts OR gv.live_at IS NOT NULL) AND (@show_future OR gv.live_at <= NOW())
  AND (SELECT count(DISTINCT t.slug) FROM doujins.tags t WHERE t.slug = ANY(@only_slugs::text[])
       AND (EXISTS (SELECT 1 FROM doujins.gallery_tags gt WHERE gt.gallery_id = g.id AND gt.tag_id = t.id)
         OR EXISTS (SELECT 1 FROM doujins.gallery_version_tags vt WHERE vt.version_id = gv.id AND vt.tag_id = t.id)))
      = cardinality(@only_slugs::text[])`
```

## Attribution export (paged evaluation data)

`hub.Attribution(ctx, signal.AttributionOptions{Stage, Window, Surface, Limit, ClickLimit, After})`
exports one deterministic sequence per (stage, window, surface): renders at
the stage in `RenderID` order with their canonical clicks, then the
`Unattributed` clicks. Every page holds at most `Limit` renders and
`ClickLimit` click rows; `Next` resumes exactly after the last row (a render
whose clicks did not fit continues with `Continued: true`). Concatenating all
pages at any bounds yields the single-page result. Clicks are signals of type
`signal.TypeClick` carrying `render_id`/`position` (`Signal.WithAttribution`).
Erased subjects are excluded through the erasure ledger at read time.

## Subject erasure completion contract

`hub.EraseSubjects(ctx, subjects)` returns an `ErasureReport`; `Complete()` is
true only when:

1. the erasure is recorded in the `erasures` ledger with `insert_quorum` =
   every replica; a down replica fails fast and deletes nothing;
2. every row of the subjects is deleted from signals, compact state, daily
   contributions, exposures and the kept pre-ContentKit tables on every
   replica (`mutations_sync = 2`), re-counted as zero, and co-engagement pairs
   they contributed to are removed;
3. from the moment the ledger row is durable, every write, read, projection
   and export evaluates the ledger inside its own statement.

The subject key is dead in that tenant forever. Residue (rows that landed
after the fence or came back with a restore) is unreadable; schedule
`hub.EnforceErasures` and run it after every restore, after re-erasing the
subjects deleted since the backup. A shared account exists in every tenant:
each host erases its own tenant.

## Migration checklist

- Create and reuse one `contentkit.Runtime` per tenant.
- Index per-version documents; move visibility/trait policy into `Eligibility`.
- Page with `Offset`/`Limit` and `HasMore`; never fetch N documents and dedupe.
- Apply the lineages with `contentkit.Migrate` per [docs/migration.md](docs/migration.md); run `content.AssignTenant` once; gate startup on `signal.CheckSchema`.
- Replace `socialkit` imports with `content`: `EntityRef`/`EntityKey`/`entity_type`/`entity_id` → `contentref.ContentRef`/`ContentKey`/`content_kind`/`content_id`; `Entities` → `Resolver`; `Content` → `Processor`; `EntityTypes` → `ContentKinds`; `parent_id` → `reply_to_id`; `Counts(kind, id)` → `Counts([]ContentRef)`; delete the `Recorder` and `Moderation` adapters.
