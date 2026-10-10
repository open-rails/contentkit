# ContentKit Host Integration Guide

ContentKit is a library. Hosts own HTTP routes, auth, visibility, hydration
and response shaping. Every call is scoped to one tenant; references of
another tenant are rejected.

## Contract

One migrate call, one constructor, one HTTP mount per tenant. The mount
serves every configured module at its sub-path: content at the root,
`/media/upload/…`, `/media/…`, `/codes/{code}` and `/taxonomy/…`
([docs/api/routes.md](docs/api/routes.md)):

```go
_ = contentkit.Migrate(ctx, contentkit.MigrateConfig{DB: sqlDB, Schema: "doujins", ClickHouse: &chmigrate.Config{...}})
rt, _ := contentkit.NewRuntime(ctx, contentkit.RuntimeConfig{
	EmbeddedConfig: contentkit.EmbeddedConfig{PG: pool, PGSchema: "doujins", Tenant: "doujins", CH: ch, CHDatabase: "hub"},
	Content: content.Options{Schema: "doujins", Identity: identity, Authz: authz, Resolver: resolver, Users: users,
		Media: &content.Media{URLs: reader, Folders: jobs}, Processor: sanitizer, Perms: content.Perms{...}, ContentKinds: []string{"gallery", "post", "tag"},
		Limits: content.Limits{Redis: rdb}},
	Uploads: uploads, Reader: reader, ReadLimit: media.RateLimit{Redis: rdb}, // /media/upload, /media
	Codes:    router,                  // /codes/{code}
	Taxonomy: taxonomy.Handler(store), // /taxonomy, for Perms.Taxonomy
})
mux.Handle("/api/contentkit/", http.StripPrefix("/api/contentkit", rt.Handler()))
```

Hosts use:

- `rt.Search(ctx, query, contentkit.HubSearchOptions{...})` → `SearchResult{Hits, HasMore, Truncated}`
- `rt.Client().SearchWithTrace(...)` for offline evaluation/debugging
- `rt.Typeahead(ctx, query, contentkit.TypeaheadOptions{...})`
- `worker.SyncOnce(ctx, rt.WorkerOptions(hostOptions))` on a schedule, `search.MarkDirty` in content transactions
- the `Hub` methods for the signal and discovery planes
- `rt.Content` (package `content`) for interactions: `Counts`, `MyReactions`, `IsFavorited`, `ListFavorites`, `LatestComments`, `ReactionsByActor`

Do not call `search` package SQL helpers from request paths.

## Content ids

`content_id` is a canonical lowercase UUIDv7 (`contentref.ValidateID`), never
reused. Every boundary refuses anything else with `contentref.ErrInvalidID`:
`ContentRef.Validate`, media refs and routes, jobs and the upload SDK.
`content` routes take a host route id (an id, alias or `{id}:en`) and key rows
by the resolver's `Ref`, or by the route id itself when `Ref` is zero, which is
then 400 `invalid_request` unless lower case: one spelling, one key. Comment
and poll ids are UUIDs in any letter case and key rows by their stored form,
so one actor has one reaction per comment however its id is spelled. The
interaction tables refuse upper case (`CHECK content_id = lower(content_id)`).
Mint ids with Postgres 18 `uuidv7()` or `contentref.NewID()`;
`contentref.Parse` validates untrusted input. A content id names the media
folder `{tenant}/{kind}/{id}/`, so a reused id (an integer sequence restarted
after a reset) would hand a new item another's files.
Taxonomy ids follow the same rule: a node is a search document keyed by its
`taxonomy_id` (`CreateNodes` mints one when omitted).

Media never adopts leftovers:

- `Manifests.Create(ctx, ref)` starts an item: it writes the empty manifest,
  hidden, and fails with `media.ErrFolderNotEmpty` (`*FolderNotEmptyError`)
  if the folder holds any object. Call it when the host row is created. A new
  item exposes nothing public until its first visibility decision: the first
  commit resolves it anonymously (`Hooks.Resolver`, required by `Uploads`),
  and `Expose` afterwards.
- A folder's first manifest edit (a commit) is refused the same way over a
  previous item's blobs.
- `Jobs.Purge(ctx, media.Deletion{Ref, Owner})` deletes a folder now, the
  explicit reset for deliberate reuse. Deleting content stays `DeleteItemsTx`.
- `Jobs.SweepOrphans(ctx, media.OrphanSweep{Tenant, Kind, Exists, Grace, Delete})`
  lists a kind's folders and reports (or deletes) those the host's `Exists`
  check omits and whose newest object is older than `Grace`. Folders whose id
  is not a content id are always orphans. Run it from a host command or job.

### Migrating legacy integer ids

