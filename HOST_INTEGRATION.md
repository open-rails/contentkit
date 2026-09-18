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

- `rt.Search(ctx, query, contentkit.HubSearchOptions{...})` → `SearchResult{Hits, HasMore, Truncated}`
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
`/favorites`, `/polls...` (incl. `/polls/{id}/answer`), `/posts...`,
`/moderation/held`, `/moderation/{kind}/{id}/resolve`; `kind` must be in
`ContentKinds`.

Ports (all in `content`):

| Port | Required | Contract |
|---|---|---|
| `Identity` | yes | reads the already-authenticated `Actor` from context; ContentKit never authenticates |
| `Authorizer` | yes | `Can(actor, perm)` for `Perms{PostWrite, PollWrite, CommentModerate, ModerationReview}`; fail-closed on error and on an unset perm |
| `ContentResolver` | yes | `Resolve(ref, actor) → Resolution{Ref, Visible, Accessible}`: the whole gating surface. `Ref` is the canonical reference rows are stored under (an alias or per-language route resolves to it); zero keeps the request; another tenant is an error. React/comment need `Accessible`, favorite needs `Visible` |
| `UserEnricher` | no | display data for author ids |
| `MediaStore` / `Storage` | no | poll/post images; `Storage` is the built-in public-bucket S3 store |
| `ContentProcessor` | no | rich-text sanitizer for comment/post bodies (default strips tags) |
| `ContentModerator` | no | `Screen(ModerationInput) → Verdict{Decision, Reason, Model, PromptVersion, Confidence}` before a comment/post publishes; absent = publish (see Moderation) |
| `AnswerClassifier` | no | `Classify(Answer) → GroupAssignment` when a free-text poll answer is stored, `Groups(tenant, poll)` when results are read; absent = free-text polls are refused (see Free-text polls) |

There is no `Recorder` and no `Moderation` port: reactions and favorites feed
the signal plane through ContentKit's own preference outbox, and the
`ContentModerator` decides comment/post publication.

Posts are ContentKit's own keyword documents (kind `post`, the post's
language): every post write queues its `DocumentKey` in the keyword schema's
dirty queue inside the write transaction; `rt.WorkerOptions` routes kind
`post` to the module's builder and lister, so one worker tick maintains host
documents and posts.

The tenant is pinned at construction and stamped on every row; a `ContentRef`
of another tenant passed to any read is `content.ErrTenant`, never remapped.
Existing single-tenant rows convert under `tenant_id = ''` and are adopted
once with `content.AssignTenant` ([docs/migration.md](docs/migration.md)).

## Moderation (comments and posts)

Every comment create/edit and every non-draft post create/update is screened
through `Options.Moderator` after sanitizing; the moderator sees the tenant,
the opaque actor, the content reference, the kind, the item id on an edit,
and the text (title + body for posts). The verdict sets the item's
`moderation` state:

- `approve` publishes (`201`/`200`).
- `reject` stores nothing and answers `422 {"error": reason}`
  (`content.RejectedError` in Go).
- `review` stores the item `held` (`202` on create): it is not counted, not
  indexed, cannot be replied to or reacted to, and is invisible to every reader
  except its author, who sees it with `moderation: "held"` and
  `moderation_reason`. An edit re-screens: a held item publishes on approval, a
  published one is withdrawn on review.
- A moderator error or an unknown decision **fails closed to `review`** with
  the reason "awaiting review"; the error is kept for the reviewer. Nothing is
  ever published unscreened and no submission is lost.
- No moderator: everything publishes.

Review queue: `rt.Content.ListHeld(ctx, kind, cursor, limit)` pages held
comments or posts oldest-first (`HeldItem` carries the body, the content
reference, the reason, model, prompt version, confidence and any moderator
error); `rt.Content.Resolve(ctx, kind, id, ReviewDecision{Decision, Reviewer,
Reason})` writes the final state: `approve` publishes (counts and the keyword
index follow), `reject` keeps the item author-visible as `rejected` with the
reason. Over HTTP: `GET /moderation/held?kind=comment|post&cursor=&limit=` and
`POST /moderation/{kind}/{id}/resolve {"decision","reason"}` (the actor id is
the reviewer), both gated by `Perms.ModerationReview`. `GET /comments/admin`
shows every state with real bodies.

`content.BasicModerator{AllowLinks, DupWindow, CensorWords}` is the
deterministic default (links, per-process duplicate guard, censor list);
compose it in front of an AI moderator with `content.Chain{basic, ai}` — the
first reject/review wins, an error fails the chain closed.

Doujins adapter (`moderator{svc}` on the old `Moderation` port): implement
`Screen` instead of `Check`; return `Verdict{Decision: DecisionReject, Reason}`
where it returned `RejectModeration(reason)`, `DecisionApprove` where it
returned nil; wire `Options.Moderator = content.Chain{&content.BasicModerator{},
userIntelligence.Moderator}` once User Intelligence's `moderate` module exists;
set `Perms.ModerationReview` and point the admin review page at
`/moderation/held` and `/moderation/{kind}/{id}/resolve`.

