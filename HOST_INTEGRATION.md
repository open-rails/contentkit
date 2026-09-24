# ContentKit Host Integration Guide

ContentKit is a library. Hosts own HTTP routes, auth, visibility, hydration
and response shaping. Every call is scoped to one tenant; references of
another tenant are rejected.

## Contract

One migrate call, one constructor, one HTTP mount per tenant:

```go
_ = contentkit.Migrate(ctx, contentkit.MigrateConfig{DB: sqlDB, Schema: "doujins", ClickHouse: &chmigrate.Config{...}})
rt, _ := contentkit.NewRuntime(ctx, contentkit.RuntimeConfig{
	EmbeddedConfig: contentkit.EmbeddedConfig{PG: pool, PGSchema: "doujins", Tenant: "doujins", CH: ch, CHDatabase: "hub"},
	Content: content.Options{Schema: "doujins", Identity: identity, Authz: authz, Resolver: resolver, Users: users,
		Media: &content.Media{URLs: reader, Folders: jobs}, Processor: sanitizer, Perms: content.Perms{...}, ContentKinds: []string{"gallery", "post", "tag"}},
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
host schema's `content_*` interaction tables. Every row is keyed by the `ContentRef` of
the host-owned work: `(tenant_id, content_kind, content_id,
content_version_id)`; comment threading is `reply_to_id`. Routes are
`/{kind}/{id}/comments|like|dislike|neutral|reaction|favorite`,
`/comments/{cid}/...`, `/comments/latest`, `/comments/admin?content_kind=`,
`/favorites`, `/polls...` (incl. `/polls/{id}/answer`), `/posts...`,
`/moderation/held`, `/moderation/{kind}/{id}/resolve`; `kind` must be in
`ContentKinds`.

Ports (in `content` unless qualified):

| Port | Required | Contract |
|---|---|---|
| `Identity` | yes | reads the already-authenticated `access.Actor` from context; ContentKit never authenticates |
| `Authorizer` | yes | `Can(actor, perm)` for `Perms{PostWrite, PollWrite, CommentModerate, ModerationReview}`; fail-closed on error and on an unset perm |
| `access.ContentResolver` | yes | `Resolve(ctx, refs, actor) → map[ContentKey]access.Resolution{Ref, Visible, Accessible, PreviewLimit, Editor}`, keyed by each requested ref's `Key()`: the whole gating surface, shared with media. Batch-first: ContentKit passes every ref a request needs in one call (`/comments/latest` resolves its whole page at once; single-item routes pass one ref), so answer it with one query, never a per-ref loop. An omitted ref denies (404); an error fails the whole batch. `Ref` is the canonical reference rows are stored under (an alias or per-language route resolves to it); zero keeps the request; another tenant is an error. React/comment need `Accessible`, favorite needs `Visible`; content ignores `PreviewLimit`. Media serves every file only when `Full()`, else the first `Units(n)` files (`PreviewLimit` N caps a `Visible` item to its first N files; free preview is `Accessible=false, PreviewLimit=3`) and `Visible` teasers; `Editor` (the actor may edit the item) unlocks `EditorOnly` variants and `edit`/`dims` in the read API |
| `UserEnricher` | no | display data for author ids |
| `Media` | no | post and poll images in ContentKit media (see below); absent = image routes answer 501 |
| `ContentProcessor` | no | rich-text sanitizer for comment/post bodies (default strips tags) |
| `ContentModerator` | no | `Screen(ModerationInput) → Verdict{Decision, Reason, Model, PromptVersion, Confidence}` before a comment/post publishes; absent = publish (see Moderation) |
| `AnswerClassifier` | no | `Classify(Answer) → GroupAssignment` when a free-text answer revision is stored; results are source-owned; absent = free-text polls are refused (see Free-text polls) |

**Post and poll images** live in media folders `{tenant}/post/{post_id}/` and
`{tenant}/poll/{poll_id}/` (`Media.PostKind`/`PollKind`). Register both kinds
with `Inline` set, route their `CanUpload` to `rt.Content.CanUpload` (PostWrite
or PollWrite, and the post or poll must exist), register the `media/image`
processor, and pass `content.Media{URLs: reader, Folders: jobs}`. The editor
uploads each image with the SDK's `uploadInline(file, {ref: {kind: "post", id}})`
(browser to bucket; the original stays private and is re-encoded to
`public/{id}.webp`), then hands the returned name to ContentKit, which stores
the plain public URL:

| Route | Body | Result |
|---|---|---|
| `POST /posts/{id}/images` | `{"image": "i-…"}` | `{"url"}` to place in the body |
| `PUT /posts/{id}/cover` | `{"image": "i-…"}` (`""` clears) | `{"cover_url"}` |
| `PUT /polls/{id}/image` | same | `{"image_url"}` |
| `PUT /polls/{id}/options/{oid}/image` | same | `{"image_url"}` |

Create and update bodies take no image URLs, so images are added once the post
or poll exists. The public URL serves after the image job runs (seconds).
Deleting a post or poll deletes its folder in the same transaction; replaced
images stay in the folder until then.

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
The host imports existing data with explicit tenant and content references
after initializing fresh stores ([docs/migration.md](docs/migration.md)).

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
  published one is withdrawn on review (`202`).
- A moderator error or an unknown decision **fails closed to `review`** with
  the reason "awaiting review"; the error is kept for the reviewer. Nothing is
  ever published unscreened and no submission is lost.
- No moderator: everything publishes.

Review queue: `rt.Content.ListHeld(ctx, kind, cursor, limit)` pages held
comments or posts oldest-first (`HeldItem` carries the body, the content
reference, the reason, model, prompt version, confidence, any moderator error
and `revision`); `rt.Content.Resolve(ctx, kind, id, ReviewDecision{Revision,
Decision, Reviewer, Reason})` writes the final state: `approve` publishes (counts and the keyword
index follow), `reject` keeps the item author-visible as `rejected` with the
reason. Over HTTP: `GET /moderation/held?kind=comment|post&cursor=&limit=` and
`POST /moderation/{kind}/{id}/resolve {"revision","decision","reason"}` (the
actor id is the reviewer), both gated by `Perms.ModerationReview`.
`GET /comments/admin` shows every state with real bodies.

Screening is revision-fenced. A decision must echo the held `revision`, so it
cannot publish an intervening edit. Edits are screened outside SQL
transactions, then a short source-revision CAS transaction commits the verdict;
a concurrent edit returns `409` for retry. Provider waits never hold row or
subject locks.

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

`content_poll_questions.kind` is `multiple_choice` (options + votes, as
before) or `free_text`: one answer per signed-in actor in
`content_poll_answers`, editable until the poll closes. `closes_at` (optional,
`PATCH`-able) and `is_active = false` close a poll for votes and answers alike
(`400 poll is closed`); results stay readable. Anonymous actors cannot answer
(an IP-keyed editable answer would let NAT neighbours overwrite each other).

Creating a free-text poll without `Options.Classifier` is refused (`501`,
`content.ErrNoClassifier`). Each stored or edited answer is classified right
after commit; a classifier failure keeps the answer with `classified: false`
and the host retries with `rt.Content.ReclassifyPending(ctx, after, limit)` on a
schedule (returns `ClassificationPage{Classified, Next}` and the first error).
Continue `Next` even after provider failure, and restart from empty when the
sweep ends. This is a per-sweep cursor, never a durable high-water mark. The classifier
returns a group id and label for an immutable `(tenant, answer_id, revision)`.
ContentKit CAS-persists that assignment only while the revision is current.
Membership, labels and counts are read from these durable assignments:
`GET /polls/{id}` (and the list) returns `answer_count`, `groups`
(`[{id, label, count}]`, sorted by count desc, label, id) and the caller's
`my_answer`. There is no provider results-read dependency. Identical answer
retries retain revision/time and completed classification. An edit clears the
old assignment until the new revision is classified; late old results cannot win.
`POST /polls/{id}/answer {"text"}` creates or replaces the caller's answer.

## Interaction erasure

`rt.EraseSubjects(ctx, subjects)` erases deleted accounts from every configured
plane: signals ([completion contract](#subject-erasure-completion-contract)),
ContentKit interaction data and data retained by
moderator/classifier providers. Standalone content consumers call
`rt.Content.EraseSubjects`; `rt.EmbeddedHub.EraseSubjects` is analytics-only. A
deliberately disabled signal plane is absent, not unfinished. Any
configured-plane failure returns an error and an incomplete report: keep the
host's obligation pending and retry (calls are idempotent). The host may
acknowledge AuthKit as soon as it has durably accepted the deletion obligation
in its own ledger; never hold that acknowledgement for provider availability.

Content erasure commits atomically:

- a permanent tenant/subject fence in `content_erased_subjects`; later writes by
  that subject fail with `content.ErrSubjectErased`;
- removal of the subject's authenticated reactions, favorites, poll votes and
  answers, with exact counter decrements;
- redaction of the subject's unpublished payloads (held/rejected comments and
  posts, draft and future-scheduled posts even when approved) and their
  moderation metadata. An item with a previously approved payload keeps it
  without republishing; a never-published item becomes a tombstone, keeping its
  row and replies.

Anonymous IP interactions, other tenants/subjects, currently approved authored
content (host retention policy) and externally stored media are untouched.
Every guarded writer takes the subject fence before other source locks; erasure
locks affected rows and applies rollup deltas in deterministic key order from
rows actually deleted, never synthesizing missing rollup or option rows. Guarded
mutations and erasure use explicit READ COMMITTED transactions, so a writer
waiting on the subject lock sees the committed fence even under a REPEATABLE
READ default.

### Provider data

Moderators/classifiers that retain personal data must configure
`content.Options.ProviderDataEraser`. Its `EraseSubjects(ctx, tenant, actorIDs)`
runs after the SQL commit and must durably fence those subjects and remove
retained data, including against calls already in flight; a queued delete is
not completion. Multiple retaining providers need a composite eraser; provider
implementations stay outside ContentKit. Ports that retain nothing implement
`StatelessPolicy()`: `BasicModerator` does (its duplicate cache is keyed by
subject and cleared on erase), and a `Chain` is stateless only when every member
is. Construction rejects a retaining port without an eraser; nil is never an
outage fallback.

The library tests qualify the port contract and source fences against real
PostgreSQL with in-memory retaining providers (paused completions, outages), not
a provider's production persistence. Each retaining adapter must prove durable
deletion/fencing across its own restarts and backups before the host marks
erasure complete. Soft-deleting a poll is not a provider erasure.

### Recovery

Fences and approved payload snapshots are durable user state. A restore must
restore/reapply fences and replay the host deletion ledger through
`EraseSubjects` before traffic resumes; provider erasure keeps the same
permanent-fence semantics across its own restores. Restored reactions and
favorites of fenced subjects never export.

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

## Media edits and slots from files

Cropping and rotating are ContentKit's: the host never decodes images.

- A file edit is the commit op `{"op":"edit","name":"001.png","edit":{"crop":{"x":0,"y":0,"w":800,"h":600},"rotate":90}}`
  (omit `edit` to clear). Crop is in the original's pixels, before the
  clockwise rotate. Variants re-derive; the original is never changed.
- A cover from a page is `Uploads.SetSlotFromFile(ctx, actor, media.SlotFromFile{Ref: ref, Slot: "cover", File: "001.png", Edit: &media.Edit{Crop: &media.Crop{X: x, Y: y, W: w}}})`
  or `POST /commit-slot-from-file {"ref","slot","file","edit"}` (→ `SlotManifest`).
  Give the slot `Aspect: 460.0 / 650` and send only the width; the height
  follows. `From` (`"from"`) takes the file from another item of the tenant,
  e.g. a channel avatar from a post image; `CanUpload` must allow both.
- Slots and inline images belong to the work: `CanUpload` is asked for
  `ref.Content()` even when a version ref is sent.
- Avatars and covers are slots with density widths; see README "Slots":

  ```go
  "avatar": {Aspect: 1, Widths: []int{128, 256, 512}, MinWidth: 512},
  "cover":  {Aspect: 3, Widths: []int{1500, 3000}, MinWidth: 1500},
  ```

  Mount `UploadHandler` with `Reader` (its origin and editor tokens build
  reply URLs). For `srcset` use `Reader.Slot(ctx, ref, actor, slot)` /
  `GET /{kind}/{id}/slots/{slot}` (resolves; 404 for items the viewer cannot see). Listings store
  the `SlotStamp` from `Hooks.SlotEncoded` (one text value per slot, e.g. a
  `cover_stamp` column) and build every output's immutable URL without reads
  with `Reader.SlotOutputs(ref, slot, stamp)`; with no stamp ("") it lists the
  widths up to `MinWidth` with revalidated URLs. Backfill with
  `Reader.Slot(...).Stamp()`. After changing slot specs,
  enqueue `ProcessJob{Ref}` per item; retired widths are deleted.
- Editors (`Resolution.Editor`) read `dims` (original size) and `edit` from
  the read API and show a `Spec{Unedited: true, EditorOnly: true}` variant,
  which lives in `editor/` behind an editor-only token; the SDK's `useCrop` keeps the rect
  in original pixels for any cropper UI.
- Cap files per item with `Kind.MaxFiles` and `Kind.TypeLimits`
  (`{"video": {MaxFiles: 1}}`); commits over a cap get 409 `too_many_files`.

## Video posters and hover previews

Every `Video` kind gets the `poster` slot (`media.VideoPoster`: 16:9, widths
480/960/1920); `poster` and `hover_preview` are reserved slot names.

- **Poster**: a frame or an uploaded image, encoded by the image job through
  the slot's edit like any slot. The video worker grabs frames from the widest
  HLS rendition into `originals/poster` (PNG) and hands them to the host's
  image job through `Config.Slots` (`media.NewProcessInserter`; the worker's
  `MEDIA_HOST_RIVER_SCHEMA`/`MEDIA_HOST_QUEUE`). Default: the first of five
  sampled frames (20–80 %) that is not black or flat. Frames whose centred
  16:9 crop is under 480 px are upscaled, so every poster has 480.
- **Hover preview**: a silent loop, default 3 s from a quarter in, bounded
  1–6 s, centred 16:9 at 12 fps, as H.264 MP4 and animated WebP at 320
  (always) and 640 px (when the video is that wide), rendered by the worker.
  MP4 measured 2.1–2.9× smaller on real footage: prefer `<video muted loop
  playsinline>`, WebP for `<img>`.
- **Publishing**: both render to `editor/` (editors only) and are copied to
  `public/` (tokenless, `public/poster_{w}.webp`,
  `public/hover_preview_{w}.mp4|.webp?v=`) only as the item's `Exposure`
  allows. `JobsConfig.Resolver` resolves the item for an anonymous actor and
  `JobsConfig.Exposure` (default `media.DefaultExposure`) decides: not
  visible (draft, deleted) → nothing; full access → poster and hover
  preview; otherwise (paid, preview cut) → the poster alone as a teaser.
  Pass a policy to vary it per item. Each poster encode and preview render
  republishes; call `jobs.PublishTx(ctx, tx, ref)` in every transaction that
  changes what anonymous viewers see (publish, unpublish, soft delete,
  restore, price or access changes). It re-resolves after writing, so it
  converges on the latest state. Without a `Resolver` nothing is published.
- Both cut from the HLS renditions (one segment range, confined ffmpeg
  inputs), so selection changes never download the source.
- Upload API (`CanUpload` on the work), each answering `VideoImages`:
  - `POST /video-poster {ref, source: "frame"|"upload"|"auto", file?, time?, sha256?, edit?}`:
    `frame` needs `time` and the ref's version, its edit in the frame's pixels
    (`video.w×h`); `upload` needs the `sha256` of an image presigned with
    `slot: "poster"`. `/edit-slot` re-edits either without a new grab.
  - `POST /video-preview {ref, file?, start?, duration?}` (no `start`: automatic).
  - `POST /video-images {ref, file?}`: outputs, selections, and the file's
    duration and frame size for the picker.
  - `GET /frame?kind=&id=&version=&file=&t=&w=`: a JPEG from one HLS
    segment; `t` clamped into the video, `w` into 64–1280 and the widest
    rendition. Needs `UploadOptions.Frames` (`video.NewFrames`, ffmpeg in the
    host image); `FrameConcurrency` (2) at once, then 429.
- Viewers: `GET /{kind}/{id}/video-images` resolves (404 when hidden) and
  lists what is published (editors: everything, from `editor/`). Listings
  build URLs without reads, `Reader.SlotOutputs(ref, media.PosterSlot,
  version)` and `Reader.HoverPreviewURLs(ref, version)`, only for items whose
  Exposure publishes them (default: posters of visible items, hover previews of free ones).

## Production media delivery

- **Media host**: serve `cmd/media-access` at `media.<site domain>` (same
  site as the pages) and use cookie delivery (`Delivery{Mode: DeliverCookie,
  CookieDomain: "<site domain>"}`); URL delivery only for apps without cookies.
- **Access worker config**: `MEDIA_ACCESS_HOSTS=media.<domain>`;
  `MEDIA_ACCESS_CORS_ORIGINS` exactly your sites' origins
  (`https://<domain>,https://www.<domain>`; no wildcards, no third parties);
  keep `Cross-Origin-Resource-Policy` at its `same-site` default so other
  sites cannot hotlink media into `<img>`/`<video>`.