Give each legacy item a UUIDv7 derived from its original `created_at`,
preserving order, and keep the old integer in a host `legacy_id` column for
redirects and references only. Use `contentref.LegacyID(namespace, kind,
legacyID, createdAt)` when an import is re-run or ids are derived in more than
one place (same inputs, same id; rows without `created_at` get 2010-01-01 plus
the id in milliseconds). `namespace` is the host's own constant (`"doujins"`,
`"hentai0"`; lowercase letters, digits, `_`, `-`) and is hashed with the kind
and id, so hosts importing the same kind and id get different ids; never change
it after an import; `contentref.IDAt(createdAt)` (random low bits) only when the id
is generated once and stored. Import its media
from the old system straight into ContentKit under the UUID `content_id`,
idempotently (skip items whose manifest already exists). No integer content ids
in storage; legacy URLs resolve through content-code aliases
([Content URLs](#content-urls-contenturl)).

## Content URLs (`contenturl`)

Every content page is `[/{lang}]/{route}/{CODE}[/{slug}]`, e.g.
`/watch/G4VRQ3ZQ5/night-before-the-counteroffensive`. The server reads only the
code; sub-pages are query parameters (`/g/{CODE}/{slug}?p=12`), so the slug can
always be omitted. The host chooses each kind's route; ContentKit owns the
code, resolution and canonicalization.

- **Codes** are 9 uppercase Crockford base32 characters (no `I L O U`), with at
  least one letter, random, unique per tenant across every kind, assigned once,
  never changed or reused. `ParseCode` accepts any case, `O`→`0`, `I`/`L`→`1`
  and hyphens. A request with another spelling of the code is redirected to the
  canonical one.
- **Registration.** Taxonomy nodes and posts register themselves (node slugs
  come from canonical names, one per language; post slugs from the post slug or
  title). Host content calls `Put` in the transaction that creates it, and again
  when a title changes. `Put` is idempotent, so the host backfills existing rows
  by calling it over them.

```go
urls, _ := contenturl.New(contenturl.Options{Pool: pool, Schema: "hentai0", Tenant: "hentai0"})
links, _ := urls.WithSQLTx(tx).Put(ctx, contenturl.Entry{ContentRef: ref, Title: video.Title,
	Titles: map[string]string{"es": video.TitleES}}) // links[0].Code, .Slug, .Slugs
byRef, _ := urls.Links(ctx, refs)                    // list pages: code and slugs for each ref
_ = urls.Merge(ctx, duplicate, survivor)             // the duplicate's URL redirects to the survivor
```

- **Canonical redirects.** The router maps kinds to routes. Mount its middleware
  in front of the pages, or in front of the SPA shell. A missing or stale slug,
  a wrong route, a merged code or a trailing slash gets a 301 to the canonical
  path, with the query kept. The canonical path carries the language prefix and
  that language's slug. `Visibility` returns `Visible`, `Hidden` or `Gone`.
  `Hidden` behaves like an unknown code and serves the host's 404 without
  leaking the slug. `Gone` answers 410. For posts, use
  `rt.Content.PostVisibility`. For taxonomy nodes, map the node state: a
  deleted node is `Gone`, and merged nodes already resolve to their survivor.
  A canonical request reaches the page
  with `FromContext(ctx)`, and gets a `Link: <…>; rel="canonical"` header when
  `BaseURL` is set.

```go
router, _ := contenturl.NewRouter(urls, contenturl.RouterOptions{
	Routes: contenturl.Routes{"video": "watch", "series": "series", "tag": "tag", "post": "blog"},
	Languages: []string{"en", "es"}, BaseURL: "https://hentai0.com",
	Visibility: func(r *http.Request, l contenturl.Link) (contenturl.Visibility, error) { /* drafts, removals */ },
})
mux.Handle("/", router.Middleware(spa))
// RuntimeConfig.Codes: router, so rt.Handler() serves GET /codes/{code}[?lang=]
```

- **Legacy aliases.** An import records the identifiers of the old site in the
  same transaction as the content. Each alias is `(source, legacy_kind, key)`,
  for example `("hentai0-legacy", "video", "346791971")` or
  `("doujins-legacy", "tag-name", "netorare")`. An alias is written once:
  repeating it is a no-op and re-pointing it is `ErrConflict`. The host keeps
  its legacy URL-shape rules. It calls `DecideAlias` and answers a 301 to
  `Path` (Matched), a 410 (Gone) or a 404 (neither), in one hop and before
  language negotiation.
- **Routes are reserved.** A content route serves only content pages: under
  it, any 9-character segment with a letter is read as a code, and a third
  segment as a slug. So `/watch/{CODE}/comments` redirects to the page. Put
  sub-pages in the query string, and keep other host pages off content routes.
- **Browser.** `@openrails/contentkit-ui/urls` (no dependencies) parses codes and
  builds paths from API links (`{content_kind, code, slug, slugs}`). Its
  `canonical` result drives a client-side `history.replaceState`. It never
  computes slugs; those come from the server.
- **Tables.** `content_codes(tenant_id, code, content_kind, content_id, slug,
  slugs, merged_into)` and `content_code_aliases`. Host SQL may join
  `content_codes` on `(tenant_id, content_kind, content_id)`, for example for
  sitemaps.

## Interactions (`content`)

The content module owns posts, comments, reactions, favorites and polls in the
host schema's `content_*` interaction tables. Every row is keyed by the `ContentRef` of
the host-owned work: `(tenant_id, content_kind, content_id,
content_version_id)`; comment threading is `reply_to_id`. Routes are
`/{kind}/{id}/comments|like|dislike|neutral|reaction|favorite`,
`/comments/{cid}/...`, `/comments/latest`, `/comments/admin?content_kind=`,
`/favorites`, `/polls...` (incl. `/polls/{id}/answer`), `/posts...`,
`/moderation/held`, `/moderation/{kind}/{id}/resolve`, `/comment-bans...`,
`/global-comment-bans...`, `/{kind}/{id}/can-comment`; `kind` must be in
`ContentKinds`.

Ports (in `content` unless qualified):

| Port | Required | Contract |
|---|---|---|
| `Identity` | yes | reads the already-authenticated `access.Actor` from context; ContentKit never authenticates |
| `Authorizer` | yes | `Can(actor, perm)` for `Perms{PostWrite, PollWrite, CommentModerate, ModerationReview, CommentBan, Taxonomy}`; fail-closed on error and on an unset perm |
| `access.ContentResolver` | yes | `Resolve(ctx, refs, actor) → map[ContentKey]access.Resolution{Ref, Visible, Accessible, Editor}`, keyed by each requested ref's `Key()`: the whole gating surface, shared with media. Batch-first: ContentKit passes every ref a request needs in one call (`/comments/latest` resolves its whole page at once; single-item routes pass one ref), so answer it with one query, never a per-ref loop. An omitted ref denies (404); an error fails the whole batch. `Ref` is the canonical reference rows are stored under (an alias or per-language route resolves to it); zero keeps the request, which must then be lower case (else 400); another tenant is an error. React/comment need `Accessible`, favorite needs `Visible`. `Owner` is the user who owns the content (its creator; `""` for site content): owner-scoped comment bans apply to it. For media an item's private files are all or nothing: `Full()` (visible and accessible) gets every one, anyone else none; what a viewer without access may see is the item's public files (a preview preset), which need only `Visible` to anonymous viewers; the ref is the item, i.e. the host's version; `Editor` (the actor may edit the item) unlocks editor reads (uploads, edits, editor views) and every private file of a visible item, so set it only for people who may see them all |
| `UserEnricher` | no | display data for author ids |
| `Media` | no | post and poll images in ContentKit media (see below); absent = image routes answer 501 |
| `ContentProcessor` | no | rich-text sanitizer for comment/post bodies (default strips tags) |
| `ContentModerator` | no | `Screen(ModerationInput) → Verdict{Decision, Reason, Model, PromptVersion, Confidence}` before a comment/post publishes; absent = publish (see Moderation) |
| `AnswerClassifier` | no | `Classify(Answer) → GroupAssignment` when a free-text answer revision is stored; results are source-owned; absent = free-text polls are refused (see Free-text polls) |

**Post and poll images** live in media folders `{tenant}/post/{post_id}/` and
`{tenant}/poll/{poll_id}/` (`Media.PostKind`/`PollKind`). Register both kinds
with a `Named` upload path (`inline/{name}`, with a `Max`) and its public preset (`To:
"{name}.webp"`), route their `CanUpload` to `rt.Content.CanUpload` (PostWrite
or PollWrite, and the post or poll must exist) and their `Hooks.Resolver` to
`rt.Content.MediaResolver()`, and pass
`content.Media{URLs: urls, Folders: jobs}`, where `urls.InlineURL` is the
public preset's URL (`reg.PublicURL(ref, name+".webp")`; a pure function).
The editor uploads each image with the SDK's `upload(file, {ref: {kind:
"post", id}, path: "inline/x.png"})` (browser to bucket; the server names it
`i-{uuid}`), then hands the name to ContentKit. Covers and poll images store
that name, not the public URL; reads derive the URL from the current registry.
The public URL serves the kind's default until the worker renders it:

| Route | Body | Result |
|---|---|---|
| `POST /posts/{id}/images` | `{"image": "i-…"}` | `{"url"}` to place in the body |
| `PUT /posts/{id}/cover` | `{"image": "i-…"}` (`""` clears) | `{"cover_url"}` |
| `PUT /polls/{id}/image` | same | `{"image_url"}` |
| `PUT /polls/{id}/options/{oid}/image` | same | `{"image_url"}` |

Create and update bodies take no image URLs, so images are added once the post
or poll exists. The public URL serves after the image job runs (seconds), and
only while the post is published: `MediaResolver` shows a post's folder like
the post (a draft, scheduled, held or rejected one only to its author and
PostWrite holders, as editors). Its editors see an unpublished post's images
through an editor read (`GET /media/{post kind}/{id}?editor`, signed
`editor_url`s); the SDK's `usePost` does this for them.
Post creation, edits, moderation decisions and soft deletion queue `ExposeTx`
in the content transaction. Soft deletion hides public media and keeps private
sources; deleting a poll still queues its folder's deletion. Replaced images
stay in the folder until it is deleted.

Migration `0007_inline_image_names` renames `content_posts.cover_url` to
`cover_name` and the poll question/option `image_url` columns to `image_name`.
It refuses nonempty legacy image fields without modifying them. This is a
coordinated cutover: stop old readers/writers, update host direct SQL readers
and legacy importers to the name columns and registry-derived URLs, then apply
the migration and start the new code. Doujins and Hentai0 currently read
`content_posts.cover_url` directly; neither can adopt this migration unchanged.
The JSON fields above remain `cover_url` and `image_url`. This migration must
not deploy independently of the host adoption work.

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

Each `Screen` is bounded by `Options.ModeratorTimeout` (5s); a failure holds
the write. After 3 consecutive failures the moderator is skipped (writes held
at once) for `Options.ModeratorCooldown` (30s); then one trial call reaches
it while other writes are still held, and its result closes or reopens the
breaker. Register
`Runtime.CheckModerator` as an optional dependency (helpers `deps`) so a
tripped moderator shows on statusz and `app_dependency_up`.

## Anonymous participation

Signed-out visitors only read unless `content.Options.Anonymous` says
otherwise: `Comments` (under an `anon_name`), `Reactions` (likes and
dislikes of works, posts and comments, keyed by IP) and `Votes`
(multiple-choice polls, keyed by IP). Free-text answers and favorites always
need a signed-in actor. A refused anonymous attempt is `401 unauthorized`.
`GET /config` answers the setting (`{"anonymous": {"comments", "reactions",
"votes"}, "comment_max_length"}`) and `GET /{kind}/{id}/can-comment` carries
`anonymous`, so a client shows a sign-in prompt or an anonymous form from the
server's answer. `content.Options.CommentMaxLength` (default 400 characters)
bounds comments and edits: longer is `422 comment_too_long` with
`details.max`; the standing carries it as `max_length`.

## Interaction limits

ContentKit limits each actor's interactions itself: the user id, or the IP
for an anonymous actor (an actor with neither is not limited here). Hosts
keep no limiter around these routes; the edge (Traefik) keeps the generic
per-IP ceiling. Every attempt spends one, so undoing (unfavorite, neutral,
`DELETE .../reaction`) costs the same as doing. Each rate is "at most `Count`
in any `Per`"; an action with two must pass both.

| Action | Routes | Default |
|---|---|---|
| `comment` | `POST /{kind}/{id}/comments` (top-level and replies) | 5 per 5 minutes and 20 per hour |
| `comment_reaction` | `POST /comments/{cid}/like\|dislike\|neutral` | 30 per minute |
| `post_reaction` | `/posts/{id}/like\|dislike\|neutral`, and `/post/{id}/...` | 30 per minute |
| `reaction` | `/{kind}/{id}/like\|dislike\|neutral`, `DELETE /{kind}/{id}/reaction` | 30 per minute |
| `favorite` | `POST` and `DELETE /{kind}/{id}/favorite` | 20 per minute |
| `poll_vote` | `POST /polls/{id}/vote`, `POST /polls/{id}/answer` | 30 per minute |

Override any of them in `content.Options.Limits` (`Comment`, `CommentReaction`,
`PostReaction`, `Reaction`, `Favorite`, `PollVote`, each `[]content.Rate{{Count, Per}}`).
`Limits.Redis` (a Redis or Garnet client) shares the counts across replicas
under `Limits.KeyPrefix` (default `contentkit:content:rl:`) plus the tenant;
without it each process counts alone (logged at start). A Redis error falls
back to the per-process count for a second and adds to the expvar
`contentkit_content_ratelimit_redis_errors`. A refusal is `429 rate_limited`
with `Retry-After` and `{"action", "retry_after"}` in the body
(`*content.RateLimitError`, `errors.Is(err, content.ErrRateLimited)`).

## Comment bans

A ban is current state only: `{user, scope, reason, until, by}`, no history.
It stops every new comment of that user in its scope: top-level, replies (to
anyone, themselves included) and edits of their comments, on any content in
the scope, their own included. It hides and deletes nothing, and does not stop
reactions, favorites or votes. Scopes:

- `owner:<id>`: content whose resolver `Owner` is `<id>`; managed by that
  owner, any signed-in user, over their own scope only.
- `global`: the whole tenant; managed by holders of `Perms.CommentBan` (an
  operator permission, e.g. AuthKit `root:comments:ban`), over that scope only.

| Route | Scope | Result |
|---|---|---|
| `GET /comment-bans?limit=&offset=` | the caller's owner scope | `[CommentBan]`, newest first |
| `PUT /comment-bans/{user}` `{"reason","until"}` | the caller's owner scope | the ban (created or replaced) |
| `DELETE /comment-bans/{user}` | the caller's owner scope | `204`, idempotent |
| `GET`, `PUT`, `DELETE /global-comment-bans[/{user}]` | `global` | the same |
| `GET /{kind}/{id}/can-comment` | the caller on that target | `{"can_comment", "ban", "anonymous", …}` |

`until` is optional (absent: until lifted) and must be in the future; an
expired ban stays listed with `expired: true` until lifted or replaced.
`CommentBan` carries `user_id`, `user` (from `UserEnricher`), `scope`,
`reason`, `until`, `banned_by`, `banned_at` and `expired`. A banned write is
`403 comment_banned` with `ban: {scope, reason, until}` (never who banned);
when both scopes apply it reports the one lasting longer
(`*content.BannedError`, `errors.Is(err, content.ErrCommentBanned)`). Anonymous
commenters have no identity to ban. `EraseSubjects` removes the bans of an
erased user and of their owner scope, and blanks them as `banned_by`.

## Free-text polls

`content_poll_questions.kind` is `multiple_choice` (options + votes, as
before) or `free_text`: one answer per signed-in actor in
`content_poll_answers`, editable until the poll closes. `closes_at` (optional,
`PATCH`-able, `null` clears it) and `is_active = false` close a poll for votes and answers alike
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
  answers, with exact counter decrements, and of comment bans of them or in
  their owner scope (global bans they set stay, without their id);
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

`worker.SyncOnce` holds no connection or transaction across host callbacks
or sink calls, so a one-connection pool suffices and callbacks may query the
same pool. Each write is a short transaction fenced by the dirty revision:
overlapping ticks only repeat work, and a stale build never overwrites a newer
one. Sink effects happen after the keyword row commits and before the row is
acknowledged. Both sink operations carry the dirty revision: apply only newer
versions atomically and retain a tombstone version after deletion. This fences
operations that finish remotely after the caller sees a timeout. A failed sink
stays queued under a new revision. Callbacks must be bounded, read-only and
respect cancellation.

## Listing with access

A paywalled listing filters in SQL, not after `LIMIT`: hide mode then
returns full keyset pages. `gate.Filter(ctx, actor)` reads the viewer once
(see README [Paywalls and listings](README.md#paywalls-and-listings-access));
`Filter.SQL(access.Columns{…})` turns it into one boolean predicate with pgx
named args (prefix `ck_`, or `ArgPrefix`). Its text depends only on the
columns, so one prepared statement serves every viewer. Put the same
predicate inside a search `Eligibility` to get hide mode in keyword search.

```go
postCols := access.Columns{ID: "p.id", Kind: "post", Level: "p.access_policy",
	Levels: map[string]access.Level{"public": access.Public, "membership": access.MembersLevel,
		"ppv": access.PPV, "members_ppv": access.MembersPPV},
	Scope: "p.channel_id", ScopeKind: "channel"}
