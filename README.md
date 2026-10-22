# contentkit

`contentkit` is the deterministic content library for the Doujins, Hentai0
and marketplace hosts: tenant-scoped **content** (items, versions, entities),
**interactions** (comments, reactions, favorites, polls), **keyword search**
over that content, the
ClickHouse **signal plane** (consumption, feedback, exposures, popularity,
erasure) and the **discovery reads** over both. It needs no model provider,
API key or vector extension. Semantic search belongs entirely to the separate, deferred User Intelligence
library; ContentKit has no semantic search configuration or runtime hook.

Design: [open-rails-tracker/contentkit/DESIGN.md](https://github.com/open-rails/tracker/blob/master/contentkit/DESIGN.md).
Host contract: [HOST_INTEGRATION.md](HOST_INTEGRATION.md). Migrations:
[docs/migration.md](docs/migration.md), [docs/restore.md](docs/restore.md).

## Release policy

Standalone gateway patches use `media-gateway/vX.Y.Z` tags and publish only
`contentkit-media-gateway:gateway-vX.Y.Z` for amd64 and arm64, keeping those
immutable tags separate from root-release image tags. They do not release the
Go modules, media worker or UI package; those components can retain their
existing versions when their code and protocol are unchanged.

ContentKit is pre-stable and releases on `v0.x`; its API may change between
minor versions. The historical `v1.0.0-rc.1` through `v1.1.1` tags were
premature and are retracted. `v1.1.2` is a self-retracted, withdrawal-only
marker carrying Go module metadata, not a supported stable API release.

The marker and `v0.12.2` identify the same source commit. Go reads retractions
from the highest release before applying them, so the marker makes fresh
`@latest` requests select the latest unretracted `v0.x` release, including
future `v0.x` releases. Existing tags remain immutable; retraction preserves
explicit pins and does not automatically downgrade existing consumers.
See [Go module retractions](https://go.dev/ref/mod#go-mod-file-retract).

## Vocabulary

| Name | Meaning |
|---|---|
| `tenant_id` | one site: `doujins`, `hentai0`, the marketplace |
| `content_id` | the host-owned work: a gallery, a video, a listing; a canonical UUIDv7, never reused ([Content ids](HOST_INTEGRATION.md#content-ids)) |
| `content_version_id` | one selectable version of that work (optional) |
| `group_id` | a group: tag, artist, series, character, channel, playlist, featured rail |
| `ContentRef` | `{TenantID, ContentKind, ContentID, ContentVersionID *string}` — the typed reference every API, key, index and cursor carries |

A language is metadata on a document or version, never part of a reference.
Every read is scoped to the tenant pinned at construction; a reference of
another tenant is an error, never remapped.

## Packages

| Package | Owns |
|---|---|
| `contentref` | `ContentRef`, `ContentKey`, `GroupID` |
| `contenturl` | uniform content URLs `/{lang}/{route}/{CODE}[/{slug}][?v={version}]`: permanent 9-character codes per tenant, 4-character version codes, slugs, merges, legacy aliases, canonical redirects, a JSON lookup; the browser half is `@openrails/contentkit-ui/urls` ([Content URLs](HOST_INTEGRATION.md#content-urls-contenturl)) |
| `access` | `Actor`, the staff permissions (`Permission`, `Permissions()`); the key grammar, rules, the `Gate`, its listing `Filter` over `content_facets`, the request memo and the `Billing` dependency ([Access and offers](#access-and-offers-access-offers)) |
| `offers` | offers on content, versions and groups, their OpenRails sync job, free group joins and the library |
| `adapters/openrails` | `access.Billing` over the OpenRails client (its own module) |
| `media` | the registry (`Config`, kinds, upload paths, private and public presets), ordered manifests with provenance and conditional-write edits, the `Store` port, direct uploads and commit ops with their HTTP API, reads and HLS playlists, the optional `UploadLimiter`, and the sweep, deletion, `Expose` and relays as River jobs |
| `media/s3` | `Store` over aws-sdk-go-v2 (Ceph RGW in production, MinIO in tests), bucket policy and point-in-time `Restore` |
| `media/image` | libvips (CGO) producer: Image presets, public presets, zips, editor views, `PublishDefaults` |
| `media/token` | media access tokens, shared by hosts and the media gateway |
| `media/layout` | object keys and the media gateway's host and default rules, dependency-free |
| `media/gateway` | the media gateway's handler (`cmd/media-gateway`) |
| `media/video` | ffmpeg producers: byte-range fMP4 HLS ladders with audio, subtitle and sprite tracks, MP4 per rung, audio, subtitles, frame grabs; `Frames` for the frame picker |
| `media/worker` | the media worker: one process for every producer, built from the host's registry (`cmd/media-worker` is the stock build) |
| `media/workqueue` | the host's side of the worker: its per-host River schema, insert-only `Queue` (enqueue, cancel), encode progress |
| `catalog` | content items of the configured kinds (a blog is kind `blog`): per-language text, creators, versions, segments, draft/published/deleted with scheduling, listing, access rules, restrictions, attributes, covers and inline images; `content_facets`, the one visibility rule (`Viewer`) and ContentKit's own content resolver, the `content_events` outbox, search documents, `Totals`, its jobs and `/v1/contents` routes |
| `content` | comments (in language threads), reactions, favorites, polls (multiple-choice and free-text) and their counts on content items, versions and groups; banned words and IP bans; authors' names from `userinfo.Lookup` and avatars from media, the `ContentProcessor` port, poll images through `Media`, the optional `ContentModerator` (held/review queue, shared with the catalog) and `AnswerClassifier` ports, per-user interaction limits, owner and global comment bans, and the HTTP routes |
| `search` | Indexed keyword search (exact/alias/substring/prefix/typo, EN/ZH/JA/KO) with eligibility, documents and dirty queue, RRF |
| `groups` | storage for content entities by kind (`kinds.Config.GroupKinds`): localized names/aliases and text, attributes, covers, parents, scopes, edges, merge; content- and version-level memberships with roles, positions and windows; effective groups for facets, per-language counts, typeahead documents, `/v1` routes |
| `signal` | ClickHouse signal plane: canonical signals, compact subject state, daily rollups, windows, erasure fence, exposures/attribution, repair |
| `popularity` | named ranking policy (`PolicyV1`) over the window metrics: ClickHouse `RankExpr` and Go `Score` in agreement, literal windows, session scorer, group popularity through the `Catalog` port (`*groups.Store`) |
| `discovery` | the public reads under `/v1`: lists, search, typeahead, similar and recommended contents, release dates, content filters, history, signals and exposures, as cards under one visibility rule; popular and standing orders; the `Candidates` port, `Engagement`, `Fallback` and the shared `Recommender` policy |
| `eval` | lexical golden-case evaluation, reports, baselines |
| `migrations` | PostgreSQL migration chain and ClickHouse baseline |
| `adapters/openrails` | its own module (opt-in): `access.Entitlements` over OpenRails `CheckEntitlements` (one request per read) |
| `e2e` | its own module: every route end to end over PostgreSQL, MinIO and a real AuthKit |
| root | `Engine`: `New` (migrates its own schema), `Start` (its River jobs), `Routes` (route groups under `/v1`), `Client` (typed in-process client), `Ready`, `Probes`, `RiverJobs`, `Close` |
| `server` | its own module: the standalone `contentkit` binary (`server/cmd/contentkit`) |

## Engine

ContentKit runs in the host's process the way OpenRails does: one `New`,
one `Start`, routes the host mounts.

```go
ck, _ := contentkit.New(ctx, contentkit.Config{
	Tenant:     "doujins",
	Database:   contentkit.DatabaseConfig{Schema: "doujins_contentkit", RiverSchema: "doujins_river"},
	Content:    siteKinds, // kinds.Config: languages, tiers, content and group kinds
	Media:      &contentkit.MediaConfig{Config: registry, Delivery: contentkit.DeliveryConfig{SigningKey: key}},
	ClickHouse: contentkit.ClickHouseConfig{Database: "doujins_signal", Addr: chAddr},
}, contentkit.Deps{Postgres: pool, Auth: ak.Authenticator(), Users: ak.UserInfo(),
	Billing: ckopenrails.Billing(merchant), Store: bucket})
fleet, _ := riverhelpers.New(ctx, pool, &river.Config{Schema: "doujins_river"}, ak.RiverJobs(), ck.RiverJobs())
_ = ck.Start(ctx, contentkit.WithRiverClient(fleet)) // or ck.Start(ctx): its own River
routes, _ := ck.Routes(contentkit.Routes{Prefix: "/api/contentkit", Groups: contentkit.AllRoutes})
for _, r := range routes {
	mux.Handle(r.Path, r.Handler)
}
```

- `New` creates or upgrades ContentKit's tables in `Database.Schema`
  (default `contentkit`), River's and the media worker queue's, and with
  `ClickHouse.Addr` the signal plane, each under its migrator's advisory
  lock. It may share the host's database, never its schema. PGroonga and
  pg_trgm live in `public`.
- `Config` is plain data (YAML/JSON); `Deps` holds connections and
  providers. Media's namespace is the tenant; content kinds' media are their
  versions' (`media.ForContent`, the `media/versions` service). All content
  is the catalog's: hosts create it through `/v1/contents` (or `Client()`).
- `Start` runs every background job: search indexing, preference sync,
  projection repair, erasure, groups' counts and windows,
  the catalog's scheduled publishing, event delivery, counts and purge,
  offer sync and the media jobs. Hosts schedule nothing. Start before
  serving: writes queue jobs.
- The engine builds the access `Gate` from `Config.Content` and
  `Deps.Billing`, and the offers service from `Config.Offers`.
- `Client()` is the HTTP surface as a Go client acting as the host, over an
  in-process transport (for example `PublishNotifications`,
  `CreateContent`).
- `Ready` fails until `Start`; `Probes` are the optional dependencies
  (ClickHouse, the bucket, Redis); `EraseSubjects` is account erasure.
- `Config.Avatars` is people's avatars: held by one site's engine, the
  primary, which other sites sharing its people forward their avatar
  writes to (HOST_INTEGRATION "Avatars").

`server/` runs the same engine standalone: a YAML config file, secrets from
the environment, an AuthKit resource server, OpenRails' remote client and
health at `/livez`, `/readyz` and `/statusz` (`server/cmd/contentkit`).

For machine publishing, [the archive publishing guide](docs/api/publishing.md)
covers `Client.PublishArchive`, app-bound API keys and the resumable HTTP
operation that submits a gallery with its artist, series and tags.
The same guide covers retained source ZIPs, automatic page normalization and
the shared editor's reversible [page review](docs/api/publishing.md#page-normalization-and-review).

## HTTP API

`Routes` returns each selected route group as a subtree under
`{Prefix}/v1`. No auth middleware goes in front: each route's gate asks
`Deps.Auth` who the request is (once) and admits it by its tier:

| Group | Sub-path | Needs |
|---|---|---|
| `ContentRoutes` | `/v1`: the content model (`GET /v1/kinds`), content items and versions (`/v1/contents`, `/v1/manage or /v1/manage/{kind}`), catalog totals, comments, reactions, favorites, polls, comment bans, moderation | always |
| `UploadRoutes` | `/v1/media/upload` | `Config.Media` |
| `PublishingRoutes` | `/v1/publishing-operations`: one ZIP and its metadata through processing and publication | `Config.Media` |
| `MediaRoutes` | `/v1/media`: reads and HLS playlists; `/v1/contents/{id}/versions/{vid}/…`: reader plan and metadata, fork, download | `Config.Media` |
| `CodeRoutes` | `/v1/codes/{code}` | always |
| `EntityRoutes` | `/v1/entities`, `/v1/contents/{id}/entities`: listings and pages, writes for `root:entities:write` | always |
| `NotificationRoutes` | `/v1/notifications`: the inbox, and the host's publish route | always |
| `OfferRoutes` | `/v1/contents/{id}/offer`, `/v1/entities/{id}/offer` and `join`, `/v1/me/library`, `/v1/offers` | `Deps.Billing` |

| Tier | Admits |
|---|---|
| `open` | anyone; never authenticates |
| `public` | anyone: anonymous without a credential, else the subject it proves (a credential that fails is 401 with the provider's challenge) |
| `user` | a signed-in person (401 anonymous, 403 an application) |
| `staff` | a person or an application holding the route's permission, one of `access.Permissions()` (`root:<resource>:<action>`), in the Authenticator's root scope, by its `Verified.Can` (403 without it) |

Refusals carry `auth.Refuse`'s status and `WWW-Authenticate`; an auth outage
is 503. ContentKit names its staff permissions (`access.Permissions()`); the
host declares them in its auth's roles from those constants. They are
root-wide; per-item rights (creators, owner-group editors) are the catalog's. Behind a
proxy, set `Request.RemoteAddr` to the client's address before the routes:
anonymous reactions, votes and limits are keyed by it. No content kind may
be named `media`, `codes`, `entities`, `contents`, `kinds`, `catalog` or `notifications`.

Every route is declared once, in its module's route table
(`internal/httpapi`): method, path, tier, query, bodies and error codes. The
table mounts the handlers and generates [`api/openapi.json`](api/openapi.json),
[`docs/api/routes.md`](docs/api/routes.md) and the browser SDK's
`sdk/ui/src/client/generated/{routes,wire,error-codes}.ts`. After changing a
route or a wire type, run `go generate ./internal/contract`;
`TestGeneratedContractIsFresh` fails while a generated file is stale.

## Search

Documents are one per content item or group and language. The engine's
search job keeps them current from the dirty queue and a bounded backfill; rejected documents are
recorded in `content_search_invalid` until their source changes. Search,
autocomplete and discovery apply eligibility before every bounded retrieval.
Autocomplete matches literal substrings of an item's own title/name/aliases,
with exact names and prefixes first. It does not suggest a gallery merely
because an associated artist matches. Full search also uses contextual keywords
and conservative one-edit typos. Search-only normalization ignores complete
English filler words `the`, `a` and `an`; filler-only queries return no matches.
Entity identity, aliases and slug normalization are unchanged.

A full-search score is a match tier: exact title 1, alias 0.9, name prefix
0.84–0.85, name substring 0.81–0.82, contextual/token matches 0.75–0.77,
and typo 0.5–0.52. Queries are capped at 256 characters, 16 tokens and two
seconds. Migration `0011_search_substrings` adds search-only expression indexes
while retaining previous functions and indexes for mixed 0.103 writers.

`GET /v1/manage/search-suggestions` is private quick navigation to editors.
Explicitly request `include_contents` and/or `include_entities`, optionally
limiting `content_kinds` / `entity_kinds`; `limit` defaults to 8 per section
and is capped at 20. Current draft/held names and internal entities are matched
only within the caller's management authority. `display_language` chooses
labels while matching current names in every language. This API does not use
body/associated-entity keywords, public reader filters or a results-page fallback.
Migration `0012_management_names` adds separate current-name views and indexes;
approved/published search documents and old writers remain unchanged.

After **every** old app, search reader, importer and worker has stopped, reclaim
the compatibility indexes explicitly with an operator pool that has DDL
authority. Do this separately for each catalog schema; it is never run by
startup or migrations:

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
defer cancel()
report, err := search.RetireLegacyIndexes(ctx, operatorPool, "doujins_contentkit", true)
```

The final flag confirms the old cohort has stopped. The operation verifies all
replacement indexes are ready and checks tracked SQL helper dependencies before
dropping the four old indexes concurrently, then the two old keyword wrappers
with `RESTRICT`. Interrupted passes are safe to retry; the report lists the
validated indexes and which old objects were dropped or already absent. Hosts
expose this as `tools retire-search-indexes --legacy-readers-stopped`. Include external/dynamic
SQL callers in the cohort review; PostgreSQL cannot track those call sites.
`contentkit_keyword_normalize`, generated entity identity and canonical alias
uniqueness remain intact, along with all current search/management indexes.

## Errors

Every module answers failures with one flat body. Branch on `code`; `error`
is a human message and may change. A code's own members appear only with it
(`retry_after`, `action`, `ban`, `blobs`, `details`). The codes and their
statuses are listed in [docs/api/routes.md](docs/api/routes.md#error-codes).

```json
{"error":"not found","code":"not_found"}
```

5xx bodies carry no cause: it goes to `Options.Logger` (`slog.Default()` when
unset) with the request method, path, status and duration. Postgres constraint
names, driver text and stack traces are logged, never served.

## Access and offers (`access`, `offers`)

An app can restrict each content kind's editorial and sales policy in its
ContentKit model. The same policy is returned by `/kinds` and used by the admin
UI; it is enforced for content creation, updates, version overrides, imports
and individual offer creation. Omitting a policy retains the full feature set.
For example, a subscription-only gallery app can set:

```yaml
kinds:
  gallery:
    # Other kind settings omitted.
    access_levels: [public, tier]
    comment_modes: [open]
```

This permits free and tier-gated content, disables individual content/version
sales and keeps comments open. Tier names still come from `tiers`; the host's
OpenRails catalog supplies those subscriptions. Allowed access levels are
`public`, `tier`, `ppv`, `group`, `group_ppv`. Allowed comment modes are `open`,
`closed`, `hidden`. An omitted or empty list permits all modes. Entity sales
remain independently controlled by each entity kind's `sellable` setting.
Changing policy does not rewrite previously stored content access rules;
migrate those deliberately before applying a stricter policy. Disallowed
individual offers stop appearing as sales immediately and the normal offer
reconciliation withdraws them from billing, retaining their purchase history.
A restored item adopts a fixed comment mode where the kind declares one.


ContentKit gives entitlement keys meaning; OpenRails grants them and never
parses one. Keys are constructed, never typed:

| Key | Opens |
|---|---|
| `access.ContentKey` → `<tenant>:content:<id>` | every version of a content item |
| `access.VersionKey` → `<tenant>:content:<id>/<version>` | one version |
| `access.GroupKey` → `<tenant>:group:<id>` | every member of the group, and content whose rule names it |
| `access.TierKey` → `premium` | versions whose rule names the tier (`kinds.Config.Tiers`) |

A `Rule` is stored as JSON on content (`content_items.access`), as a
version's override (`content_versions.access`) and on sellable groups:
`{"level": "public" | "tier" | "group" | "ppv" | "group_ppv", "tiers", "group_id", "preview": {"pages" | "segments"}, "free_after"}`.
`Rule.Validate(kinds)` checks it; `Rule.Facet()` projects it onto
`content_facets`. A version opens with any of: its version key, its content
key, the key of a group holding it, a tier its rule names, its rule's group
(`group`), or a free rule (`public`, past `free_after`).

A content item's `body` is part of what it sells: reads return it only to its
editors and to viewers who open a version; others get `description` and the
paywall `decision`. Content, version and card decisions list `offers`, each
sold key's offer (`key`, `target`, `terms`, `price_id`; a paid one once
synced), the shape of a group's `access.offer`; tier keys are the host's to
label. A `group_ppv` item's product requires the group key
(OpenRails `required_entitlements`), so OpenRails refuses a non-member's order
(`entitlement_required`) for the key the decision `requires`. Content a group
owns (`owner_group_id`, a channel) lists under `owner_group=` and carries the
group's card as `owner`; owning is not membership and opens nothing.

```go
gate, _ := access.NewGate(access.GateConfig{Tenant: "onlydemo", Kinds: cfg.Kinds, Billing: ckopenrails.Billing(client)})
f, _ := gate.Filter(ctx, actor)                  // one billing read: tiers, content and group keyspaces
pred, args, _ := f.SQL("f", access.Hide)         // over content_facets f: only what the viewer opens
keep, short, _ := gate.Settle(ctx, actor, f, items) // free unless a keyspace was truncated
ds, _ := gate.Decide(ctx, actor, items)          // Allowed, or Sell, Requires, Preview, FreeAfter
prices, _ := offerSvc.Live(ctx, ds[0].Sell)      // which Sell keys are on sale, with their prices
```

- `access.WithMemo(ctx)` keeps the viewer's `Filter` and the keys read for
  the request: a request costs one billing read. Anonymous viewers cost none.
- Tiers in `kinds.Config.TokenClaims` are answered from the verified token
  when its `auth.Verified` implements `access.Entitled`; a refund then lags
  by the token TTL.
- A failed read fails closed: `Filter.Unknown()`, `Decision.Unknown` and an
  error wrapping `access.ErrUnavailable`; free rules keep serving.
- Past `access.HeldLimit` keys a keyspace is truncated: `SQL(Hide)` admits its
  candidates and `Settle` decides them in one exact read.

`access.Billing` is the billing dependency: `Held`, catalog edits (products
and prices) and free grants. `adapters/openrails.Billing` implements it over
the OpenRails client, embedded or remote.

`offers` sells content, versions and groups (`PUT|DELETE
/contents/{id}/offer`, `/contents/{id}/versions/{vid}/offer`,
`/entities/{id}/offer`). Its River job syncs each target to one OpenRails
product whose key and only entitlement is the access key, named after the
title, with one price (`offers.PriceKey`): a reprice is a new price, a
withdrawal archives the product and buyers keep their key. A free group
offer (amount 0) is joined with `POST /entities/{id}/join`; `GET /me/library`
lists the content a viewer bought.

## Media

Media is an app-defined, self-describing file system in one private bucket;
no database table records what media exists. The app declares its kinds in
one registry (`media.Config`); HOST_INTEGRATION "Media" covers the registry,
commit ops, reads and exposure.

```text
{namespace}/{kind}/{id}/manifest.json         gzip JSON: the ordered file list with provenance; never served
                       /private/sha256-{hex}  every blob: uploads, derived files, editor views (token)
                       /public/{name}         app-declared names, e.g. cover-460.webp (anyone)
                       /temp/{name}           staged uploads (u-{uuid}) and in-flight writes; never served
{namespace}/{kind}/{id}/{version}/...         a content version's item, laid out alike
{namespace}/{kind}/_default/public/{name}     a public preset's default image
```

A content kind's items are its versions, `{kind, content, version}`, each in
its content's folder, which holds no item of its own: covers and body images
live in versions too; other kinds' items have no versions. Private blobs are content-addressed and immutable;
public names carry a generation. Every URL is
`https://media.<site>/v1/{namespace}/{kind}/{id}[/{version}]/{public|private}/{name}`.
`media/versions` publishes versions to viewers (see HOST_INTEGRATION "Media").

`Config.Media` is the registry (`media.Config`, without hooks) and how it is
served; the engine builds the uploads, reads, journal, worker queue and jobs
from it and `Deps.Store`. `Deps.PurgePublic` hears of overwritten public URLs
for a CDN purge. Every media item is ContentKit's: content kinds' through
the catalog's resolver, groups', avatars' and, when the registry declares
the `poll` kind, poll images.

The user-avatar kind comes from `github.com/open-rails/contentkit/media/avatar`;
`avatar.User()` includes its upload rules, embedded default silhouette and
a `Stable` preset (its current image also at fixed names, so its URL is
built from the person's id). Publish defaults with
`media/image.PublishDefaults` as part of deployment.

**Manifests.** Every edit runs under the `Locker` (required: `PGLocker`, a
Postgres advisory lock shared by every process on the bucket) and writes with
`If-Match` (or `If-None-Match: *`) once the store reports conditional PUT,
retrying on conflict. The edit is normalized (canonical order, download
names, dropped originals) and validated; an unchanged manifest is not
written. Reads go through an in-process cache bounded by bytes and
revalidated by ETag, so a read is never stale. A 2,000-page gallery is about
1.5 MB of JSON and 400 KB stored; a 2-hour video about 5 KB, its segment
tables living in index blobs. Every read and write stops at
`MaxManifestBytes` (8 MiB of JSON; the default 128 MiB cache holds five).
Edits stop 4 KiB short of it and only when they grow the manifest, so a
full item can still shrink and the `hidden` and `full` flags always fit. A
commit is refused (413 `too_large`) when the item, once processed, would
pass that: it projects every output its presets have yet to write. When
outputs still overrun it, the worker marks the item `full` instead of
recording them: producers then write no private output for it (its public
files still render), and editor reads report `full` (state `full`), until a
commit frees the bytes the refused record was short of (`deficit`, counting
what removed uploads would still have added). An item holds
at most `MaxUploads` (10,000) uploads, an upload's meta at most 4 KiB of
JSON and the item's 16 KiB; names are capped at 200 bytes of JSON, written
unescaped (`<` is one byte). The manifest object records the quota its
uploads were charged, so deleting an item never needs it to decode, and
hiding deletes `public/` before touching it.

**The bucket is optional at startup.** `s3.New` never dials; register
`store.Check(ctx)` as the host's optional S3 dependency probe. Unless
`Config.Capabilities` declares them, the first use (a write, or `Check`)
probes the backend's capabilities, one probe at a time; a failed, throttled
or cut-off probe records nothing, its write is `media.ErrUnavailable` and the
next use probes again. An unreachable
or 5xx bucket is `media.ErrUnavailable`: 503 `unavailable` over HTTP, and in
media jobs a River snooze (not an attempt) while a fresh `Check` confirms the
outage, capped by `MaxOutageSnoozes` (`media.SnoozeUnavailable`).

**Uploads** go straight to the bucket, to a staged name: up to 64 MiB is one
PUT to `temp/u-{uuid}` signed with its type, length and SHA-256, larger files
are multipart (8–16 MiB parts, each signed with its length and SHA-256,
resumed through `ListParts`, completed by the server from a signed ticket;
nothing is stored). The browser's hash only finds an identical blob already
in the folder (no upload). After the commit, the worker's place job hashes
each staged upload and writes it to `private/sha256-{hex}` of the bytes it
read (`Manifests.Place`); an existing blob is never overwritten, so a blob
always holds the bytes of its name. The optional `UploadLimiter`
(`media.NewPGLimiter`) rate-limits uploaders and charges each upload's size
(at least 64 KiB) to the grant's `Owner`; growth past the quota fails with 413
`quota_exceeded`, and deleting an item releases it.

**Media gateway** (`cmd/media-gateway`, `media/gateway`, image
`ghcr.io/open-rails/contentkit-media-gateway:{tag}`): `public/` to anyone
(`public, max-age=300, stale-while-revalidate=86400`, falling back to the
kind's `_default` for declared names), `private/sha256-{hex}` with the item's
token in `?t=` or the `mt` cookie (`private, immutable`), and
`download/sha256-{hex}-{uuid}` only with a `?t=` download token of that one
key (the version download route's). An item token opens every private file of
its item or none, never a download; `?dl={name}` serves the file as a download
under that name when it has the file's type. Everything else and every denial
is one `no-store` 404. It keeps no state: rate limit the media host at the
ingress; the read API limits the items a viewer opens per hour
(`ReaderOptions.Issuance`). It needs `MEDIA_GATEWAY_S3_ENDPOINT`, a key
reading only `*/private/*`, `*/download/*` and `*/public/*`, `MEDIA_GATEWAY_TOKEN_KEY` and
`_TOKEN_KEY_PREVIOUS`, `MEDIA_GATEWAY_HOSTS`
(`media.doujins.ai=doujins,accounts; media.hanime.media=hentai0,accounts`),
`MEDIA_GATEWAY_BUCKETS`, each namespace's bucket, one per app
(`doujins=prod-doujins; hentai0=prod-hentai0; accounts=prod-doujins`),
`MEDIA_GATEWAY_CORS_ORIGINS` and `MEDIA_GATEWAY_DEFAULTS`
(`layout.FormatDefaults(media.GatewayConfig(reg).Defaults)`).

**The media worker** (`media/worker`) runs every producer from the host's
worker River schema (`media/workqueue`, `MEDIA_WORKER_SCHEMA`, per host:
hosts sharing a database never share one): images, zips, public presets and
editor views (`media/image`, libvips) and HLS, MP4, audio, subtitles and
frames (`media/video`, ffmpeg). The host presigns, commits, exposes and reads,
and links only `media/workqueue`. The worker hands readiness
(`Hooks.ItemReady`), purges (`Hooks.PurgePublic`) and sweeps back to the
host's River schema (`MEDIA_HOST_RIVER_SCHEMA`), so the stock build
(`cmd/media-worker`, image `ghcr.io/open-rails/contentkit-media-worker`)
needs only the registry as JSON (`MEDIA_KINDS_FILE`): an engine's is
`Config.MediaKinds()`, which `contentkit media-config -config contentkit.yaml`
prints. Hosts with a `Private.Choose` build their own:

```go
cfg, _ := worker.FromEnv(ctx) // DATABASE_URL, MEDIA_S3_*, MEDIA_WORKER_SCHEMA, MEDIA_HOST_RIVER_SCHEMA, MEDIA_WORKER_*
cfg.Kinds = reg                // the host's registry
w, _ := worker.New(ctx, cfg)   // no DDL: the host's migrate step runs workqueue.Migrate
_ = w.Run(ctx)                 // until SIGTERM; running jobs get MEDIA_WORKER_SHUTDOWN_GRACE
```

It never exits for a missing dependency: `Run` takes no jobs until the
bucket answers, and `MEDIA_METRICS_ADDR` serves `/livez`, `/readyz`,
`/statusz` and `app_dependency_up` with /metrics.

**Images** (`media/image`, CGO over libvips): WebP at each `Image` spec
(inside or cover box, quality, blur), through the upload's edit (crop in
EXIF-oriented source pixels, then a clockwise quarter rotation); nothing is
upscaled. GIF and WebP animations keep every frame, delay and loop count;
`MaxPixels` (100 MP over all frames), `MaxFrames` (1000) and
`MaxAnimationSeconds` (60) bound sources, and a stored source over its
Upload's `MaxBytes` is refused unread. Uploads feeding image presets take
only `media.ImageTypes` (JPEG, PNG, WebP, GIF, AVIF, HEIF, TIFF), the
declared type binds the decoder, and every other libvips loader is blocked
(libvips 8.13 or later). Public presets render every width through the same
spec (a width past the edited image at its width) and carry `from` and `fp`
as object metadata.

A private image preset can instead use `Format: media.ImageFormatSource`
and `To: "download-pages/{name}{ext}"` to retain the upload's format.
With no edit or blur and an inside fit, files already inside the configured
box keep their exact bytes; zero dimensions leave that side unbounded.
`Width: 6000, Height: 6000` proportionally limits the longest edge to 6,000
pixels without cropping or enlarging. Only an oversized or edited image is
re-encoded: JPEG, PNG, WebP and GIF support this, including animated GIF and
WebP; other accepted codecs can pass through unchanged but cannot currently
be resized in source format. `Quality` controls JPEG/WebP re-encoding;
unchanged files keep their bytes and PNG stays lossless. `KeepOriginals: true`
retains the uploaded source even when a bounded download is generated, and
the uploaded ZIP is not rewritten. A `Zip: "download-pages/"` preset packs
those files in the selected reading order, excluding pages omitted by ingestion review. Public
images, thumbnails and editor previews continue to use WebP.

Deploy media workers and gateways that understand the source-format registry
before an app starts declaring it. When replacing an existing download
preset, queue `Regenerate` for existing items so their new download renditions
are generated; changing configuration alone does not enqueue every item.
Reader ZIPs are not generated during placement. Their download grant returns an
`archive?...` URL that streams the selected source-format pages in virtual
sequential order. Ordinary archives are spooled to a temporary disk file as they
stream, then atomically cached only after the complete ZIP succeeds. One cache
builder and a 512 MiB spool limit bound temporary storage per process. Busy or
oversized transfers stream without caching; they never wait for another builder.
The full
SHA-256 cache key covers the ordered blobs, output extensions, types and archive
recipe; reorder, exclusion, replacement and output-policy changes select a new
key. Cached objects stay under the version's never-public `temp/` folder and
expire with `JobsConfig.TempTTL` (48 hours by default). Each transfer, including
a cache hit, repeats current visibility, access, chapter-release and host-policy
checks, and refuses stale tickets or a draft source order that differs from its
frozen publication. Personalized watermarked archives are generated fresh with
`private, no-store` and never share this cache. Generic non-reader ZIP presets
still produce their declared processing outputs. An editor's signed archive
URL delegates only that exact transfer for its short lifetime (five minutes by
default), matching private media URLs. It carries no reusable session or global
permission; role revocation takes effect when an already-issued editor URL
expires. Soft deletion, source edits and invalid chapter markers revoke access
even before expiry. Verified token tiers explicitly configured in `TokenClaims`
travel as signed entitlement context for this exact archive and its short
download-ticket lifetime, as a presigned download did before. The archive never
replays the JWT or creates a session or general permission credential. Ordinary
audience grants still recheck current access and billing-backed entitlements.
Both editor and token-tier delegation last until the download ticket expires,
even if the original JWT expires sooner, matching prior presigned URLs. The
verified-entitlement contract exposes no credential-expiry accessor; keep
`Downloads.TTL` short. Current gate configuration, policy, source and deletion
checks still apply to each transfer.

Reader page identity is the SHA-256 digest of the original source image.
`FileInfo.page_id` and reader-plan page `id` expose that digest; logical names
are display/path metadata. The manifest's ordered source list determines page
numbers and consecutive ZIP entry names, without renaming source blobs when
pages move. Reader metadata and chapter starts refer to source hashes.
Upload commits remain asynchronous: a staged file has no verified `page_id`
until placement finishes. Wait for placement before saving reader metadata or
chapter starts that reference that source; a filename is not a fallback ID.

Reader chapters accept `start_page` and derive their end from the next start,
or the end of the version. Inserting or reordering unmarked pages does not
require renumbering markers. Missing, duplicated or out-of-order starts require
repair; authorization resolves them against the exact published snapshot.
Legacy numeric ranges remain supported for non-reader content.

Personalized, watermarked archives stream per request with
`Cache-Control: private, no-store`; they are never placed in a shared cache.
Ordinary reader archives use the bounded, on-demand private cache described
above; only non-reader ZIP processing presets are generated ahead of time.

**Video** (`media/video`): an `HLS` preset encodes the ladder (rung = short
side, default 2160/1080/480, none above the source) in each of the worker's
codecs (`MEDIA_WORKER_CODECS`, default `av1,h264`), plus AAC per audio
track, WebVTT per text subtitle and a seek sprite. Each rendition is one
byte-range fMP4 blob whose segment table is its own index blob; stages are
published rung by rung (the upload stays pending until the last), a
compliant top rung is stream-copied, and sources outside the aspect bounds
fail with `video_aspect_unsupported`. An `MP4` preset muxes H.264 at one rung with the default audio;
`Audio` gives an HLS track and an M4A (optional EBU R128 loudness);
`Subtitles` converts SRT and SSA/ASS to clean WebVTT. Playlists are built per
request by the read API. Encode progress comes from
`workqueue.NewProgressSource` (`ReaderOptions.Progress`).

**Jobs** (`media.Jobs`, composed into the host's River client): the sweep
collects garbage by manifest reference (unreferenced private blobs a grace
period old, unexpected public names at once, `temp/` by age); folder
deletion (with a late-upload second pass and quota release); `Expose`;
`Regenerate`; `SweepOrphans`; and the worker's relays.

**Paywalls**: media gates each item through `Hooks.Resolver`. Reader grants
cover a permitted span of an immutable publication, limited by chapter release
and access rules; source originals remain editor-only. Existing signed URLs
retain their normal expiry and revocation-window semantics.

**Browser package** (`sdk/ui`, `@openrails/contentkit-ui` on npm, one version
per release): the client (uploads, commit ops, reads, `waitFor`, frames, HLS),
content URLs, React hooks and styled components. See
[sdk/ui/README.md](sdk/ui/README.md).

## Groups

Group kinds come from `kinds.Config.GroupKinds` (ordered, parent, scoped_by,
owner, sellable, auth_scope, windows, version_level, roles, attributes).
Reader editors use one page per stable version code. Shared entity edits use
`PUT /contents/{id}/entities/shared`; per-version credits use
`PUT /contents/{id}/versions/{vid}/entities`. Catalog responses keep the same
separation: `Content.entities` contains shared associations only, while each
`Version.entities` contains that version's own associations. The UI SDK's
`entitiesForVersion(content, version)` combines and deduplicates the two for
public display without mixing credits from sibling versions.
These routes replace only their
own scope in a transaction, including newly created entity names, and preserve
sibling versions. The older all-scopes entity replacement remains available
for imports and callers deliberately replacing the entire work.

Writes are batches in one transaction; memberships keep
`content_facets.group_ids` current, `Recount` (an engine job) rebuilds
per-language counts and automatic covers from the facets, and
`RefreshWindows` re-derives facets as rail windows open and close.

The engine builds the store from `Config.Content`, serves it as
`EntityRoutes` and runs `Recount` (every 15 minutes) and `RefreshWindows`
(every minute) as its jobs; groups' keyword documents are its search job's.

## Catalog

`catalog.Store` owns content items of the kinds in `Config.Content.Kinds`.
Writes are batches; each transaction keeps `content_facets` current (rules
through `access.Rule.Facet`, groups through `EffectiveGroups`), marks search
documents dirty, queues `content_events`, renames offers' products after
titles and calls media (expose, republish, fork, delete). Published with a
future `publish_at` is scheduled until a job makes it live. A deleted item
stays readable to holders of its own keys; its media is erased after
`DeletedRetention` unless it was sold.

The `Viewer` is the one visibility rule: published content whose release time
has arrived, the domain's scope and the viewer's content filters. Every
published item can be listed; its edition's access rule controls reading.
It uses `access.Filter.SQL` for lists
and `Gate.Decide` for items; staff (`root:content:view-restricted`),
creators and owner-group editors see everything. Discovery lists and searches
under it; media and interactions resolve through it. Held text goes to the
shared review queue, screened by `Deps.Moderator`. `GET /v1/catalog/totals`
(`kinds`, `languages`) counts what anyone and each tier opens, per kind and
language; a sibling deployment reads it with
`catalog.NewTotalsClient(baseURL, nil).Totals(ctx, catalog.TotalsQuery{Kinds, Languages})`.

[Content filters](docs/content-filters.md) form one app-scoped entity layer across
explicit `applies_to` content kinds. Domains define their offered toggles and
defaults; `require_entities` independently scopes hard requirements by kind.
One saved entity choice affects galleries and posts together. Public browsing,
including staff browsing, honors optional choices; authorized management keeps
its visibility. Fixed and required domain rules also constrain direct access.
`kinds.DecodeContentFilters` strictly reads an embedded app's policy block.

## Signal plane and discovery

The signal plane runs inside the engine over `Config.ClickHouse`; its public
routes (signals, exposures, history, popular, similar, recommend) land with
#122.

- Identity is `(tenant, content ref, subject, type, event id)`; retries,
  reordering and revisions converge, nothing is incremented. Record the work
  view and, separately, the selected version's view (`g1.WithVersion(v)`).
- Work reads (`History`, `Popular`, `SeenIDs`, co-engagement) count the work
  once across versions; `States` and `Metrics` read exactly the references
  given, work or version.
- Windows are whole UTC days, 7/30/90/365/all, no decay.
- Rank by a named policy, not the default rank: `popularity.ByName("v1")`,
  `popularity.New(popularity.Config{Source: hub, Policy: policy})` over the engine's hub, then
  `ranker.Popular` / `ranker.Scores` / `ranker.Groups`
  ([docs/popularity-policy.md](docs/popularity-policy.md)).
- Similar and recommended content draw candidates from `Deps.Candidates`
  (default: co-engagement); see
  [discovery candidates](HOST_INTEGRATION.md#discovery-candidates).
- `EraseSubjects` is account erasure with a quorum-written fence; see
  [HOST_INTEGRATION.md](HOST_INTEGRATION.md#subject-erasure-completion-contract)
  and, for interaction data, [interaction erasure](HOST_INTEGRATION.md#interaction-erasure).
- Reactions and favorites reach the signal plane from their own revisioned
  rows: the engine's preference job syncs them (watermark with a commit
  overlap) and re-sends them daily; see
  [HOST_INTEGRATION.md](HOST_INTEGRATION.md#preference-boundary-reactions-and-favorites-into-the-signal-plane).

## Testing

```sh
CONTENTKIT_TEST_URL=postgres://...  CONTENTKIT_PROFILE_URL=postgres://... \
CONTENTKIT_TEST_CH_ADDR=localhost:9000 CONTENTKIT_TEST_CH_USER=... CONTENTKIT_TEST_CH_PASSWORD=... CONTENTKIT_TEST_CH_CLUSTER=... \
go test ./... -race -count=1 -p 2
```

Media tests also need an S3 backend (`CONTENTKIT_TEST_S3_ENDPOINT`,
`_ACCESS_KEY`, `_SECRET_KEY`, optional `_REGION`, `_BUCKET` and `_REQUIRE`; see
`media/internal/s3test`). CI runs them on MinIO; `media/image` runs in its
own CI job with libvips, and the other jobs exclude it. To record a Ceph RGW
release's capabilities, point the same variables at an RGW bucket and run
`go test ./media/... -v -count=1`; the log prints the probed capabilities.
Manifest edits run under `PGLocker`, so the tests also need
`CONTENTKIT_TEST_URL` (they skip without it).
`media/video` tests also need `ffmpeg` and `ffprobe` on `PATH` (they skip
without them unless `CONTENTKIT_TEST_FFMPEG=1`).

| Backend | Conditional PUT | SHA-256 enforced | Notes |
|---|---|---|---|
| MinIO RELEASE.2025-09-07 | yes | yes | drops `AbortIncompleteMultipartUpload` (expires uploads itself) |
| Ceph 19.2 / 20.2 standalone `dbstore` RGW | no | no | wrong Range bytes; not representative of RADOS-backed RGW |
| production Ceph RGW (RADOS, 2026-09-24) | no (`If-Match` yes, `If-None-Match: *` ignored) | no | Range, `response-content-disposition`, versioning, lifecycle (incl. `AbortIncompleteMultipartUpload`) and multipart work; hosts wire `PGLocker` |

Tests run against real PGroonga Postgres and ClickHouse+Keeper and skip
without the variables; `CONTENTKIT_PROFILE_URL` needs `CREATEDB`. Regenerate the eval baseline with
`CONTENTKIT_EVAL_UPDATE=1`.

User inboxes, native comment events and OpenRails imports use the shared notification library. See [notifications](docs/notifications.md) for APIs, identity mapping, retained-history semantics and erasure.