## Free-text polls

`social_poll_questions.kind` is `multiple_choice` (options + votes, as
before) or `free_text`: one answer per signed-in actor in
`social_poll_answers`, editable until the poll closes. `closes_at` (optional,
`PATCH`-able) and `is_active = false` close a poll for votes and answers alike
(`400 poll is closed`); results stay readable. Anonymous actors cannot answer
(an IP-keyed editable answer would let NAT neighbours overwrite each other).

Creating a free-text poll without `Options.Classifier` is refused (`501`,
`content.ErrNoClassifier`). Each stored or edited answer is classified right
after commit; a classifier failure keeps the answer with `classified: false`
and the host retries with `rt.Content.ReclassifyPending(ctx, limit)` on a
schedule (returns the count classified and the first error). The classifier
owns the assignments and may re-cluster; results are read from it:
`GET /polls/{id}` (and the list) returns `answer_count`, `groups`
(`[{id, label, count}]`, sorted by count desc, label, id) and the caller's
`my_answer`; a failed `Groups` read degrades to `groups_unavailable: true`.
`POST /polls/{id}/answer {"text"}` creates or replaces the caller's answer.

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
any searched language. Hosts re-check authorization while hydrating.

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

## External document consumers

`DocumentSink` is an optional, neutral change feed for external indexes,
caches and audit consumers. Semantic search belongs entirely to the deferred
User Intelligence library; ContentKit exposes no semantic search hook.

## Worker

`worker.SyncOnce` permits one writer per schema and tenant; a competing tick
returns without work. Documents and queue acknowledgements
commit together in Postgres; external sink effects do not share that transaction.
Both sink operations carry the dirty revision: apply only newer versions
atomically and retain a tombstone version after deletion. This fences operations
that finish remotely after the caller sees a timeout. A failed sink stays queued under a
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

## Taxonomy (nodes, assignments, effective tags, counts)

`taxonomy.Store` owns the generic catalog of one tenant. Enable its lineage
with `contentkit.MigrateConfig{Taxonomy: true}`; it follows keyword migrations
in `SearchSchema`. Construct the optional store with that schema and the
host's kinds, languages and count-eligibility rule. Assign work-level
tags with a work reference and version traits with a version reference; read
`EffectiveTags` (work ∪ version) when hydrating. For "every requested tag on
one version" use `taxonomy.RequireAll(schema, ids)` as `FilterSQL`/`FilterArgs`
next to your `Eligibility`: both hold on the same `sd` row, so a Spanish
request for `colored` is never satisfied by an English colored edition plus a
Spanish original. `Store.Browse` runs that join without a query text.

Counts derive from your documents and `Options.CountEligibility` (your public
visibility join); call `RecountContent` when a work's versions or visibility
change, `RebuildCounts` after bulk loads written with
`AssignOptions{SuppressCounts: true}`. Mark your content documents dirty in
the transaction that calls `Assign`/`Unassign` (`store.WithTx(tx)`).
Typeahead documents of nodes are built by the store: register its kinds with
the worker through `store.Lister`/`store.Builder`. Mount `taxonomy.Handler`
behind your admin authorization. Adoption: [docs/taxonomy-migration.md](docs/taxonomy-migration.md).

## Preference boundary (reactions and favorites into the signal plane)

Reactions and favorites are exported to the signal plane through ContentKit's
own outbox, never through a host callback. Each mutation, inside its own
transaction: takes an advisory lock on `(schema, tenant, actor, canonical
reference, axis)` **before** reading state, applies the social change and its
counters, allocates `revision` from `content_preference_revision_seq`
(`BIGINT INCREMENT 1 NO CYCLE CACHE 1`) with `clock_timestamp()` and upserts
`content_preference_snapshots`. A no-op allocates nothing; neutral and
unfavorite keep a zero-valued snapshot; a rollback exports nothing; anonymous
(IP) reactions are never subjects.