// One tier flag (Doujins, Hentai0):
galleryCols := access.Columns{ID: "g.id", Kind: "gallery", TierName: "premium",
	Level:  "CASE WHEN g.is_premium THEN 'premium' ELSE 'public' END",
	Levels: map[string]access.Level{"public": access.Public, "premium": access.TierLevel}}
```

- `Kind` must be a `GateConfig.OwnedKinds` entry, `ScopeKind` a
  `MemberKinds` entry and `TierName` a declared tier: the filter then holds
  the viewer's whole holdings and the predicate is exact. `Tier` instead of
  `TierName` names a per-row tier column.
- A level value missing from `Levels` is never admitted, owned or not.
- Every `Columns` string is trusted SQL; never build one from input.
- Lock mode (locked items shown with a paywall) uses `f.Allows(item)` per row
  and `gate.Decide` for `Sell`/`Requires`, with no further read.

Indexes the listing needs:

| Listing | Index | Plan |
|---|---|---|
| Global feed | `(published_at DESC, id DESC) WHERE <published>` | index scan in order, access as a filter, stops at `LIMIT n+1` |
| Scope page | `(channel_id, published_at DESC, id DESC) WHERE <published>` | `channel_id = $1` index condition |
| Sparse visibility | the two above and the primary key | `Filter.Branches`, below |

A viewer who sees a small fraction of a feed scans far for each page. Split
the predicate by access class with `Filter.Branches` (public, tier, members,
owned, candidates; classes that can match nothing are left out), limit each
branch on its own index and merge:

```sql
SELECT * FROM (
  (SELECT … FROM posts p WHERE <published> AND <keyset> AND <branch 1> ORDER BY p.published_at DESC, p.id DESC LIMIT @n)
  UNION
  (SELECT … FROM posts p WHERE <published> AND <keyset> AND <branch 2> ORDER BY p.published_at DESC, p.id DESC LIMIT @n)
) page ORDER BY published_at DESC, id DESC LIMIT @n
```

**Truncation.** A keyspace holding more than `HeldLimit` keys (default 1,000,
at most 10,000) leaves `Filter.Complete(kind)` false, and the predicate also
admits that keyspace's candidate rows. `gate.Settle(ctx, actor, f, items)`
decides them in one exact read and returns `keep` and `short` (a dropped row:
the page may hold fewer rows). It reads nothing when the filter decided every
row, so call it on every hide-mode page. Only viewers past the limit get
short pages.

**Serving.** `gate.Resolver(facts)` is the `ContentResolver` for content and
media: one `Facts` query per batch, at most one billing read, and
`Accessible = Visible && !Withheld && (Editor || Bypass || allowed)`. In a
request that already read the `Filter` it costs nothing for items of declared
kinds. A media token (1 h TTL in 4 h
windows) opens every private file of its item, so a refund or revocation
reaches a viewer who holds one within about 5 hours.

## Media

Media is a self-describing file system in one private bucket; no database
table records what media exists. The app declares everything in one registry
at startup (`media.Config`); ContentKit hard-codes no kinds, names or
layouts.

- **Items.** An item is `{namespace}/{kind}/{id}/`: the host's version id is
  the item id (ContentKit has no versions; a work's versions are separate
  items). It holds `manifest.json` (gzip JSON, never served), `private/`
  (hash-named blobs: uploads, derived files, editor views), `public/`
  (app-declared names such as `cover-460.webp`) and `temp/` (in-flight
  server-side writes).
- **Manifest.** An ordered virtual file system over the item's private
  blobs: each Upload's files in natural order unless an op reorders them,
  then each private preset's outputs in their uploads' order. A derived file
  records `from`, `preset` and `fp` (its inputs' fingerprint); it is stale
  exactly when the fingerprint no longer matches. Public files are never in
  it: their names are deterministic.
- **URLs.** `https://media.<site>/v1/{namespace}/{kind}/{id}/{public|private}/{name}`.
  `media.PublicURL(base, ns, kind, id, name)` and `media.SrcSet(…)` (or
  `Registry.PublicURL`/`SrcSet`) build public URLs with no lookup; a missing
  public file is served its kind's default by the media gateway.

