# contentkit

`contentkit` is the deterministic content library for the Doujins, Hentai0
and marketplace hosts: tenant-scoped **interactions** (posts, comments,
reactions, favorites, polls), **keyword search** over host content, the
ClickHouse **signal plane** (consumption, feedback, exposures, popularity,
erasure) and the **discovery reads** over both. It needs no model provider,
API key or vector extension. Semantic search belongs entirely to the separate, deferred User Intelligence
library; ContentKit has no semantic search configuration or runtime hook.

Design: [open-rails-tracker/contentkit/DESIGN.md](https://github.com/open-rails/tracker/blob/master/contentkit/DESIGN.md).
Host contract: [HOST_INTEGRATION.md](HOST_INTEGRATION.md). Migrations:
[docs/migration.md](docs/migration.md), [docs/restore.md](docs/restore.md).

## Release policy

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
| `taxonomy_id` | a generic ContentKit record: tag, artist, series, creator, character, voice actor |
| `ContentRef` | `{TenantID, ContentKind, ContentID, ContentVersionID *string}` — the typed reference every API, key, index and cursor carries |

A language is metadata on a document or version, never part of a reference.
Every read is scoped to the tenant pinned at construction; a reference of
another tenant is an error, never remapped.

## Packages

| Package | Owns |
|---|---|
| `contentref` | `ContentRef`, `ContentKey`, `TaxonomyID` |
| `contenturl` | uniform content URLs `[/{lang}]/{route}/{CODE}[/{slug}]`: permanent 9-character codes per tenant, slugs, merges, legacy aliases, canonical redirects, a JSON lookup; the browser half is `sdk/urls` ([Content URLs](HOST_INTEGRATION.md#content-urls-contenturl)) |
| `access` | `Actor`, the batch `ContentResolver` port (`Resolve(ctx, refs, actor) → map[ContentKey]Resolution`; an omitted ref denies) and its `Resolution{Ref, Visible, Accessible, Editor}`, shared by `content` and media; paywalls: the entitlement key grammar, item rules, the `Gate`, its listing `Filter` (with a SQL predicate) and the request memo over the `Entitlements` port ([Paywalls and listings](#paywalls-and-listings-access)) |
| `access/accesstest` | an in-memory `Entitlements` and `CountingEntitlements`, for "one billing read per page" tests |
| `media` | the registry (`Config`, kinds, upload paths, private and public presets), ordered manifests with provenance and conditional-write edits, the `Store` port, direct uploads and commit ops with their HTTP API, reads and HLS playlists, the optional `UploadLimiter`, and the sweep, deletion, `Expose` and relays as River jobs |
| `media/s3` | `Store` over aws-sdk-go-v2 (Ceph RGW in production, MinIO in tests), bucket policy and point-in-time `Restore` |
| `media/image` | libvips (CGO) producer: Image presets, public presets, zips, editor views, `PublishDefaults` |
| `media/token` | media access tokens, shared by hosts and the media gateway |
| `media/layout` | object keys and the media gateway's host and default rules, dependency-free |
| `media/gateway` | the media gateway's handler (`cmd/media-gateway`) |
| `media/video` | ffmpeg producers: byte-range fMP4 HLS ladders with audio, subtitle and sprite tracks, MP4 per rung, audio, subtitles, frame grabs; `Frames` for the frame picker |
| `media/worker` | the media worker: one process for every producer, built from the host's registry (`cmd/media-worker` is the stock build) |
| `media/workqueue` | the host's side of the worker: its per-host River schema, insert-only `Queue` (enqueue, cancel), encode progress |
| `content` | posts, comments, reactions, favorites, polls (multiple-choice and free-text) and their counts over `ContentRef`, in the host schema's `content_*` interaction tables; the `Identity`/`Authorizer`/`UserEnricher`/`ContentProcessor` ports, post and poll images through `Media`, the optional `ContentModerator` (held/review queue) and `AnswerClassifier` ports, per-user interaction limits, owner and global comment bans, and the HTTP routes |
| `search` | PGroonga keyword search (exact/alias/prefix/typo, EN/ZH/JA/KO), documents and dirty queue, RRF, the `DocumentSink` port |
| `worker` | one tenant's document maintenance: dirty queue, bounded backfill, sink delivery |
| `taxonomy` | generic catalog: nodes (tags, artists, creators, characters, series, seasons, voice actors), localized names/aliases, edges, content assignments, effective tags, per-language counts, typeahead documents, admin routes |
| `signal` | ClickHouse signal plane: canonical signals, compact subject state, daily rollups, windows, erasure fence, exposures/attribution, repair |
| `popularity` | named ranking policy (`PolicyV1`) over the window metrics: ClickHouse `RankExpr` and Go `Score` in agreement, literal windows, session scorer, taxonomy popularity through the host `Catalog` port |
| `discovery` | `SimilarTo`/`Recommend`: the `Candidates` port, the default co-engagement source (`Engagement`), `Fallback`, and the shared exclusion/fill policy (`Recommender`) |
| `eval` | lexical golden-case evaluation, reports, baselines |
| `migrations` | PostgreSQL migration chain and ClickHouse baseline |
| `adapters/authkit` | its own module (opt-in): account avatars and content authors from AuthKit; see HOST_INTEGRATION "Account avatars" |
| `adapters/openrails` | its own module (opt-in): `access.Entitlements` over OpenRails `CheckEntitlements` (one request per read) |
| root | `Runtime` (one constructor: hub + content + HTTP mount), `Migrate` (all PostgreSQL features and optional ClickHouse signals), `Client` (keyword search + typeahead), `EmbeddedHub` (signal + discovery) |

## Install

One call installs all PostgreSQL features in a host-selected schema (which
may also hold application tables) and the signal plane in ClickHouse. PGroonga
and pg_trgm live in public; no vector extension is required:

```go
_ = signal.CreateDatabase(ctx, adminCH, "hub", cluster)
_ = contentkit.Migrate(ctx, contentkit.MigrateConfig{
	DB: sqlDB, Schema: "doujins",
	ClickHouse: &chmigrate.Config{ClientAddr: addr, Database: "hub", App: "contentkit_signal", Cluster: cluster},
})
```

The baselines initialize fresh stores; newer PostgreSQL migrations upgrade the
current lineage, not retired migration chains. See [docs/migration.md](docs/migration.md).

## Runtime

```go
rt, _ := contentkit.NewRuntime(ctx, contentkit.RuntimeConfig{
	EmbeddedConfig: contentkit.EmbeddedConfig{PG: pool, PGSchema: "doujins", Tenant: "doujins", CH: ch, CHDatabase: "hub"},
	Content: content.Options{Schema: "doujins", Identity: identity, Authz: authz, Resolver: resolver, ContentKinds: []string{"gallery", "post"}},
})
mux.Handle("/api/social/", http.StripPrefix("/api/social", rt.Handler()))
counts, _ := rt.Content.Counts(ctx, []contentkit.ContentRef{rt.Content.Ref("gallery", "42")})
_ = worker.SyncOnce(ctx, rt.WorkerOptions(hostWorkerOptions)) // host documents + posts
```

`rt` is the `Hub` (search, typeahead, signals, discovery) plus `rt.Content`
(interactions). See [HOST_INTEGRATION.md](HOST_INTEGRATION.md).

## Search

Documents are one per content reference and language: a gallery with an
English original, an English colored edition and a Spanish original is three
documents keyed by version. Hosts mark changes in the transaction that changes
content and run one worker tick per schedule:

```go
_ = search.MarkDirty(ctx, tx, schema, []search.DirtyMark{{DocumentKey: search.DocumentKey{ContentRef: ref, Language: "en"}}})

_ = worker.SyncOnce(ctx, worker.Options{
	Pool: pool, Schema: schema, Tenant: "doujins",
	SupportedLanguages: []string{"en", "es"}, ContentKinds: []string{"gallery"},
	ListContent:           listGalleries,          // bounded pages of refs, for backfill
	BuildKeywordDocuments: buildGalleryDocuments,  // refs -> KeywordDocument{Title, Aliases, Keywords}
	Sink:                  nil,                    // optional DocumentSink
})
```

The worker records rejected host documents in `content_search_invalid` with
their dirty-queue revision, validation error and failure time. It continues
with other documents and backfill, but does not acknowledge the rejected row.
Inspect that table for repair; marking the document dirty after correcting its
source advances the revision and retries it. A deletion clears the invalid
record. Run the PostgreSQL migrations before starting a worker built against
this schema.

Querying groups documents per work before paging; the host's eligibility join
decides, per document, whether that one row is visible and which is preferred:

```go
client, _ := contentkit.NewClient(contentkit.ClientConfig{Pool: pool, Schema: schema, Tenant: "doujins"})
page, _ := client.Search(ctx, query, contentkit.SearchOptions{
	Language: "es", ContentKinds: []string{"gallery"}, Limit: 20,
	Eligibility: &contentkit.Eligibility{SQL: eligibilitySQL, Args: args},
})
for _, hit := range page.Hits { /* hit.ContentID (work), hit.Version(), hit.Language, hit.Score */ }
```

`SearchHit.Score` is the keyword match tier (exact title 1, alias 0.9,
token/prefix 0.75–0.77, typo 0.5–0.52). `SearchWithTrace` returns retrieval
provenance for evaluation. Query limits: 256 characters, 16 tokens, a
two-second ceiling per request.

## Ports

| Port | Called when | Contract |
|---|---|---|
| `DocumentSink` | the worker publishes or deletes a document | `Upsert(PublishedDocument)`, `Delete(DocumentKey, Version)`; at-least-once, atomic newer-version wins across both operations, with a retained deletion tombstone; a failing sink keeps the row queued and never blocks the keyword index |

`DocumentSink` is a neutral document change feed for external indexes, caches
or audit consumers. ContentKit ships no sink implementation or AI-specific
configuration.

## Errors

Both mounted handlers (`content.Runtime.Handler`, `taxonomy.Handler`) answer
failures with one flat body. Branch on `code`; `error` is a human message and
may change.

```json
{"error":"not found","code":"not_found"}
```

| Status | Code | Meaning |
|---|---|---|
| 400 | `invalid_request` | malformed or semantically invalid input |
| 401 | `unauthorized` | no identity |
| 403 | `forbidden` | identity present, not permitted |
| 403 | `comment_banned` | a comment ban applies; `ban` is `{scope, reason, until}` |
| 404 | `not_found` | absent, unpublished or soft-deleted (existence is hidden) |
| 409 | `conflict` | state or revision conflict |
| 422 | `moderation_rejected` | a `ContentModerator` refused the write; `error` is the author-facing reason |
| 429 | `rate_limited` | an interaction limit; `Retry-After` header, `action` and `retry_after` (seconds) in the body |
| 501 | `not_configured` | the host never wired the port this route needs (`Media`, `AnswerClassifier`) |
| 500 | `tenant_mismatch` | a host port answered with another tenant's data |
| 500 | `internal_error` | anything else |

5xx bodies carry no cause: it goes to `Options.Logger` (`slog.Default()` when
unset) with the request method, path, status and duration. Postgres constraint
names, driver text and stack traces are logged, never served.

## Paywalls and listings (`access`)

ContentKit gives entitlement keys meaning; OpenRails grants them and never
parses one. Keys are constructed, never typed:

| Key | Grants |
|---|---|
| `access.Own(ref)` → `content:<tenant>:<kind>:<id>` | one work (bought, rented, bundled, granted), at every level |
| `access.Members(scope)` → `members:<tenant>:<kind>:<id>` | membership of a scope: a channel, series, collection |
| `access.Tier("premium")` → `premium` | a merchant-wide tier, interpreted only when declared in `GateConfig.Tiers` |

Ids are content ids, keys are work-level (a version shares its work's key),
tenants and kinds match `[a-z0-9_-]{1,64}`, and `ParseKey` refuses anything
else. An item's `Rule` is a host fact: `Public`, `TierLevel` (any of
`Tiers`), `MembersLevel` (`Scope`), `PPV`, or `MembersPPV` (owned; buying it
requires the membership).

```go
gate, _ := access.NewGate(access.GateConfig{Entitlements: ckopenrails.Entitlements(bill), Tenant: "onlydemo",
	Tiers: []string{"premium"}, MemberKinds: []string{"channel"}, OwnedKinds: []string{"post"}})

f, err := gate.Filter(ctx, actor) // one billing read: the tiers and the keys held per keyspace
pred, args, _ := f.SQL(postColumns) // hide mode: access inside the listing query, so pages are full
rows, _ := pool.Query(ctx, `SELECT `+cols+` FROM posts p WHERE p.state = 'published'
	AND (p.published_at, p.id) < (@cursor_at, @cursor_id) AND `+pred+`
	ORDER BY p.published_at DESC, p.id DESC LIMIT @n`, pgx.NamedArgs(args))
keep, short, _ := gate.Settle(ctx, actor, f, items) // free unless a keyspace was truncated
ok, known := f.Allows(item)                         // lock mode: pure, no I/O
ds, _ := gate.Decide(ctx, actor, items)             // paywalls: Allowed, Sell, Requires
reg, _ := media.NewRegistry(media.Config{ /* … */ Hooks: media.Hooks{Resolver: gate.Resolver(facts)}})
```

- `access.WithMemo(ctx)` keeps the viewer's `Filter` and the keys read for
  the request, so `Filter` then `Decide` (or a resolver) is one read. The
  content and media handlers install it; install it in host middleware too.
- `Decide` and `Settle` take a batch; there is no single-item check.
- A failed read fails closed per item: `Filter.Unknown()` (its SQL admits
  public rows only), `Decision.Unknown` (denied, nothing offered), and an
  error wrapping `access.ErrUnavailable`. Public content keeps serving.
- `GateConfig.Claims` (optional) answers a declared tier present in the
  verified token without a read; a refund then lags by the token TTL.
- `accesstest.CountingEntitlements` lets host tests assert one read per page.

| Operation | Billing reads |
|---|---|
| Listing page, lock or hide mode | 1 (`Filter`) |
| Hydration, item pages, comments in the same request | 0 (memo; items of declared kinds) |
| Media item read (token mint) | 1 (the item's own, membership and tier keys) |
| Media files, HLS segments | 0 (the token covers them) |
| Viewer holding more than `HeldLimit` keys in a keyspace (hide mode) | +1 per page (`Settle`) |
| Anonymous viewer | 0 |

Embedded OpenRails answers a read with two index-backed statements on the
host's Postgres; standalone adds one HTTPS round trip. HOST_INTEGRATION
[Listing with access](HOST_INTEGRATION.md#listing-with-access) covers columns,
indexes, UNION listings and truncation.

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
{namespace}/{kind}/_default/public/{name}     a public preset's default image
```

An item is the host's version (`gallery/456` English, `gallery/789` Korean).
Private blobs are content-addressed and immutable; public names are fixed and
overwritten in place (ETags and a CDN purge keep them fresh). Every URL is
`https://media.<site>/v1/{namespace}/{kind}/{id}/{public|private}/{name}`.

```go
reg, _ := media.NewRegistry(media.Config{Namespace: "doujins", BaseURL: "https://media.doujins.ai",
	Kinds: []media.Kind{Gallery, accountmedia.User},
	Hooks: media.Hooks{Resolver: resolver, CanUpload: authorizer, PurgePublic: purge, ItemReady: ready}})
store, _ := s3.New(s3.Config{Bucket: "media", Endpoint: rgw, PublicEndpoint: "https://s3.doujins.ai", UsePathStyle: true,
	AccessKeyID: id, SecretAccessKey: secret})
key, _ := token.ParseKey(os.Getenv("MEDIA_TOKEN_KEY")) // "{kid}:{base64}", shared with media-gateway
_ = workqueue.Migrate(ctx, pool, "doujins_media_worker") // this host's worker schema
queue, _ := workqueue.New(pool, reg, "doujins_media_worker")
jobs, _ := media.NewJobs(media.JobsConfig{Store: store, Registry: reg, Locker: media.PGLocker(pool), Pool: pool,
	Processes: queue, Limiter: limiter})
uploads, _ := media.NewUploads(media.UploadOptions{Store: store, Manifests: jobs.Manifests(), Tickets: &ring,
	Limiter: limiter, Queue: queue, Frames: frames})
reader, _ := media.NewReader(media.ReaderOptions{Manifests: jobs.Manifests(), Queue: queue, Progress: progress,
	Delivery: media.Delivery{Mode: media.DeliverCookie, CookieDomain: "doujins.ai", SigningKey: key}})
mux.Handle("/api/media/upload/", http.StripPrefix("/api/media/upload", media.UploadHandler(uploads, media.UploadHandlerOptions{Actor: actorOf})))
mux.Handle("/api/media/", http.StripPrefix("/api/media", reader.Handler(media.HandlerOptions{Identity: identity})))
// composed into the host's River client: jobs.RiverJobs()
_ = jobs.ExposeTx(ctx, tx, ref)                                          // whenever anonymous visibility changes
_ = jobs.DeleteItemsTx(ctx, tx, media.Deletion{Ref: ref, Owner: owner}) // in the host's delete transaction
```

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
`store.Check(ctx, prefix)` as the host's optional S3 dependency probe. Its
first success probes the backend's capabilities unless `Config.Capabilities`
declares them; a throttled or cut-off probe records nothing. An unreachable
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
token in `?t=` or the `mt` cookie (`private, immutable`). A token opens every
private file of its item or none; `?dl={name}` serves the file as a download
under that name when it has the file's type. Everything else and every denial
is one `no-store` 404. It keeps no state: rate limit the media host at the
ingress; the read API limits the items a viewer opens per hour
(`ReaderOptions.Issuance`). It needs `MEDIA_GATEWAY_S3_ENDPOINT`, `_S3_BUCKET`, a key
reading only `*/private/*` and `*/public/*`, `MEDIA_GATEWAY_TOKEN_KEY` and
`_TOKEN_KEY_PREVIOUS`, `MEDIA_GATEWAY_HOSTS`
(`media.doujins.ai=doujins,accounts; media.hanime.media=hentai0,accounts`),
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
needs only the registry as JSON (`MEDIA_KINDS_FILE`). Hosts with a
`Private.Choose` build their own:

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

**Video** (`media/video`): an `HLS` preset encodes the ladder (rung = short
side, default 2160/1080/480, none above the source) in each of the worker's
codecs (`MEDIA_WORKER_CODECS`, default `av1,h264`), plus AAC per audio
track, WebVTT per text subtitle and a seek sprite. Each rendition is one
byte-range fMP4 blob whose segment table is its own index blob; stages are
published rung by rung (the upload stays pending until the last), a
compliant top rung is stream-copied, and sources outside the aspect bounds
fail. An `MP4` preset muxes H.264 at one rung with the default audio;
`Audio` gives an HLS track and an M4A (optional EBU R128 loudness);
`Subtitles` converts SRT and SSA/ASS to clean WebVTT. Playlists are built per
request by the read API. Encode progress comes from
`workqueue.NewProgressSource` (`ReaderOptions.Progress`).

**Jobs** (`media.Jobs`, composed into the host's River client): the sweep
collects garbage by manifest reference (unreferenced private blobs a grace
period old, unexpected public names at once, `temp/` by age); folder
deletion (with a late-upload second pass and quota release); `Expose`;
`Regenerate`; `SweepOrphans`; and the worker's relays.

**Paywalls**: `Hooks.Resolver` is `gate.Resolver(facts)` (see
[Paywalls and listings](#paywalls-and-listings-access)); one item read is one
billing read, and its token covers every file and segment.

**Browser SDK** (`sdk/upload`, `@openrails/contentkit-upload`, attached to
each release): hashing, uploads, commit ops, reads, `waitFor`, the frame
picker, `publicURL`/`srcSet`, React hooks and UI. See
[sdk/upload/README.md](sdk/upload/README.md).

## Taxonomy

Nodes, names, edges and assignments are tenant-scoped; effective tags are the
work's assignments ∪ the selected version's; `RequireAll` makes a multi-node
filter hold on one eligible version inside the same join as search. The PostgreSQL baseline always installs the taxonomy tables; see
[docs/taxonomy-migration.md](docs/taxonomy-migration.md).

```go
store, _ := taxonomy.New(taxonomy.Options{Pool: pool, Schema: schema, Tenant: "doujins",
	Kinds: []string{"tag", "artist", "character", "series", "voice_actor"}, Languages: []string{"en", "es"},
	CountEligibility: &search.Eligibility{SQL: releasedVersionSQL}})
_ = store.WithTx(tx).Assign(ctx, []taxonomy.Assignment{{ContentRef: g1.WithVersion(v2), TaxonomyID: "colored"}}, taxonomy.AssignOptions{})
tags, _ := store.EffectiveTags(ctx, []contentkit.ContentRef{g1.WithVersion(v2)})
filter, args, _ := taxonomy.RequireAll(schema, []taxonomy.TaxonomyID{"colored"})
page, _ := client.Search(ctx, q, contentkit.SearchOptions{Language: "es", ContentKinds: []string{"gallery"}, FilterSQL: filter, FilterArgs: args, Eligibility: elig})
mux.Handle("/admin/taxonomy/", http.StripPrefix("/admin/taxonomy", taxonomy.Handler(store)))
```

`ListNodes` backs a catalog index page directly: the display name in the
request language (falling back to the store's configured order, or pinned with
`LanguageMode`), the per-language content count, an A-Z index, a name+alias
search, hide-empty, five orders and offset paging with a total. The zero
`ListOptions` keeps the keyset contract a full admin sync wants.

```go
page, _ := store.ListNodes(ctx, taxonomy.ListOptions{Kind: "artist", Language: "es",
	ContentKind: "gallery", MinCount: 1, Sort: taxonomy.SortCount, Offset: 40, Limit: 20})
// page.Total, and per row: Name, NameLanguage, Count.
letter, _ := store.ListNodes(ctx, taxonomy.ListOptions{Kind: "artist", Language: "es", NamePrefix: "a"})
cast, _ := store.ListNodes(ctx, taxonomy.ListOptions{Kind: "character", Related: "s-fate", Relation: taxonomy.RelationMemberOf})
```

Worker: `ContentKinds: append(hostKinds, store.Kinds()...)`, `ListContent:
store.Lister(listGalleries)`, `BuildKeywordDocuments: store.Builder(buildGalleryDocuments)`.

## Signal plane and discovery

```go
hub, _ := contentkit.NewEmbedded(contentkit.EmbeddedConfig{
	PG: pool, PGSchema: schema, CH: ch, CHDatabase: "hub", Tenant: "doujins",
	Scorers:  map[string]signal.Scorer{"gallery": galleryScorer},
	Catalogs: map[string]contentkit.ContentCatalog{"gallery": galleryCatalog},
})
g1 := hub.Content("gallery", "g1")
_ = hub.RecordSignals(ctx, []signal.Signal{{ContentRef: g1, Subject: user, Type: signal.TypeView, EventID: sessionID, Revision: checkpoint, OccurredAt: start, Progress: 95, ProgressMax: 100}})
states, _ := hub.States(ctx, user, []contentkit.ContentRef{g1, g1.WithVersion("v2")})
top, _ := hub.Popular(ctx, "gallery", signal.PopularOptions{Window: signal.LastDays(30, time.Now())})
```

- Identity is `(tenant, content ref, subject, type, event id)`; retries,
  reordering and revisions converge, nothing is incremented. Record the work
  view and, separately, the selected version's view (`g1.WithVersion(v)`).
- Work reads (`History`, `Popular`, `SeenIDs`, co-engagement) count the work
  once across versions; `States` and `Metrics` read exactly the references
  given, work or version.
- Windows are whole UTC days, 7/30/90/365/all, no decay.
- Rank by a named policy, not the default rank: `popularity.ByName("v1")`,
  `popularity.New(popularity.Config{Source: hub, Policy: policy})`, then
  `ranker.Popular` / `ranker.Scores` / `ranker.Taxonomy`
  ([docs/popularity-policy.md](docs/popularity-policy.md)).
- `SimilarTo` and `Recommend` draw candidates from `EmbeddedConfig.Candidates`
  (default: co-engagement); see
  [discovery candidates](HOST_INTEGRATION.md#discovery-candidates).
- `EraseSubjects` is account erasure with a quorum-written fence; see
  [HOST_INTEGRATION.md](HOST_INTEGRATION.md#subject-erasure-completion-contract)
  and, for interaction data, [interaction erasure](HOST_INTEGRATION.md#interaction-erasure).
- Reactions and favorites reach the signal plane from their own revisioned
  rows: schedule `rt.SyncPreferences` (watermark with a commit overlap) and
  `rt.ResyncPreferences` (full re-send); see
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
