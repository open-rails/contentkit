# Publishing an archive

`Client.PublishArchive` submits one gallery and its metadata, uploads one ZIP
straight to object storage, and waits for processing and optional publication.
It uses the same publishing-operation API whether the client is remote or
created by `Engine.Client()`.

```go
file, err := os.Open("gallery.zip")
if err != nil {
    return err
}
defer file.Close()

// Save this UUID before submitting. Reuse it after a timeout or restart.
operationID := uuid.NewString()
result, err := client.PublishArchive(ctx, contentkit.ArchivePublication{
    OperationID: operationID,
    File:        file,
    Filename:    "gallery.zip",
    Request: publishing.Request{
        Content: catalog.ContentInput{
            Kind: "gallery",
            Text: map[string]catalog.Text{
                "en": {Title: "Example gallery"},
            },
        },
        Entities: []publishing.EntityReference{
            {Kind: "artist", Name: "Example Artist", Language: "en"},
            {Kind: "series", Name: "Example Series", Language: "en"},
            {Kind: "tag", Name: "Color", Language: "en"},
        },
        CreateMissingEntities: true,
        Publish:             publishing.WhenReady,
        Source: &publishing.Source{Provider: "my-import", ID: "gallery-42"},
    },
})
if err != nil {
    // result.ID remains usable after a failed request. Persist it.
    return err
}
fmt.Println(result.State, result.Result.Path)
```

The example uses `github.com/open-rails/contentkit` and its `catalog` and
`publishing` packages; `uuid` is `github.com/google/uuid`. Configure the remote
client's HTTP transport to send `Authorization: Bearer <api-key>`. Pass only the
host prefix to `NewClient`, for example `https://site.example/api/contentkit`;
the client adds `/v1`. Its credential-carrying transport is never reused for
object storage. A custom `StorageClient` must also be free of credential injection.

The host provisions API keys through its identity provider. For native AuthKit,
provision an app-bound key with `app:content:write`; creating missing taxonomy
also needs `app:entities:write`. Key creation stays in AuthKit, not ContentKit.
`when_ready` requires the AuthKit adapter's durable publishing authorization
(or an equivalent provider implementation). It records a nonsecret reference
to the original credential and checks its live authority before publication.
Authority is checked before committing publication or its future schedule. An
already accepted schedule follows the catalog's normal publication policy. Other providers and
interactive sessions can prepare a `draft` and publish it explicitly later.

## HTTP sequence

All paths below are relative to the host's configured ContentKit prefix.
Tenant and app scope come from that deployment and the authenticated key, not
from fields a caller supplies.

1. `PUT /v1/publishing-operations/{uuid}` sends all metadata and the ZIP declaration.
2. Upload bytes to the returned presigned storage URL, or use the existing
   multipart routes when a multipart plan is returned.
3. `POST /v1/publishing-operations/{uuid}/complete` with `{}` accepts the upload.
4. `GET /v1/publishing-operations/{uuid}` reads processing progress and the result.

A small ZIP needs two ContentKit writes, one storage PUT and status reads. Large
ZIPs need additional multipart signing, part uploads and assembly calls. The Go
helper handles those details, checksums, bounded retry delays and part resumption.
It does not forward the API key or cookies to storage or follow storage redirects.

### Page names and order

The archive's images start in natural filename order, then the manifest owns
reading order. Reordering never renames files or changes their source hashes.
Downloads stream ZIP entries in manifest order while retaining the chosen
filenames; an external viewer may sort extracted files by name instead.

Ingestion preserves meaningful names such as `00a.png`, `06-07.png` and
`cover.png`. Batch cleanup is deliberately narrow: consistent redundant
numeric prefixes/suffixes, repeated numbers such as `02_2`, and inconsistent
numeric padding. Original names remain in source metadata; cleanup is flagged
for review. Ambiguous names can be renamed explicitly in an unsaved editor
draft. Filename collisions gain a readable suffix rather than overwriting
another image.

Ordinary streamed archives are cached only after completion. Their cache key
includes ordered source hashes and filenames, so reordering or renaming creates
a fresh archive. Personalized watermarked archives are never shared-cached.

### Prepare or resume

```http
PUT /v1/publishing-operations/78069d1d-b0f1-45de-94f9-09f1a2754efe
Authorization: Bearer <api-key>
Content-Type: application/json
```

