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
| `access` | `Actor`, the batch `ContentResolver` port (`Resolve(ctx, refs, actor) → map[ContentKey]Resolution`; an omitted ref denies) and its `Resolution{Ref, Visible, Accessible, PreviewLimit, Editor}`, shared by `content` and media |
| `media` | per-item folders and keys, kind registry, the `Store` port, manifests with conditional-write edits, direct uploads and their HTTP API, the optional `UploadLimiter`, sweep, folder deletion and processing as River jobs |
| `media/s3` | `Store` over aws-sdk-go-v2 (Ceph RGW in production, MinIO in tests), bucket policy and point-in-time `Restore` |
| `media/image` | libvips (CGO) processor: WebP variants, public slots, zip downloads |
| `media/token` | media access tokens, shared by hosts and the access worker |
| `media/video` | ffmpeg encode jobs: byte-range fMP4 HLS ladder, AAC per audio track, WebVTT per text subtitle, sprite, per-quality MP4 downloads, poster frames and hover previews; `Frames` for the poster picker; River in schema `media_worker` (`cmd/media-worker`) |
| `media/tiered` | optional `public`/`members`/`ppv`/`members_ppv`/`premium` policy over an entitlement `Checker` (hosts adapt OpenRails `CheckEntitlements`) |
| `content` | posts, comments, reactions, favorites, polls (multiple-choice and free-text) and their counts over `ContentRef`, in the host schema's `content_*` interaction tables; the `Identity`/`Authorizer`/`UserEnricher`/`ContentProcessor` ports, post and poll images through `Media`, the optional `ContentModerator` (held/review queue) and `AnswerClassifier` ports, and the HTTP routes |
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
| 501 | `not_configured` | the host never wired the port this route needs (`Media`, `AnswerClassifier`) |
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
                    /originals/{sha256-hex | slot | slot.json | i-uuid}            never served
                    /staging/u-{uuid}                                              multipart uploads until placed; never served
                    /blobs/sha256-{hex}                                            immutable derivatives (viewer token)
                    /editor/{sha256-hex | poster_w.webp | hover_preview_w.mp4}     editor-only (editor token)
                    /public/{slot_width | i-uuid}.webp | hover_preview_w.mp4       slots, inline images, published video images (rewritten in place)
```

Host wiring (one tenant; errors elided):

```go
kinds, _ := media.NewRegistry(
	media.Kind{Name: "gallery", Versioned: true, Types: []string{"image/png", "image/jpeg"}, MaxBytes: 10 << 20,
		Specs: map[string]media.Spec{"thumb": {Width: 460, Height: 650, Fit: media.FitCover, Quality: 80}, "high": {Quality: 90}},
		Slots: map[string]media.Slot{"cover": {Aspect: media.Ratio("46:65"), Widths: []int{230, 460, 920}}},
		Zip:   "high"},
	media.Kind{Name: "video", Types: []string{"video/mp4", "video/x-matroska"}, MaxBytes: 20 << 30, Video: true})
store, _ := s3.New(s3.Config{Bucket: "media", Endpoint: rgw, PublicEndpoint: "https://s3.doujins.ai", UsePathStyle: true,
	AccessKeyID: id, SecretAccessKey: secret, Capabilities: caps}) // caps from media.Probe
key, _ := token.ParseKey(os.Getenv("MEDIA_TOKEN_KEY")) // "{kid}:{base64}", shared with media-access
ring, _ := token.NewRing(key, nil)

jobs, _ := media.NewJobs(media.JobsConfig{Store: store, Kinds: kinds, Tenants: []string{"d"}, Limiter: limiter, Resolver: resolver})
manifests, _ := media.NewManifests(store, kinds, media.ManifestOptions{Locker: media.PGLocker(pool), Jobs: jobs})
images, _ := image.New(image.Config{Store: store, Kinds: kinds, Manifests: manifests})
_ = jobs.AddProcessor(images.Process)
_ = video.Migrate(ctx, pool) // River schema media_worker, run by cmd/media-worker
videos, _ := video.NewEnqueuer(pool, kinds)
_ = jobs.AddProcessor(videos.Processor())
client, _ := riverhelpers.New(ctx, pool, &river.Config{Schema: "public"}, runtime.RiverJobs(), jobs.RiverJobs())

