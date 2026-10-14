# ContentKit Host Integration Guide

ContentKit runs in the host's process as an engine (or standalone, `server/`)
for one tenant. The host configures it, mounts its routes and passes
providers; it never reads or writes ContentKit's storage.

## Contract

One `New`, one `Start`, the routes mounted under one prefix, each route group
at its sub-path under `/v1` ([docs/api/routes.md](docs/api/routes.md)):

```go
ck, _ := contentkit.New(ctx, cfg, contentkit.Deps{
	Postgres: pool, ClickHouse: ch, Redis: rdb,
	Auth:  ak.Authenticator(), // who a request is (authkit/auth)
	Users: ak.UserInfo(),      // authors' names (authkit/userinfo)
	Store: bucket, Moderator: moderator, BodyProcessor: sanitizer,
})
_ = ck.Start(ctx, contentkit.WithRiverClient(fleet)) // fleet built with ck.RiverJobs()
routes, _ := ck.Routes(contentkit.Routes{Prefix: "/api/contentkit", Groups: contentkit.AllRoutes})
for _, r := range routes {
	mux.Handle(r.Path, r.Handler)
}
```

## Auth

ContentKit has no auth of its own and no auth middleware goes in front of
it. The host implements two standard-library-only contracts from AuthKit's
module (`github.com/open-rails/authkit/auth` and
`github.com/open-rails/authkit/userinfo`), or passes AuthKit's implementations.
ContentKit links neither AuthKit's server nor its database runtime:

- `Deps.Auth` (`auth.Authenticator`, `ak.Authenticator()`): who a
  request is. Each route's gate asks it at most once per request and builds
  the `access.Actor` from the `Verified`'s `Identity`: `ID` is the subject,
  `Kind` a person (`auth.SubjectUser`) or an application, `Invoker` who acts
  on the subject's behalf when someone does, `Verified` the credential.
- `Deps.Users` (`userinfo.Lookup`, `ak.UserInfo()`): authors'
  names and usernames (Authors below).

ContentKit names its staff permissions, always the full
`root:<resource>:<action>`, as typed constants (`access.Permission`,
`access.Permissions()`); the host names none and spells none out:

| Constant | Permission | Holders may |
|---|---|---|
| `access.ContentWrite` | `root:content:write` | write, publish, delete and restore any content item and its images |
| `access.ContentViewRestricted` | `root:content:view-restricted` | see every content item in any state, listing, site or filter |
| `access.PollsWrite` | `root:polls:write` | write and delete polls, options and images; see inactive ones |
| `access.CommentsModerate` | `root:comments:moderate` | edit, delete and restore anyone's comments; list every comment |
| `access.CommentsBan` | `root:comments:ban` | ban users from commenting anywhere in the tenant |
| `access.ModerationReview` | `root:moderation:review` | list and resolve held comments and content |
| `access.GroupsWrite` | `root:groups:write` | create, edit, merge and delete groups, and set memberships |

A user-owned group kind with `auth_scope` (channels) has its own
permissions, held in each group's AuthKit scope, whose persona declares
`access.GroupPermissions(kind)`: `{kind}:content:write` (its content),
`{kind}:settings:manage` (the group), `{kind}:offers:write`,
`{kind}:media:write` and `{kind}:comments:moderate` (delete, hold, release
and ban on the group's content). The group's owner holds them all.

Declare them in the auth's roles from the constants, so a rename breaks the
build instead of drifting:

```go
rbac := authkit.NewRoles()
perm := map[access.Permission]iam.Perm{}
for _, p := range access.Permissions() {
	perm[p] = rbac.Root.Declare(p.String())[0]
}
rbac.Root.Role("moderator", perm[access.CommentsModerate], perm[access.ModerationReview], perm[access.CommentsBan])
rbac.Root.Role("editor", perm[access.ContentWrite], perm[access.PollsWrite])
```

Staff hold them in the Authenticator's root scope, which it names (AuthKit's
`auth.Root`, `RootScope(ctx)`, asked per check: unresolved is 503, never a
grant). There is no scope or permission
setting: a staff member's permissions are root-wide, and per-item rights are
the resolver's (`Owner`, `Editor`). The gate asks the `Verified`'s
`Can(root, perm)`, live. `New` refuses an Authenticator that names no
root scope, or whose roles lack one of the permissions
(`auth.PermissionCatalog`).

| Tier | Admits | Refuses |
|---|---|---|
| `open` (`/config`, `/media/presets`, `/codes/{code}`) | anyone; never authenticates | |
| `public` | anonymous without a credential, else the subject it proves | a credential that fails: 401 `unauthorized` |
| `user` | a signed-in person | anonymous: 401; an application: 403 `forbidden` |
| `staff` | a person or an application holding the route's permission in the root scope | anonymous: 401; without it: 403 |

