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
| `content_id` | the host-owned work: a gallery, a video, a listing |
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
| `access` | `Actor`, the `ContentResolver` port and its `Resolution{Ref, Visible, Accessible, PreviewLimit}`, shared by `content` and media |
| `media` | per-item folders and keys, kind registry, the `Store` port, manifests with conditional-write edits, direct uploads and their HTTP API, the optional `UploadLimiter`, sweep, folder deletion and processing as River jobs |
| `media/s3` | `Store` over aws-sdk-go-v2 (Ceph RGW in production, MinIO in tests), bucket policy and point-in-time `Restore` |
| `media/token` | media access tokens, shared by hosts and the access worker |
| `media/video` | ffmpeg encode jobs: byte-range fMP4 HLS ladder, AAC per audio track, WebVTT per text subtitle, sprite, per-quality MP4 downloads; River in schema `media_worker` (`cmd/media-worker`) |
| `media/tiered` | optional `public`/`members`/`ppv`/`members_ppv`/`premium` policy over an entitlement `Checker` (hosts adapt OpenRails `CheckEntitlements`) |
| `content` | posts, comments, reactions, favorites, polls (multiple-choice and free-text) and their counts over `ContentRef`, in the host schema's `content_*` interaction tables; the `Identity`/`Authorizer`/`UserEnricher`/`MediaStore`/`ContentProcessor` ports, the optional `ContentModerator` (held/review queue) and `AnswerClassifier` ports, and the HTTP routes |
| `search` | PGroonga keyword search (exact/alias/prefix/typo, EN/ZH/JA/KO), documents and dirty queue, RRF, the `DocumentSink` port |
| `worker` | one tenant's document maintenance: dirty queue, bounded backfill, sink delivery |
| `taxonomy` | generic catalog: nodes (tags, artists, creators, characters, series, seasons, voice actors), localized names/aliases, edges, content assignments, effective tags, per-language counts, typeahead documents, admin routes |
| `signal` | ClickHouse signal plane: canonical signals, compact subject state, daily rollups, windows, erasure fence, exposures/attribution, repair |
| `popularity` | named ranking policy (`PolicyV1`) over the window metrics: ClickHouse `RankExpr` and Go `Score` in agreement, literal windows, session scorer, taxonomy popularity through the host `Catalog` port |
| `discovery` | `SimilarTo`/`Recommend`: the `Candidates` port, the default co-engagement source (`Engagement`), `Fallback`, and the shared exclusion/fill policy (`Recommender`) |
| `eval` | lexical golden-case evaluation, reports, baselines |
| `migrations` | one PostgreSQL baseline and one ClickHouse baseline |
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

These are fresh-store baselines, not an in-place upgrade of old migration
chains; see [docs/migration.md](docs/migration.md).

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
| 404 | `not_found` | absent, unpublished or soft-deleted (existence is hidden) |
| 409 | `conflict` | state or revision conflict |
| 422 | `moderation_rejected` | a `ContentModerator` refused the write; `error` is the author-facing reason |
| 501 | `not_configured` | the host never wired the port this route needs (`MediaStore`, `AnswerClassifier`) |
| 500 | `tenant_mismatch` | a host port answered with another tenant's data |
| 500 | `internal_error` | anything else |

5xx bodies carry no cause: it goes to `Options.Logger` (`slog.Default()` when
unset) with the request method, path, status and duration. Postgres constraint
names, driver text and stack traces are logged, never served.

## Media