### Registry

```go
var Gallery = media.Kind{Name: "gallery", KeepOriginals: true,
	Uploads: []media.Upload{
		{Path: "originals/{name}", Types: images, MaxBytes: 10 << 20},
		{Path: "cover", Types: images, MaxBytes: 10 << 20},
	},
	Private: []media.Private{
		{Name: "thumb", From: "originals/{name}", To: "thumb/{name}.webp", Image: &media.Image{Width: 460, Height: 650, Fit: media.FitCover}},
		{Name: "high", From: "originals/{name}", To: "high/{name}.webp", Image: &media.Image{Quality: 90}},
		{Name: "zip", To: "download/pages.zip", Download: "{title}.zip", Zip: "high/"},
	},
	Public: []media.Public{
		{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{230, 460, 920},
			Image: media.Image{Aspect: media.Ratio("46:65")}, Default: "cover.png"},
		{Name: "preview", From: "originals/{name}", To: "preview-{n}.webp", First: 3, Image: media.Image{Width: 1280}},
	},
	Defaults: defaultsFS, // holds cover.png
}

var Video = media.Kind{Name: "video", KeepOriginals: true,
	Uploads: []media.Upload{
		{Path: "source", Types: videoTypes, MaxBytes: maxVideo},
		{Path: "subs/{name}", Types: media.SubtitleTypes, MaxBytes: 32 << 20},
		{Path: "poster", Types: images, MaxBytes: 10 << 20, Frames: "source"},
	},
	Private: []media.Private{
		{Name: "hls", From: "source", To: "hls/", HLS: &media.HLS{Ladder: []int{2160, 1080, 480}}},
		{Name: "mp4-1080", From: "source", To: "video/source-1080p.mp4", Download: "{title} (1080p).mp4", MP4: media.Rung(1080)},
		{Name: "vtt", From: "subs/{name}", To: "vtt/{name}.vtt", Subtitles: &media.Subtitles{}},
	},
	Public: []media.Public{{Name: "poster", From: "poster", To: "poster-{w}.webp", Widths: []int{640, 1280, 1920}, Default: "poster.png"}},
}

reg, err := media.NewRegistry(media.Config{Namespace: "doujins", BaseURL: "https://media.doujins.ai",
	Kinds: []media.Kind{Gallery, accountmedia.User},
	Hooks: media.Hooks{Resolver: resolver, CanUpload: authorizer, PurgePublic: purge, ItemReady: ready}})
```