```json
{
  "content": {
    "kind": "gallery",
    "text": {"en": {"title": "Example gallery"}}
  },
  "entities": [
    {"kind": "artist", "name": "Example Artist", "language": "en"},
    {"kind": "series", "name": "Example Series", "language": "en"},
    {"kind": "tag", "name": "Color", "language": "en"}
  ],
  "archive": {
    "name": "gallery.zip",
    "size": 123456,
    "sha256": "<64 lowercase hexadecimal characters>"
  },
  "publish": "when_ready",
  "create_missing_entities": true,
  "source": {"provider": "my-import", "id": "gallery-42"}
}
```

The response is HTTP 200 with an operation receipt. In `awaiting_upload` it
also includes `upload`, the existing `media.PresignReply`. Send the exact method
and headers in `upload.put` to its URL; an already-present blob needs no upload.
For multipart, use `upload.multipart` with `/v1/media/upload/parts/list`,
`/parts`, and `/complete`. The SDK performs these calls automatically.

Entity kinds must be configured by the app. A reference can be `{"id":"..."}`
instead of a name. Names use exact normalized matching, not fuzzy search. An
ambiguous name fails and asks for an ID. Scoped taxonomy uses a nested `scope`
reference, for example a character scoped to its series. Unknown entities are
created only when `create_missing_entities` is true and the key has permission.
Ownership entities and entities granting authorization require explicit creation.
All metadata and taxonomy changes are committed together, before storage URLs
are issued. Invalid metadata leaves no partial gallery behind.

The response's `entities` list reports resolved IDs, canonical names and a
`created` flag, including parent scopes. Repeating an operation returns its
original resolution receipt. Existing canonical names and declared aliases are
matched case-insensitively, ignoring spaces, underscores and hyphen variants:
`big-breasts`, `Big Breasts` and `bigbreasts` resolve to the same entity. Semantic
synonyms need a declared alias; the API does not guess that two different artist
names belong to the same person. Multiple matching IDs produce an explicit
ambiguity error instead of an arbitrary association.

The request is immutable. Retrying the same UUID and normalized request returns
the original content/version IDs and renews an expired upload plan when safe.
Different input returns HTTP 409 `operation_conflict`. An optional unique
`source.provider` + `source.id` prevents another operation from importing that
external item again; its conflict is `source_conflict`. Source URLs are
provenance only: this endpoint does not fetch arbitrary URLs.

### Complete and observe

`POST .../{uuid}/complete` with `{}` returns HTTP 202. ContentKit owns the exact
media commit; callers do not assemble a second JSON payload of blob IDs.
Repeating completion is safe. A status response resembles:

```json
{
  "id": "78069d1d-b0f1-45de-94f9-09f1a2754efe",
  "state": "published",
  "content_id": "...",
  "version_id": "...",
  "progress": {"stage": "complete", "completed": 20, "total": 20},
  "result": {
    "path": "/en/g/EXAMPLE/example-gallery",
    "pages": 20
  },
  "created_at": "2026-10-15T12:00:00Z",
  "updated_at": "2026-10-15T12:00:05Z"
}
```

`GET` returns no upload URLs and never advances processing. HTTP 200 means the
receipt was read; inspect `state`:

| State | Meaning |
|---|---|
| `awaiting_upload` | Upload or resume the ZIP, then complete. |
| `processing` | Importing pages or generating required media. |
| `publishing` | Publication accepted; waiting for public media exposure. |
| `ready` | Processing finished; `publish: draft` requested no publication. |
| `scheduled` | Processing finished; publication is scheduled for `publish_at`. |
| `review_required` | The configured moderation policy requires review. |
| `published` | Published successfully. |
| `blocked` | Permission was revoked, metadata/media changed, or another prerequisite prevents publication. Inspect `error`. |
| `failed` | Processing failed permanently, for example an invalid archive. Inspect `error`. |

A structured `error` has `code`, `message`, optional `field` and `retryable`.
Retryable dependency failures can recover in the background. The helper returns
review/blocked/failed receipts with a typed `publishing.OperationError`; it never treats
an HTTP 200 failure receipt as successful publication. Cancelling the helper
stops waiting, not the server operation. Save the UUID and call it again with
the same file and metadata to resume.