Personal interactions (comments, reactions, votes, favorites, answers, own
bans) are a person's: an application gets 403 on public routes too. No staff
action asks for a recent sign-in: they are moderation and editing, all
reversible. Every refusal has `auth.Refuse`'s status and `WWW-Authenticate`
(a DPoP challenge stays the provider's) and ContentKit's body; an auth
outage, a panic or a `Can` error is 503 `unavailable`, and a credential that
ended between `Authenticate` and `Can` is 401. Hooks get the actor:
`access.Actor.Can(ctx, scope, perm)` asks its `Verified`, and `Person()`
tells a person from an application.

Anonymous actors are keyed by `Request.RemoteAddr` (reactions, votes,
limits). Behind a proxy, rewrite it to the client's address in a
trusted-proxy step before the mount, or every anonymous visitor is the proxy.

A host mounting a module alone passes the same Auth: `content.Options.Auth`
(and `UserInfo`), `media.HandlerOptions.Auth`,
`media.UploadHandlerOptions.Auth`, `groups.Store.Handler(auth)`;
`contenturl.Router.Handler` needs none.

Hosts use the routes and `ck.Client()` (the same surface in process).
Search, typeahead, signals and discovery run inside the engine; their routes
land with #122.

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
Group ids follow the same rule: a group is a search document keyed by its
id (`CreateGroups` mints one when omitted).

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

Every content page is `/{lang}/{route}/{CODE}[/{slug}]`, e.g.
`/en/watch/G4VRQ3ZQ5/night-before-the-counteroffensive`. Every page is under
its language, the default included. The server reads only the code; sub-pages
are query parameters (`/en/g/{CODE}/{slug}?p=12`), so the slug can always be
omitted. The host chooses each kind's route; ContentKit owns the language
prefix, the code, resolution and canonicalization.

- **Codes** are 9 uppercase Crockford base32 characters (no `I L O U`), with at
  least one letter, random, unique per tenant across every kind, assigned once,
  never changed or reused. `ParseCode` accepts any case, `O`→`0`, `I`/`L`→`1`
  and hyphens. A request with another spelling of the code is redirected to the
  canonical one.
- **Registration.** Groups and content items register themselves (group
  slugs come from canonical names, one per language; content slugs from each
  language's slug or title). The bulk import keeps a record's code (`code`).
- **Versions.** A gallery's editions share its page; the language prefix picks
  the default edition (the first live version serving the language). A
  specific version is `?v={version code}` (`contenturl.VersionParam`), never a
  path segment, so a version's canonical URL is its language page. Version
  codes are 4 Crockford characters with a letter (`ParseVersionCode`), unique
  within their content, fixed for the version's life; every version wire type
  carries `code`, and the import keeps a version's `code`. A `v` naming no live
  version shows the default edition. Hosts drop their own parameters
  (Doujins `?ver=`, Hentai0 `?v=<uuid>`).

```go
urls, _ := contenturl.New(contenturl.Options{Pool: pool, Schema: "hentai0", Tenant: "hentai0"})
byRef, _ := urls.Links(ctx, refs) // list pages: code and slugs for each ref
```

- **Canonical redirects.** The router maps kinds to routes. Mount its middleware
  in front of the pages, or in front of the SPA shell. A missing language
  prefix, a missing or stale slug, a wrong route, a merged code or a trailing
  slash gets a 301 to the canonical path, with the query (`?v=` included)
  kept. An unprefixed request (an old link) goes to the language
  Accept-Language names, else the default (the first of `Languages`, which
  the router requires). Every path ContentKit answers (content, cards, groups,
  the codes and alias lookups, sitemap feeds) carries its language prefix and
  that language's slug. `Visibility` returns `Visible`, `Hidden` or `Gone`.
  `Hidden` behaves like an unknown code and serves the host's 404 without
  leaking the slug. `Gone` answers 410. The engine's codes route group answers
  content items' visibility itself (`catalog.Store.Visibility`). For groups, map the group state: a deleted group
  is `Gone`, and merged groups already resolve to their survivor.
  `kinds.Config.Routes()` maps every content and group kind to its route.
  A canonical request reaches the page
  with `FromContext(ctx)`, and gets a `Link: <…>; rel="canonical"` header when
  `BaseURL` is set.

```go
router, _ := contenturl.NewRouter(urls, contenturl.RouterOptions{
	Routes: cfg.Routes(), Languages: []string{"en", "es"}, BaseURL: "https://hentai0.com",
	Visibility: func(r *http.Request, links []contenturl.Link) ([]contenturl.Visibility, error) { /* drafts, removals, per link */ },
})
mux.Handle("/", router.Middleware(spa))
// the engine's codes route group serves GET /v1/codes/{code}[?lang=&v=] (v: the live version's id) and POST /v1/codes/aliases, routed by Config.Content's kinds
```

- **Legacy aliases.** The bulk import (`POST /v1/import`, `aliases`; see
  [docs/import.md](docs/import.md)) records the old site's identifiers. Each
  alias is `(source, legacy_kind, key)`, for example
  `("hentai0-legacy", "video", "346791971")` or
  `("doujins-legacy", "tag-name", "netorare")`. An alias is written once:
  repeating it is a no-op and re-pointing it is a conflict. The host keeps
  its legacy URL-shape rules. It calls `POST /v1/codes/aliases?lang=`
  (`Client.PostCodesAliases`, up to 100 per call) and answers a 301 to
  `resolved.path` (`found`, with `locator` for a position such as a page),
  a 410 (`gone`) or a 404 (`not_found`), in one hop and before language
  negotiation.
- **Routes are reserved.** A content route serves only content pages: under
  it, any 9-character segment with a letter is read as a code, and a third
  segment as a slug. So `/watch/{CODE}/comments` redirects to the page. Put
  sub-pages in the query string, and keep other host pages off content routes.
- **Browser.** `@openrails/contentkit-ui/urls` (no dependencies) parses codes and
  builds paths from API links (`{content_kind, code, slug, slugs}`):
  `path(link, {language, query, version})` and `url()` always prefix the
  language (`languagePrefix`), `version` adds `?v=`; `hreflang()` lists every
  language plus `x-default`, the default language's prefixed URL, never a
  `?v=`. Its `canonical` result drives a client-side `history.replaceState`.
  It never computes slugs; those come from the server. `useContentDetail`,
  `VersionPicker` and `useVersionMenuItems` read and write `?v=` themselves
  (through the provider's `navigate`, else the browser history).
- **Tables.** `content_codes(tenant_id, code, content_kind, content_id, slug,
  slugs, merged_into)` and `content_code_aliases`. Host SQL may join
  `content_codes` on `(tenant_id, content_kind, content_id)`, for example for
  sitemaps.

## Interactions (`content`)

The content module owns comments, reactions, favorites and polls in
ContentKit's `content_*` interaction tables. They target ContentKit's own
content items (optionally one version, `?version=`) and groups, stored under
the target's kind from `kinds.Config` and its id: `(tenant_id, content_kind,
content_id, content_version_id)`. Routes are
`/v1/contents/{id}/comments|like|dislike|reaction|favorite|can-comment` and
the same under `/v1/groups/{id}/`, then `/comments/{cid}/...`,
`/comments/latest`, `/comments/admin?content_kind=&author=`, `/polls...`,
`/moderation/...`, `/comment-bans...` and `/global-comment-bans...`. A merged
group answers as its survivor; a deleted one, or content the request's site
excludes, is `404`.

Comments carry a `language` (one of the site's, or none): `?language=`
narrows a list or the latest feed to one thread, and a reply joins its
comment's. A content item's `comments` setting (`PATCH /v1/contents/{id}`,
its editors) is `open`, `closed` (existing comments show; new ones and edits
answer `403 comments_closed`) or `hidden` (none show). Comments on content
follow its access, as its `body` does: to a viewer who can't open the item (or
a version-level comment's version), and doesn't edit it, lists, replies and the
latest feed withhold them, the standing (`GET .../can-comment`) says `locked`,
and posting, replying, editing and reacting answer `403 comments_locked`.
Counts stay visible; moderation queues keep them. The reader's favorites
and reactions, as cards, are `GET /v1/me/favorites` and `GET
/v1/me/reactions?target=contents|groups&kind=&value=like|dislike` (tag
preferences); their counts reach `content_facets` (`likes`, `dislikes`,
`favorites`, `rating`) through the catalog's counts job.

Ports (in `content` unless qualified):

| Port | Required | Contract |
|---|---|---|
| `Auth` | yes | `auth.Authenticator` (Auth above); the engine fills it from `Deps.Auth` |
| `UserInfo` | no | `userinfo.Lookup`: authors' names (Authors below); the engine fills it from `Deps.Users` |
| `Media` | no | poll images and avatars in ContentKit media (see below); absent = image routes answer 501 |
| `ContentProcessor` | no | rich-text sanitizer for comment bodies (default strips tags) |
| `ContentModerator` | no | `Screen(ModerationInput) → Verdict{Decision, Reason, Model, PromptVersion, Confidence}` before a comment or catalog text publishes; absent = publish (see Moderation) |
| `AnswerClassifier` | no | `Classify(Answer) → GroupAssignment` when a free-text answer revision is stored; results are source-owned; absent = free-text polls are refused (see Free-text polls) |

**Authors.** Comments, replies, the latest-comments feed, held items and
bans show each author as `{id, name, username, avatar, avatar_srcset}`:
`name` and `username` from `UserInfo.Get` (never the email), with one `Get`
per response, never per author, and the avatar at its stable URL, built
from the id (`Config.Avatars`, Avatars below). An id the directory does not
hold (a deleted account) has no author; a directory failure is logged and
leaves bare ids, never failing the response. Without `UserInfo` authors are
bare ids.

**Content and poll images.** A content item's cover is its content-level
media item's (`{tenant}/{kind}/{id}/`, uploads marked `Content`); its body
images are its body item's (`{tenant}/{kind}.body/{id}/`, `media.BodySuffix`,
derived by `media.ForContent` from the kind's `Named` `Content` uploads and
their public presets). A body item shows only to viewers who open the item
(and its editors): a locked item's body images have no public files and are
never listed or signed to anyone else, while a buyer's body shows them
through the item token (a private `{preset}-open` rendition); an item anyone
opens keeps public files. Poll images live in `{tenant}/poll/{poll_id}/`.
Each is a `Named` upload (the server names it `i-{uuid}`) with a public
preset. The catalog and the content module answer their uploads and reads.

Nothing stores an image URL: a public file's name carries a generation that
changes on every republish. Covers and poll images store the name; a body
stores a reference, `contentkit:i-{uuid}` (the SDK's `imageRef`), which
`Deps.BodyProcessor` must keep. Reads resolve them to current public files in
one lookup per response; one without a public file stays as it is.

| Route | Body | Result |
|---|---|---|
| `POST /v1/contents/{id}/images` | `{"image": "i-…"}` | `{"ref", "url"}`: the reference for the body; a URL that shows it now (null until rendered) |
| `PATCH /v1/contents/{id}` | `{"cover": "i-…"}` (`""` clears) | the content, `cover.url` null until it has a public file |
| `PUT /v1/polls/{id}/image` | same | `{"image_url"}` (same) |
| `PUT /v1/polls/{id}/options/{oid}/image` | same | `{"image_url"}` (same) |

Public files exist only while the item is live; its editors see an
unpublished item's images through an editor read (the SDK's `useContent`).

There is no `Recorder` and no `Moderation` port: reactions and favorites feed
the signal plane through ContentKit's own preference outbox, and the
`ContentModerator` decides comment and catalog text publication.

The tenant is pinned at construction and stamped on every row; a `ContentRef`
of another tenant passed to any read is `content.ErrTenant`, never remapped.
The host imports existing data with explicit tenant and content references
after initializing fresh stores ([docs/migration.md](docs/migration.md)).

## Moderation (comments and content)

Every comment create/edit and every text write of live or publishing content
is screened
through `Options.Moderator` after sanitizing; the moderator sees the tenant,
the opaque actor, the content reference, the kind, the item id on an edit,
and the text (title, description and body for content; a held edit of a
live item keeps serving its approved text). The verdict sets the item's
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

Review queue: `GET /v1/moderation/held?kind=comment|content&cursor=&limit=` pages
held comments or content oldest-first (each carries the body, the content
reference, the reason, model, prompt version, confidence, any moderator error
and `revision`); `POST /v1/moderation/{kind}/{id}/resolve
{"revision","decision","reason"}` (the actor id is the reviewer) writes the
final state: `approve` publishes (counts and the keyword index follow),
`reject` keeps the item author-visible as `rejected` with the reason. Both
need `root:moderation:review`.
`GET /comments/admin` shows every state with real bodies and authors, by content kind or `author`.

Screening is revision-fenced. A decision must echo the held `revision`, so it
cannot publish an intervening edit. Edits are screened outside SQL
transactions, then a short source-revision CAS transaction commits the verdict;
a concurrent edit returns `409` for retry. Provider waits never hold row or
subject locks.

Banned words and IP bans are data staff edit (`root:comments:moderate`):
`GET|POST|DELETE /v1/moderation/banned-words` (`{"word", "action"}`: `mask`,
the default, replaces each word containing it with asterisks; `reject`
refuses the text) and `GET|POST|DELETE /v1/moderation/ip-bans`
(`{"network", "reason", "until"}`: an address or CIDR network). They apply to
every comment and free-text answer before the moderator; an IP-banned write
answers `403 comment_banned` with ban scope `ip`, and moderators are exempt.

`content.BasicModerator{AllowLinks, DupWindow}` is the deterministic default
(links, per-process duplicate guard); compose it in front of an AI moderator
with `content.Chain{basic, ai}`: the first reject/review wins, an error
fails the chain closed.

Doujins adapter (`moderator{svc}` on the old `Moderation` port): implement
`Screen` instead of `Check`; return `Verdict{Decision: DecisionReject, Reason}`
where it returned `RejectModeration(reason)`, `DecisionApprove` where it
returned nil; wire `Options.Moderator = content.Chain{&content.BasicModerator{},
userIntelligence.Moderator}` once User Intelligence's `moderate` module exists;
grant `root:moderation:review` and point the admin review page at
`/moderation/held` and `/moderation/{kind}/{id}/resolve`.

Each `Screen` is bounded by `Options.ModeratorTimeout` (5s); a failure holds
the write. After 3 consecutive failures the moderator is skipped (writes held
at once) for `Options.ModeratorCooldown` (30s); then one trial call reaches
it while other writes are still held, and its result closes or reopens the
breaker. The engine's `contentkit_moderator` probe fails while it is tripped.

## Anonymous participation

Signed-out visitors only read unless `content.Options.Anonymous` says
otherwise: `Comments` (under an `anon_name`), `Reactions` (likes and
dislikes of content and comments, keyed by IP) and `Votes`
(multiple-choice polls, keyed by IP). Free-text answers and favorites always
need a signed-in actor. A refused anonymous attempt is `401 unauthorized`.
`GET /config` answers the setting (`{"anonymous": {"comments", "reactions",
"votes"}, "comment_max_length"}`) and `GET /{kind}/{id}/can-comment` carries
`anonymous`, so a client shows a sign-in prompt or an anonymous form from the
server's answer. `content.Options.CommentMaxLength` (default 2,200 characters)
bounds comments and edits: longer is `422 comment_too_long` with
`details.max`; the standing carries it as `max_length`.

## Interaction limits

ContentKit limits each actor's interactions itself: the user id, or the IP
for an anonymous actor (an actor with neither is not limited here). Hosts
keep no limiter around these routes; the edge (Traefik) keeps the generic
per-IP ceiling. Every attempt spends one, so undoing (unfavorite,
`DELETE .../reaction`) costs the same as doing. Each rate is "at most `Count`
in any `Per`"; an action with two must pass both.

| Action | Routes | Default |
|---|---|---|
| `comment` | `POST /{contents\|groups}/{id}/comments` (top-level and replies) | 5 per 5 minutes and 20 per hour |
| `comment_reaction` | `POST /comments/{cid}/like\|dislike`, `DELETE /comments/{cid}/reaction` | 30 per minute |
| `reaction` | `POST /{contents\|groups}/{id}/like\|dislike`, `DELETE …/reaction` | 30 per minute |
| `favorite` | `POST` and `DELETE /{contents\|groups}/{id}/favorite` | 20 per minute |
| `poll_vote` | `POST /polls/{id}/vote`, `POST /polls/{id}/answer` | 30 per minute |

Override any of them in `content.Options.Limits` (`Comment`, `CommentReaction`,
`Reaction`, `Favorite`, `PollVote`, each `[]content.Rate{{Count, Per}}`).
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
- `global`: the whole tenant; managed by holders of `root:comments:ban` (an
  operator permission, e.g. AuthKit `root:comments:ban`), over that scope only.

| Route | Scope | Result |
|---|---|---|
| `GET /comment-bans?limit=&offset=` | the caller's owner scope | `[CommentBan]`, newest first |
| `PUT /comment-bans/{user}` `{"reason","until"}` | the caller's owner scope | the ban (created or replaced) |
| `DELETE /comment-bans/{user}` | the caller's owner scope | `204`, idempotent |
| `GET`, `PUT`, `DELETE /global-comment-bans[/{user}]` | `global` | the same; `GET /global-comment-bans/{user}` is that user's ban, `404` when none |
| `GET /{kind}/{id}/can-comment` | the caller on that target | `{"can_comment", "ban", "anonymous", …}` |

`until` is optional (absent: until lifted) and must be in the future; an
expired ban stays listed with `expired: true` until lifted or replaced.
`CommentBan` carries `user_id`, `user` (Authors), `scope`,
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
and the engine's reclassify job retries every pending answer every 10
minutes, each sweep from the start. The classifier
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

`ck.EraseSubjects(ctx, subjects)` erases deleted accounts from every configured
plane: signals ([completion contract](#subject-erasure-completion-contract)),
ContentKit interaction data, avatars (on the avatar primary; see Avatars)
and data retained by moderator/classifier providers. A
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
- redaction of the subject's unpublished payloads (held/rejected comments)
  and their moderation metadata. An item with a previously approved payload keeps it
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

## Search documents

ContentKit builds its documents: one per content item and language from the
catalog (title, aliases, its groups' names as keywords; a held edit indexes
the approved text) and one per group and language. Writes mark them dirty in
their own transaction; discovery's search applies the catalog's visibility
rule (`catalog.Viewer`) inside every retrieval route, before any limit.

## Discovery (`/v1`, route group `discovery`)

Hosts list, search and recommend only through these routes
([docs/api/routes.md](docs/api/routes.md)); ContentKit applies the catalog's
visibility rule (access, site scope, content filters, listing and language)
over `content_facets`, and answers one card per content item with the version
that fits the reader (`display_language` first, then position).

- `GET /contents`: `kind`, `groups` (all of), `exclude_groups`, `owner_group`,
  `creators` (credited user ids), `access` (`public`: what anyone opens now;
  `tier`, `group`, `ppv`, `group_ppv`: what stays locked at that level),
  `tiers` (locked behind one of them: a subscribe page's premium), `traits`,
  `attribute=name:value` (declared filterable attributes), `released_on`
  (`YYYY-MM-DD` or `YYYY-MM`, in `tz`), `view_status`
  (`unseen|in_progress|completed`, signed in), `language` (only content
  serving it), `display_language`, `mode` (`lock` marks locked cards,
  `hide` lists only what the reader opens), `sort` (`newest`, `oldest`,
  `title`, `popular[:period]`, `top_rated[:period]`, `most_favorited`,
  `most_viewed` (all-time viewers plus an imported `view_baseline`),
  `position` in the first of `groups`, which must be of an ordered group
  kind, `random`), `cursor`, `limit`. A reader's content filters never hide
  a group's content from a list requiring that group: a hidden tag's page
  shows what its count counts, while other lists leave it out.
- `GET /search` (`q`, the same filters; `relevance` personalized for a
  signed-in reader, or any list sort of the best 1000 matches), `GET
  /typeahead` (contents and groups).
- `GET /contents/{id}/similar`, `GET /recommendations`: `Deps.Candidates`
  (co-engagement by default), admitted through the reader's visibility.
  Recommendations fill from the last 30 days' popularity, then all-time,
  then the newest content the reader has not seen.
- `GET /release-dates` (`by=day|month`, the list filters): counts and free
  counts per date.
- `GET|PUT /me/content-filters`: the configured `content_filters`, each the
  reader may show or hide, and `include_deleted`.
- `GET|DELETE /me/history`, `POST /signals` (views and clicks; signed out,
  `session` names the browser), `POST /exposures`.
- `GET /me/favorites`, `GET /me/reactions`: what the reader favorited or
  liked and disliked, contents as cards or groups, most recent first.
- `GET /sitemap/contents?kind=`, `GET /sitemap/groups?kind=` (open): the
  request site's live, listed pages by id (`cursor`, up to 10000 a page),
  each with `lastmod` and its path per language it serves. Content filters
  and access don't apply.
- Staff (`root:content:view-restricted`): `GET /stats/activity` (views,
  viewers and signed-in viewers per day) and `GET /stats/top` (`metric`
  views or comments, `period`, `access`, `tiers`). `GET /me/stats`: the
  reader's finished contents per kind, comments and the reactions they got.
- `GET|DELETE /me/searches` (signed out, `session`): the reader's recent
  searches, one per query, newest first, which `GET /search` keeps (its
  `session` names a signed-out browser). A clear hides what came before it
  (`?query=` removes one); an hourly job deletes hidden ones, erased
  people's and those unused for 180 days. Erasure deletes them.

A card carries title and path in the display language, cover renditions, the
version (languages, traits, pages, duration), the version's groups with pages
by kind, likes, dislikes, favorites and all-time viewers, the access decision
(`allowed`, `sell`, `requires`, `preview`, `free_after`), the signed-in
reader's favorite and progress, `published_at` and attributes. Content
detail (`GET /contents/{id}`, `.../versions/{vid}`) carries the same
`viewer` standing on the item and each version, to resume from. Without a
signal plane (no ClickHouse, or one that is down, which fails the
`contentkit_clickhouse` probe with the cause) the engine runs on Postgres:
`GET /v1/kinds` says `"signals": false`, contentkit-ui sends no signals or
exposures, popular sorts use what `content_facets` holds, and signal reads
and writes answer 501 `not_configured`, or 503 `unavailable` while a
configured one is down. The engine retries a down ClickHouse in the
background (migrations, connection, schema) and checks a live one every ten
seconds; signals come back on without a restart, and contentkit-ui reads the
model again a minute after it last found them off.

## Search job

The engine's search job (every 30 s, and at start) drains the dirty queue and
runs a bounded backfill. It holds no connection or transaction across
builders; each write is a short transaction fenced by the dirty revision, so
overlapping runs only repeat work and a stale build never overwrites a newer
one.

## Media

Media is a self-describing file system in one private bucket; no database
table records what media exists. The app declares everything in one registry
at startup (`media.Config`); ContentKit hard-codes no kinds, names or
layouts.

- **Items.** An item is `{namespace}/{kind}/{id}/`, or for a content kind
  (`Kind.Versions`, derived from the kinds config by `media.ForContent`) a
  version's `{namespace}/{kind}/{content}/{version}/` beside its content's
  item, which holds the `Content` uploads (the cover). An item holds
  `manifest.json` (gzip JSON, never served), `private/` (hash-named blobs:
  uploads, derived files, editor views), `public/` (published renditions
  such as `cover-460-{generation}.webp`) and `temp/` (in-flight server-side
  writes).
- **Content versions.** `media/versions` answers media's hooks from the
  content model: editors (staff, creators, owner-group editors) upload, a
  person their own avatar; reads follow the catalog's
  visibility, plus segments' release and the rule's preview;
  a version's media writes its pages, duration, size and
  audio onto `content_versions` (`Facets` refreshes listings). A `Publish`
  kind reaches viewers only through `{kind}.published` items: one per
  segment of its pages, a locked version's previews (its rule's, else its
  kind's `preview`), and one playback item,
  planned by `GET /v1/contents/{id}/versions/{vid}/reader`. Forks, page-archive
  imports, reader metadata and downloads (a host watermark, quota buckets)
  complete it. A content with no cover of its own gets its first live
  version's leading released page as its cover (an upload marked
  `auto_cover`); a cover an editor uploads replaces it for good.
- **Manifest.** An ordered virtual file system over the item's private
  blobs: each Upload's files in natural order unless an op reorders them,
  then each private preset's outputs in their uploads' order. A derived file
  records `from`, `preset` and `fp` (its inputs' fingerprint); it is stale
  exactly when the fingerprint no longer matches. Each upload records its
  public publications: the current generation of each preset and, while
  the worker writes one, its successor.
- **URLs.** `https://media.<site>/v1/{namespace}/{kind}/{id}[/{version}]/{public|private}/{name}`.
  Public names carry a generation that changes on every render, so a URL
  built from a preset's `to` template is never valid: it serves the kind's
  default image, or 404 when the preset declares no `Default`. Build public
  URLs with the lookup in "Public image URLs".

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
	Kinds:   []media.Kind{Gallery, avatar.User()},
	Avatars: media.Avatars{Kind: "user", Preset: "avatar"}, // people's avatars (Authors)
	Hooks:   media.Hooks{Resolver: resolver, CanUpload: authorizer, PurgePublic: purge, ItemReady: ready}})
