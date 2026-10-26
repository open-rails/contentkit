# Uploading media

Content creation, file upload and publication are separate operations. Create a
content item/version first, then upload to that media owner. Uploading never
publishes the content.

## Three-step upload

All API paths are relative to the host's ContentKit prefix. The authenticated
credential and deployment select the app; callers cannot choose another tenant.

1. `POST /v1/media/upload/presign` supplies the media reference, configured
   upload path, byte length, content type and SHA-256. The response supplies the
   storage URL and exact request headers, or an existing matching blob.
2. Send the bytes to the signed storage URL. Use the returned multipart
   instructions for a large file. Send no ContentKit API credentials to storage.
3. `POST /v1/media/upload/commit` supplies the same owner reference, a caller-saved
   operation ID and the exact staged-file operations. The commit durably accepts
   the upload and makes its processing job ready to run immediately.

The commit returns promptly; extraction and rendition generation are background
media work. Worker capacity determines when execution starts. A ZIP uploaded to
an app's archive import path is unpacked into the configured page upload path.
The source ZIP is retained. Individual images use the same upload protocol.

`Client.IngestMedia(ctx, storageClient, ref, operationID, files)` is the Go helper
for this sequence. For Doujins, the archive path is `import/gallery.zip`, the
reference names the gallery and its version, and the content type is
`application/zip`. Other applications configure their own upload paths.

Save the operation ID before committing and retain the exact commit body for
an uncertain response. Replay that body to recover the result; do not rebuild a
different set of operations under the same ID. Conflicting input is rejected.
The browser upload queue keeps the pending body scoped to its API and account.
Use a separate unauthenticated storage client; storage redirects are refused.

If the Go helper's commit response is interrupted, `errors.As` can retrieve a
`*contentkit.MediaCommitError`. Persist its `Commit` body and retry that body
with `PostMediaUploadCommit`; `Unwrap` retains the original failure for normal
retry decisions. Reuploading with a newly constructed body under the same
operation ID is not a substitute for replaying the accepted request.

## Processing status

Poll the existing media-read API for the same owner. Editor reads include
processing status; `status=1` requests the compact status without media URLs or
a paginated file listing. It requires permission to edit that media owner.

Status includes queued/processing/ready/failed state, upload and archive counts,
completed/total/failed rendition counts by preset, video/audio encoding progress
and bounded file failures. Counts describe the complete owner, not just the
current page of files. The data comes from committed media state and worker
checkpoints, so it remains available after closing the browser or restarting a
server. Reading status neither starts jobs nor issues media access grants.

The UI's shared `useMediaRead` polling and `MediaReadinessNotice` show this state
while work is pending. Returning to the editor reads the current status again.

## Metadata and publication

Use ordinary content and entity APIs to edit titles, artists, series, tags and
version metadata independently of uploads. Entity associations can resolve
canonical names and declared aliases and create permitted missing entities.

`POST /v1/contents/{id}/publish` publishes explicitly, optionally with a
`publish_at` timestamp. Until then, the content stays a draft. Publication can
be refused while required media is not ready; inspect media status and retry
publication after processing finishes. There is no automatic publish-after-upload
mode or separate publishing-operation status endpoint.

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
If automatic cleanup would leave no pages, one source remains included. The normal upload flow needs no QA-start or approval request.

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

The common publication worker applies the next audience at its scheduled
deadline, updates the completed reader policy and records retained release
facts. Reads use that completed policy; passing a deadline alone does not
complete pending publication. Durable requests catch up after downtime without
a polling sweep. Consumers use the event's effective timestamp separately from
its recording timestamp.
Backdating a newly added grant does not pretend access existed before that edit.

The migration converts old `free_after` rules into equivalent public releases.
New writes use `releases`; Doujins and Hentai0 do not opt in to schedules.