The existing catalog and media APIs remain available for editing, multiple
versions, explicit review or custom upload flows. This operation initially
covers one archive-backed reader version. Its worker checks the imported
archive checksum, exact ordered pages, required renditions, cover, original
metadata revisions and authority before automatic publication. Editing the
pending draft prevents it from silently publishing different content.

## Page normalization and review

Ingestion retains the uploaded ZIP and each original page. Generated reader
images, thumbnails and download ZIPs are separate renditions. Excluding a page
from the reading order does not delete its original. This policy applies to new
ingestion; it cannot recover source archives removed by an older import.

Initial ordering uses natural numeric order. When padding is inconsistent,
`1.png`, `2.png`, `10.png` become `01.png`, `02.png`, `10.png`; meaningful names
and numeric gaps remain. A joined spread such as `002-003.png` stays one reader
image and normally replaces separate pages 2 and 3 in the reading order.
The manifest owns page order independently of filenames: reordering never
renames a page. Editors can explicitly rename files before Save. ZIP entries
follow manifest order and retain those filenames.

Exact duplicates, confidently empty pages and singles covered by a two-page
spread are removed automatically, and an archive's pages are put in natural
name order. Native analysis examines the original pixels before cropping or
resizing, and a failure to classify a valid image does not block publication.
If automatic cleanup would leave no pages, one source remains included. The
normal three-request upload flow needs no QA-start or approval request.

Each automatic change is a quiet, dismissible notice for editors:

```http
GET  /v1/contents/{content-id}/versions/{version-id}/page-notices
POST /v1/contents/{content-id}/versions/{version-id}/page-notices/{notice}/dismiss
POST /v1/contents/{content-id}/versions/{version-id}/page-notices/{notice}/restore
```

A `removed` notice lists the removed pages and why (`blank`, `duplicate`,
`spread`); restoring puts them back where they were. A `reordered` notice gives
each page's archive position (`from`) and applied position (`to`); reorder by
moving pages instead. Neither ever blocks saving or publishing. The Go client
exposes `GetContentVersionPageNotices`, `PostContentVersionPageNoticeDismiss`
and `PostContentVersionPageNoticeRestore`; the browser client uses
`versions.pageNotices`, `dismissPageNotice` and `restorePageNotice`.

Versions with explicit chapter/segment ranges preserve their existing selection
and source order, so automatic removal never moves a page across a release or
access boundary; restoring such pages needs the chapters edited first.

For browsing existing items, `GET /v1/manage/gallery?q=...` selects a configured
kind directly in the path. `/v1/manage` lists across kinds. Both apply the same authorization.
`/v1/entities` manages the typed associated records. ContentKit calls these
records **entities**; an association links content to an entity.

## Drafts and scheduled releases

`"publish": "draft"` processes the archive and stops at a private, ready draft.
It is also the default when the mode is omitted. Publish it later through
`POST /v1/contents/{id}/publish`, optionally with a `publish_at` timestamp.

For automatic future release, include both fields in the initial operation:

```json
{
  "publish": "when_ready",
  "publish_at": "2026-11-01T18:00:00Z"
}
```

Processing begins after `/complete`; the timestamp is the earliest public
release time, not the time to start unzipping. If processing finishes early,
the operation reports `scheduled` and the Go helper returns successfully. If
processing finishes late, release occurs when it is ready. A scheduled receipt
includes its planned date and content path; the content stays private until
release. Continue reading the same operation to observe eventual `published`.


## Audience releases

Publication dates determine when content and its versions become available.
An access rule can add audiences at later timestamps through `releases`:

```json
{
  "level": "tier", "tiers": ["early"],
  "releases": [
    {"at": "2027-01-02T00:00:00Z", "level": "tier", "tiers": ["standard"]},
    {"at": "2027-01-09T00:00:00Z", "level": "public"}
  ]
}
```

The early audience keeps access as later audiences open. Tier names have no
implicit ordering. Release audiences use existing tier or group entitlements;
a public audience needs neither. The content and version publication dates,
moderation and media readiness still apply. A version override replaces the
whole inherited access policy. Omitting `releases` keeps ordinary access rules.

Reads evaluate timestamps directly. The five-minute publication recorder adds
release facts to the retained event log and catches up after downtime. Consumers
use the event's effective timestamp separately from its recording timestamp.
Backdating a newly added grant does not pretend access existed before that edit.

The migration converts old `free_after` rules into equivalent public releases.
New writes use `releases`; Doujins and Hentai0 do not opt in to schedules.