- **`content.ContentCanonicalizer`** (`Options.Canonicalizer`, required for
  export) maps the resolver's reference to the one reference the reaction row,
  the counts rollup and the snapshot all use. Doujins strips the language
  suffix (`"42:en"` → `"42"`); explicit version feedback keeps its
  `content_version_id`; a language suffix never implies a version; comment
  threads keep their localized reference; `ok=false` keeps a kind (taxonomy)
  out. `MyReactions`/`IsFavorited` accept route references and read under the
  canonical one; `Counts` reads exactly the reference given (likes/favorites
  under the canonical reference, comment counts under the thread's).
- **Delivery**: schedule `rt.DeliverPreferences(ctx, after, pageSize, maxRows)` from
  the host worker (e.g. a River periodic job every few seconds). It pages
  pending rows in key order per sweep (no persisted high-water mark), writes
  one `signal` event per subject × reference × axis (`Type` = axis,
  `EventID` = `contentkit.PreferenceEventID`, `Revision` and `OccurredAt`
  copied, `Value` = current value), and acknowledges exactly the revision sent
  (`GREATEST` guarded by `sent <= revision`). A failing sink or a closed
  ClickHouse leaves rows pending; a failing key never starves others;
  at-least-once with convergence by revision.
- **Erasure**: call `rt.EraseSubjects` (fence in the signal plane, then purge
  the obligations) from the account-deletion handoff; a late delivery for a
  fenced subject is the terminal `PreferenceSubjectErased`, never a retry.
- **Bounded delivery**: keep the returned `Next` cursor for the current sweep
  and pass it as `after` on the next call, including after a sink error. Reset
  to zero when exhausted; never persist it as a global high-water mark.
- **Repair**: `rt.ReplayPreferences(ctx, after, pageSize, maxRows)` re-delivers
  every snapshot (acknowledged rows and zeros included) and resumes from the
  returned `Next`; newer snapshots still win by revision.
- **Cutover** (once per host, writers paused): `contentkit.Migrate` (social
  0004) → stop the old callback bridge and drain its queues →
  `rt.Content.SeedPreferenceRevisionFloor(maxExistingSinkRevision)` (fails
  closed outside `[0, MaxInt64/2]`, only ever advances) →
  `rt.Content.MigratePreferences(PreferenceMigrationOptions{ExportedKeys, DryRun})`
  (archives the language-scoped rows, collapses them: dislike wins, else
  like; favorite if any; recomputes counts; seeds snapshots at real source
  times and zeros for previously exported keys) → retire the old
  per-transition signal identities in the sink → start the delivery worker
  → `ReplayPreferences` once from the zero key.

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

## Popularity (policy-ranked)

Hosts rank by a named policy, never by the default rank: resolve the configured
name at startup and build one `popularity.Ranker` per hub.

```go
policy, err := popularity.ByName(cfg.PopularityPolicy) // "" = v1; unknown names refuse startup
ranker, err := popularity.New(popularity.Config{Source: hub, Policy: policy, Cache: cache, Catalog: catalog})

window, err := popularity.WindowForPeriod(period, time.Now()) // 7d|30d|90d|365d|all, else error
top, err := ranker.Popular(ctx, "gallery", window, offset+limit)     // ClickHouse RankExpr, global; slice for the page
scores, err := ranker.Scores(ctx, "gallery", artistGalleryIDs, window) // Go over Metrics: host-selected candidates
artists, err := ranker.Taxonomy(ctx, "gallery", "artist", window)     // member sums through the Catalog port
```

- `Hit` carries the score next to the raw metrics: show `Viewers` and
  `MeanEngagement()` as public counts, never anything derived from the score.
- Register `popularity.SessionScorer` (or your own `signal.Scorer`) per content
  kind so session scores are coverage of the selected version × dwell.
- `Catalog.Assignments(tenant, contentKind, taxonomyKind, ids)` maps ranked
  works to their taxonomy ids from the host's tables; ContentKit never records
  a signal against a taxonomy id.
- Cache keys carry tenant, policy name, kind, window and bounds; two policies
  sharing one cache never read each other's entries.

See [docs/popularity-policy.md](docs/popularity-policy.md) for the formula,
priors, the judged fixture and the host adoption steps.

## Migration checklist

- Create and reuse one `contentkit.Runtime` per tenant.
- Index per-version documents; move visibility/trait policy into `Eligibility`.
- Page with `Offset`/`Limit` and `HasMore`; never fetch N documents and dedupe.
- Apply the lineages with `contentkit.Migrate` per [docs/migration.md](docs/migration.md); run `content.AssignTenant` once; gate startup on `signal.CheckSchema`.
- Wire `Options.Moderator` (a `Chain` of `BasicModerator` and the AI moderator), `Options.Classifier` for free-text polls, `Perms.ModerationReview`, and schedule `ReclassifyPending`.
- Adopt the preference boundary (doujins #888 / hentai0 #594): pin this ContentKit, implement `ContentCanonicalizer`, delete the callback-time bridge (`internal/social` `recorder`, `discovery.Recorder.Reaction`, `socialReactionSignal`) and every per-delivery signal-identity adapter, schedule `DeliverPreferences`, wire `EraseSubjects` into deletion, run the cutover above once, rewrite direct SQL readers (`split_part(entity_id, ':', 1)`, favorite-key helpers) to the canonical `content_id`.
- Replace `socialkit` imports with `content`: `EntityRef`/`EntityKey`/`entity_type`/`entity_id` → `contentref.ContentRef`/`ContentKey`/`content_kind`/`content_id`; `Entities` → `Resolver`; `Content` → `Processor`; `EntityTypes` → `ContentKinds`; `parent_id` → `reply_to_id`; `Counts(kind, id)` → `Counts([]ContentRef)`; delete the `Recorder` and `Moderation` adapters.
