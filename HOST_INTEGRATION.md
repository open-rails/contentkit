# ContentKit Host Integration Guide

ContentKit runs in the host's process as an engine (or standalone, `server/`)
for one tenant. The host configures it, mounts its routes and passes
providers; it never reads or writes ContentKit's storage
([Host references](#host-references)).

## Contract

One `New`, one `Start`, the routes mounted under one prefix, each route group
at its sub-path under `/v1` ([docs/api/routes.md](docs/api/routes.md)), and
`StartJobs` in the process that works jobs:

```go
ck, _ := contentkit.New(ctx, cfg, contentkit.Deps{
	Postgres: pool, ClickHouse: ch, Redis: rdb,
	Auth:  ak.Authenticator(), // who a request is (authkit/auth)
	Users: ak.UserInfo(),      // authors' names (authkit/userinfo)
	Store: bucket, Moderator: moderator, BodyProcessor: sanitizer,
})
_ = ck.Start(ctx) // serve: requests enqueue jobs
routes, _ := ck.Routes(contentkit.Routes{Prefix: "/api/contentkit", Groups: contentkit.AllRoutes})
for _, r := range routes {
	mux.Handle(r.Path, r.Handler)
}
```

### Jobs

ContentKit runs its background work (search indexing, preference sync,
popularity refresh, publication, retention and release work) on its own River
client, on `Deps.Postgres` in `Config.Database.RiverSchema` (default
`{schema}_river`), never in a host's fleet or schema. `Start` builds that
client so requests enqueue; it works nothing. The process that works jobs
calls `StartJobs` (it starts the engine first if needed); `Close` stops them:

```go
// API process
_ = ck.Start(ctx)
// worker command (`run-worker`)
_ = ck.StartJobs(ctx)
defer ck.Close(ctx)
```

Which process works jobs is code, never configuration. The standalone
`server/` calls `StartJobs` itself. Media processing is the stock
`contentkit-media-worker` image's ([Media worker schema](#media-worker-schema)).
`Engine.Reconcile` repairs derived catalog state (facets, access snapshots,
search marks and content paths) after a model change; the server CLI runs it as
`contentkit reconcile`.

## Host references

ContentKit can run embedded or as a remote service with its own database on
another machine. Hosts use only its client (`Engine.Client()` or
`contentkit.NewClient`), its routes and its event log: no SQL on ContentKit
tables, no shared transactions, no host callbacks inside ContentKit
transactions and no ContentKit table or schema names in host code.

A host keeps its own links to entities and editions, such as a translation's
artists and tags or its finished edition. ContentKit neither tracks nor
protects them: deleting or merging a linked entity and purging an edition are
allowed, and the host repairs its links.

| Need | Route (Go client) |
| --- | --- |
| Entities by id, in every state | `GET /v1/manage/entities?ids=` (`GetManageEntities`), at most 100: each with `state`, a merged one with its final survivor `merged_into`; unknown ids are absent. |
| Names to entities | `GET /v1/manage/entities/match?kind=&scope=&language=&normalized=&name=` (`GetManageEntitiesMatch`), at most 100 repeated `name`s: one result per name with at most two active, public ids (two is ambiguous). Literal by default; `normalized=true` folds case, accents and whitespace. An empty `scope` matches unscoped entities. |
| Edition status | `GET /v1/versions?ids=` (`GetVersions`), at most 100: `version_id`, `content_id`, `kind`, `languages`, `deleted`, `live`, `live_from`; a purged edition is absent. |
| Changes | `GET /v1/events` (`EventReader`): `entity.created`, `entity.updated`, `entity.deleted`, `entity.restored` and `entity.merged` beside the content and version events ([docs/events.md](docs/events.md)). |

The host reads the log with its own cursor and repairs what an event names:
links to a merged entity move to its `merged_into`, links to a deleted entity
or a purged edition are the host's to drop or keep. Whether an edition is
published now is `live_from` compared with the time; no event fires when it
passes. Queries that joined ContentKit tables read statuses in batches and
join in the host.

## Public surface

Hosts import the root package (`New`, `Config`/`Deps`, `Start`/`StartJobs`/
`Close`/`Ready`/`Probes`/`Reconcile`, `Routes`, `Client`/`NewClient`,
`EventReader`, account lifecycle, `MigratePostgres`/`MigrateClickHouse`),
`kinds`, `contentref`, `access` (permissions, keys, rules, `Actor`, the
`Billing` port), the wire and data types in `catalog`, `entities`, `channels`,
`content`, `contenturl`, `discovery`, `importer`, `signal`, `media` and
`media/versions`, and the stock `media/s3`, `media/avatar` and
`media/gateway` (with `media/token` and `media/layout`). Every store,
constructor, job and SQL helper is under `internal/`, which the compiler
keeps from hosts; `internal/boundaries` enforces the rest. The same code works
against an embedded engine (`Engine.Client()`) and a remote one (`NewClient`).

| Before | Now |
| --- | --- |
| `catalog.New`, `entities.New`, `channels.New`, `discovery.New`, `importer.New`, `contenturl.New`/`NewRouter`, their `Store`/`Options`/`Querier` | the routes, through the client |
| the blog group's `channels.New(...).List`/`Get` | `GetManageChannels(ctx, GetManageChannelsQuery{Slug: "blog", Limit: 2})`, then `GetManageChannel`: `auth_scope`, `owner_user_id` |
| `catalog.NewTotalsClient(base, hc).Totals` | `contentkit.NewClient(base, hc).GetCatalogTotals` |
| `catalog.RefreshPaths`, `Engine.ReconcileFacets` | `Engine.Reconcile` (`contentkit reconcile`) |
| `Engine.RiverJobs`, `WithRiverClient` | `Start` in API processes, `StartJobs` in the worker process ([Jobs](#jobs)) |
| `Engine.SetAccountDeleted`/`AnonymizeAccount` from a remote host | `PutAccountDeleted`, `PostAccountAnonymize` |
| `media.NewRegistry`, `media.Hooks`, `media/worker`, `media/workqueue`, `media/image`, `media/video` | the stock `contentkit-media-worker` image, `MEDIA_KINDS_FILE` from `Config.MediaRegistry()` |
| `workqueue.Migrate`, `migrations.Postgres` | `MigratePostgres` |
| `access.WithMemo`, `access.NewGate` | none: the engine memoizes each request |
| `catalog.ErrNotFound`, `entities.ErrConflict`, … | `*contentkit.Error` `Code` (`contentkit.CodeNotFound`, …) |
| `kindsroute.ContentModel` | `contentkit.ContentModel` |

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
| `access.ContentCreate` | `root:content:create` | create and read their own content outside channels |
| `access.ContentEditOwn` | `root:content:edit-own` | read, edit, publish and manage media of content they created outside channels |
| `access.ContentWrite` | `root:content:write` | write and publish content, delete and restore its versions, and manage its images |
| `access.ContentViewRestricted` | `root:content:view-restricted` | see every content item in any state, listing, site or filter |
| `access.PollsWrite` | `root:polls:write` | write and delete polls, options and images; see scheduled ones |
| `access.CommentsModerate` | `root:comments:moderate` | edit, delete and restore anyone's comments; list every comment |
| `access.CommentsBan` | `root:comments:ban` | ban users from commenting anywhere in the tenant |
| `access.ModerationReview` | `root:moderation:review` | list and resolve held comments and content |
| `access.EntitiesWrite` | `root:entities:write` | create, edit, merge and delete entities, and set memberships |
| `access.AccountsLifecycle` | `root:accounts:lifecycle` | deliver AuthKit's account lifecycle over HTTP (a host service) |

Channels have their own AuthKit group in the `channel` persona. Declare
`access.ChannelPermissions()` and compose host-defined contributor, editor and
moderator roles from those capabilities. `channel:content:create` permits own
submissions, `channel:content:edit-own` manages own posts, and
`channel:content:write` manages all content/versions in that channel. Settings,
channel media and comment moderation have separate capabilities. Current grants
determine authority; metadata associations and owner attribution grant nothing.
See [Posts and channels](docs/posts-and-channels.md) for the complete configuration,
normal migration call and fixed-blog/creator-channel examples.

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
`media.UploadHandlerOptions.Auth`, `entities.Store.Handler(auth)`;
`contenturl.Router.Handler` needs none.

Hosts use the routes and `ck.Client()` (the same surface in process).
Search, typeahead, signals and discovery run inside the engine; their routes
land with #122.

Ordinary content deletion always addresses versions: `Catalog.DeleteVersions` in
embedded Go, `Client.DeleteContentVersion` remotely, or
`DELETE /v1/contents/{id}/versions/{vid}`. Hidden single-version kinds expose their
sole version in `Content.Versions` and use the same operation. Deleting the final
undeleted version soft-deletes its parent in the same transaction. Restoring an
edition with `PutVersions` / `PATCH .../versions/{vid}` and `state: "published"`
restores only a parent automatically deleted for having no versions, preserving
its earlier draft or publication schedule. Moderation and owner deletion remain
separate embedded lifecycle operations and cannot be bypassed this way. There
is no ordinary parent-content DELETE route or Go method.

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
Entity ids follow the same rule: an entity is a search document keyed by its
id (`CreateEntities` mints one when omitted).

Media never adopts leftovers:

- A folder's first manifest edit (a commit) is refused with
  `media.ErrFolderNotEmpty` (`*FolderNotEmptyError`) over a previous item's
  blobs.
- `Jobs.Purge(ctx, media.Deletion{Ref, Owner})` deletes a folder now, the
  explicit reset for deliberate reuse. Content owners commit `RetireItemsTx`; their existing purge executes it.
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
- **Registration.** Entities and content items register themselves (entity
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

Cards and content reads carry each item's `code` and language-prefixed `path`.

- **Canonical redirects.** `GET /v1/codes/{code}?lang=&v=` (`Client.GetCode`)
  answers `contenturl.Resolved`; the host parses its own paths
  (`contenturl.ParsePath`, `AcceptLanguage`) and redirects to `resolved.path`. A missing language
  prefix, a missing or stale slug, a wrong route, a merged code or a trailing
  slash gets a 301 to the canonical path, with the query (`?v=` included)
  kept. An unprefixed request (an old link) goes to the language
  Accept-Language names, else the default (the first of `Languages`, which
  the router requires). Every path ContentKit answers (content, cards, entities,
  the codes and alias lookups, sitemap feeds) carries its language prefix and
  that language's slug. `Visibility` returns `Visible`, `Hidden` or `Gone`.
  `Hidden` behaves like an unknown code and serves the host's 404 without
  leaking the slug. `Gone` answers 410. The engine's codes route group answers
  content items' and entities' visibility itself: a deleted entity is `Gone`,
  and merged entities resolve to their survivor. `kinds.Config.Routes()` maps
  every content and entity kind to its route.

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

## Locked content (`kinds.Kind.Locked`)

What a viewer who doesn't open content (or a version) sees of it is each
kind's policy:

```yaml
content_kinds:
  gallery:
    locked:
      show: [cover, entities] # of description, cover, entities, attributes, length, reactions, favorites, views, comments
      preview: {pages: 3}   # optional: none unless set
```

Its id, code, path, title, languages, traits, owner, creators and paywall
(`decision`) always show; its body and private media never do. A kind
without `locked` shows `description, cover, entities, attributes, length`: no
preview, counts or comments. With it, exactly what it names.

| `show` | Withheld otherwise |
|---|---|
| `description`, `attributes` | the content's and locked versions' (empty) |
| `cover` | detail and cards (none); nor is it an entity's automatic cover |
| `entities` | detail and cards (none) |
| `length` | versions' and cards' `pages` and `duration_ms`, versions' `segments`; the reader plan's `pages` and locked segments |
| `reactions` | like and dislike counts (cards, `GET .../reaction`); liking and disliking answer `403 forbidden` |
| `favorites` | favorite counts (cards, `.../favorite`); favoriting needs only visibility: a wishlist is the reader's own |
| `views` | view counts (cards, detail) |
| `comments` | comments, as below |

Hidden counts are absent, never zero. `preview` is the free leading pages
when the access rule names none; a rule's own `preview` wins. Release copies
those released pages' reading renditions to the version's public area, and the
reader plan lists them as unsigned public URLs to every reader who does not
open the version.
Detail, cards (lists, search, typeahead, rails, similar, recommendations,
entity pages, `me` favorites and history), the reader plan and interactions
apply it; the media read API lists a locked item's public files alone (its
cover, and a version's free preview pages), never its private files or their
count.
Staff, creators and primary-channel editors see everything. Sitemaps,
codes and notifications carry only ids, codes and paths; stats and import
are staff's. `GET /v1/kinds` spells out each kind's effective `locked`.

## Downloads (`kinds.Kind.Download`, `Deps.Downloads`)

`GET /v1/contents/{id}/versions/{vid}/download` serves what the version's kind
serves to viewers who open the whole version (every segment released;
editors and staff always), as the host's policy allows, each granted
download charged once to `media.downloads`:

```yaml
content:
  content_kinds:
    video:
      download: {serve: original}   # default of players; or an MP4 preset ("mp4-1080"); {off: true} for none
    gallery: {}                     # readers serve pages: the media kind's Publish.Download zip
media:
  downloads:
    anonymous: [{limit: 5, per: 1h}]   # by address
    user: [{limit: 20, per: 1h}]
    tiers: [{tier: premium, quotas: [{limit: 50, per: 1h}]}]   # the first tier held replaces user
    ttl: 5m                            # a download URL's life
```

- **Policy** is the host's `Deps.Downloads` (`versions.DownloadPolicy`):
  `Decide(ctx, DownloadRequest) (DownloadDecision, error)` runs after the
  access check, before the quota, for files and zips. The request carries
  the actor, the tiers it holds, `Editor`, the version, its kind, its
  `Language` and `Title` (in that language when it has one there), its
  entities (`{kind, slug, name, attributes}`, its own and its content's, named in its
  language), its effective `access.Rule`, `Released` (when it went live) and
  `Pages` (0 for a file). The decision allows it, or refuses with the host's
  `Reason` and `AvailableAt` (403 `download_unavailable`, nothing charged),
  names pages (from 1) to `Mark` (files ignore marks) and may `Name` the
  file (no extension; ContentKit cleans and bounds it and adds the
  extension). `Mark(ctx, actor, version, page, bytes)` marks them. Nil
  allows everything and marks nothing.
- The route answers `200` JSON `FileDownload{url, name, type, size,
  expires_at}`; navigate to `url` (contentkit-ui's `useVersionDownload` and
  `VersionDownloadButton` do). Public and private files are the media host's,
  a private one with an exact-file token, so bytes never pass the app;
  without the policy's `Name`, files are named `{title} [{version code}].{ext}`,
  zips by the zip preset's `Download` template. Zips and retained originals are
  `archive?for=&mark=&name=&ticket=`, relative to the download route:
  `GET .../versions/{vid}/archive` streams them, marking the ticket's pages.
- `202 Pending` while the file is placed or produced, `403` locked or
  `download_unavailable`, `404` no download for the kind, `409 no_download`
  when the version has nothing to serve, `429 rate_limited` with
  `retry_after`.

## Interactions (`content`)

The content module owns comments, reactions, favorites and polls in
ContentKit's `content_*` interaction tables. They target ContentKit's own
content items (optionally one version, `?version=`) and entities, stored under
the target's kind from `kinds.Config` and its id: `(tenant_id, content_kind,
content_id, content_version_id)`. Routes are
`/v1/contents/{id}/comments|like|dislike|reaction|favorite|can-comment`,
`/v1/entities/{id}/like|dislike|reaction` (entities take reactions only), then `/comments/{cid}/...`,
`/comments/latest`, `/comments/admin?content_kind=&author=`, `/polls...`,
`/moderation/...`, `/comment-bans...` and `/global-comment-bans...`. A merged
entity answers as its survivor; a deleted one, or content the request's site
excludes, is `404`.

Comments carry a `language` (one of the site's, or none): `?language=`
narrows a list or the latest feed to one thread, and a reply joins its
comment's. A content item's `comments` setting (`PATCH /v1/contents/{id}`,
its editors) is `open`, `closed` (existing comments show; new ones and edits
answer `403 comments_closed`) or `hidden` (none show). Comments on content
a viewer can't open (or a version-level comment's version) and doesn't edit
follow its kind's locked view (Locked content below): unless it shows
`comments`, lists, replies and the latest feed withhold them, the standing
(`GET .../can-comment`) says `locked`, and posting, replying, editing and
reacting answer `403 comments_locked`; moderation queues keep them. The reader's favorites,
as cards, are `GET /v1/me/favorites`; reaction and favorite counts reach `content_facets` (`likes`, `dislikes`,
`favorites`, `rating`) through the catalog's counts job.

Ports (in `content` unless qualified):

| Port | Required | Contract |
|---|---|---|
| `Auth` | yes | `auth.Authenticator` (Auth above); the engine fills it from `Deps.Auth` |
| `UserInfo` | no | `userinfo.Lookup`: authors' names (Authors below); the engine fills it from `Deps.Users` |
| `Media` | no | poll images and avatars in ContentKit media (see below); absent = image routes answer 501 |
| `ContentProcessor` | no | plain-text processor for comment bodies (default only trims whitespace; render as text) |
| `ContentModerator` | no | `Screen(ModerationInput) → Verdict{Decision, Reason, Model, PromptVersion, Confidence}` before a comment publishes; absent = publish (see Moderation) |

**Authors.** Comments, replies, the latest-comments feed, held items and
bans show each author as `{id, name, username, avatar, avatar_srcset}`:
`name` and `username` from `UserInfo.Get` (never the email), with one `Get`
per response, never per author, and the avatar at its stable URL, built
from the id (`Config.Avatars`, Avatars below). An id the directory does not
hold (a deleted account) has no author; a directory failure is logged and
leaves bare ids, never failing the response. Without `UserInfo` authors are
bare ids.

**Publishing waits for media.** Publishing or scheduling an item, or
publishing a version of a published item, answers `409 not_ready` while a
version that would go live still has pages or video producing, a failed file,
or a publication not yet current for its files; `error` says which. Scheduled
items go live at their time without another check, so they need ready media
when scheduled. Uploads a client has not committed are not visible to the
server: editors keep Publish disabled until their uploads commit. The SDK's
`contents.publishWhenReady` waits out the seconds between files finishing and
their publication going current; headless publishing already waits.

**Content and poll images.** Every file of a content lives in a version,
`{tenant}/{kind}/{id}/{version}/`: there is no content-level item, and
versions share no files. A version's cover is one of its uploads with a
public preset (shown while locked). Its body images are its
`Body` uploads (`Named`, with a private image preset and no public one, of a
kind that publishes nothing): private files behind the version's item token,
so a locked version's body images reach nobody else, and its openers, free or
paid, are signed them as they are pages. A body image taken by `POST
/contents/{id}/images` belongs only to its selected version. New versions begin
empty and receive explicit uploads; no sibling text or images are copied. Poll images
live in `{tenant}/poll/{poll_id}/`. Each is a `Named` upload (the server
names it `i-{uuid}`).

**One default-version rule.** A language opens the first live version by
position serving it, else the first live version (`position` is creation
order until staff reorder; contentkit-ui `defaultVersion`, Go
`catalog.DefaultVersion`). Cards show that version and its cover and link to
the language page; a card a filter narrows to another version links with
`?v=`. Detail carries every version's `cover`; `Content.cover` is the default
edition's (meta tags). Entity covers and sitemap `image`s follow the same rule
per language.

Nothing stores an image URL: a public file's name carries a generation that
changes on every republish. Covers and poll images store the name; a body
stores a reference, `contentkit:i-{uuid}` (the SDK's `imageRef`), which
`Deps.BodyProcessor` must keep. Reads resolve a body's references to signed
URLs of the opened version's private images, a cover to its current public
file; one not rendered yet stays as it is.

| Route | Body | Result |
|---|---|---|
| `POST /v1/contents/{id}/images` | `{"image": "i-…", "version": "…"}` | `{"ref", "url"}`: the reference for the body; a URL that shows it now (null until rendered) |
| `PATCH /v1/contents/{id}/versions/{vid}` | `{"cover": "i-…"}` (`""` returns to its first) | the version, `cover.url` null until it has a public file |
| `PUT /v1/polls/{id}/image` | same | `{"image_url"}` (same) |
| `PUT /v1/polls/{id}/options/{oid}/image` | same | `{"image_url"}` (same) |

Public files exist only while the item is live; its editors see an
unpublished item's images through an editor read (the SDK's `useContent`).

There is no `Recorder` and no `Moderation` port: reactions and favorites feed
the signal plane through ContentKit's own preference outbox, and the
`ContentModerator` decides comment publication.

The tenant is pinned at construction and stamped on every row; a `ContentRef`
of another tenant passed to any read is `content.ErrTenant`, never remapped.
The host imports existing data with explicit tenant and content references
after initializing fresh stores ([docs/migration.md](docs/migration.md)).

## Moderation (comments)

Every comment create/edit is screened through `Options.Moderator` after
sanitizing; the moderator sees the tenant, the opaque actor, the commented
content's reference, the kind, the comment id on an edit, and the text.
Content items are not moderated: their editors are authorized staff and
owners. The verdict sets the comment's `moderation` state:

- `approve` publishes (`201`/`200`).
- `reject` stores nothing and answers `422 {"error": reason}`
  (`content.RejectedError` in Go).
- `review` stores the item `held` (`202` on create): it is not counted, not
  indexed, cannot be replied to or reacted to, and is invisible to every reader
  except its author, who sees it with `moderation: "held"` and
  `moderation_reason`. An edit re-screens: a published one is withdrawn on
  review (`202`). Its author can't edit a held or rejected comment
  (`403 comment_moderated`), so only a reviewer's approval or a release
  publishes it; a moderator's edit of a held one publishes on approval.
- A moderator error or an unknown decision **fails closed to `review`** with
  the reason "awaiting review"; the error is kept for the reviewer. Nothing is
  ever published unscreened and no submission is lost.
- No moderator: everything publishes.

Review queue: `GET /v1/moderation/held?kind=comment&cursor=&limit=` pages
held comments oldest-first (each carries the body, the content
reference, the reason, model, prompt version, confidence, any moderator error
and `revision`); `POST /v1/moderation/{kind}/{id}/resolve
{"revision","decision","reason"}` (the actor id is the reviewer) writes the
final state: `approve` publishes (counts and the keyword index follow),
`reject` keeps the comment author-visible as `rejected` with the reason. Both
need `root:moderation:review`.
`GET /comments/admin` shows every state with real bodies and authors, by content kind or `author`, including `author_hidden` for accounts hidden during a ban. The review queue also keeps hidden authors’ held comments.

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
every comment body and anonymous name before the moderator; an IP-banned write
answers `403 comment_banned` with ban scope `ip`, and moderators are exempt.
Imported comments get the mask words too, but a reject word never refuses
imported history.

Doujins adapter (`moderator{svc}` on the old `Moderation` port): implement
`Screen` instead of `Check`; return `Verdict{Decision: DecisionReject, Reason}`
where it returned `RejectModeration(reason)`, `DecisionApprove` where it
returned nil; grant `root:moderation:review` and point the admin review page at
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
(multiple-choice polls, keyed by IP and counted separately from signed-in
votes, so one person can vote once each way and a changing address votes
again). Favorites always need a signed-in actor. A refused anonymous attempt is `401 unauthorized`.
`GET /config` answers the setting (`{"anonymous": {"comments", "reactions",
"votes"}, "comment_max_length"}`) and `GET /{kind}/{id}/can-comment` carries
`anonymous`, so a client shows a sign-in prompt or an anonymous form from the
server's answer. `content.Options.CommentMaxLength` (default 2,200 characters)
bounds comments and edits: longer is `422 comment_too_long` with
`details.max`; the standing carries it as `max_length`. An anonymous name is trimmed
and limited to 60 Unicode characters (`400 invalid_request` beyond that).
Banned-word rules mask the stored name or reject it with `422 moderation_rejected`.

## Interaction limits

ContentKit limits each actor's interactions itself: the user id, or the IP
for an anonymous actor (an actor with neither is not limited here). Hosts
keep no limiter around these routes; the edge (Traefik) keeps the generic
per-IP ceiling. Every attempt spends one, so undoing (unfavorite,
`DELETE .../reaction`) costs the same as doing. Each rate is "at most `Count`
in any `Per`"; an action with two must pass both.

| Action | Routes | Default |
|---|---|---|
| `comment` | `POST /{contents\|channels}/{id}/comments` (top-level and replies), `PATCH /comments/{cid}` | 5 per 5 minutes and 20 per hour |
| `comment_reaction` | `POST /comments/{cid}/like\|dislike`, `DELETE /comments/{cid}/reaction` | 30 per minute |
| `reaction` | `POST /{contents\|entities}/{id}/like\|dislike`, `DELETE …/reaction` | 30 per minute |
| `favorite` | `POST` and `DELETE /{contents\|channels}/{id}/favorite` | 20 per minute |
| `poll_vote` | `POST /polls/{id}/vote` | 30 per minute |

Override any of them in `content.Options.Limits` (`Comment`, `CommentReaction`,
`Reaction`, `Favorite`, `PollVote`, each `[]content.Rate{{Count, Per}}`).
`Limits.Redis` (a Redis or Garnet client) shares the counts across replicas
under `Limits.KeyPrefix` (default `contentkit:content:rl:`) plus the tenant;
without it each process counts alone (logged at start). A Redis error falls
back to the per-process count for a second and adds to the expvar
`contentkit_content_ratelimit_redis_errors`. A refusal is `429 rate_limited`
with `Retry-After` and `{"action", "retry_after"}` in the body
(`*content.RateLimitError`, `errors.Is(err, content.ErrRateLimited)`).

`content.Options.DuplicateWindow` defaults to zero (off). When enabled, a
signed-in author cannot save the same processed body on the same target,
reply parent (or top level), and language within that duration. Non-deleted
comments in every moderation/visibility state participate. Refusals return
`422 duplicate_comment`; anonymous comments are exempt. The check runs inside
the insert transaction after parent validation, under the account's existing
transaction advisory lock, so concurrent creates cannot both pass and failed
saves leave no duplicate record.

Thread lists, replies, latest comments and the admin list reject offsets above
10,000 with `400 invalid_request` and `offset is at most 10000`. Other lists
keep their existing pagination behavior.

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
commenters have no identity to ban. `AnonymizeAccount` removes the bans of a
deleted user and of their owner scope, and blanks them as `banned_by`.

For account bans, call `Engine.SetAccountHidden(ctx, accountID, true)` and
pass `false` when the ban ends. This is separate from scoped comment bans and
account deletion. It hides that author's comments from other readers, latest
comments, published counts and account comment statistics. Authors can still
see their own hidden comments in threads/replies and their standing count,
just as they see their held comments; the latest feed remains public-only.
Staff lists keep them. New comments by a hidden author are stored hidden.
The account's likes and dislikes on comments, content and entities also stop
counting and notifying until unhidden; favorites and poll votes are unaffected.
Reactions remain stored, and those hidden at permanent anonymization stay uncounted.

Only approved, undeleted comments with `author_hidden=false` contribute to
`comment_count` or `reply_count` or publish notices. Hiding withdraws reply
and like notices; unhiding restores eligible notices with their existing
identities. Holds, rejections and deletions survive hide/unhide. Approving,
editing or restoring a hidden comment cannot publish it until unhidden.
Repeated hide/unhide calls do not change counts again. A like notice uses
`comment.liked:<comment id>:<liker id>`: unlike withdraws it and re-like
restores the same notice, without adding another.

## Polls

Polls accept predefined choices only. `kind` defaults to `multiple_choice`;
other kinds are rejected with `400 invalid_request`.
Create a poll with two to ten options in one of the site's languages (a
question of at most 300 characters, options of at most 200), then vote through
`POST /polls/{id}/vote`. A vote is final: concurrent retries never add another
vote. A signed-in vote stores no IP address. Signed-out voting follows
`Config.anonymous.votes`.

`closes_at` (optional, `PATCH`-able, `null` clears it) and `is_active = false`
close a poll for voting; its results remain readable, on its own link too.
`closes_at` must come after `live_at`. Scheduled questions are
visible only to staff until their opening time.

Free-text submission, automatic grouping and classifier dependencies are
removed. The database stores only choice polls; questions have no kind column
or answer table.

## Account lifecycle and interaction erasure

Applications consume AuthKit's retained lifecycle events. On `user.deleted`,
call `ck.SetAccountDeleted(ctx, id, true)`; on restoration pass `false`. A host
of a remote ContentKit calls the routes instead, as a service holding
`access.AccountsLifecycle` (`root:accounts:lifecycle`):
`PUT /v1/accounts/{id}/deleted` with `{"deleted": true|false}`
(`Client.PutAccountDeleted`) and `POST /v1/accounts/{id}/anonymize`
(`Client.PostAccountAnonymize`), both 204 and idempotent.
This changes author display to `<Deleted>` without deleting interactions.
Comment responses use the existing `anon_name` shape without a profile or
account id, so older UI clients display the same label. Ownership remains in
storage until purge and is restored with the account. A deleted account can
also be hidden; clearing either state does not clear the other. Permanent
anonymization preserves comment visibility and counts, including hidden
comments, while removing the hidden-author account marker.
Check current account state before replaying an old deletion/restoration event.

Pass no `OnPurge` callback: AuthKit purges first and records `user.purged`.
Each host's event reader calls `ck.AnonymizeAccount(ctx, id)` and advances its
cursor only after it succeeds. Comments and anonymous reaction/favorite/vote
contributions remain, with their existing counts. Their fresh random actor
key preserves one-person-per-work ranking across versions and interaction
types. Reading statistics use that anonymous key too; the temporary account-to-key mapping is discarded on
completion. Inbox, avatar, recent-search and provider cleanup remains part of
the operation. A failure is retryable. No periodic erasure sweep is required:
the event reader keeps the obligation pending until completion. Normal startup
allows unfinished purges, including ones the reader has not reached, and never
waits on the avatar primary's receipt for them. The shared avatar URL can remain
reachable until its primary finishes. The `contentkit_account_anonymization`
probe reports purge obligations older than 15 minutes without this app's receipt;
it does not affect `New`.

For a purge, `New` returns `ErrErasureReplayRequired` only when this app's receipt
exists but neither its local completion fence nor lifecycle state `purged`
remains, proving a catalog restore. An app intent without a local fence or
`purging` row also blocks startup. Restore replay repeats `AnonymizeAccount`,
preserving anonymous contributions and removing restored comment, creator and
owner attribution.
Retry replay until its report is complete; a cleanup failure returns an error
and leaves work remaining.

For replicated ClickHouse, configure `ClickHouse.Cluster` with the complete
single-shard replica set, and route its connection pool only within that set.
Cleanup verifies Keeper membership and waits for cluster-wide synchronization
before copying. Missing, unhealthy or incomplete replica configuration keeps
cleanup pending before moving signal data. Counts retain their totals while
the signed-in/anonymous reader split changes. Privileged content/entity imports
and moderator provenance remain host responsibilities; this is not a general
backup-restore or privileged-write policy.

The signal plane follows the [signal-plane contract](#signal-plane-account-deletion).
A deliberately disabled signal plane is absent, not unfinished. Any
configured-plane failure returns an error: keep the deletion pending and retry.

Content anonymization commits atomically:

- a permanent tenant/subject fence in `content_erased_subjects`; later writes by
  that subject fail with `content.ErrSubjectErased`;
- the subject's reactions, favorites and poll votes move to the anonymous key
  with counts unchanged; comments (shown as `<Deleted>`), authored items,
  owned entities and reposts lose the account id;
- removal of the subject's comment notifications, creator profile, content
  filter preferences and comment bans of them or in their owner scope (bans
  they set stay, without their id).

Other tenants and subjects and externally stored media are untouched.
Every guarded writer takes the subject fence before other source locks.
Guarded mutations and anonymization use explicit READ COMMITTED transactions,
so a writer waiting on the subject lock sees the committed fence even under a
REPEATABLE READ default.

### Provider data

Moderators that retain personal data must configure
`content.Options.ProviderDataEraser`. Its `EraseSubjects(ctx, tenant, actorIDs)`
runs after the SQL commit and must durably fence those subjects and remove
retained data, including against calls already in flight; a queued delete is
not completion. Multiple retaining providers need a composite eraser; provider
implementations stay outside ContentKit. Ports that retain nothing implement
`StatelessPolicy()`. Construction rejects a retaining port without an eraser; nil is never an
outage fallback.

The library tests qualify the port contract and source fences against real
PostgreSQL with in-memory retaining providers (paused completions, outages), not
a provider's production persistence. Each retaining adapter must prove durable
deletion/fencing across its own restarts and backups before the host marks
erasure complete. Soft-deleting a poll is not a provider erasure.

### Recovery

Fences and approved payload snapshots are durable user state. A restore must
restore/reapply fences and run `ReplayErasureFences` (`tools
replay-content-erasures`), which repeats `AnonymizeAccount` for every recorded
deletion, before traffic resumes; provider erasure keeps the same
permanent-fence semantics across its own restores. Restored reactions and
favorites of fenced subjects never export.

## Search documents

ContentKit builds its documents: one per content item and language from the
catalog (title, aliases, its entities' names as keywords) and one per entity
and language. Writes mark them dirty in
their own transaction; discovery's search applies the catalog's visibility
rule (`catalog.Viewer`) inside every retrieval route, before any limit.

## Discovery (`/v1`, route group `discovery`)

Hosts list, search and recommend only through these routes
([docs/api/routes.md](docs/api/routes.md)); ContentKit applies the catalog's
visibility rule (access, site scope, content filters, listing and language)
over `content_facets`, and answers one card per content item with the version
that fits the reader (`display_language` first, then position).

- `GET /contents`: `kind`, `groups` (all of), `owner_group`,
  `creators` (credited user ids), `access` (`public`: what anyone opens now;
  `tier`, `group`, `ppv`, `group_ppv`: what stays locked at that level),
  `tiers` (locked behind one of them: a subscribe page's premium), `traits`,
  `attribute=name:value` (declared filterable attributes), `released_on`
  (`YYYY-MM-DD` or `YYYY-MM`, in `tz`), `view_status`
  (`unseen|in_progress|completed`, signed in), `language` (only content
  serving it), `display_language`, `mode` (`lock` marks locked cards,
  `hide` lists only what the reader opens), `sort` (`newest`, `oldest`,
  `title`, `popular[:period]`, `top_rated`, `most_favorited`,
  `position` in the first of `groups`, which must be of an ordered entity
  kind, `random`), `cursor`, `limit`. A reader's content filters never hide
  an entity's content from a list requiring that entity: a hidden tag's page
  shows what its count counts, while other lists leave it out. A card's
  `access` has the reader's decision (`allowed`, `sell`) and the version's
  effective rule for everyone (`level`, `tiers`; `public` once free), and
  its `version.has_audio` says the version's media has audio.
- `GET /search` (`q`, the same filters; `relevance` personalized for a
  signed-in reader, or any list sort of every match), `GET
  /typeahead` (direct title/entity-name/alias prefixes only; no contextual
  keywords or typo expansion). `SearchBox` puts matching recent queries first,
  followed by entities and content titles; full search remains broader.
- `GET /contents/{id}/similar`, `GET /recommendations`: `Deps.Candidates`
  (co-engagement by default), admitted through the reader's visibility.
  Recommendations fill from the last 30 days' popularity, then all-time,
  then the newest content the reader has not seen.
- `GET /release-dates` (`by=day|month`, the list filters): counts and free
  counts per date.
- `GET /releases` (`days=10`, `time_zone=America/Denver`, the reader/content
  filters and `card_entities`): a complete calendar-day window of card
  metadata, newest first. `DailyReleases` reports `from_date`, `through_date`,
  `time_zone` and every `ReleaseDay`, including empty days. It accepts 1–31
  days, defaults to 10 and UTC, and has no item count cap or cursor. Reveal
  cards from that response in the browser; mount thumbnails only as needed.
  Future, unlisted and deleted publications are excluded.
- Popular ordering uses the shared ContentKit quality score, not personal
  affinity.
- `GET /content-filters` (public): the configured `content_filters`, their
  entrypoint defaults and the current reader's effective settings.
  `PATCH /me/content-filters` atomically merges submitted shared entity choices,
  preserving omitted values. One entity choice
  applies across the configured `content_filters.applies_to` content kinds.
  `POST /me/content-filters/initialize` adopts a new account's guest choices
  only when no saved settings exist; its `account_id` must match the signed-in
  account. Trigger initialization on registration, never ordinary sign-in.
  Guest browsers persist choices locally and send
  `ContentKit-Reader-Preferences: {"filters":[{"entity_id":"configured-entity-id","visible":false}]}`.
  The header is ignored for authenticated accounts, grants no access, and
  accepts at most 8 KiB and 128 configured filters. Duplicate IDs and malformed values return `400 invalid_request`; unknown or
  fixed guest IDs are ignored and cannot override policy. Allow the header in the
  host's CORS policy, and prune stale browser choices against a public
  `GET /content-filters` without the header before attaching them to reads.
  Account language remains the host's existing profile preference.
- `GET|DELETE /me/history`, `POST /signals` (explicit clicks; signed out,
  `session` names the browser), `POST /exposures`.
- `GET /me/favorites`: what the reader favorited, contents as cards or
  channels, most recent first.
- `GET /sitemap/contents?kind=`, `GET /sitemap/entities?kind=` (open): the
  request site's live, listed pages by id (`cursor`, up to 10000 a page),
  each with `lastmod` and its path per language it serves. Content filters
  and access don't apply.
- Staff (`root:content:view-restricted`): `GET /stats/activity` (views,
  viewers and signed-in viewers per day) and `GET /stats/top` (`metric`
  views or comments, `period`, `access`, `tiers`). `GET /me/stats`: the
  reader's finished contents per kind, comments and the reactions they got.
- `GET|DELETE /me/searches` (signed out, `session`): the reader's recent
  searches, one per query, newest first, which `GET /search` keeps (its
  `session` names a signed-out browser). A clear deletes them (`?query=`
  only that one); erasure deletes them too, and an hourly job deletes those
  unused for 180 days and any recorded while an erasure ran.

A card carries title and path in the display language, cover renditions, the
version (languages, traits, pages, duration), the version's entities with pages
by kind, likes, dislikes, favorites and all-time viewers, the access decision
(`allowed`, `sell`, `requires`, `preview`, `releases`), the signed-in
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

A write that changes documents queues the engine's search job in its
transaction; the job also runs at start and every 5 minutes as a backstop. A
run expands renamed entities to their items in batches, drains the dirty queue
and runs a bounded backfill; while work remains, the next run follows at once.
It holds no connection or transaction across builders; each write is a short
transaction fenced by the dirty revision, so overlapping runs only repeat work
and a stale build never overwrites a newer one.

## Media

Media is a self-describing file system in one private bucket; no database
table records what media exists. The app declares everything in one registry
at startup (`media.Config`); ContentKit hard-codes no kinds, names or
layouts.

- **Items.** App media uses `tenant/{tenant}/app/{app}/{kind}/{id}/`.
  For a content kind, `id` is the version ID; a content has no item of its
  own. Account media uses `tenant/{tenant}/account/{kind}/{id}/`.
  Both roots derive from `Config.Scope`. An item holds
  `manifest.json` (gzip JSON, never served), `private/` (hash-named blobs:
  uploads, derived files, editor views), `public/` (published renditions
  such as `cover-460-{generation}.webp`) and `temp/` (in-flight server-side
  writes).
- **Content versions.** `media/versions` answers media's hooks from the
  content model: editors (staff, creators, primary-channel editors) upload, a
  person their own avatar; reads follow the catalog's
  visibility, plus segments' release and the rule's preview;
  a version's media writes its pages, duration, size and
  audio onto `content_versions` (`Facets` refreshes listings). A `Publish`
  kind's reader (its page segments, a locked version's previews (its rule's,
  else its kind's `preview`) and its playback) is planned by
  `GET /v1/contents/{id}/versions/{vid}/reader`. Page-archive
  imports, reader metadata and downloads (a host watermark, quota buckets)
  complete it. A reader version with no cover of its own gets its leading
  released page as its cover (an upload marked `auto_cover`); a cover an
  editor uploads replaces it for good.
- **Manifest.** An ordered virtual file system over the item's private
  blobs: each Upload's files in natural order unless an op reorders them,
  then each private preset's outputs in their uploads' order. A derived file
  records `from`, `preset` and `fp` (its inputs' fingerprint); it is stale
  exactly when the fingerprint no longer matches. Each upload records its
  public publications: the current generation of each preset and, while
  the worker writes one, its successor.
- **URLs.** `https://media.<site>/v1/tenant/{tenant}/app/{app}/{kind}/{id}/{public|private|download}/{name}`.
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

cfg.Media = &contentkit.MediaConfig{Config: media.Config{BaseURL: "https://media.doujins.ai",
	Kinds:   []media.Kind{Gallery, avatar.User()},
	Avatars: media.Avatars{Kind: "user", Preset: "avatar"}}} // people's avatars (Authors)
worker, _ := cfg.MediaRegistry() // the stock worker's MEDIA_KINDS_FILE
```

- **Site images.** A kind no content or entity kind owns (nor `Avatars`, nor
  `poll`) holds images of records the host keeps, keyed by the host's id:
  Doujins' translation requests' covers. In the engine anyone reads its
  public files (`GET /v1/media/{kind}/{id}`) and holders of
  `root:content:write` or `root:import:write` upload them.
- **Account avatars** are the engine's `Config.Avatars` (Avatars below).
  `avatar.User()` (`github.com/open-rails/contentkit/media/avatar`) is the
  standard kind: JPEG, PNG and WebP up to 10 MiB, square WebP at 64, 128,
  256 and 512 pixels, a default silhouette (publish it with the other
  defaults: `media-worker publish-defaults`). Its preset is `Stable`:
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
- **Private presets** have one producer: `Image` (with `Shapes`, below),
  `HLS` (byte-range fMP4 ladder, audio and subtitle tracks, seek sprite; each
  track's segment table is its own private blob), `MP4` (one rung, H.264),
  `Zip` (the files under a prefix, in manifest order), `Audio` (an HLS track
  and an M4A) or `Subtitles` (clean WebVTT; a video lists them after its own
  tracks). Nothing is upscaled.
  Image presets default to WebP. A private preset with
  `Image.Format: media.ImageFormatSource` must end its `To` with `{ext}`
  (the upload's original extension), such as `download-pages/{name}{ext}`.
  With an inside fit and no edit/blur, a source within its width/height box
  passes through byte-for-byte. `Width: 6000, Height: 6000` bounds either
  edge proportionally; zero leaves an edge unbounded. Oversized or edited
  JPEG, PNG, WebP and GIF files retain their codec, including animation;
  other accepted codecs support unchanged passthrough only. Keep archival
  uploads with `KeepOriginals: true`; a ZIP preset over `download-pages/`
  packs the resulting original-format pages in reading order. Upgrade the
  worker/gateway cohort before declaring this registry, then regenerate
  existing items to replace an old download rendition.
  `Shapes` give an image preset another `Image` for uploads of another
  aspect (width:height), wider than `WiderThan` and no wider than
  `NoWiderThan` (unset is open). They are data, so the stock worker runs
  them; they never overlap and keep the preset's format and animation; an
  upload in none renders `Image`. Tall pages keep more height, wide pages
  more width:

  ```json
  {"name": "low", "from": "originals/{name}", "to": "low-res/{name}.webp",
   "image": {"width": 1206, "height": 2096, "quality": 90},
   "shapes": [
     {"no_wider_than": "1:2", "image": {"width": 1206, "height": 5065, "quality": 90}},
     {"wider_than": "1:1", "image": {"width": 2412, "height": 2096, "quality": 90}}]}
  ```
- **Public presets** render an upload through its edit at fixed names,
  `{w}` for each width (a width past the edited image renders at its width,
  so every name exists). The object carries its `from` and `fp` as metadata.
  The first public preset of an upload bounds its edit: the crop is fitted to
  `Image.Aspect`, and an edit narrower than `MinWidth` fails.
- **Previews.** There is no partial access to `private/`. A `Publish`
  kind shows part of a locked version through its access rule's (or kind's
  `locked`) `preview`: release copies the leading released pages' renditions
  to `public/`, and the reader plan lists their URLs.
- **Originals.** `KeepOriginals` keeps uploads once their private outputs
  exist (otherwise the blob is dropped and the file marked `gone`; public
  presets' sources are always kept). Except a version's declared audio
  (`Publish.Playback`), the read API never returns an upload's path or URL
  to viewers.
- **Host-only presets.** `Private.HostOnly` keeps an output out of generic
  reads, downloads and playlists.
- **`private/` is all or nothing.** One token per item opens every private
  file of it. `HostOnly` decides what reads list, not what the token
  reaches: a viewer with access who learns a blob's hash can fetch it.
- **Defaults.** A kind's `Defaults` (an `fs.FS`, e.g. `go:embed`) holds the
  images its public presets name in `Public.Default`; the registry checks
  they exist. A shared kind ships its own, so importing apps merge nothing.
- **Shared kinds.** Account avatars use `tenant/{tenant}/account/{kind}/{id}/`.
  One app owns the shared avatar journal and worker; other sites reach it
  through its API (Avatars below) and serve its files from their media hosts
  (`MEDIA_GATEWAY_BUCKETS`). The root is derived from `Config.Scope`.
- The stock worker reads the registry as JSON (`MEDIA_KINDS_FILE`; hooks
  are not included): an engine's is `Config.MediaKinds()`, printed
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
- `AnonymizeAccount` erases avatars on the primary only; people are shared,
  so the primary's deletion (the account deletion every site shares) removes
  them.

### Uploads and commit ops

The upload route group serves the upload API at `/v1/media/upload`:

| Route | Does |
| --- | --- |
| `POST /presign {ref, path, type, size, sha256}` | Checks the path's Upload and `CanUpload`; answers `exists`, one `put`, or a `multipart` ticket, and the `path` to commit (cleaned, with an extension, server-named for `Named`). |
| `POST /parts`, `/parts/list`, `/complete`, `/abort` | Multipart parts bound to their SHA-256; resume; assemble. |
| `POST /commit {ref, ops}` | Applies ops in one conditional write, then enqueues placement and processing. Each uploader's commits are rate limited (`UploadOptions.Commits`, default 1/s, burst 30; 429 `rate_limited`), except for exempt grants. |

Uploads land at a staged name, `temp/u-{uuid}`, which presign answers as
`blob` (or the folder's identical blob, with `exists`); the put op commits
that name. The media worker then hashes each staged upload and places it at
`private/sha256-{hex}` of its bytes before processing (an upload gone or
changed by then fails with `not_uploaded`), so no client ever writes under a
blob name. The upload routes admit a signed-in person or application;
`CanUpload` decides. Importers upload with `Client.IngestMedia` (presign,
PUT or parts, commit; [docs/import.md](docs/import.md)); a file whose `From`
names a key of the item's bucket outside ContentKit's namespaces is copied
server-side instead, so no byte passes through the importer.

| Op | Does |
| --- | --- |
| `put {path, blob, index?, meta?, edit?, unattached?, create_id?}` | Adds an upload at its natural position (or `index` among its Upload's), or replaces the one with the same stem. A nonzero UUID `create_id` instead requires an unused stem; repeating that ID is a no-op retry, even after placement or attachment. |
| `edit {path, edit}` | Crops and rotates an image upload; nil clears. |
| `move {path, index}` | Reorders an upload; its outputs follow. |
| `rename {path, to}` | Renames an upload within its Upload, and its outputs. |
| `remove {path, takedown?}` | Removes an upload and its outputs (cancelling its processing). With `takedown` it also removes the frames grabbed from the upload and the zips that bundled it, then deletes at once the blobs the commit dropped, the removed uploads' editor views and staged objects, and the item's unused public files. With an exempt grant (`UploadGrant.Exempt`, staff) it instead deletes every private blob the item no longer references, earlier versions and all editor views included, which costs re-rendering them; sent again on the path already gone, it runs that sweep again. If a takedown's deletes fail it says so: staff complete it with the exempt takedown, otherwise the leftovers go at the grace sweep. For anyone else a takedown of a path already gone is `not_found`. |
| `attach {path, index?, meta?}` | Makes an unattached upload part of the item. |
| `copy {from: {id, path}, to?, edit?}` | Copies an upload within the kind, including the current item. Copies within an Upload preserve current private outputs; another Upload or a supplied edit renders its own presets from the original. Blobs copied from another item are reserved like uploads (rate limits, pending quota) before they move. |
| `frame {path, t \| auto, edit?, from?}` | Fills an upload from a frame of its `Frames` video, the one a player shows `t` seconds in; a new video grabs again. With `from: {kind?, id, version?, path}` the frame is of another item's video upload (`kind` defaults to this item's, in its namespace), which the editor must also be able to write; the upload must allow `image/png`, and the worker grabs it once, a still that later changes to that video leave alone. |
| `meta {meta}` | Sets the template values (`{title}` in download names). |
| `regenerate {preset?, force?}` | Asks the worker to redo stale outputs and retry uploads that failed for their current blob and edit, which any writer of the item may do; `force` redoes every output and needs an exempt grant. |

`CanUpload` is asked for `UploadTarget{Ref, Path}` per op (the upload's stem;
"" for item-wide ops), and for a copy's or frame's source too. A new item starts hidden
unless anonymous viewers may see it (the engine's access rules). `ProcessOnUpload`
tells the SDK to commit uploads unattached as they land; readers leave them
out until `attach`.

### Reads and playback

The media route group serves the read API at `/v1/media`
(`Config.Media.ReadLimit`):

- `GET /{kind}/{id}?prefix=low-res/&offset=&limit=&download&editor` answers
  `{access, expires, meta, public, locked, total, hls, files: [{path, type, size, w, h, url}]}`
  in manifest order. `access` is `full` or `none`. With access every listed
  file has a URL: plain under the item cookie
  (`Path=/v1/{ns}/{kind}/{id}/private/`) in cookie mode, with `?t={item
  token}` in URL mode. Without it there is no token, cookie or meta, and the
  only files listed are public ones: a locked version's free preview pages,
  delivered publicly at release. Nothing else tells what a locked item holds,
  only `locked: true` that it holds something. `public` is the item's public
  files (covers), for every viewer who can see it. `expires`
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

- `Images(ctx, media.ImageQuery{Ref, Preset, Name}...)` answers many queries in
  one read: an item's images, one preset's, or one upload's by its `{name}`
  (an inline image's `i-{uuid}`). Store names, not URLs, and resolve them
  when rendering; post bodies store references (Interactions).
  `image.URL(width)` picks a rendition, `image.SrcSet()` gives `srcset`.
- `Registry.DefaultImage` is a preset's default.

A republish keeps the current files published until their successor is
ready, and cleanup retires a generation only after the projection names the
next one. Browsers keep using a read's `public` or `usePublicImage`.

### Public storage classes

- Declared public presets remain public before release and after unpublishing.
  Publication and billing changes only change database access policy and private
  token issuance. There is no exposure method or worker.
- Render the defaults from the deploy step with the worker image:
  `media-worker publish-defaults` (`MEDIA_KINDS_FILE`, `MEDIA_S3_*`, no
  database) writes each `Public.Default` to
  `{ns}/{kind}/_default/public/sha256-{digest}.webp` and prints the
  gateway's `MEDIA_GATEWAY_DEFAULTS`. The images come from
  `MEDIA_DEFAULTS_DIR`, over ContentKit's own (the avatar silhouette).

### Processing and readiness

- A commit marks an upload `pending` with the presets it feeds; the worker
  clears each as it records the outputs. The worker also finds stale outputs
  itself (a changed spec or recipe after a deploy) and visits every item of
  the kind; the `regenerate` op does one item. A `meta` change rewrites download names only.
- An item is ready when no attached upload is pending (including declared
  public presets) and every zip packs its inputs; failed once
  nothing is pending and an upload failed for its current blob and edit.
  Processor completion appends a retained processing fact and queues real
  version metadata work in one transaction; hosts follow it through the event
  log, never inside the transaction.
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

### File retirement, deletion and erasure

- Manifest edits admit their exact next manifest and ingest job in PostgreSQL
  before the conditional S3 write. Ingest retries that admitted operation.
  Removed keys remain durable intents for the existing content purge. A file shared by retained entries stays live;
  reordering entries does not delete or rename it.
- The existing content-deletion sweep calls `jobs.PurgeRetired(ctx, limit)`
  for database-recorded allocation and deletion deadlines. It does not search the bucket for unreferenced files. Unfinished
  private allocations retain `JobsConfig.Grace` (24 h by default); temporary
  uploads and caches expire after seven days and must then be uploaded again.
  Known retired keys receive a bounded retry to catch already-issued writes.
  Unknown objects require an explicit integrity investigation.
- Configure the S3 adapter's lifecycle policy and allow `x-amz-tagging` in
  upload CORS. Worker credentials also need `s3:PutObjectTagging` and
  `s3:DeleteObjectTagging`. Canonical temp objects receive an expiry tag; copies into
  permanent storage clear it. `Store.Configure` preserves unrelated lifecycle
  rules, expires temp objects and aborts multipart uploads after seven days.
  Use equivalent server settings where multipart lifecycle is unsupported.
- `jobs.RetireItemsTx(ctx, tx, media.Deletion{Ref, Owner})` records an
  incarnation-bound deletion with the owning tombstone. `RetirementStartedTx`
  prevents restoration after the claim; `RetirementComplete` permits the owner
  to mark completion only after physical deletion succeeds. The content purge
  retains exact late-write targets after the owning row is finalized.
  `Jobs.Erase` is synchronous permanent erasure; `Jobs.Purge` is an explicit
  reset with a stable operation ID. Neither needs a media host queue.
- Restore: `s3.Store.Restore(ctx, prefix, t)` brings back manifests and
  their referenced blobs. Normal processing rebuilds declared public presets.

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
- `Config.Media.WorkerSchema`, which `MigratePostgres` migrates;
- the worker's `MEDIA_WORKER_SCHEMA`.

It is required. Hosts sharing a database must never share one: a worker
drains every job in its schema, so it would take another host's jobs (same
queue and job kinds) and fail them against its own registry and bucket. Queue
names (`media_ingest`, `media_derivatives`) are fixed within a schema.
Every accepted upload passes through `media_ingest`, so a worker must run even
for kinds without presets. Ingest preserves uploaded audio without conversion;
video processing retains its configured soundtracks. Autoscalers (KEDA) can
count available/running/retryable rows in these queues in `{schema}.river_job`.

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
  partitions it serves (`tenant_<tenant-id>_app_<app-id>` and
  `tenant_<tenant-id>` for account media). `MEDIA_GATEWAY_BUCKETS` maps each
  partition to its bucket; the account partition uses the avatar primary's
  bucket. URL paths use the tenant/app or tenant/account roots above.
  `MEDIA_GATEWAY_CORS_ORIGINS` exactly your sites' origins
  (`https://<domain>,https://www.<domain>`; no wildcards, no third parties);
  `MEDIA_GATEWAY_DEFAULTS` the public names that fall back to a kind's
  `_default` item. `Cross-Origin-Resource-Policy` is always `same-site`.
- **Bucket**: one per app, private (no public ACL or policy); the gateway's
  key reads only `*/private/*` and `*/public/*` of each; only the hosts write.
- **CDN**: may cache `public/` in a shared cache. Current publication URLs
  contain a generation; optional stable aliases follow the gateway's
  `public, max-age=300, stale-while-revalidate=86400` policy and ETag
  revalidation. ContentKit does not enqueue CDN invalidation jobs. Never
  cache `private/` in a shared cache: the token is not part of a cache key
  the CDN checks, so a cached object would be served without one. Private
  blobs are hash-named and `private, max-age=31536000, immutable`.
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
    process). It is enforced in `Reader.Grant`, so reads and playlists
    pass it (downloads have their own `media.downloads` quotas); over the limit the read API answers 429
    `rate_limited` with `Retry-After`, and `Grant` returns a `LimitError`
    (`ErrRateLimited`). Opening the same item again within an hour of first
    opening it is free;
    viewers without access, the item's editors and `Issuance.Exempt` actors
    (staff) are not counted. An anonymous viewer is counted by `Actor.IP`
    (an IPv6 address by its /64). The read API fills it from `RemoteAddr`;
    a host route that calls `Grant` or `Read` itself for an anonymous actor
    must set it, or the grant fails with `ErrNoViewerKey` rather than put
    every anonymous viewer in one count. The per-process count forgets some
    viewers once it is full, and they start again.
  - `HandlerOptions.Limit` bounds requests to the read API (at most `Burst`
    per `Burst/PerSecond` per viewer; default 120 per minute).
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
  `contentkit:media:rl:`); replicas then share one sliding window per viewer
  (sorted-set commands in MULTI, no Lua, keys expire a window after their last
  request; replica clocks need NTP).
  A Redis error fails open to the per-process limit for 1 s, logs a warning
  once per outage and increments expvar
  `contentkit_media_ratelimit_redis_errors`: the limit is abuse protection,
  tokens and visibility checks still gate every file.
- **Access**: the engine's access rules gate metadata and private-file reads. Public
  object URLs are always free. Ordinary removal and permanent purge still erase
  obsolete objects.

## Entities

`entities.Store` owns the app's artists, tags, series, characters and
collections; their kinds come from `kinds.Config.EntityKinds`. Shared and
version-level associations are replaced independently. Writes refresh association
facets in the same transaction. Entity counts and automatic covers are derived
on reads, so edits and release times take effect without a recount job.
Channels have a separate `/channels` interface and cannot be read or changed
through ordinary metadata operations. Channel lifecycle changes call AuthKit synchronously and return failures;
descriptive entities need no authorization lifecycle worker.
The entity routes remain `/v1/entities…` and `/v1/contents/{id}/entities`.

Keyword documents enrich content with active associations and their ancestors.
Collection associations remain active until explicitly changed; subscription
and rental expiry belong to the entitlement provider.

Stable descriptive membership fields (`groups`) and channel
access-rule fields (`group_id`),
access values (`group`, `group_ppv`), billing key namespaces (`:group:`) and
AuthKit permission strings and deterministic scope identities retain their
encodings. Descriptive membership fields refer to entities; access-rule `group_id` and
`group`/`group_ppv` refer to paid channel membership. Real AuthKit permission
groups hold editorial roles independently of those read entitlements.

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
- **Account deletion**: `ck.AnonymizeAccount` moves the subject's rows to
  an anonymous key behind the source fence and fences the signal plane, which
  drops any late send.
- **Restore**: after restoring PostgreSQL against a retained signal plane, the
  preference revision floor must be raised to the sink's newest revision
  before writers resume: `POST /v1/import` with `preference_floor` (only ever
  advancing).

## Signal-plane account deletion

`AnonymizeAccount` completes its signal step only when:

1. the old account is recorded in the `erasures` ledger with `insert_quorum` =
   every replica; a down replica fails fast and moves nothing;
2. every row of the account in signals, compact state, daily contributions,
   exposures and the kept pre-ContentKit tables is copied to the anonymous key
   and deleted on every replica (`mutations_sync = 2`), re-counted as zero;
3. from the moment the ledger row is durable, every write, read, projection
   and export evaluates the ledger inside its own statement.

The subject key is dead in that tenant forever. Residue (rows that landed
after the fence or came back with a restore) is unreadable. After a restore,
run `ReplayErasureFences` for the accounts deleted since the backup. A shared account exists in every tenant:
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

Hosts declare popularity presets per content kind and scope. Preset requests
read stored rankings; arbitrary whole-UTC-day ranges read prepared daily counts.
The score is the mean daily `U/(N+20)`, where `U` counts qualified participants
once per person/item/day and `N` is the day's active eligible audience. Arbitrary
range requests outside trustworthy prepared coverage are rejected; an unprepared
preset stays pending. Recent days remain provisional through the 24-hour receipt window,
then finalized days are retained and reused until an explicit rebuild. See
[docs/popularity-policy.md](docs/popularity-policy.md) for qualification,
coverage, refresh and retention semantics. `top_rated` keeps its separate
rating behavior.

## Migration checklist

- Import legacy data through `POST /v1/import` ([docs/import.md](docs/import.md)), never into ContentKit's tables.
- Create and reuse one `contentkit.Engine` per tenant.
- Read lists, search and recommendations through the discovery routes; send actual reading snapshots through `POST /engagement` and explicit clicks through `POST /signals`.
- Run migrations explicitly through the host's development/deployment command ([docs/migration.md](docs/migration.md)); app startup checks schema readiness.
- Wire `Options.Moderator` when comments require screening and grant `root:moderation:review` to reviewers. Polls use predefined choices.
- Adopt the preference boundary (doujins #888 / hentai0 #594): pin this ContentKit, delete the callback-time bridge (`internal/social` `recorder`, `discovery.Recorder.Reaction`, `socialReactionSignal`) and every per-delivery signal-identity adapter, wire `Engine.AnonymizeAccount` into deletion and move favorite and reaction readers onto the routes.
- Replace `socialkit` imports with `content`: `EntityRef`/`EntityKey`/`entity_type`/`entity_id` → `contentref.ContentRef`/`ContentKey`/`content_kind`/`content_id`; `Entities` → `Resolver`; `Content` → `Processor`; `EntityTypes` → `ContentKinds`; `parent_id` → `reply_to_id`; `Counts(kind, id)` → `Counts([]ContentRef)`; delete the `Recorder` and `Moderation` adapters.
- Replace `content.Options.Storage`/`Media` (`StorageConfig`, `MediaStore`) with `Options.Media` over media, register the content kinds' `Named` body uploads (`Body`, a private image preset) and the `poll` kind with its public preset, and move image uploads to the SDK's `upload` plus the image routes above.
- Adopt the one media model (v0.62.0): declare the registry (`media.Config`), use version ids as item ids, replace slots with upload paths and public presets, and wire the hooks; see "Media".
- Rename `content.Options.PrivateDataEraser` to `ProviderDataEraser`.
- Replace host SQL on ContentKit tables with the reads and events in [Host references](#host-references).

Channel create/delete/restore calls the existing AuthKit client synchronously.
The same channel/group identity makes caller retries safe, including a lost
remote response after commit. Incomplete/deleted channels grant no authority.
There is no `contentkit_entity_sync` or replacement lifecycle job. Imports finish
their channel calls after committing identity, then report provider failures per
record so replay uses the same external IDs.

## Authored content and editions

The parent item carries shared identity, channel/editorial policy, creator credits
and inherited metadata associations. `VersionInput.Text` owns titles, summaries,
rich body text and aliases; source/original titles and uploaded files also belong
to the edition. The version's languages determine which edition a language-specific
feed selects. Shared metadata is combined with that edition's own metadata for
presentation without mutating siblings.

Hosts create an initial edition with `ContentInput.Versions`, including hidden
single-edition post kinds, and edit it through the normal version interface.
New additional versions have no inherited text/media. `Content.title`, `body`,
`description` and `images` are read-only projections from `selected_version_id`,
not parent storage or another writable surface. Parent text routes and version
fork/copy controls are removed. Ordinary source uploads and derivative processing
remain observable through the existing media progress interface.

## Post contributors and attribution

A host grants typed capabilities through its AuthKit roles. `ContentCreate`
permits personal posts outside channels; `ChannelContentCreate` permits
posting in one channel. `ContentEditOwn` and `ChannelContentEditOwn` permit
editing and publishing the caller's own posts. A normal contributor role grants
both create and edit-own. A submission-only role can grant create alone: its
author can read the submitted draft but cannot edit, publish or upload media.
Current create or edit-own authority is required for an author's private reads;
edit-own is required for mutations. Removing those grants applies on the next
request, even when the account remains recorded as the author.

`ContentWrite` and `ChannelContentWrite` retain broad staff/team-editor
semantics. Read-restricted auditors can see drafts but are not media editors.
For every channel, AuthKit's current owner/team roles are authoritative;
`owner_user_id` attribution does not bypass revoked grants. A missing or retired
authoring group grants no direct-owner fallback. Fixed staff blogs use the same
channel model as creator feeds, with `creation: staff` and optional delegated
blog-editor assignments. Channel-bearing items share their parent channel and
read policy across all versions; channel-free kinds retain their configured
version access behavior.

Personal authority also requires immutable `content_items.created_by`, set from
the authenticated tenant account. Editable creator credits are display metadata,
not grants. Imported content without `created_by` remains staff/channel-managed.
App-level own-post permissions never authorize access to arbitrary channels.
OpenRails continues to own paid audience entitlements and their expiration.

### Removed periodic jobs

`contentkit_reclassify` and `contentkit_projection_repair` are no longer
registered or scheduled. Existing rows do not block startup or migrations.
River handles an unknown kind as an ordinary failed attempt and discards the
job when its stored attempt limit is exhausted (three for these former timers).
No application handler runs. Terminal history remains, and queues the fleet
does not consume are unaffected.

No special ContentKit cleanup option is required. An operator who wants to
remove queue noise can use River's existing cancellation tools on reviewed
exact job IDs; empty arguments on the shared queue do not establish app ownership.