```

- **Site images.** A kind no content or group kind owns (nor `Avatars`, nor
  `poll`) holds images of records the host keeps, keyed by the host's id:
  Doujins' translation requests' covers. In the engine anyone reads its
  public files (`GET /v1/media/{kind}/{id}`) and holders of
  `root:content:write` or `root:import:write` upload them.
- **Account avatars** are the engine's `Config.Avatars` (Avatars below).
  `avatar.User()` (`github.com/open-rails/contentkit/media/avatar`) is the
  standard kind: JPEG, PNG and WebP up to 10 MiB, square WebP at 64, 128,
  256 and 512 pixels, a default silhouette (publish it with the other
  defaults through `media/image.PublishDefaults`). Its preset is `Stable`:
  besides its generations, the current image is kept at the logical names
  (`avatar-128.webp`), so its URL is built from the person's id and the
  gateway serves the default there until one is uploaded.
  `Registry.Avatars(ids...)` builds them without a lookup. A host building
  its own media without the engine authorizes the kind in `CanUpload`: a
  person their own, staff anyone's (exempt).

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
  `accounts/{kind}/{id}/` and is served on every site's media host
  (account avatars). An app's own namespace is never a shared one. One app
  holds a shared namespace, in its bucket and journal, processed by its
  worker; other sites reach it through its API (Avatars below) and serve
  its files from their media hosts (`MEDIA_GATEWAY_BUCKETS`).
- The stock worker reads the registry as JSON (`MEDIA_KINDS_FILE`; `Choose`
  and hooks are not included): an engine's is `Config.MediaKinds()`, printed
  by `contentkit media-config`.

### Avatars

```go
// The primary (Doujins): its media holds every avatar.
cfg.Avatars = &contentkit.AvatarsConfig{} // avatar.User(), preset "avatar"