uploads, _ := media.NewUploads(media.UploadOptions{Store: store, Kinds: kinds, Manifests: manifests,
	Authorizer: hostUploads, Tickets: &ring, Limiter: limiter, Queue: jobs})
reader, _ := media.NewReader(media.ReaderOptions{Manifests: manifests, Kinds: kinds, Resolver: resolver, Hooks: hooks,
	Delivery: media.Delivery{Mode: media.DeliverCookie, BaseURL: "https://media.doujins.com", CookieDomain: "doujins.com", SigningKey: key}})
mux.Handle("/api/media/upload/", http.StripPrefix("/api/media/upload", media.UploadHandler(uploads, media.UploadHandlerOptions{Tenant: "d", Actor: actorOf,
	Reader: reader})))
mux.Handle("/api/media/", http.StripPrefix("/api/media", reader.Handler(media.HandlerOptions{Tenant: "d", Identity: identity})))

_ = jobs.PublishTx(ctx, tx, ref)                                          // in any transaction that changes what anonymous viewers see
_ = jobs.DeleteItemsTx(ctx, tx, media.Deletion{Ref: ref, Owner: owner}) // in the host's delete transaction
_ = jobs.EraseUserTx(ctx, tx, "d", userID, deletions...)                 // the user's items plus user/{id}/
```

The access worker (`cmd/media-access`, image
`ghcr.io/open-rails/contentkit-media-access:{tag}`, same tag as the hosts'
ContentKit) serves `BaseURL`. It needs `MEDIA_ACCESS_S3_ENDPOINT`,
`_S3_BUCKET`, a read-only key (`_S3_ACCESS_KEY_ID`, `_S3_SECRET_ACCESS_KEY`)
allowed only `*/blobs/*`, `*/editor/*` and `*/public/*`, `MEDIA_ACCESS_TOKEN_KEY` and
`_TOKEN_KEY_PREVIOUS` (the hosts' `{kid}:{base64}` ring), `MEDIA_ACCESS_HOSTS`
(the media host names; empty serves any Host, warned) and
`MEDIA_ACCESS_CORS_ORIGINS` (the sites' exact origins, with credentials;
empty breaks hls.js, warned; wildcards and paths are refused); secrets may be
given as `{VAR}_FILE`. `public/` is served without a token (`no-cache`);
`blobs/` needs `?t=` or an `mt` cookie (`private, immutable`; 403 without a
valid token); `editor/` needs an editor token; manifests, `originals/` and
unknown keys are 404. Every object carries `Cross-Origin-Resource-Policy:
same-site` (`MEDIA_ACCESS_RESOURCE_POLICY=cross-origin` only when the pages
live on another site than the media), so other sites cannot embed it with
`<img>`/`<video>`. See HOST_INTEGRATION "Production media delivery".
`cmd/media-worker` documents its environment.

`Edit` writes with `If-Match` (or `If-None-Match: *`) and retries on conflict;
without `Capabilities.ConditionalPut` it serializes on a Postgres advisory lock
instead. Reads are cached in process and revalidated by ETag. Presigned PUTs
bind `Content-Type`, `Content-Length` and `x-amz-checksum-sha256`.

**Uploads** go straight to the bucket (`media.Uploads`, served by
`media.UploadHandler`): the host's `UploadAuthorizer.CanUpload` (AuthKit) runs
at presign and commit for the folder written (slots and inline images: the
work, `ref.Content()`), and the kind's types and size cap bind every presign.
Up to 64 MiB is one PUT to `originals/sha256-{hex}` signed with its type,
length and SHA-256; larger files are multipart to `staging/u-{uuid}` with
8–16 MiB parts, each signed with its length and SHA-256, resumed through
`ListParts` and completed by the server (a signed ticket carries the S3
UploadId; nothing is stored). The manifest names a staged upload `u-{uuid}`
until `Manifests.Place` moves it to `originals/sha256-{hex}` with the hash
computed while reading it (server-side copy, or none when the folder already
holds the hash; every reference renamed; staging deleted; idempotent).
Slot originals stay fixed-name (`originals/{slot}`): single checksum-bound
PUTs pinned by ETag in the slot record, so hashing their names would only add
garbage collection. Slot originals PUT to `originals/{slot}`. A kind
with `Inline` takes inline images: presign with `inline: true` names a new
`i-{uuid}`, whose original PUTs to `originals/{id}` and is committed with
`commit-slot`; it is re-encoded with the `Inline` spec to `public/{id}.webp`.
Commit is one conditional manifest edit (`insert`, `replace`, `move`,
`rename`, `remove`, `edit`) that HEAD-checks each new original, re-hashes it when the
store does not enforce checksums, and enqueues a `ProcessJob`. `Kind.MaxFiles`
and per-type `Kind.TypeLimits` (`"image"`, `"video"`: `MaxBytes` replacing the
kind's, `MaxFiles`) cap a manifest: a commit that ends over a cap and adds to
it fails with 409 `too_many_files`. A kind may mix images (`Specs`) and videos
(`Video`); each processor handles only its own files.

**Edits** are non-destructive: `File.Edit{Crop{x,y,w,h}, Rotate}` crops in the
source's pixels (EXIF orientation applied), then rotates clockwise by 0, 90,
180 or 270. The `edit` op sets or (without `edit`) clears it; `insert` and
`replace` may carry one. It is checked against `File.Dims`, the source's size
recorded by processing (before that, by the processor, which reports an
out-of-bounds edit to `Hooks.Failed`). A variant's `spec` is
`Spec.For(edit)`, so changing or clearing an edit re-derives that file's
variants (and the zip) from the untouched original. `Spec.EditorOnly`
variants (recorded `editor: true`) are signed only when the resolver's
`Resolution.Editor` is set; `Spec.Unedited` (must be `EditorOnly`) ignores
edits: an editor's view of the whole source. Such blobs live in `editor/`,
which no viewer token or cookie covers; editors' URLs carry an editor token.
No master is written. `meta.w/h` is the edited size; the read API returns
`edit` and `dims` to editors.

**Slots** are fixed public images such as avatars and covers, rendered at
several widths for high-density screens:

```go
Slots: map[string]media.Slot{
	"avatar": {Aspect: media.Aspect1x1, Widths: []int{128, 512}}, // small, large
	"cover":  {Aspect: media.Ratio("3:1"), Widths: []int{900, 3000}, MinWidth: 600},
}
```

A slot's `Edit` uses the same crop (original pixels, EXIF-oriented) and rotate;
the crop's height follows its width at `Aspect` (a `media.Aspect` ratio in
lowest terms, written `"W:H"` in JSON and config: `media.Ratio("9:16")`,
`ParseAspect`, constants `Aspect1x1`, `Aspect3x1`, `Aspect4x5`, `Aspect16x9`,
`Aspect9x16`, `Aspect21x9`; all maths is integer, heights round half up), and
no crop means the largest centred one. The original PUTs to `originals/{slot}`
and `POST /commit-slot {ref, slot, sha256, edit}` commits it; `POST /edit-slot
{ref, slot, edit}` re-edits the kept original without an upload;
`Uploads.SetSlotFromFile(ctx, actor, SlotFromFile{Ref, Slot, From, File,
Edit})` (`POST /commit-slot-from-file {ref, slot, from, file, edit}`) copies a
manifest image (of `From`, default `Ref`: another item of the tenant needs
`CanUpload` on both; default edit: the file's own); `POST /slot-original` returns the original to
uploaders for the editor. The edit and the job's result live in
`originals/{slot}.json`, so spec changes re-encode with it. Each width is
`{slot}_{width}.webp`, rewritten in place (one PUT per object) on every
change: no versions. Nothing is upscaled: a width wider than the edited image
is rendered at the edited width under its own name, so every width exists
once the slot is set. An edit outside
the original or narrower than `MinWidth` (default the smallest width) is refused (by the job when the original's size
is not yet known: `Hooks.Failed`, keeping the served outputs). Slot routes and
the read API's `GET /{kind}/{id}/slots/{slot}` answer `SlotManifest{aspect,
edit, dims, outputs: [{name, w, h, url}], pending, error}`. URLs are fixed;
the access worker serves `public/` and editor outputs `no-cache` with the
object's ETag, so browsers revalidate (304 when unchanged) and see a new crop
on the next load. Listings link a slot without reads:
`Hooks.SlotEncoded(ctx, ref, slot, aspect)` reports a written slot and the
host records that it is set (and, for native slots, the aspect);
`Reader.ListedSlot(ref, slot, aspect)` lists every width at its fixed URL.
`Slot{Aspect: media.AspectNative}` keeps the edited image's own shape: no crop
by default, crops of any shape.

**Image processing** (`media/image`, CGO over libvips via govips; install
`libvips-dev` to build it). `image.New(Config{Store, Kinds, Manifests, Specs,
Hooks})` gives `Process(ctx, media.ProcessJob)`; register it with
`jobs.AddProcessor(proc.Process)` and pass `jobs` as `UploadOptions.Queue`. It derives WebP variants per the kind's `Specs` (or a per-file
`SpecChooser`) from each file's `master`, else `original`, through its edit,
only where a variant is missing or its `spec` differs, stores them as `blobs/sha256-…`, and
records them in one manifest edit per pass that drops results for sources
replaced meanwhile; it repeats until a commit that landed during the run is
covered too. A kind with `Zip` set gets `downloads.zip`: a stored zip of that
variant in file order, rebuilt only when its `inputs` hash changes; its
display name comes from `Hooks.DownloadName` at read time. Slots and inline
images re-encode `originals/{slot}` into `public/` in place (`no-cache`, ETag),
with writes conditional on the output's previous ETag and skipped when they
already match. Undecodable sources go to `Hooks.Failed` and are not retried.

The optional `UploadLimiter` (`media.NewPGLimiter` over the baseline's
`content_media_*` tables) rate-limits uploaders (files/hour, bytes/day → 429)
and enforces per-owner quota. The rule: a commit that grows the owner's stored
originals (the change in the manifest's distinct originals) past its quota
fails with 413 `quota_exceeded` and writes nothing. Presign reservations only
refuse early (used + pending + size); they lapse after a day, which never lets
a late commit past the quota. Exempt grants are charged but never refused.

The bucket needs CORS allowing `PUT` from the app origins with the
`Content-Type` and `x-amz-checksum-sha256` headers, and the
`AbortIncompleteMultipartUpload: 1 day` rule `Store.Configure` sets.

**Video** (`media/video`, run by `cmd/media-worker`) encodes each `video/*`
manifest file: H.264 High with a capped CRF per rung (live action: 2160 CRF 23
at most 16 Mbit/s, 1440 23/9M, 1080 23/6M, 720 22/3.5M, 480 21/1.5M; VBV
buffer 2× the cap; `Video.Profile: media.VideoAnimation` adds `-tune animation`
at CRF 20–21 and lower caps), keyframes every 2 s without scene cuts, 4 s
segments, at the kind's ladder (`Kind.Video = &media.Video{Ladder: []int{1080, 720, 480}}`;
default `media.DefaultLadder`, 2160/1440/1080/720/480), AAC per audio track,
WebVTT per text subtitle and a 10×10 sprite whose tiles keep the source
aspect (short side 90). A rung N is the output's **short side** (a 1080 rung
of a vertical video is 1080 wide); rungs above the source's short side are
dropped (a smaller source gets one rung at its own short side). Every frame is
then capped, aspect kept, at 4096 px per side and a 3840×2160 area (common
H.264 hardware decode limits), so a 21:9 2160 rung is 4096×1756 and an 8K
source is downscaled to 3840×2160; a rung whose capped frame repeats the next
one's is dropped. Output is square-pixel (SAR applied), at most 60 fps, and
frames above 1080p-class carry the lowest fitting level (5.0/5.1/5.2);
smaller ones keep x264's. `hls.video[]` records `rung` and the true `w`/`h`.
Sources whose display aspect is outside `Video.MinAspect`–`MaxAspect`
(default 1/2.4–2.4, admitting 2560×1080 and 2.39:1 cinema; 0.5% slack) fail permanently: the file's `hls` becomes
`{source, spec, error}` with no renditions, `Hooks.Failed` (in the encoder's
process) gets `video.ErrAspect`, editors see `failed` in the read API, and
it is retried only when the source or the kind's bounds change. Each rendition and
audio track is one single-file fMP4 blob whose segments are
`[offset, length, seconds]` (the init segment is `[0, segments[0].offset)`);
each rung also gets a muxed MP4 in `downloads["{file}-{N}p"]` (video,
every audio track, subtitles). Blobs are written first; one manifest edit then
records `hls` and `downloads` only if the file still derives from the encoded
original, so a replaced file keeps its previous `hls` until then. Outputs are
byte-identical on retry (same encoder, presets and `Threads`). Each pass
decodes the source once and scales a lanczos cascade (each rung from the one
above; the sprite from the smallest); x264 runs preset `faster` up to 1080
and `veryfast` above (`Config.Preset`, `TopPreset`), each rung with its
frame-area share of `Config.Threads`; rungs mux and upload concurrently.
**Two stages:** rungs up to 1080 (with audio, subtitles, sprite and their
downloads) are published first with `hls.pending` listing the rungs above;
the worker queues a follow-up job (same args, River priority 2) that encodes
those and adds them to `hls.video` in one manifest edit. Both stages key
frames at the same times, so players switch between their rungs seamlessly;
progress reports `stage`/`stages`. **Passthrough:** when the source already
is a compliant top rung (MP4/MOV constant-rate 8-bit 4:2:0 progressive
H.264 High/Main ≤ level 5.2, unrotated, at the rung's exact frame, within its
bitrate cap, with an IDR on each 2 s keyframe time) that rung is stream-copied
and checked against a sibling rung's segments; otherwise it is encoded.
`Config.Encoder` picks libx264 (default without a GPU) or NVENC (`auto` uses
it when a probe encode works; a file it fails on re-encodes with x264).
ffmpeg reads only local files
(`-protocol_whitelist file`) through container demuxers (mov/mp4, matroska/webm, avi,
mpegts, flv, ogg, asf, mpeg): playlists and concat lists are refused. Changing
the ladder or profile changes `video.Spec(video)`, so files re-encode. Jobs live in River schema `media_worker` in the host
database: hosts run `video.Migrate` and enqueue through `video.NewEnqueuer`
(insert-only; register `enqueuer.Processor()` with `media.Jobs.AddProcessor`;
`enqueuer.Cancel(ctx, ref)` cancels an item's queued and running jobs of
both stages). A job is `{ref, versioned, video}`; jobs are not unique, and a
job for a fresh manifest is a no-op. The worker's environment is
documented in `cmd/media-worker`.

After each encode the job grabs the item's **poster** frame (the `poster`
slot; the image job encodes it) and renders its **hover preview** (silent MP4
and animated WebP loops) from their selections; see HOST_INTEGRATION "Video
posters and hover previews".

**Encode progress**: with `ReaderOptions.Progress: video.NewProgressSource(pool)`
the read API adds `progress` to each visible video file still pending (none
yet, or a replaced source), and `GET /{kind}/{id}/video-images` adds the item's
current step. The worker parses ffmpeg `-progress` and writes, at most every
`Config.ProgressInterval` (2 s) plus on phase changes, a per-file map to its own
River row (`metadata.contentkit_progress`, cleared when the job ends; no extra
table). Contract (`media.EncodeProgress`): `phase` (`queued` downloading
probing encoding muxing uploading publishing, then item-wide `images`),
`queue_position` (1 = next; waiting jobs only), `segments_done`/`segments_total`
(HLS segments, `ceil(duration/4)`), `percent` (time-based, never decreasing),
`speed` (×realtime, smoothed over ~8 s), `eta` (seconds: remaining media /
speed plus projected uploads / measured throughput), `at` (unix ms), `stalled`
(a running job silent for a minute). It reveals only timing and queue depth,
so every viewer allowed the file gets it; one indexed query, only for items
with a pending video.

**Playback** is served by `Reader.Handler` next to the read API, generated per
request after one `Resolve` (`private, no-store`; the folder cookie is set in
cookie mode): `/{kind}/{id}/hls/{file}/master.m3u8?audio=&subs=` (optional
id/language filters; `RESOLUTION` is the rung's true w×h; the first variant is
the highest rung up to 1080p, where Safari/iOS native HLS starts, then the rest
by descending bandwidth), `video/{N}.m3u8`, `audio/{id}.m3u8`,
`subs/{id}.m3u8`, `sprite.vtt`, and `/{kind}/{id}/download/{key}` (302 to the
signed `dl=` URL, full access only; name from `Hooks.DownloadName`). Media
playlists are `EXT-X-BYTERANGE` lines over one blob URL per rendition. A file
plays when the grant allows it (full access, inside a preview cut, or a
teaser); preview viewers get per-file URL tokens. In the browser, hls.js needs
`xhrSetup: xhr => { xhr.withCredentials = true }` in cookie mode and the
worker's `Origins` must list the site; native Safari/iOS HLS should be checked
in cookie mode and switched to URL mode if it does not send the cookie.

Tokens are `kid.exp.base64url(HMAC-SHA256(secret, "{scope}|{exp}"))`: a scope
is a folder (`…/blobs/`, covering the objects directly under it), one key, or
`{key}#dl={name}` for a download name. Expiry is window-aligned (default 4 h);
`token.Ring` verifies the current and previous key. Editors get a folder token
for `…/editor/`.

`Reader.Handler` limits each viewer (`HandlerOptions.Limit`, default 2
requests/s, burst 120; keyed by `Actor.ID`, else `Actor.IP`, else the peer
address) with 429 `rate_limited` + `Retry-After`, and logs every signed
response (`media urls signed`: viewer, ref, access, expiry, and a short hash
of a folder token) so a leaked URL traces to its viewer.

Media's River jobs (`jobs.RiverJobs()`) compose into the host client through
`helpers/river`; edits schedule a sweep and commits enqueue processing:

- **Sweep** (per folder, 24 h after each edit and in a daily pass over
  `Tenants`): deletes `blobs/`, hash-named `originals/` and `staging/` no
  manifest in the folder references, only once every manifest and the object
  itself are older than `Grace` (24 h; plus 1 day for staged multipart
  objects, which may be dated at initiation). Also unreferenced `editor/` blobs. Slot originals,
  slot and hover-preview outputs and manifests are never swept.
  Invariant: it deletes only objects no manifest references and no in-flight
  commit can newly reference. Presign reuses an existing original, and a
  commit accepts one, only while a manifest references it or it is well
  before the sweep's cutoff (grace/2 for presign; grace/4, at most 1 h, for
  commit); otherwise the client uploads it again. Set `UploadOptions.Grace`
  to the same grace (taken from `Queue` when it is the `*Jobs`).
- **Deletion** removes the whole folder, manifests and `public/` first, then again after
  `LateUploadWindow` (25 h) for PUTs and multipart completions that land late.
  With a `Limiter`, the owner's quota (the manifests' `OriginalBytes`) is
  released once.
- Processing: `jobs.Enqueue` (the uploads' `ProcessQueue`) runs one pending
  job per ref and slot through every `AddProcessor` processor. An Enqueue
  (or `ScheduleSweep`) while an equal job runs queues one follow-up that
  starts after it, since the running job may have read its inputs before the
  change; an equal job still waiting absorbs it.
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
`media/internal/s3test`). CI runs them on MinIO; `media/image` runs in its
own CI job with libvips, and the other jobs exclude it. To record a Ceph RGW
release's capabilities, point the same variables at an RGW bucket and run
`go test ./media/... -v -count=1`; the log prints the probed capabilities.
On a backend without conditional PUT the tests edit manifests under
`PGLocker`, so they also need `CONTENTKIT_TEST_URL` (they skip without it).
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