- **Bucket**: private (no public ACL or policy); the worker's key is
  read-only on `*/blobs/*`, `*/editor/*` and `*/public/*`; only the hosts
  write.
- **CDN**: may cache `public/` in a shared cache (URLs with a current `?v=`
  are immutable). Never cache `blobs/` or `editor/` in a shared cache: the
  token is not part of a cache key the CDN checks, so a cached object would be
  served without one. They are `private` for the browser cache.
- **Key rotation**: add the new key to every access worker as
  `MEDIA_ACCESS_TOKEN_KEY` with the old one as `_TOKEN_KEY_PREVIOUS`, then
  switch the hosts' `Delivery.SigningKey`, then drop the previous key after
  the longest token lifetime (TTL rounded up to the window, about 5 h by default).
- **Scraping**: keep `HandlerOptions.Limit` on (default 2/s, burst 120 per
  viewer); behind a proxy set `Actor.IP` so anonymous viewers are not one key.
  Signed-URL logs name the viewer.
- **Multiple replicas**: the limit is per process unless `Limit.Redis` is set
  (logged at startup), so N replicas allow N times it. Pass the host's
  go-redis client for Redis or Microsoft Garnet (`Limit.KeyPrefix`, default
  `contentkit:media:rl:`); replicas then share one sliding-window count per
  viewer (at most `Burst` per `Burst/PerSecond` window; plain INCR/PEXPIRE/GET
  in MULTI, no Lua, keys expire after two windows; replica clocks need NTP).
  A Redis error fails open to the per-process limit for 1 s, logs a warning
  once per outage and increments expvar
  `contentkit_media_ratelimit_redis_errors`: the limit is abuse protection,
  tokens and visibility checks still gate every file.