// Another site sharing its people (Hentai0): keeps none.
cfg.Avatars = &contentkit.AvatarsConfig{
	Primary: "https://doujins.dev/api/v1/contentkit", // the primary's routes
	Key:     os.Getenv("CONTENTKIT_AVATARS_KEY"),     // the primary's API key: root:avatars:write
}
```

- Every author and the content model's `avatars` (`GET /v1/kinds`, a
  `Stable` preset rule) name avatars at the site's own media host,
  `{media}/v1/accounts/user/{id}/public/avatar-{w}.webp`: nothing is looked
  up. The gateway's resource policy is same-site, so each site's host
  serves them: `MEDIA_GATEWAY_HOSTS` gives every media host `accounts` and
  `MEDIA_GATEWAY_BUCKETS` maps `accounts` to the primary's bucket.
- On the other site, a person's avatar presigns, commits and editor reads
  (`/v1/media/upload/{presign,commit}`, `/v1/media/{kind}/{id}`) go through
  its own routes, which forward them to the primary's with `Key`: only
  their own avatar, within `Media.CommitLimit`. Their token never leaves
  their site and its AuthKit; the browser's one cross-site request is the
  presigned PUT to the primary's bucket (the bucket's CORS lists every
  site). Read URLs come back on the site's media host. The site keeps no
  avatar row, object or worker job; a primary that is down answers 503.
- The primary authorizes a person's own avatar, and anyone's to holders of
  `root:avatars:write` (`access.AvatarsWrite`): staff, and the other sites'
  keys (AuthKit API keys in the root group, a role with that permission
  alone), which it answers with token URLs rather than cookies.
- contentkit-ui's `AvatarUpload` defaults to the viewer's avatar from the
  content model, `useAvatar(id)` builds an image, and both work unchanged
  on either site.
- `EraseSubjects` erases avatars on the primary only; people are shared, so
  the primary's erasure (the account deletion every site shares) removes
  them.

### Uploads and commit ops

The upload route group serves the upload API at `/v1/media/upload`:

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
blob name. The upload routes admit a signed-in person or application;
`CanUpload` decides. Importers upload with `Client.IngestMedia` (presign,
PUT or parts, commit; [docs/import.md](docs/import.md)); a file whose `From`
names a key of the item's bucket outside ContentKit's namespaces is copied
server-side instead, so no byte passes through the importer. Server-side
code calls `Uploads.Ingest`, from a stream (`Body`) or the same copy (`From`).

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
| `regenerate {preset?, force?}` | Asks the worker to redo stale outputs and retry uploads that failed for their current blob and edit, which any writer of the item may do; `force` redoes every output and needs an exempt grant. |

`CanUpload` is asked for `UploadTarget{Ref, Path}` per op (the upload's stem;
"" for item-wide ops), and for a copy's source too. A new item starts hidden
unless anonymous viewers may see it (`Hooks.Resolver`). `ProcessOnUpload`
tells the SDK to commit uploads unattached as they land; readers leave them
out until `attach`.

### Reads and playback

The media route group serves the read API at `/v1/media`
(`Config.Media.ReadLimit`):

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
  generation suffix, so a URL built from `to` is not an item's image; its
  current images come from a read's `public` (or, server-side, the lookup
  below).
- `GET /{kind}/{id}/hls/{dir}master.m3u8` (a ladder in `hls`), `{path}.m3u8`
  (a track) and `{dir}sprite.vtt` are built per request from the manifest's
  tracks and their index blobs (cached by hash).

### Public image URLs

Every item's current publications are projected into Postgres
(`content_media_publications`) in the same journal transaction that makes
them current, so server-side rendering resolves a whole page with one
indexed query and no bucket read. `*media.Manifests` (`jobs.Manifests()`):

- `PresetImages(ctx, "cover", refs...)` returns one `PublicImage` per ref, in
  order: its first current image of the preset, else the preset's default
  (`From` is empty), else one without renditions. Use it for cards and
  avatars: `image.URL(width)` picks a rendition, `image.SrcSet()` gives
  `srcset` with the encoded widths.
- `Images(ctx, media.ImageQuery{Ref, Preset, Name}...)` answers many queries in
  one read: an item's images, one preset's, or one upload's by its `{name}`
  (an inline image's `i-{uuid}`). Store names, not URLs, and resolve them
  when rendering; post bodies store references (Interactions).
- `PublicImages(ctx, ref)` is one item's images; `Registry.DefaultImage`
  is a preset's default.

A republish keeps the current files published until their successor is
ready, and cleanup retires a generation only after the projection names the
next one. Browsers keep using a read's `public` or `usePublicImage`.

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
  `{ns}/{kind}/_default/public/sha256-{digest}.webp` and returns the
  mappings. `media.GatewayConfig(reg, publication.Defaults)` gives the
  gateway's namespaces and `MEDIA_GATEWAY_DEFAULTS` (`layout.FormatDefaults`).

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

## Upgrading from v0.67

Since v0.68 ContentKit reads no item stored by v0.67 or earlier until the
media upgrade has converted it; v0.68.0 itself has no upgrade, so skip it.
Reads answer 503 `upgrade_required` (`media.ErrUpgradeRequired`
in Go), commits are refused, and cleanup, recovery and workers leave such an
item untouched.

Code changes:

- `media.JobsConfig.Journal` is required: `media.NewPGJournal(pool, contentSchema, queue)`.
  `UploadOptions.Queue` is gone; the journal supplies the queue.
- `worker.Config.ContentSchema` is required.
- `POST /media/upload/commit` requires `operation_id` (a canonical UUID,
  reused unchanged on retry); `Uploads.Commit` takes it.
- `GatewayConfig(reg, defaults)` takes the defaults `image.PublishDefaults` returned.
- `Registry.SrcSet` and `media.SrcSet` are removed: build public URLs with
  the lookup ("Public image URLs"), never from a preset's template.
- River jobs `contentkit_media_recover` (every minute) and
  `contentkit_media_upgrade`.

Order:

1. Stop the v0.67 media worker.
2. Deploy the host: its migrations run, and `contentkit_media_upgrade`
   starts with its River client and queues batches until done. It converts
   each version 2 manifest in place under the commit journal: private blobs
   keep their names and become the item's allocations, and the
   template-named public files become their uploads' current publications,
   so nothing is copied or rendered again and existing URLs keep working
   until a later reprocess publishes a generation. It also projects every
   item's publications. It is idempotent: a crash leaves an item either
   unconverted or converted and recovered, and a rerun converges. A one-off
   command can run `jobs.Upgrade(ctx, limit)` in a loop instead.
3. Wait until `jobs.UpgradeStatus(ctx)` reports `Done` (also in
   `content_media_upgrades`). Items it cannot convert are listed in
   `Failures` (`content_media_upgrade_failures`) with the reason and left as
   they were (`upgrade_required`) while the pass goes on; only an unreachable
   store or database fails the job. Failures are retried on host start and
   daily, so a fixed release retries them when deployed.
4. Deploy the media gateway and media worker images at the same version.
   The gateway serves both the legacy and the current names.

## Upgrading from v0.69

Post bodies store image references (`contentkit:i-{uuid}`), never URLs;
reads resolve them (Interactions). There is no compatibility for stored
public URLs: a body that names one keeps naming a file that goes stale.

- `content.Media.URLs` (`MediaURLs.InlineURL`) is replaced by
  `Images`: pass `jobs.Manifests()`. `NewRuntime` fills `Media.Reader` from
  its `Reader`.
- `POST /posts/{id}/images` answers `{"ref", "url"}`: store `ref`, show
  `url`. Cover and poll image routes answer null until the image has a
  public file.
- Posts carry `cover` (the name) and `images`; `body` is resolved.
- The host's `PostBodyProcessor` must keep `contentkit:` image URLs.
- Rewrite stored bodies: replace each image URL of a post's own folder
  (`…/{kind}/{post_id}/public/i-{uuid}….webp`) with `contentkit:i-{uuid}`.
- SDK: `usePost` stores references (`update`, `storedBody`) and shows them
  (`editorBody`, `imageSrc(name)`, `coverSrc`); `media.uploadInline` adds
  `ref`, and its `url` is a preview (null while none exists);
  `posts.imageURL` is `posts.inlineImage`. `defaultImage` and `usePublicImage` give no image for
  a preset without a default (`PresetRule.default`).

## Upgrading from v0.70

ContentKit takes the host's auth through AuthKit's contract interfaces and builds its own
gates (Auth). No compatibility: the old ports are gone.

- `RuntimeConfig.Auth: ak.Authenticator()` and `UserInfo: ak.UserInfo()`
  replace `content.Options.Identity`, `Authz` and `Users`; delete the auth
  middleware in front of the mount and the `Identity`/`Authorizer` code.
  `content.Perms` is gone: ContentKit names its permissions
  (`access.Permissions()`, full `root:<resource>:<action>` names), held in
  the Authenticator's root scope. Declare them in the auth's roles from the
  constants (Auth); there is no permission or scope setting.
- `adapters/authkit` is deleted: authors are ContentKit's (`Config.Avatars`),
  and account uploads are a few lines of `CanUpload` (Account avatars).
- `media.HandlerOptions.Identity` and `UploadHandlerOptions.Actor` are `Auth`;
  `media.Identity`, `content.Identity`, `content.Authorizer`,
  `content.UserEnricher` and `content.Runtime.Guard` are gone.
- `access.Actor.Kind` is an `auth.SubjectKind`; it gains `Invoker`,
  `Verified`, `Can` and `Person`. Hooks check staff with
  `actor.Can(ctx, scope, perm)`.
- Behind a proxy, rewrite `Request.RemoteAddr` to the client's address:
  anonymous actors are keyed by it.
- Applications (API keys) may use staff routes by permission; personal
  interactions refuse them (403). A failed permission check is 503 (or 401
  when the credential ended), no longer 403.
- `/config`, `/media/presets` and `/codes/{code}` never authenticate; every
  other route answers 401 for a credential that fails, with the provider's
  `WWW-Authenticate`.
- `PublicUser` gains `name`. A deleted account's comments have no `author`.
- Authentication and user-directory types now come from AuthKit v1.24.0:
  change imports from `github.com/open-rails/helpers/auth` and
  `github.com/open-rails/helpers/userinfo` to `github.com/open-rails/authkit/auth`
  and `github.com/open-rails/authkit/userinfo`; the conformance kit is
  `github.com/open-rails/authkit/auth/authtest`. These Go types have the new
  package identity; update the host's implementations and every library
  sharing them together. Shared `helpers/deps` and `helpers/river` utilities
  remain dependencies. A custom host needs no AuthKit runtime.
- The separately versioned `adapters/openrails` module now targets OpenRails
  v0.240.0. Its client port takes `CheckEntitlementsParams` with `CustomerID`
  in the request. Custom wrappers must use that signature; embedded hosts
  pass the client returned by `Engine.MerchantBySlug`.

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
  namespaces it serves (`media.<domain>=<tenant>,accounts`) and
  `MEDIA_GATEWAY_BUCKETS` each namespace's bucket (`accounts` is in the avatar
  primary's); URLs are
  `https://media.<domain>/v1/{ns}/{kind}/{id}/{public|private}/{name}`.
  `MEDIA_GATEWAY_CORS_ORIGINS` exactly your sites' origins
  (`https://<domain>,https://www.<domain>`; no wildcards, no third parties);
  `MEDIA_GATEWAY_DEFAULTS` the public names that fall back to a kind's
  `_default` item. `Cross-Origin-Resource-Policy` is always `same-site`.
