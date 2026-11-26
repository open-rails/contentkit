# contentkit

`contentkit` is the deterministic content library for the Doujins, Hentai0
and marketplace hosts: tenant-scoped **content** (items, versions, entities),
**interactions** (comments, reactions, favorites, polls), **keyword search**
over that content, the
**activity log** in ClickHouse (opens, reading progress, reactions, hides,
searches) with the **reading state** and daily rollups folded from it, and the
**discovery reads** over both. It needs no model provider,
API key or vector extension. Semantic search belongs entirely to the separate, deferred User Intelligence
library; ContentKit has no semantic search configuration or runtime hook.

Design: [open-rails-tracker/contentkit/DESIGN.md](https://github.com/open-rails/tracker/blob/master/contentkit/DESIGN.md).
Host contract: [HOST_INTEGRATION.md](HOST_INTEGRATION.md). Hosts use ContentKit
only through its client, routes and event log, never SQL on its tables: it can
run on its own database ([host references](HOST_INTEGRATION.md#host-references)).
Post/channel policies: [docs/posts-and-channels.md](docs/posts-and-channels.md). Migrations:
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
| `Scope.TenantID` | the business owning an account directory and its apps |
| `Scope.AppID` | one tenant-owned app and its separate content catalog, such as Doujins or Hentai0 |
| `content_id` | the host-owned work: a gallery, a video, a listing; a canonical UUIDv7, never reused ([Content ids](HOST_INTEGRATION.md#content-ids)) |
| `content_version_id` | one selectable version of that work (optional) |
| `channel_id` | the overall item's primary authoring channel |
| `entity_id` | an associated tag, artist, series, character, playlist or featured rail |
| `ContentRef` | `{TenantID, ContentKind, ContentID, ContentVersionID *string}` — the typed reference every API, key, index and cursor carries |

A language is metadata on a document or version, never part of a reference.
Each engine serves the app pinned by `Scope`. Apps can share tenant-owned
account data without sharing their catalogs. `ContentRef.TenantID` retains the
catalog partition key: it is `Scope.AppID`, distinct from the business's
`Scope.TenantID`. `Scope` is required.
A reference to another catalog is an error, never remapped. See [scope.go](scope.go).

## Packages

Hosts import only the public packages. Each domain package holds its route
bodies' wire and data types and its ports; its implementation is the
`internal/<domain>store` twin, which the compiler keeps from hosts
([Public surface](HOST_INTEGRATION.md#public-surface)).

| Package | Owns |
|---|---|
| root | `Engine`: `New` (validates or migrates storage according to `Database.SkipMigrations`), `Start` (serve; requests enqueue jobs), `StartJobs` (work its jobs on its own River), `Routes` (route groups under `/v1`), `Client`/`NewClient` (the typed client of every route), `EventReader`, account lifecycle, `Reconcile`, `Ready`, `Probes`, `Close`; `Config`, `Deps`, `MigratePostgres`, `MigrateClickHouse`, error codes |
| `kinds` | the content model: content and entity kinds, languages, tiers, channels |
| `contentref` | `ContentRef`, `ContentKey`, `EntityID` |
| `access` | `Actor`, the staff permissions (`Permission`, `Permissions()`), the key grammar, rules and the `Billing` port ([Access](#access-access)) |
| `catalog`, `entities`, `channels`, `content`, `contenturl`, `discovery`, `importer`, `signal`, `media`, `media/versions` | the routes' request and response types, events, the media registry's config types and the bucket port (`media.Store`), and the host ports (`content.ContentModerator`, `entities.Scopes`, …) |
| `media/s3` | `media.Store` over aws-sdk-go-v2 (Ceph RGW in production, MinIO in tests), bucket policy and point-in-time `Restore` |
| `media/avatar` | the standard account avatar kind |
| `media/gateway`, `media/token`, `media/layout` | the media gateway's handler (`cmd/media-gateway`), its access tokens and object keys |
| `adapters/authkit`, `adapters/openrails` | their own modules: AuthKit auth and scopes, `access.Billing` over OpenRails |
| `server` | its own module: the standalone `contentkit` binary (`server/cmd/contentkit`) |
| `internal/catalogstore` | content items, versions, `content_facets`, the one visibility rule, the `content_events` log ([cursor reads](docs/events.md)), search documents, totals, retention jobs |
| `internal/entitystore`, `internal/channelstore` | entities by kind and channels: names, text, covers, scopes, merges, memberships |
| `internal/contentstore` | comments, reactions, favorites, polls, bans, moderation, interaction limits |
| `internal/discoverystore`, `internal/search`, `internal/popularity` | public reads: lists, search, typeahead, similar and recommended, release dates, history; keyword search; ranking policy |
| `internal/signalstore`, `internal/readingstore`, `internal/analytics` | the ClickHouse activity log and daily rollups; reading state, history and sign-in links in PostgreSQL; the job folding one into the other ([docs/analytics.md](docs/analytics.md)) |
| `internal/mediastore`, `internal/versionstore` | media manifests, uploads, reads, processing records; version reader plans, publication and downloads |
| `internal/entitlement` | the access `Gate`, its listing `Filter` and the request memo |
| `internal/urlstore`, `internal/importstore` | content codes, slugs, aliases; the bulk import |
| `internal/media/{worker,workqueue,image,video}` | the media worker (`cmd/media-worker`), its queue, libvips and ffmpeg producers |
| `internal/migrations` | PostgreSQL migration chains and ClickHouse baseline |

## Engine

ContentKit runs in the host's process the way OpenRails does: one `New`,
one `Start`, routes the host mounts.

```go
ck, _ := contentkit.New(ctx, contentkit.Config{
	Scope:            &contentkit.Scope{TenantID: tenantID, AppID: appID}, // stable provisioned IDs
	Database:   contentkit.DatabaseConfig{Schema: "doujins_contentkit", SkipMigrations: true}, // River: doujins_contentkit_river
	Content:    siteKinds, // kinds.Config: languages, tiers, content and entity kinds
	Media:      &contentkit.MediaConfig{Config: registry, Delivery: contentkit.DeliveryConfig{SigningKey: key}},
	ClickHouse: contentkit.ClickHouseConfig{Database: "analytics", Addr: chAddr},
}, contentkit.Deps{Postgres: pool, Auth: ak.Authenticator(), Users: ak.UserInfo(),
	Billing: ckopenrails.Billing(merchant), Store: bucket})
_ = ck.Start(ctx)     // API process: serve; requests enqueue jobs
_ = ck.StartJobs(ctx) // worker process: work ContentKit's jobs on its own River
routes, _ := ck.Routes(contentkit.Routes{Prefix: "/api/contentkit", Groups: contentkit.AllRoutes})
for _, r := range routes {
	mux.Handle(r.Path, r.Handler)
}
```

- With `Database.SkipMigrations: true`, `New` checks prepared storage read-only.
  Run the explicit host migration command first; it calls the applicable
  PostgreSQL, media and ClickHouse migration stages. `MigratePostgres` also
  provisions configured fixed channels and their groups through
  `MigrationOptions.Scopes`; hosts need no separate seeding code. The default `false` retains
  embedded startup migrations. Catalog tables use `Database.Schema` (default
  `contentkit`); account tables can use a separately shared schema. PGroonga and
  pg_trgm live in `public`. See [migration instructions](docs/migration.md).
- `Config` is plain data (YAML/JSON); `Deps` holds connections and
  providers. `Scope` binds the business and app; media paths derive from that
  scope, with tenant-shared account media. Content kinds' media are their
  versions' (`media.ForContent`, the `media/versions` service). All content
  belongs to ContentKit: hosts create it through `/v1/contents` (or `Client()`).
- `Start` runs every background job: search indexing, the analytics job,
  entity popularity, publication events, event retention and content purge,
  and the media jobs. Hosts schedule nothing. Start before
  serving: writes queue jobs.
- The engine builds the access `Gate` from `Config.Content` and
  `Deps.Billing`. Billing supplies authoritative entitlement reads; product
  and price management stays in OpenRails.
- `Client()` is the HTTP surface as a Go client acting as the host, over an
  in-process transport (for example `PostContents`,
  `CreateContent`).
- `Ready` fails until `Start`; `Probes` are the optional dependencies
  (ClickHouse, the bucket, Redis); `AnonymizeAccount` is account deletion.
- `Config.Accounts.Avatars` is people's avatars: held by one site's engine, the
  primary, which other sites sharing its people forward their avatar
  writes to (HOST_INTEGRATION "Avatars").

`server/` runs the same engine standalone: a YAML config file, secrets from
the environment, an AuthKit resource server, OpenRails' remote client and
health at `/livez`, `/readyz` and `/statusz` (`server/cmd/contentkit`).

For machine uploads, [the media upload guide](docs/api/uploads.md) covers
app-bound API keys, presign/upload/commit, durable processing status and explicit
publication. The same guide covers retained source ZIPs, automatic page
normalization and the editor's reversible [page review](docs/api/uploads.md#page-normalization-and-review).

## HTTP API

`Routes` returns each selected route group as a subtree under
`{Prefix}/v1`. No auth middleware goes in front: each route's gate asks
`Deps.Auth` who the request is (once) and admits it by its tier:

| Route group | Sub-path | Needs |
|---|---|---|
| `ContentRoutes` | `/v1`: the content model (`GET /v1/kinds`), content items and versions (`/v1/contents`, `/v1/manage or /v1/manage/{kind}`), catalog totals, comments, reactions, favorites, polls, comment bans, moderation | always |
| `UploadRoutes` | `/v1/media/upload` | `Config.Media` |
| `MediaRoutes` | `/v1/media`: reads and HLS playlists; `/v1/contents/{id}/versions/{vid}/…`: reader plan and metadata, download | `Config.Media` |
| `CodeRoutes` | `/v1/codes/{code}` | always |
| `EntityRoutes` | `/v1/entities`, `/v1/contents/{id}/entities`: listings and pages, writes for `root:entities:write` | always |
| `DiscoveryRoutes` | public lists/search, history and `/v1/me/library` for purchased content | always; purchased access uses `Deps.Billing` |

| Tier | Admits |
|---|---|
| `open` | anyone; never authenticates |
| `public` | anyone: anonymous without a credential, else the subject it proves (a credential that fails is 401 with the provider's challenge) |
| `user` | a signed-in person (401 anonymous, 403 an application) |
| `staff` | a person or an application holding the route's permission, one of `access.Permissions()` (`root:<resource>:<action>`), in the Authenticator's root scope, by its `Verified.Can` (403 without it) |

Refusals carry `auth.Refuse`'s status and `WWW-Authenticate`; an auth outage
is 503, and any other panic while serving is 500 `internal_error`. ContentKit names its staff permissions (`access.Permissions()`); the
host declares them in its auth's roles from those constants. They are
root-wide; per-item rights (creators, primary-channel editors) are the catalog's. Behind a
proxy, list it in `Config.TrustedProxies`, or set `Request.RemoteAddr` to the client's address before the routes:
location rules and anonymous reactions, votes and limits are keyed by it ([client address](HOST_INTEGRATION.md#client-address)). No content kind may
be named `media`, `codes`, `entities`, `contents`, `kinds`, `catalog` or `notifications`.

Every route is declared once, in its module's route table
(`internal/httpapi`): method, path, tier, query, bodies and error codes. The
table mounts the handlers and generates [`api/openapi.json`](api/openapi.json),
[`docs/api/routes.md`](docs/api/routes.md) and the browser SDK's
`sdk/ui/src/client/generated/{routes,wire,error-codes}.ts`. After changing a
route or a wire type, run `go generate ./internal/contract`;
`TestGeneratedContractIsFresh` fails while a generated file is stale.

## Search

Documents are one per content item or entity and language. The engine's
search job keeps them current from the dirty queue, renamed entities (expanded
to their items in batches) and a bounded backfill; rejected documents are
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
and typo 0.5–0.52. Queries are capped at 1 KiB, 256 characters, 16 tokens
of at most 128 normalized characters and two seconds; typos are tried only
in tokens of up to 16 characters. Each search document stores its normalized terms, so a bounded candidate window keeps the strongest
name matches. A search sorted by newest, title or popularity orders every
literal match; typo matches come from the nearest-distance window.

`GET /v1/manage/search-suggestions` (tier `editor`) is private quick navigation to editors.
Explicitly request `include_contents` and/or `include_entities`, optionally
limiting `content_kinds` / `entity_kinds`; `limit` defaults to 8 per section
and is capped at 20. Current draft/held names and internal entities are matched
only within the caller's management authority. `display_language` chooses
labels while matching current names in every language. This API does not use
body/associated-entity keywords, public reader filters or a results-page fallback.
Separate current-name views and indexes serve it; approved/published search
documents are unchanged.

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

## Access (`access`)

OpenRails owns products, prices, checkout, subscriptions, purchases and granted
entitlements. ContentKit owns content and its access requirements. Applications
compose OpenRails's product/price API and billing UI with ContentKit's content
editor; no local offer catalog or billing synchronization jobs are involved.
The same boundary applies to embedded and HTTP deployments.

A subscription-only app can restrict its content model to free and tier access:

```yaml
content_kinds:
  gallery:
    # Other kind settings omitted.
    access_levels: [public, tier]
    comment_modes: [open]
```

The model is returned by `/kinds` and enforced on creation, updates, version
overrides and imports. Allowed access levels are `public`, `tier`, `ppv`,
`group`, `group_ppv`; an omitted list permits every level. Comment modes are
`open`, `closed`, `hidden`. Tier names come from `tiers`; their products and
prices are defined in OpenRails. `channels.entitled: true` exposes
channel-membership standing, without defining a price or making every post paid. Changing the model does
not rewrite existing access rules or billing products.

ContentKit defines stable entitlement keys; OpenRails grants them without
interpreting their syntax:

| Key | Opens |
|---|---|
| `access.ContentKey` → `<tenant>:content:<id>` | legacy audit/migration only; grants no edition access |
| `access.VersionKey` → `<tenant>:content:<id>/<version>` | one version |
| `access.ChannelKey` → `<tenant>:group:<id>` | channel-owned content whose parent policy requires that membership |
| `access.TierKey` → `premium` | versions whose rule names that tier |

Use these existing keys in an OpenRails product's `entitlements`. A product may
grant several keys; its product key and its granted entitlements are separate
concepts. OpenRails's `required_entitlements` can require a channel membership
before a customer buys an individually gated item. Product/price edits report
OpenRails errors directly. They do not first save local terms for later sync.

For example, after creating a ContentKit item, its authoring flow can create a
native OpenRails product and price with the OpenRails Go client:

```go
key, err := access.VersionKey(tenant, contentID, versionID)
if err != nil { return err }
product, err := billingClient.CreateProduct(ctx, billing.CreateProductParams{
    Key: "edition-" + versionID, DisplayName: title,
    Entitlements: []string{key.String()},
})
if err != nil { return err }
_, err = billingClient.CreatePrice(ctx, billing.CreatePriceParams{
    ProductID: product.ID, Key: "standard", UnitAmount: 5_000_000, Currency: "USD",
})
return err
```

Here `billing` is `github.com/open-rails/openrails/billing`; amounts are in
millionths of a currency unit, so the example price is $5. The same calls work
through the embedded or remote OpenRails client. Price history, retries and
checkout are owned by OpenRails. A channel product uses `access.ChannelKey`
instead, while a Premium product grants the configured tier key.

`Rule` is stored on content and, for channel-free kinds, optional version
overrides. Channel posts share the overall item's policy. It is validated
against the model and projected onto `content_facets`. `Gate` reads grants
through `access.Billing.Held`; optional `access.TenureReader` supplies account
statistics. Access decisions expose `Sell` entitlement keys and `Requires` as
hints for the host's OpenRails checkout integration, never copied prices.

The host passes `Deps.Billing` (`ckopenrails.Billing(client)`); the gate is
the engine's own. Each request reads entitlements once. Verified token
claims can supply configured tiers; revocation then has the token's lifetime.
Billing read failures fail closed for protected content, while public content
remains readable. Truncated keyspace reads are settled with exact-key checks.
`GET /v1/me/library` lists purchased content using the same authoritative grants
and domain visibility boundary.

See [the ownership decision](docs/adr/0002-openrails-owns-commerce.md).

## Media

Media is an app-defined file system in one private bucket. The bucket is the only record of an item's files: each item has one manifest, rewritten in place, and every file is named by the SHA-256 of its bytes in the area that decides who may read it. PostgreSQL keeps only a cache of the manifests, rebuilt from the bucket on demand. The app declares its kinds in one registry (`media.Config`); HOST_INTEGRATION "Media" covers the registry, commit ops, reads and exposure.

```text
tenant/{tenant}/app/{app}/{kind}/{id}/manifest.json      the item's files; never served
                                     /internal/{sha256}  unreleased uploads and outputs; never served
                                     /private/{sha256}   released files, behind the item's token
                                     /public/{sha256}    covers, free pages; served to anyone
                                     /tmp/u-{uuid}       staged uploads; never served
tenant/{tenant}/app/{app}/{kind}/_default/               the kind's default images, an item like any other
```

For versioned content, `id` is the version ID. Its parent content ID exists only in the database and has no media folder. Covers, pages and body images belong to the version. Domain APIs still use `{kind, content, version}` for authorization and relationships. Other kinds use their own item ID. Shared account media uses `tenant/{tenant}/account/` instead of an app root. Media URLs add `/v1/` before the storage key.

`GET /v1/contents/{id}` reads the database-only parent and reports its selected default edition in `selected_version_id`. `GET /v1/versions/{vid}` reads a specific edition, returning the same parent detail with only that version. Shared entities remain in `Content.entities`; version-specific associations remain in `Version.entities`. Their effective set is the union, without copying shared memberships into each edition.

`Config.Media` is the registry (`media.Config`, without hooks) and how it is served; the engine builds the uploads, reads, cache, worker queue and jobs from it and `Deps.Store`. Every media item is ContentKit's: content kinds' through the catalog's resolver, entities', avatars' and, when the registry declares the `poll` kind, poll images.

The user-avatar kind comes from `github.com/open-rails/contentkit/media/avatar`; `avatar.User()` includes its upload rules and embedded default silhouette. An avatar's URL names its hash, so the API lists it beside what a person wrote (one cache read per response). `media-worker publish-defaults` renders every kind's default images into its `_default` item as part of deployment; reads and `GET /v1/media/presets` (`default_image`) show them for items without their own.

**Manifests.** An edit reads the manifest with its ETag, applies its change and writes it back with `If-Match` (`If-None-Match: *` to create). A `412` re-reads and re-applies, so every change is a re-appliable delta that checks the state it changes; a commit's operation id and request digest are kept for the latest 32 commits, so a retried commit applies once and an id reused for another batch is refused. Each write raises `rev` and names its `op`. Files an edit stops referencing go to the manifest's `retired` list with a due time: 7 days for a replaced file (the undo window), an hour for a release's leftovers, at once for withdrawn, taken-down or erased files. An edit refuses to reference bytes the bucket does not hold, and a hash whose DELETE may still be in flight (GC marks it `deleting` with a conditional write before deleting, one attempt, and the mark settles two minutes). Reads are served from an in-process cache (an LRU bounded by bytes) at the revision `content_media_items` records; the bucket is asked only for a revision the process lacks. Writes always read the bucket. A 2,000-page gallery is about 1.5 MB of JSON and 400 KB stored; a 2-hour video about 5 KB, its segment tables living in index blobs. Every read and write stops at `MaxManifestBytes` (16 MiB of JSON). An item holds at most `MaxUploads` (10,000) uploads, an upload's meta at most 4 KiB of JSON and the item's 16 KiB; names are capped at 200 bytes of JSON.

**The bucket must never have had versioning.** On a bucket that ever had versioning, Ceph RGW checks `If-Match` apart from the write, so two writers can both land. `s3.New` never dials; register `store.Check(ctx)` as the host's optional S3 dependency probe. The first write probes the bucket's versioning and conditional PUT, and media writes are refused (`ErrBucketVersioned`, `ErrConditionalPutRequired`) unless it never had versioning and honours both conditions. A failed or cut-off probe records nothing, its write is `media.ErrUnavailable` and the next use probes again. An unreachable or 5xx bucket is `media.ErrUnavailable`: 503 `unavailable` over HTTP, and in media jobs a River snooze (not an attempt) while a fresh `Check` confirms the outage, capped by `MaxOutageSnoozes`. Recovery from deletes is the manifests' `retired` grace, not bucket versions.

**Cache.** `content_media_items` holds one row per item: its revision and ETag, processing status and public images, written forward-only by `rev` after every manifest write. It drives processing events, version facts, release and garbage-collection wakes, and listings' images. `media-worker rebuild` refills it from the bucket; a dropped table rebuilt this way serves the same lists.

**Uploads** go straight to the bucket, to a staged name: up to 64 MiB is one PUT to `tmp/u-{uuid}` signed with its type, length and SHA-256, larger files are multipart (8–16 MiB parts, each signed with its length and SHA-256, resumed through `ListParts`, completed by the server from a signed ticket; nothing is stored). The browser's hash only finds identical bytes the item already holds (no upload). After the commit, the worker's place job hashes each staged upload and writes it under its hash: `internal/` for a version (until its release), `private/` for other kinds. The upload limiter rate-limits uploaders; there is no storage quota.

**Release is a move.** A version's uploads and outputs wait in `internal/`. Its release job (one per version, woken by every catalog change and manifest write, snoozed until the next chapter or audience deadline) changes nothing while the version's uploads still process (a release is all or nothing); otherwise it copies what is now released to `private/` (`public/` for a free version's pages and a locked version's preview pages), writes the manifest naming the copies, and retires the `internal/` copies. A crash anywhere re-runs from the manifest: copies are idempotent, and until the manifest switches readers see the old areas complete. Withdrawal is the same move back; a withdrawn file's served copy is deleted at once; putting it back before GC marked it takes it out of `retired`, and after that waits only until the delete has settled (two minutes). Areas are decided per file: pages with identical bytes in different chapters reference the same hash in different areas. Public preset outputs (covers) are always in `public/`; a `HostOnly` preset's stay in `internal/`. Originals never move: downloads read them in `internal/`.

**Garbage collection** (`contentkit_media_gc`, one River job per item) deletes only what the manifest retired: entries past their due time are first marked `deleting` (fencing re-reference), deleted, and dropped once the DELETE has settled. After a write it lists the item's folder once for orphans older than a day (a crashed writer's), which the manifest does not name. Nothing else lists the bucket.

**Media gateway** (`cmd/media-gateway`, `media/gateway`, image `ghcr.io/open-rails/contentkit-media-gateway:{tag}`) reads no manifest or database. It serves `public/` to anyone and `private/` with the item's token, carried by the `mt` cookie path-scoped to that item's `private/` (or `?t=` for applications); `internal/`, `tmp/` and the manifest are never served. Served names are hashes, so every answer is `max-age=31536000, immutable` (private ones add `CDN-Cache-Control: no-store`, and every answer `Referrer-Policy: no-referrer`). `?dl={name}` serves the file as a download under that name when it has the file's type. A soft-deleted or erased item's reserved `public/_contentkit_erased` fence refuses all of its files, checked with one HEAD per read. Everything else and every denial is one `no-store` 404. It keeps no state: rate limit the media host at the ingress; the read API limits the items a viewer opens per hour (`ReaderOptions.Issuance`). It needs `MEDIA_GATEWAY_S3_ENDPOINT`, a key reading only `*/private/*` and `*/public/*`, `MEDIA_GATEWAY_TOKEN_KEY` and `_TOKEN_KEY_PREVIOUS`, and `MEDIA_GATEWAY_ROUTES` (a JSON list of `{host, scope, bucket, origins}`, one per tenant account or app root a media host serves).

**The media worker** (`internal/media/worker`) runs every producer from the host's worker River schema (`MEDIA_WORKER_SCHEMA`, per host: hosts sharing a database never share one): images, public presets and editor views (libvips) and HLS, subtitles and frames (ffmpeg). The host presigns, commits and reads through the engine: hosts link no codecs and need no ffmpeg, so they build with `CGO_ENABLED=0` (CI checks every package but `internal/media/{image,video,worker}` and `cmd/media-worker`). The editor's poster picker decodes the encoded video in the browser. The stock build (`cmd/media-worker`, image `ghcr.io/open-rails/contentkit-media-worker`) needs only the registry as JSON (`MEDIA_KINDS_FILE`): an engine's is `Config.MediaRegistry()`, which `contentkit media-config -config contentkit.yaml` prints. Every host runs this image; `MigratePostgres` migrates its queue. Its subcommands are `publish-defaults` and `rebuild`.

It never exits for a missing dependency: `Run` takes no jobs until the bucket answers, and `MEDIA_METRICS_ADDR` serves `/livez`, `/readyz`, `/statusz` and `app_dependency_up` with /metrics. With `Config.Metrics`, the worker leading its River schema also exports the queue backlog (`media_video_jobs`, `media_video_tenant_backlog`, ...); hosts collect none.

**Images** (`media/image`, CGO over libvips): WebP at each `Image` spec (inside or cover box, quality), through the upload's edit (crop in EXIF-oriented source pixels, then a clockwise quarter rotation); nothing is upscaled: a cover box larger than its source renders the source's largest centred crop at the box's aspect. GIF and WebP animations keep every frame, delay and loop count; `MaxPixels` (100 MP over all frames, lowered per upload by `Upload.MaxPixels`; avatars 25 MP), `MaxFrames` (1000) and `MaxAnimationSeconds` (60) bound sources, every rendering is checked against the same pixel bound (frames × output area) before it decodes, and a stored source over its Upload's `MaxBytes` is refused unread. A JPEG or WebP still rendered small decodes at a fraction of its size. Uploads feeding image presets take only `media.ImageTypes` (JPEG, PNG, WebP, GIF, AVIF, HEIF, TIFF), the declared type binds the decoder, and every other libvips loader is blocked (libvips 8.13 or later). A source-format preset's unchanged copy drops the upload's metadata (EXIF, XMP, IPTC, comments, trailing data) without re-encoding, keeping its orientation and colour profile; `Image.KeepMetadata` keeps it, and is required for AVIF, HEIF and TIFF sources. Public presets render every width through the same spec (a width past the edited image at its width). Re-deriving unchanged outputs writes nothing: the same bytes have the same name.

A private image preset can instead use `Format: media.ImageFormatSource` to retain the upload's format. With no edit and an inside fit, files already inside the configured box keep their exact pixels, without their metadata unless `KeepMetadata` is set; zero dimensions leave that side unbounded. `Width: 6000, Height: 6000` proportionally limits the longest edge to 6,000 pixels without cropping or enlarging. Only an oversized or edited image is re-encoded. A reader kind's `Publish.Download` names such a `HostOnly` preset: the ZIP then carries a page's bounded rendition in place of an oversized original.

**Masters.** An upload has three roles in its manifest: the original (as uploaded, never modified), its `master` (what a release serves and downloads carry) and its renditions, which derive from the master and record its hash (`input`) beside their spec fingerprint. A kind's `master` recipe is a chain of host hooks (`media.MasterHook`: straightening, contrast, a GPU upscaler) run by the media worker on each image and video upload; each master records the recipe's hooks, versions, parameters and fingerprint. The default, the identity recipe, makes the original the master: the manifest names the original's hash and nothing else is stored, copied or moved. Only a hook that changes pixels stores a master of its own, lossless (WebP lossless, PNG past 16,383 px, FFV1 in Matroska), which a release moves to `private/` with its version. A failing hook keeps the upload's previous master and renditions, so the release is unchanged. `contentkit regenerate -kind K [-id ID]…` (`Engine.RegenerateMedia`) queues the worker over a kind's items after a deploy changed a recipe or a rendition's spec: masters whose recipe changed are made again, then renditions whose master or spec changed, each published as a manifest revision with what it replaced retired; current items write nothing, so it can be re-run.

**Downloads.** A kind's `Download` serves `pages` (the readers' default) or `original` (the players'), always the masters, read by the API where they are stored (the originals in `internal/` under the identity recipe). A pages download is a ZIP streamed in reading order: stored entries without timestamps, so its bytes depend only on its entries. Unwatermarked, the same revision is byte-identical on every request, with an exact `Content-Length`, an `ETag` of that revision and byte ranges, cached privately under the viewer's ticket. A ZIP the host's policy watermarks (`DownloadPolicy.Mark`) is marked and zipped anew for every request and is never cached: `Cache-Control: private, no-store` and `CDN-Cache-Control: no-store`, with no length, validator or ranges. Each transfer repeats current visibility, access, chapter-release and host-policy checks, and refuses stale tickets. An original download streams the video's master (its original, by default) as `{title}` plus its extension, with its length, ETag and ranges. Downloads are charged to the viewer's download quota (`media.downloads`).

Reader page identity is the SHA-256 digest of the original source image. `FileInfo.page_id` and reader-plan page `id` expose that digest; logical names are display/path metadata. The manifest's ordered source list determines page positions and ZIP entry order. Reader metadata and chapter starts refer to source hashes. Upload commits remain asynchronous: a staged file has no verified `page_id` until placement finishes. Wait for placement before saving reader metadata or chapter starts that reference that source; a filename is not a fallback ID.

Reader chapters accept `start_page` and derive their end from the next start, or the end of the version. Inserting or reordering unmarked pages does not require renumbering markers. Missing, duplicated or out-of-order starts require repair. Chapters are metadata: their boundaries, names and release dates move no files beyond the release's move of the pages they open. A locked version's free preview pages (its rule's `preview`, else its kind's) are released to `public/`, listed in the reader plan as plain, cacheable URLs; scheduled chapters never contribute preview pages before they release. Media reads accept up to 64 repeated `path` query parameters to sign the selected permitted files without changing their manifest order. The SDK batches larger selections and refreshes active grants before expiry.

**Video** (`media/video`): the `contentkit_media_derivatives` job on `media_derivatives` owns planning and conversion. It checkpoints bounded encoded ranges, yields between them so shorter videos can run, and resumes successful ranges after shutdown. An `HLS` preset encodes the ladder (rung = short side, default 2160/1080/480, none above the source) in each of the worker's codecs (`MEDIA_WORKER_CODECS`, default `av1,h264`), plus AAC per audio track, WebVTT per text subtitle and a seek sprite. An HDR source (PQ or HLG) keeps its range in 10-bit AV1 with its HDR10 metadata; its H.264 rungs, sprite and frames are tone-mapped to BT.709 SDR, and master playlists label each variant's `VIDEO-RANGE`. Each rendition is one byte-range fMP4 blob whose segment table is its own index blob; a compliant top rung is stream-copied, and sources outside the aspect bounds fail with `video_aspect_unsupported`. `Subtitles` converts SRT and SSA/ASS to clean WebVTT. Playlists are built per request by the read API, with the worker queue's encode progress. The original is kept as uploaded and is what a download serves.

Audio attachments are played as uploaded after verified placement, with no conversion job. A reader kind's `Publish.Playback` names its audio upload pattern, such as `audio/{name}`; page cues keep the same track names. The release moves declared audio with the pages. Audio tracks inside a movie still belong to its video conversion.

**Deletion and erasure.** Soft deletion records the deletion in the manifest and writes the gateway fence at once; files are kept, and restore removes the fence. Erasure writes the permanent fence, then an empty manifest whose files are all retired and due, and deletes them; GC deletes any late write. Staged uploads expire by bucket lifecycle (`s3.Store.Configure`: seven-day `tmp/` expiration and incomplete-multipart abort, preserving other applications' rules).

**Paywalls**: media gates each item through the engine's access rules. A version's `private/` holds only released files, so an item token opens what has been released until it expires; originals of readers' pages stay in `internal/` outside the ZIP download.

**Browser package** (`sdk/ui`, `@openrails/contentkit-ui` on npm, one version per release): the client (uploads, commit ops, reads, `waitFor`, frames, HLS), content URLs, React hooks and styled components. See [sdk/ui/README.md](sdk/ui/README.md).

## Entities

Entity kinds come from `kinds.Config.EntityKinds` (ordered, parent, scoped_by,
version_level, roles, attributes, popular). They describe content; channel ownership and
authority are configured separately with `channels` and a content kind's `channel`
policy. See [Posts and channels](docs/posts-and-channels.md) for fixed staff blogs,
creator feeds and reposts.

`popular: {content_kind}` enables `sort=popular`, refreshed hourly from recent
account favourites in Postgres. See [Entity popularity](docs/popularity-policy.md#entity-popularity).

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

Keyword documents include active associations and their active ancestors.
Writes refresh association facets in the same transaction. Entity counts and
automatic entity covers read current facets and release timestamps; no periodic
recount or timed-membership job is needed. Manual entity covers remain explicit.
Likes and favorites keep their existing atomic counters.

Entity writes record `entity.*` events on the catalog event log. Merging or
deleting an entity a host links to is allowed; hosts repair their links from
`GET /v1/manage/entities?ids=`, `GET /v1/manage/entities/match` and the events.
A deleted entity's avatar and cover are unavailable to everyone until it is
restored: after the write commits, the same request writes its item's deletion
fence and the gateway answers 404; no bytes move. A failure after the commit is
reported, and retrying the same write or import completes it. Purge makes the
fence permanent and erases the files.

The engine builds separate entity and channel interfaces from `Config.Content`.
Metadata operations use `/entities`; channel operations use `/channels`. Channels
retain their editor teams and optional billing entitlements. Their lifecycle
operations call AuthKit directly, report failures and support idempotent caller
retries. No entity/channel sync worker is registered. Physical deletion stays
with the content-purge sweep. Ordinary tags and series need no lifecycle work
beyond their media fence.
One primary channel governs the overall item and all of its versions; a repost
only references that original from another channel and grants no editing rights.

Channel access uses `access.ChannelLevel`, `access.ChannelPPV` and
`access.Rule.ChannelID`. Its existing JSON field `group_id`, access values
`group`/`group_ppv`, and opaque billing keys containing `:group:` keep their
encodings so saved policies, frozen plans and purchases retain their identities.
These are channel membership rules, not permission granted by descriptive tags.

## Content and versions

An overall item holds shared identity, its primary channel/editorial authority,
creators and inherited entity associations. Every edition owns its authored title,
summary, body/HTML, language, original title and media. An English gallery and a
Chinese edition can use completely different image bytes while inheriting the
same artist/series/tag associations. Each edition can add its own associations.

Create initial editions through `ContentInput.Versions` and edit their text/media
through the version interface. Omitting initial versions creates one empty
edition; adding another version starts empty. There is no automatic body-image
propagation or copy-version operation. Reader `Content` and discovery cards
project the selected edition's presentation, identified by `selected_version_id`;
that convenience does not put content back onto the parent or borrow a sibling's
text/files. See [import](docs/import.md) and [migration](docs/migration.md).


The catalog (`internal/catalogstore`) owns content items of the kinds in `Config.Content.Kinds`.
Writes are batches; each transaction keeps `content_facets` current (rules
through `access.Rule.Facet`, entities through `EffectiveEntities`), marks search
documents dirty, queues `content_events` and calls media (republish,
delete). Content edits do not rename or reprice billing products. Published with a
future `publish_at` is publication intent; readers see the completed release
after its deadline and required media preparation finish. A deleted item is
unavailable to everyone, buyers included: reads hide it and the media gateway
answers 404 for its files at once. Restoring reopens it to the same purchases;
otherwise its media is erased after `DeletedRetention`. OpenRails retains product and purchase identity after
content and media are gone; deleting content does not delete billing history. A
product can grant multiple entitlements, so content deletion does not implicitly
archive products or cancel subscriptions. Hosts explicitly retire affected
products in OpenRails before deleting their last purchasable target.

The `Viewer` is the one visibility rule: published content whose release time
has arrived, the domain's scope and the viewer's content filters. Every
published item can be listed; its edition's access rule controls reading.
It uses `access.Filter.SQL` for lists
and `Gate.Decide` for items; staff (`root:content:view-restricted`),
creators and primary-channel editors see everything. Discovery lists and searches
under it; media and interactions resolve through it. Held text goes to the
shared review queue, screened by `Deps.Moderator`. `GET /v1/catalog/totals`
(`kinds`, `languages`) counts what anyone and each tier opens, per kind and
language; a sibling deployment reads it with
`contentkit.NewClient(baseURL, nil).GetCatalogTotals(ctx, contentkit.GetCatalogTotalsQuery{Kinds, Languages})`.
Each process caches a count per site and kind: it reloads in the background
after ten minutes, and the last good count serves for up to a day while reloads
fail.

Content jobs use `contentkit_content_*` River kinds. Their previous
`contentkit_catalog_*` kinds remain dispatch aliases, and the queue route stays
`contentkit_catalog` so already queued jobs can drain. The Go package remains
`catalog`; OpenRails uses catalog for its products and prices.

During deployment, keep old workers away from newly named jobs: stop the old
fleet before starting the new one, or first deploy workers that recognize both
names while producers still use the old names. River fetches unknown kinds by
default, so overlapping old and new fleets can fail new jobs. Rollback has the
same constraint: drain newly named jobs or retain workers that recognize them.

[Content filters](docs/content-filters.md) form one app-scoped entity layer across
explicit `applies_to` content kinds. Domains define their offered toggles and
defaults; `require_entities` independently scopes hard requirements by kind.
One saved entity choice affects galleries and posts together. Public browsing,
including staff browsing, honors optional choices; authorized management keeps
its visibility. Fixed and required domain rules also constrain direct access.
`kinds.DecodeContentFilters` strictly reads an embedded app's policy block.

## Analytics and discovery

ClickHouse holds the activity log; PostgreSQL holds each reader's reading
state and history. Product pages read only PostgreSQL. See
[docs/analytics.md](docs/analytics.md).

- The browser reports `open` and `progress` (`POST /v1/activity`); the server
  logs reactions, favorites, hides and searches where they happen. A row's
  subject is the account, else the browser's anonymous id
  (`ContentKit-Anonymous-Id`).
- Each process buffers rows and writes them every second as one async INSERT.
- The analytics job (every 15 minutes) folds new rows into `reading_state` and
  `reading_history` and sign-in links into the account, rebuilds today's
  `content_daily` and `audience_daily` hourly, and deletes erased rows daily.
- History, continue reading, card progress, `view_status` and hidden
  galleries read `reading_state`; hiding (`PUT /v1/contents/{id}/hidden`) is
  immediate.
- `AnonymizeAccount` deletes the account's reading and fences its rows; see
  [account lifecycle](HOST_INTEGRATION.md#account-lifecycle-and-interaction-erasure).

## Testing

The replacement public-contract suite is in [tests/critical](tests/critical/README.md).
It uses real AuthKit, OpenRails, PostgreSQL, ClickHouse, S3, media workers and
the built browser SDK. Configure owned disposable services as documented there.

```sh
python3 tests/critical/run.py --race
```

The runner fails on missing fixtures, skipped or missing selected cases, zero
executions, or incomplete scenario/operation review. This draft replacement is
still incomplete; focused development runs are explicitly partial evidence.
Features merge into the open release candidate (`rc/<version>`, named in
[OPEN.md](OPEN.md)), the candidate merges into `master`, and a release is a tag
on `master`. CI runs on ready PRs only: light build, type and static checks into
a release candidate, every suite once for the candidate's PR into `master`.
Ordinary source pushes run nothing. See [docs/ci.md](docs/ci.md).

Previously observed backend capabilities (not replacement-suite qualification):

| Backend | Conditional PUT | SHA-256 enforced | Notes |
|---|---|---|---|
| MinIO RELEASE.2025-09-07 | yes | yes | drops `AbortIncompleteMultipartUpload` (expires uploads itself) |
| Ceph 19.2 / 20.2 standalone `dbstore` RGW | no | no | wrong Range bytes; not representative of RADOS-backed RGW |
| production Ceph RGW (RADOS, 2026-09-24) | no (`If-Match` yes, `If-None-Match: *` ignored) | no | Range, `response-content-disposition`, versioning, lifecycle (incl. `AbortIncompleteMultipartUpload`) and multipart work; hosts wire `PGLocker` |

AuthKit owns user inboxes and their UI. ContentKit publishes or withdraws native comment notices through its injected two-method publisher. See [notifications](docs/notifications.md) for source identities and publisher configuration.