- **Visibility**: wire `JobsConfig.Resolver` and call `PublishTx` on every
  visibility change (see "Video posters and hover previews"); `DeleteItemsTx`
  removes `public/` first.

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

`taxonomy.Store` owns the generic catalog of one tenant. The PostgreSQL
baseline installs its tables in the host-selected schema. Construct the
optional store with that schema and the
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

Reactions and favorites reach the signal plane straight from their own rows.
`content_reactions.value` is -1/0/1 and `content_favorites.value` is 1/0:
neutral and unfavorite keep the row at 0. Every value change stamps the row
with `revision` from `content_preference_revision_seq` under the row lock; a
no-op allocates nothing. Only authenticated rows export; anonymous (IP)
reactions and comment threads never do.

- **`content.ContentCanonicalizer`** (`Options.Canonicalizer`, required for
  export) maps the resolver's reference to the one reference the reaction row,
  the counts rollup and the export all use. Doujins strips the language
  suffix (`"42:en"` → `"42"`); explicit version feedback keeps its
  `content_version_id`; a language suffix never implies a version; comment
  threads keep their localized reference; `ok=false` keeps a target out, and
  is re-checked at sync time. `MyReactions`/`IsFavorited` accept route
  references and read under the canonical one; `Counts` reads exactly the
  reference given.