- **Bucket**: one per app, private (no public ACL or policy); the gateway's
  key reads only `*/private/*` and `*/public/*` of each; only the hosts write.
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
    (an IPv6 address by its /64). The read API fills it from `RemoteAddr`;
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
  ingress's rate. Behind a proxy rewrite `RemoteAddr` to the client's
  address, or every anonymous viewer is counted as the proxy.
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

## Groups

`groups.Store` owns one tenant's groups in ContentKit's schema; their kinds
come from `kinds.Config.GroupKinds`. Content joins a group at content level or,
for a `version_level` kind, one version; `SetContentGroups` replaces a
content's memberships in the kinds it names. Membership writes, group
deletes, parent moves and merges keep `content_facets.group_ids` current (a
version's effective groups, ancestors included: `EffectiveGroups`).
`Options.ContentChanged` runs in the same transaction for whatever else
derives from them. Engine jobs: `Recount` (every 15 minutes: (counts per group, content kind and
language from the facets, and automatic covers) and `RefreshWindows`
(every minute: featured-rail windows). The engine's search job builds group
documents. The group route group serves `/v1/groups…` and
`/v1/contents/{id}/groups`; writes need `root:groups:write`.

## Preference boundary (reactions and favorites into the signal plane)

Reactions and favorites reach the signal plane straight from their own rows.
`content_reactions.value` is -1/0/1 and `content_favorites.value` is 1/0:
neutral and unfavorite keep the row at 0. Every value change stamps the row
with `revision` from `content_preference_revision_seq` under the row lock; a
no-op allocates nothing. Only authenticated rows export; anonymous (IP)
reactions and comment threads never do.

- **Reference**: the reaction row, the counts rollup and the export all use
  the resolver's canonical reference (`Resolution.Ref`); comment reactions
  never export. `Counts` reads exactly the reference given.
- **Sync**: the engine's preference job runs every 30 s. It sends rows past the tenant's watermark in revision
  order as one `signal` event per subject × reference × axis (`Type` = axis,
  `EventID` = `"current"`, `Revision`, `OccurredAt` =
  row `updated_at`, `Value`), then records a checkpoint; the newest revision
  wins, so re-sends are harmless. Each scan restarts from the newest
  checkpoint older than `Options.PreferenceSyncOverlap` (default 5m) before
  the previous sync: a write is delivered if its transaction commits within
  the overlap of allocating its revision. A failed sync advances nothing.
- **Repair**: the engine re-sends every preference daily (zeros included),
  which repairs a sink loss.
- **Erasure**: `ck.EraseSubjects` deletes the subject's rows behind the source
  fence and fences the signal plane, which drops any late send.
- **Restore**: after restoring PostgreSQL against a retained signal plane, the
  preference revision floor must be raised to the sink's newest revision
  before writers resume: `POST /v1/import` with `preference_floor` (only ever
  advancing).

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
after the fence or came back with a restore) is unreadable; the engine's
erasure job removes it daily. After a restore, re-erase the subjects deleted
since the backup. A shared account exists in every tenant:
each host erases its own tenant.

## Discovery candidates

Similar and recommended lists get candidates from one port:

```go
type Candidates interface { // package discovery
	Similar(ctx context.Context, anchor contentref.ContentRef, q Query) ([]Candidate, error)
	ForSubject(ctx context.Context, subject signal.Subject, q Query) ([]Candidate, error)
}
```

Candidates are ranked best-first; oversample past `q.Limit`. The
`Recommender` applies one policy to every source: tenant and kinds, never the
anchor, seen and disliked works, the reader's visibility (admission in one
batch), dedupe, limit, and the popularity fill. `Deps.Candidates` nil is
`discovery.Engagement`; wrap a provider in `discovery.Fallback` so errors and
thin results fall back to engagement.