Design: [MEDIA-DESIGN.md](https://github.com/open-rails/tracker/blob/master/contentkit/MEDIA-DESIGN.md).
One private bucket; each item owns a folder the library keys:

```text
{tenant}/{kind}/{id}/manifest.json | manifests/{version}.json
                    /originals/{sha256-hex | u-uuid | slot}   never served
                    /blobs/{sha256-hex | u-uuid}              immutable derivatives
                    /public/{name}.webp                       public slots
```

```go
kinds, _ := media.NewRegistry(media.Kind{Name: "gallery", Versioned: true, Types: []string{"image/png"}, MaxBytes: 10 << 20})
store, _ := s3.New(s3.Config{Bucket: "media", Endpoint: rgw, PublicEndpoint: "https://s3.doujins.ai", UsePathStyle: true,
	AccessKeyID: id, SecretAccessKey: secret, Capabilities: caps}) // caps from media.Probe
manifests, _ := media.NewManifests(store, kinds, media.ManifestOptions{Locker: media.PGLocker(pool)})
_, _ = manifests.Edit(ctx, ref, func(m *media.Manifest) error { m.Files = append(m.Files, f); return nil })
```

`Edit` writes with `If-Match` (or `If-None-Match: *`) and retries on conflict;
without `Capabilities.ConditionalPut` it serializes on a Postgres advisory lock
instead. Reads are cached in process and revalidated by ETag. Presigned PUTs
bind `Content-Type`, `Content-Length` and `x-amz-checksum-sha256`.

**Uploads** go straight to the bucket (`media.Uploads`, served by
`media.UploadHandler`): the host's `UploadAuthorizer.CanUpload` (AuthKit) runs
at presign and commit, and the kind's types and size cap bind every presign.
Up to 64 MiB is one PUT to `originals/sha256-{hex}` signed with its type,
length and SHA-256; larger files are multipart to `originals/u-{uuid}` with
8–16 MiB parts, each signed with its length and SHA-256, resumed through
`ListParts` and completed by the server (a signed ticket carries the S3
UploadId; nothing is stored). Slot originals PUT to `originals/{slot}`.
Commit is one conditional manifest edit (`insert`, `replace`, `move`,
`rename`, `remove`) that HEAD-checks each new original, re-hashes it when the
store does not enforce checksums, and enqueues a `ProcessJob`.

The optional `UploadLimiter` (`media.NewPGLimiter` over the baseline's
`content_media_*` tables) rate-limits uploaders (files/hour, bytes/day → 429)
and reserves per-owner quota at presign (→ 413 `quota_exceeded`); commit
settles usage to the change in the manifest's originals, and expired
reservations lapse after a day. Exempt grants skip it.

The bucket needs CORS allowing `PUT` from the app origins with the
`Content-Type` and `x-amz-checksum-sha256` headers, and the
`AbortIncompleteMultipartUpload: 1 day` rule `Store.Configure` sets.

**Video** (`media/video`, run by `cmd/media-worker`) encodes each `video/*`
manifest file in one ffmpeg pass: H.264 High (CRF 22, preset fast, keyframes
every 4 s) at 2160/1440/1080/720/480 lines no taller than the source, AAC per
audio track, WebVTT per text subtitle and a 10×10 sprite. Each rendition and
audio track is one single-file fMP4 blob whose segments are
`[offset, length, seconds]` (the init segment is `[0, segments[0].offset)`);
each quality also gets a muxed MP4 in `downloads["{file}-{height}p"]` (video,
every audio track, subtitles). Blobs are written first; one manifest edit then
records `hls` and `downloads` only if the file still derives from the encoded
original, so a replaced file keeps its previous `hls` until then. Outputs are
byte-identical on retry. Jobs live in River schema `media_worker` in the host
database: hosts run `video.Migrate` and enqueue through `video.NewEnqueuer`
(insert-only; register `enqueuer.Processor()` with `media.Jobs.AddProcessor`); the worker's environment is
documented in `cmd/media-worker`.

Tokens are `kid.exp.base64url(HMAC-SHA256(secret, "{scope}|{exp}"))`: a scope
is a folder (`…/blobs/`, covering the objects directly under it), one key, or
`{key}#dl={name}` for a download name. Expiry is window-aligned (default 4 h);
`token.Ring` verifies the current and previous key.

Media's River jobs compose into the host client through `helpers/river`:

```go
jobs, _ := media.NewJobs(media.JobsConfig{Store: store, Kinds: kinds, Tenants: []string{"d"}, Limiter: limiter})
manifests, _ := media.NewManifests(store, kinds, media.ManifestOptions{Jobs: jobs}) // edits schedule a sweep
uploads, _ := media.NewUploads(media.UploadOptions{ /* … */ Queue: jobs})       // commits enqueue processing
client, _ := riverhelpers.New(ctx, pool, &river.Config{Schema: "public"}, runtime.RiverJobs(), jobs.RiverJobs())
_ = jobs.DeleteItemsTx(ctx, tx, media.Deletion{Ref: ref, Owner: owner})  // in the host's delete transaction
_ = jobs.EraseUserTx(ctx, tx, "d", userID, deletions...)                  // the user's items plus user/{id}/
```

- **Sweep** (per folder, 24 h after each edit and in a daily pass over
  `Tenants`): deletes `blobs/` and hash-named `originals/` no manifest in the
  folder references, only once every manifest and the object itself are older
  than `Grace` (24 h; plus 1 day for `u-` multipart objects, which may be
  dated at initiation). Slot originals, `public/` and manifests are never swept.
  Invariant: it deletes only objects no manifest references and no in-flight
  commit can newly reference. Presign reuses an existing original, and a
  commit accepts one, only while a manifest references it or it is well
  before the sweep's cutoff (grace/2 for presign; grace/4, at most 1 h, for
  commit); otherwise the client uploads it again. Set `UploadOptions.Grace`
  to the same grace (taken from `Queue` when it is the `*Jobs`).
- **Deletion** removes the whole folder, manifests first, then again after
  `LateUploadWindow` (25 h) for PUTs and multipart completions that land late.
  With a `Limiter`, the owner's quota (the manifests' `OriginalBytes`) is
  released once.
- Processing: `jobs.Enqueue` (the uploads' `ProcessQueue`) runs one pending
  job per ref and slot through every `AddProcessor` processor; a commit
  that lands during the run (its Enqueue absorbed) makes the job rerun them.
- Media packages add workers with `jobs.Register(func(*river.Config) error)`
  before composition and enqueue with `jobs.Insert`/`InsertTx`.
- Restore: [docs/restore.md](docs/restore.md#media).

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
`media/internal/s3test`). CI runs them on MinIO. To record a Ceph RGW
release's capabilities, point the same variables at an RGW bucket and run
`go test ./media/... -v -count=1`; the log prints the probed capabilities.
`media/video` tests also need `ffmpeg` and `ffprobe` on `PATH` (they skip
without them unless `CONTENTKIT_TEST_FFMPEG=1`).

| Backend | Conditional PUT | SHA-256 enforced | Notes |
|---|---|---|---|
| MinIO RELEASE.2025-09-07 | yes | yes | drops `AbortIncompleteMultipartUpload` (expires uploads itself) |
| Ceph 19.2 / 20.2 standalone `dbstore` RGW | no | no | wrong Range bytes; not representative of RADOS-backed RGW |
| production RGW (RADOS) | not yet measured | not yet measured | run the suite against the dev cluster |

Tests run against real PGroonga Postgres and ClickHouse+Keeper and skip
without the variables; `CONTENTKIT_PROFILE_URL` needs `CREATEDB`. Regenerate the eval baseline with
`CONTENTKIT_EVAL_UPDATE=1`.