- **Sync**: schedule `rt.SyncPreferences(ctx)` from the host worker (e.g.
  every few seconds). It sends rows past the tenant's watermark in revision
  order as one `signal` event per subject × reference × axis (`Type` = axis,
  `EventID` = `contentkit.PreferenceEventID`, `Revision`, `OccurredAt` =
  row `updated_at`, `Value`), then records a checkpoint; the newest revision
  wins, so re-sends are harmless. Each scan restarts from the newest
  checkpoint older than `Options.PreferenceSyncOverlap` (default 5m) before
  the previous sync: a write is delivered if its transaction commits within
  the overlap of allocating its revision. A failed sync advances nothing.
- **Repair**: schedule `rt.ResyncPreferences(ctx)` (e.g. daily) and run it
  after sink loss: a full re-send, zeros included.
- **Erasure**: `rt.EraseSubjects` deletes the subject's rows behind the source
  fence and fences the signal plane, which drops any late send.
- **Restore**: after restoring PostgreSQL against a retained signal plane, run
  `rt.Content.SeedPreferenceRevisionFloor(maxSinkRevision)` before writers
  resume (only ever advances; fails closed outside `[0, MaxInt64/2]`).

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

## Discovery candidates

`SimilarTo` and `Recommend` get candidates from one port:

```go
type Candidates interface { // package discovery
	Similar(ctx context.Context, anchor contentref.ContentRef, q Query) ([]Candidate, error)
	ForSubject(ctx context.Context, subject signal.Subject, q Query) ([]Candidate, error)
}
```

Candidates are ranked best-first; oversample past `q.Limit`. The hub then
applies the same policy to every source: tenant and `ContentKinds`, never the
anchor, seen works (per options), always disliked works, dedupe, limit, and the
`Recommend` popularity fill. `EmbeddedConfig.Candidates` nil =
`discovery.Engagement` (co-engagement seeded from the subject's top works).
To switch sources, set that one field; wrap it in `Fallback` so errors and thin
results fall back to engagement:

```go
engagement, err := discovery.NewEngagement(ch, "hub", "doujins")
cfg.Candidates = discovery.Fallback{Primary: aiSource, Secondary: engagement, OnError: logErr}
```

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
- Apply the baselines with `contentkit.Migrate` per [docs/migration.md](docs/migration.md); gate startup on `signal.CheckSchema`.
- Wire `Options.Moderator` (a `Chain` of `BasicModerator` and the AI moderator), `Options.Classifier` for free-text polls, `Perms.ModerationReview`, and schedule `ReclassifyPending`.
- Adopt the preference boundary (doujins #888 / hentai0 #594): pin this ContentKit, implement `ContentCanonicalizer`, delete the callback-time bridge (`internal/social` `recorder`, `discovery.Recorder.Reaction`, `socialReactionSignal`) and every per-delivery signal-identity adapter, schedule `SyncPreferences` (and `ResyncPreferences` as the periodic repair), wire `EraseSubjects` into deletion, rewrite direct SQL readers (`split_part(entity_id, ':', 1)`, favorite-key helpers) to the canonical `content_id` and filter `content_favorites` on `value = 1`.
- Replace `socialkit` imports with `content`: `EntityRef`/`EntityKey`/`entity_type`/`entity_id` → `contentref.ContentRef`/`ContentKey`/`content_kind`/`content_id`; `Entities` → `Resolver`; `Content` → `Processor`; `EntityTypes` → `ContentKinds`; `parent_id` → `reply_to_id`; `Counts(kind, id)` → `Counts([]ContentRef)`; delete the `Recorder` and `Moderation` adapters.
- Replace `content.Options.Storage`/`Media` (`StorageConfig`, `MediaStore`) with `Options.Media` over media, register the `post` and `poll` kinds with `Inline`, and move image uploads to the SDK's `uploadInline` plus the image routes above (`POST /posts/media`, multipart `POST .../cover|image` and `image_url`/`cover_url` in write bodies are gone).
- Rename direct SQL on `social_*` tables to `content_*` (`social_entity_counts` → `content_interaction_counts`) and `content.Options.PrivateDataEraser` to `ProviderDataEraser`.