## Popularity (policy-ranked)

The engine's `contentkit_popularity` job (hourly, with ClickHouse) copies
`popularity.PolicyV1` scores per period (`7d`, `30d`, `90d`, `365d`, `all`),
each window's rating (Wilson lower bound of the subjects whose feedback is
positive) and all-time viewers into `content_facets.popularity`, so a filtered
`popular` or `top_rated` sort is one Postgres query. A view is scored by
`popularity.SessionScore` against its version: pages (reader kinds, 8 s of
dwell a page), seconds (player kinds) or scroll percent (post kinds). See
[docs/popularity-policy.md](docs/popularity-policy.md) for the formula.

## Migration checklist

- Import legacy data through `POST /v1/import` ([docs/import.md](docs/import.md)), never into ContentKit's tables.
- Create and reuse one `contentkit.Engine` per tenant.
- Read lists, search and recommendations through the discovery routes; send views as `POST /signals`.
- `contentkit.New` applies the baselines ([docs/migration.md](docs/migration.md)) and checks the signal schema.
- Wire `Options.Moderator` (a `Chain` of `BasicModerator` and the AI moderator), `Options.Classifier` for free-text polls, grant `root:moderation:review`, and schedule `ReclassifyPending`.
- Adopt the preference boundary (doujins #888 / hentai0 #594): pin this ContentKit, delete the callback-time bridge (`internal/social` `recorder`, `discovery.Recorder.Reaction`, `socialReactionSignal`) and every per-delivery signal-identity adapter, wire `Engine.EraseSubjects` into deletion, rewrite direct SQL readers (`split_part(entity_id, ':', 1)`, favorite-key helpers) to the canonical `content_id` and filter `content_favorites` on `value = 1`.
- Replace `socialkit` imports with `content`: `EntityRef`/`EntityKey`/`entity_type`/`entity_id` → `contentref.ContentRef`/`ContentKey`/`content_kind`/`content_id`; `Entities` → `Resolver`; `Content` → `Processor`; `EntityTypes` → `ContentKinds`; `parent_id` → `reply_to_id`; `Counts(kind, id)` → `Counts([]ContentRef)`; delete the `Recorder` and `Moderation` adapters.
- Replace `content.Options.Storage`/`Media` (`StorageConfig`, `MediaStore`) with `Options.Media` over media, register the content kinds' content-level `Named` uploads and the `poll` kind with its public preset, and move image uploads to the SDK's `upload` plus the image routes above.
- Adopt the one media model (v0.62.0): declare the registry (`media.Config`), use version ids as item ids, replace slots with upload paths and public presets, and wire the hooks; see "Media".
- Rename direct SQL on `social_*` tables to `content_*` (`social_entity_counts` → `content_interaction_counts`) and `content.Options.PrivateDataEraser` to `ProviderDataEraser`.