- **Uploads** are paths: a literal (`cover`) or a pattern (`originals/{name}`);
  a file's path adds its extension (`cover.png`), and a put to the same stem
  replaces it. `Max` caps an Upload's files, `Frames` lets the `frame` op
  grab the upload from a video upload, `Named` lets the server name it (`i-{uuid}`, inline images;
  it needs a `Max`).
- **Private presets** have one producer: `Image` (per-file `Choose` in Go),
  `HLS` (byte-range fMP4 ladder, audio and subtitle tracks, seek sprite; each
  track's segment table is its own private blob), `MP4` (one rung, H.264),
  `Zip` (the files under a prefix, in manifest order), `Audio` (an HLS track
  and an M4A) or `Subtitles` (clean WebVTT; a video lists them after its own
  tracks). Nothing is upscaled.
- **Public presets** render an upload through its edit at fixed names,
  `{w}` for each width (a width past the edited image renders at its width,
  so every name exists). The object carries its `from` and `fp` as metadata.
  The first public preset of an upload bounds its edit: the crop is fitted to
  `Image.Aspect`, and an edit narrower than `MinWidth` fails.
- **Previews** are public presets with `First: N`: the first N attached
  uploads of a `{name}` Upload in manifest order, `{n}` in `To` their position
  from 1, so the URLs
  (`{BaseURL}/v1/{ns}/{kind}/{id}/public/preview-{n}.webp`) need no manifest.
  Anyone who can see the item sees them; a hidden item has none. A position
  is rendered again when another upload takes it (a reorder, an insert, a
  removal) or its upload is replaced or cropped again; its old image is
  deleted with that commit, so the name is a 404 until the worker has
  rendered it, and names past the last upload are deleted. This is the only way to show part
  of an item: there is no partial access to `private/`.
- **Originals.** `KeepOriginals` keeps uploads once their private outputs
  exist (otherwise the blob is dropped and the file marked `gone`; public
  presets' sources are always kept). `ServeOriginals` lists uploads in reads
  for viewers with access; otherwise the read API never returns an upload's
  path or URL.
- **Host-only presets.** `Private.HostOnly` keeps an output out of generic
  reads, downloads and playlists; a host route applies its own checks and
  calls `grant.HostURL(path, download)`, which answers the file's URL with
  the item token for a viewer with access.
- **`private/` is all or nothing.** One token per item opens every private
  file of it. `ServeOriginals` and `HostOnly` decide what reads list, not
  what the token reaches: a viewer with access who learns a blob's hash can
  fetch it. Put what must be gated separately in another item.
- **Defaults.** A kind's `Defaults` (an `fs.FS`, e.g. `go:embed`) holds the
  images its public presets name in `Public.Default`; `NewRegistry` checks
  they exist. A shared kind ships its own, so importing apps merge nothing.
- **Shared kinds.** A kind with `Namespace: "accounts"` lives at
  `accounts/{kind}/{id}/` and is served on every importing site's media host
  (account avatars). An app's own namespace is never a shared one. Every app
  importing a shared kind (hosts and media workers) must lock in one
  Postgres database: `PGLocker` takes advisory locks, which are per database,
  and Ceph RGW has no conditional PUT to fall back on.
- The stock worker reads the registry as JSON (`json.Marshal(reg)` into
  `MEDIA_KINDS_FILE`; `Choose` and hooks are not included).

### Uploads and commit ops

`rt.Handler()` serves the upload API at `/media/upload` (`RuntimeConfig.Uploads`);
mounted alone it is `media.UploadHandler(uploads, opts)`, behind the host's auth:

| Route | Does |
| --- | --- |
| `POST /presign {ref, path, type, size, sha256}` | Checks the path's Upload and `CanUpload`; answers `exists`, one `put`, or a `multipart` ticket, and the `path` to commit (cleaned, with an extension, server-named for `Named`). |
| `POST /parts`, `/parts/list`, `/complete`, `/abort` | Multipart parts bound to their SHA-256; resume; assemble. |
| `POST /commit {ref, ops}` | Applies ops in one conditional write, then enqueues placement and processing. Each uploader's commits are rate limited (`UploadOptions.Commits`, default 1/s, burst 30; 429 `rate_limited`), except for exempt grants. |
| `GET /frame?kind&id&path&t&w` | A JPEG still of a video upload for the frame picker (`UploadOptions.Frames`). |

Uploads land at a staged name, `temp/u-{uuid}`, which presign answers as
`blob` (or the folder's identical blob, with `exists`); the put op commits
that name. The media worker then hashes each staged upload and places it at
`private/sha256-{hex}` of its bytes before processing (an upload gone or
changed by then fails with `not_uploaded`), so no client ever writes under a
blob name. Server-side imports use `Uploads.Ingest`, which stages the same
way.

| Op | Does |
| --- | --- |
| `put {path, blob, index?, meta?, edit?, unattached?, create_id?}` | Adds an upload at its natural position (or `index` among its Upload's), or replaces the one with the same stem. A nonzero UUID `create_id` instead requires an unused stem; repeating that ID is a no-op retry, even after placement or attachment. |
| `edit {path, edit}` | Crops and rotates an image upload; nil clears. |
| `move {path, index}` | Reorders an upload; its outputs follow. |
| `rename {path, to}` | Renames an upload within its Upload, and its outputs. |
| `remove {path, takedown?}` | Removes an upload and its outputs (cancelling its processing). With `takedown` it also removes the frames grabbed from the upload and the zips that bundled it, then deletes at once the blobs the commit dropped, the removed uploads' editor views and staged objects, and the item's unused public files (purged). With an exempt grant (`UploadGrant.Exempt`, staff) it instead deletes every private blob the item no longer references, earlier versions and all editor views included, which costs re-rendering them; sent again on the path already gone, it runs that sweep again. If a takedown's deletes fail it says so: staff complete it with the exempt takedown, otherwise the leftovers go at the grace sweep. For anyone else a takedown of a path already gone is `not_found`. |
| `attach {path, index?, meta?}` | Makes an unattached upload part of the item. |
| `copy {from: {id, path}, to?, edit?}` | Copies an upload within the kind, including the current item. Copies within an Upload preserve current private outputs; another Upload or a supplied edit renders its own presets from the original. Blobs copied from another item are reserved like uploads (rate limits, pending quota) before they move. |
| `frame {path, t \| auto}` | Fills an upload from a frame of its `Frames` video; a new video grabs again. |
| `meta {meta}` | Sets the template values (`{title}` in download names). |
| `regenerate {preset?, force?}` | Asks the worker to redo stale outputs (all with `force`, which needs an exempt grant). |

`CanUpload` is asked for `UploadTarget{Ref, Path}` per op (the upload's stem;
"" for item-wide ops), and for a copy's source too. A new item starts hidden
unless anonymous viewers may see it (`Hooks.Resolver`). `ProcessOnUpload`
tells the SDK to commit uploads unattached as they land; readers leave them
out until `attach`.

### Reads and playback

`rt.Handler()` serves the read API at `/media` (`RuntimeConfig.Reader`,
`ReadLimit`); mounted alone it is `reader.Handler(media.HandlerOptions{Identity, Limit})`:

- `GET /{kind}/{id}?prefix=low-res/&offset=&limit=&download&editor` answers
  `{access, expires, meta, previews, total, hls, files: [{path, type, size, w, h, url | locked}]}`
  in manifest order. `access` is `full` or `none`. With access every listed
  file has a URL: plain under the item cookie
  (`Path=/v1/{ns}/{kind}/{id}/private/`) in cookie mode, with `?t={item
  token}` in URL mode. Without it there is no URL, token or cookie, and each
  file is `{path, type, size, locked}`: nothing names a blob. `previews` are
  the item's public preview URLs, for every viewer who can see it. `expires`
  is when the token stops working; a read before then answers the same token,
  so clients keep what they have and read again shortly before (the SDK's
  gallery and player do). With `download`, each URL carries the file's
  download name as an unsigned `dl`, which the gateway sends as
  `Content-Disposition: attachment` when the name is plain and has the
  file's type. With `editor` (editors only), the
  uploads come with `edit`, `frame`, `meta`, `pending`, `failed`, encode
  `progress` and an `editor_url`: the editor view, rendered on demand by the
  worker (`ReaderOptions.Queue`) into `private/` under the hash of its source
  and `Config.Editor`, and swept a grace period later. An editor read also
  carries the kind's `uploads`: each upload path's types, `max_bytes`,
  `max` files, crop `aspect` and `min_width`, and for video its limits in
  effect and the `min_aspect`/`max_aspect` its HLS presets accept, so
  clients refuse a file before uploading it. Viewers never get these
  fields, even for served originals.
- `GET /presets` lists every kind's public preset rules (`base`,
  `namespace`, `kind`, `name`, `from`, `to`, `widths`, `aspect`, `min_width`,
  `first`): crop bounds and widths without a read. Published files carry a
  generation suffix, so a URL built from `to` names the kind's default image;
  an item's current images come from a read's `public`.
- `GET /{kind}/{id}/hls/{dir}master.m3u8` (a ladder in `hls`), `{path}.m3u8`
  (a track) and `{dir}sprite.vtt` are built per request from the manifest's
  tracks and their index blobs (cached by hash).

### Public files, exposure and purge

- Call `jobs.ExposeTx(ctx, tx, ref)` whenever anonymous visibility changes
  (publish, unpublish, hold, soft delete, restore). Hiding deletes and purges
  `public/` at once; unhiding renders the public presets again from their
  kept sources. Paid items keep public covers; what they gate is `private/`.
- `Hooks.PurgePublic(ctx, urls)` hears every overwrite, deletion and first
  write of a public name (the CDN may hold the default). The worker's writes
  reach it through the host's media queue. After a change, the SDK refetches
  with `cache: "reload"`.
- Render the defaults from the deploy step:
  `image.PublishDefaults(ctx, store, reg)` (it needs libvips) writes each
  `Public.Default` from its kind's `Defaults` to
  `{ns}/{kind}/_default/public/{name}` and returns the keys
  to purge. `media.GatewayConfig(reg)` gives the gateway's namespaces and
  `MEDIA_GATEWAY_DEFAULTS` (`layout.FormatDefaults`).

### Processing and readiness

- A commit marks an upload `pending` with the presets it feeds; the worker
  clears each as it records the outputs. The worker also finds stale outputs
  itself (a changed spec or recipe after a deploy): `Jobs.Regenerate(ctx,
  kind, preset)` visits every item of a kind, and the `regenerate` op one
  item. A `meta` change rewrites download names only.
- An item is ready when no attached upload is pending (public presets count
  only while it is visible) and every zip packs its inputs; failed once
  nothing is pending and an upload failed for its current blob and edit.
  `Hooks.ItemReady(ctx, tx, ref, readiness)` runs in a host transaction
  after the worker's jobs, through the host's media queue; it must be
  idempotent. `Hooks.Failed` runs where the producer runs.
- Manifests stop at `media.MaxManifestBytes` (8 MiB of JSON). A commit is
  refused (413 `too_large`) when the item, processed, would pass that. If
  outputs still overrun it, the worker marks the item `full` instead of
  recording them: producers write no private output for it (public files
  still render), readiness is `full` and editor reads say `full: true`,
  until a commit frees the bytes the refused record was short of (the
  manifest's `deficit`, counting what removed uploads would still have
  added); a smaller shrink or removal re-runs nothing.
  The SDK's `waitFor` rejects with `too_large` while an item is full.
  Failure messages are capped at 300 bytes.
- `ManifestOptions.CacheBytes` (default 128 MiB) holds decoded manifests,
  each about three times its JSON: the largest costs 24 MiB, and a read
  that misses decodes it again (about 0.25 s and 60 MiB at 8 MiB, anonymous
  reads included). Size it for the hot set of large items; it never drops
  below two of the largest.
- Refused images record `failed` (`message`, `code`, `details`):
  `image_unreadable`, `image_too_large`, `image_too_small`,
  `animation_not_allowed`, `animation_too_long`, `animation_unsupported`,
  `checksum_mismatch`. GIF and WebP animations keep every frame, delay and
  the loop count; `Image.Animation: media.AnimationReject` refuses them.
  AVIF and HEIF stills need libheif with an AV1 decoder
  (`libheif-plugin-dav1d`); sequences are refused.
- Video and audio uploads are measured from their real packets, not the
  container's declared duration and rate. `Upload.Video`
  (`media.VideoLimits`) caps an upload's running time, frame area, output
  frame rate and planned encode work (output pixels × output frames over
  its HLS and MP4 rungs and codecs); zero fields take
  `media.DefaultVideoLimits` (4 h, 60 fps, 8192×4320 px, 4e13). An upload
  past them, or a video averaging under one frame a second, fails before
  any encode with `video_too_long`, `video_too_large` or
  `video_over_budget`; one outside its HLS presets' aspect bounds with
  `video_aspect_unsupported` (`details` has `min_aspect` and `max_aspect`).
  A commit past an upload path's `Max` answers `too_many_files` with
  `details.max`. A source declaring a faster rate than its packets
  hold is encoded at its real rate.

### Sweep, deletion and erasure

- The sweep collects garbage by manifest reference: private blobs the
  manifest does not reference once they are older than `JobsConfig.Grace`
  (24 h) by their own age, however often the item is edited; public names no
  preset expects at once (purged); and `temp/` after `TempTTL` (48 h). A
  periodic pass covers every folder. An unreferenced blob that old cannot be
  committed again by name (`not_uploaded`: upload it again). For content
  that must go now, use `remove` with `takedown`.
- `jobs.DeleteItemsTx(ctx, tx, media.Deletion{Ref, Owner})` deletes items from
  the host's delete transaction (a second pass catches late uploads); a host
  deletes every version item of a work, and account erasure deletes
  `accounts/user/{id}/` once. `Jobs.Purge` deletes now; `SweepOrphans` finds
  folders the host no longer has.
- Restore: `s3.Store.Restore(ctx, prefix, t)` brings back manifests and
  their referenced blobs; then `Expose` the restored items.

## Media worker schema

Each host names its own media worker River schema, e.g. `doujins_media_worker`
and `hentai0_media_worker`, and passes it everywhere:
- `workqueue.Migrate(ctx, pool, schema)` and `workqueue.New(pool, registry, schema)`;
- `workqueue.NewProgressSource(pool, schema)`;
- `worker.Config.Schema` (`MEDIA_WORKER_SCHEMA`).

It is required. Hosts sharing a database must never share one: a worker
drains every job in its schema, so it would take another host's jobs (same
queue and job kinds) and fail them against its own registry and bucket. Queue
names (`media_place`, `media_image`, `media_video`, `media_audio`) are fixed
within a schema; every upload passes through `media_place`, so a worker must
run even for kinds without presets. Autoscalers (KEDA) count `{schema}.river_job` rows of `media_video`
and `media_audio` (available/running/retryable).

It is never the host's own River schema either: River elects one leader per
schema, and only the leader enqueues periodic jobs and runs maintenance, from
its own client's config. A media worker leading the host's schema would stop
the host's periodic jobs. The worker migrates its schema itself.

## Production media delivery

- **Media host**: serve `cmd/media-gateway` at `media.<site domain>` (same
  site as the pages) and use cookie delivery (`Delivery{Mode: DeliverCookie,
  CookieDomain: "<site domain>"}`); URL delivery only for apps without cookies.
  Either way one item token opens the whole item's `private/`, and viewers
  without access get none.
- **Media gateway config**: `MEDIA_GATEWAY_HOSTS` maps each media host to the
  namespaces it serves (`media.<domain>=<tenant>,accounts`); URLs are
  `https://media.<domain>/v1/{ns}/{kind}/{id}/{public|private}/{name}`.
  `MEDIA_GATEWAY_CORS_ORIGINS` exactly your sites' origins
  (`https://<domain>,https://www.<domain>`; no wildcards, no third parties);
  `MEDIA_GATEWAY_DEFAULTS` the public names that fall back to a kind's
  `_default` item. `Cross-Origin-Resource-Policy` is always `same-site`.
- **Bucket**: private (no public ACL or policy); the gateway's key reads only
  `*/private/*` and `*/public/*`; only the hosts write.
- **CDN**: may cache `public/` in a shared cache: names are fixed and
  overwritten in place (`public, max-age=300, stale-while-revalidate=86400`,
  revalidated by ETag), so wire `Hooks.PurgePublic` to purge every URL it is
  given. Never cache `private/` in a shared cache: the token is not part of a
  cache key the CDN checks, so a cached object would be served without one.
  Private blobs are hash-named and `private, max-age=31536000, immutable`.
- **Denials are 404 by design**: a missing, malformed, expired, wrong-scope
  or unknown-key token on `private/` gets the same response as a
  missing object (status, headers, body; `Cache-Control: no-store`), decided
  before any bucket request, so a response never reveals that protected
  content exists. The reason is logged at debug (`media-gateway: denied`).
  Clients that re-grant on expiry must treat 404 on these URLs as "refresh
  and retry once"; the SDK player's `refresh` does.
- **Key rotation**: add the new key to every access worker as
  `MEDIA_GATEWAY_TOKEN_KEY` with the old one as `_TOKEN_KEY_PREVIOUS`, then
  switch the hosts' `Delivery.SigningKey`, then drop the previous key after
  the longest token lifetime (TTL rounded up to the window, about 5 h by default).
  A scope change does not revoke already-issued item tokens: they remain
  valid until expiry while their signing key is accepted. If immediate
  revocation is required, coordinate a key replacement without accepting the
  old key and have clients refresh their media grants.
- **Scraping** is limited in three places, none of them the media gateway
  (it keeps no state):
  - `ReaderOptions.Issuance` bounds the distinct items a viewer is given the
    token of per hour (default 120 per account, 600 per anonymous IP;
    `Issuance.Redis` shares the count across replicas, else it is per
    process). It is enforced in `Reader.Grant`, so reads, playlists and
    `HostURL` routes all pass it; over the limit the read API answers 429
    `rate_limited` with `Retry-After`, and `Grant` returns a `LimitError`
    (`ErrRateLimited`). Opening the same item again within the hour is free;
    viewers without access, the item's editors and `Issuance.Exempt` actors
    (staff) are not counted. An anonymous viewer is counted by `Actor.IP`
    (an IPv6 address by its /64). The read API fills it from the connection;
    a host route that calls `Grant` or `Read` itself for an anonymous actor
    must set it, or the grant fails with `ErrNoViewerKey` rather than put
    every anonymous viewer in one count. The per-process count holds at most
    65,536 viewers; past that it forgets some, who start again.
  - `HandlerOptions.Limit` bounds requests to the read API (default 2/s,
    burst 120 per viewer).
  - The ingress of the media host limits downloads per IP (Traefik's
    `rateLimit`). Clients treat its 429 as "over the limit" and do not retry
    in a loop (the SDK does not).

  So an account pulls at most `Issuance.PerHour` items an hour, each at the
  ingress's rate. Behind a proxy set `Actor.IP` to the client's address, or
  every anonymous viewer is counted as the proxy.
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
- **Visibility**: wire `Hooks.Resolver` and call `ExposeTx` on every
  visibility change; `DeleteItemsTx` removes `public/` first.

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
the worker through `store.Lister`/`store.Builder`. `RuntimeConfig.Taxonomy:
taxonomy.Handler(store)` serves the admin routes at `/taxonomy` to actors
holding `Perms.Taxonomy`; mounted alone, put it behind your admin authorization. Adoption: [docs/taxonomy-migration.md](docs/taxonomy-migration.md).

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
- `Popular` serves at most `Config.MaxDepth` works (default 2000) and reads
  power-of-two prefixes, so a request-chosen offset can neither deepen a read
  nor add cache entries. `popularity.NewMemoryCache(maxBytes)` is LRU within
  its byte budget; a host cache passed as `Cache` must be bounded too.

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
- Replace `content.Options.Storage`/`Media` (`StorageConfig`, `MediaStore`) with `Options.Media` over media, register the `post` and `poll` kinds with a `Named` upload path and its public preset, and move image uploads to the SDK's `upload` plus the image routes above (`POST /posts/media`, multipart `POST .../cover|image` and `image_url`/`cover_url` in write bodies are gone).
- Adopt the one media model (v0.62.0): declare the registry (`media.Config`), use version ids as item ids, replace slots with upload paths and public presets, and wire the hooks; see "Media".
- Rename direct SQL on `social_*` tables to `content_*` (`social_entity_counts` → `content_interaction_counts`) and `content.Options.PrivateDataEraser` to `ProviderDataEraser`.
