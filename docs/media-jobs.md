# Media jobs

ContentKit uses two media processing job types, one release job and one garbage-collection job:

| Operation | River kind | Queue |
|---|---|---|
| Validate/unpack/attach uploads | `contentkit_media_ingest` | `media_ingest` |
| Generate required image/video outputs | `contentkit_media_derivatives` | `media_derivatives` |
| Move a version's released files into served areas | `contentkit_publication` | `contentkit_publication_<catalog hash>` |
| Delete an item's retired files | `contentkit_media_gc` | `contentkit_media_gc` |

The release and GC jobs are one per item: each is woken by the item's manifest writes (through the `content_media_items` cache) and snoozed until the item next needs it.

A version can contain images and video. Its derivative job uses the configured
image presets and video codecs/renditions, including soundtracks. Original audio
is pass-through. Chunking and encoder checkpoints are internal progress, not
separate River job types. Completed work survives retries; source/incarnation
checks prevent stale work from replacing newer uploads. A force request is
consumed once. Ingest and derivative concurrency are configured independently.

## Masters first

A derivative attempt first makes the masters its kind's recipe (`media.Kind.Master`) has yet to make, with the worker's hooks, then derives from them. An image's renditions, and a video's HLS renditions, seek sprite and grabbed frames (whose own renditions follow), record the master's hash, so a new master re-derives each of them; a frame waits for its video's master. A worker refuses to start while a recipe names a hook it lacks or at another version: the stock image registers none, and a host with hooks runs its own binary over `media/worker.Main` ([Masters](../HOST_INTEGRATION.md#masters-mediakindmaster)). A failing hook leaves the upload's previous master and everything derived from it in place while the job retries. A master or rendition past 1 GiB is streamed from its file in 64 MiB parts, each checked by its SHA-256 (S3 refuses a single PUT past 5 GiB).

## Processing and publication are separate

Ingest validates the archive before attaching its files. The derivative attempt
prepares page order and automatic cover inputs, then produces missing required
outputs. Drafts receive the same preparation without becoming published.
Generated edition outputs remain in `internal/`.

Publish records intent for now or a future date. The release job moves each
due chapter's ready pages, and once anything is released the version's other
ready outputs, into `private/` (`public/` when free), writes the manifest naming
them, and retires the `internal/` copies. What an earlier release served of a
page stays served while its replacement processes.
A video becoming partially playable does not complete publication. An upload a
kind marks `Required` counts as a required output: without it the release
waits, and an existing release that lacks it is withdrawn rather than kept.
`Engine.Reconcile` queues that withdrawal for releases made before the upload
was required.

There is no standalone playable flag, daily playable scan, version preparation
worker, body-image-copy worker or version-fork operation. New versions have their
own text and media. The media gateway's rule is the area: `public/` is public,
`private/` requires the item's token, `internal/` is never served.

## Progress and retries

Processors update manifests/checkpoints; the cache appends retained processing
facts when an item's status changes. Admin JSON, Go and SSE observers can show
partial progress without exposing unreleased files. Every manifest write wakes
the version's release; no readiness relay or additional preparation job is necessary.

Rendered images are recorded and reported one at a time. `PageGroup` (stock
workers: `MEDIA_WORKER_PAGE_GROUP`) above 1 records up to that many in one
manifest edit, or those finished within 5 s of the group's first, which bulk
imports use to cut manifest edits. An attempt after one that failed, timed out
or died records one at a time again, so a page that kills the worker cannot keep
the rest of its group unrecorded.

A failed ingest or derivative attempt retries through that same durable job.
Video attempts checkpoint bounded work so successful chunks/renditions are not
encoded again. After five failed attempts, the shared worker records remaining
image, video, subtitle, and frame work as failed before cancelling the job.
Completed outputs and checkpoints survive. Regeneration clears these failures,
including frames that failed before producing their first blob. Shutdowns,
checkpoint yields, and bounded storage-outage snoozes retain their existing
retry behavior. Jobs cancelled by an older worker are not scanned automatically;
regenerate affected items to retry their remaining outputs. Storage mutation and
source identity rules remain authoritative.
GC deletes only files an item's manifest retired, once due; there is no
bucket-wide scanner.

A video attempt encodes one chunk, assembling the rung that chunk completes,
then yields its job behind the other queued videos. Every queued video so
advances one chunk per pass, and a large backlog finishes its videos only near
its end. `VideoSlice` (stock workers: `MEDIA_WORKER_VIDEO_SLICE`, a duration
such as `20m`) lets an attempt keep taking its own video's next chunks until
that long after it started, so videos finish one after another. A slice ends
when its own video becomes playable, and after any chunk while a job of higher
priority waits, so a playable video's ladder or a backfill never runs ahead of
a video still waiting to play. Another video waiting for its first playback
can wait up to one slice behind a video still encoding its own. It is capped at half of `JobTimeout`, leaving the other half
for the last chunk it starts. Shutdown ends a slice after its current chunk.
The default, 0, yields after every chunk.

## Scratch disk

Jobs write scratch files under `TempDir` (stock workers: `MEDIA_WORKER_TMP`, default the system temporary directory). Put it on disk, not a memory-backed tmpfs, and size it for every job that can run at once in one process: `MEDIA_WORKER_INGEST_CONCURRENCY` (default 2) times the largest accepted ZIP upload (`MaxBytes` of its `Publish.Import` upload) plus one page (`MaxBytes` of its `Publish.Pages` upload), and `MEDIA_WORKER_CONCURRENCY` (default 1) times the largest video source plus its encoded outputs. A ZIP is validated in one streaming pass and its pages are then extracted again, one at a time, as each batch is written, so its expanded size (up to 2 GiB) never lands on disk.

## Adoption

Use matching host libraries, media-worker images (or a host's own worker binary, built from the same release) and KEDA triggers. Configure
`IngestWorkers`, `DerivativeWorkers` and `JobTimeout`; stock workers use
`MEDIA_WORKER_INGEST_CONCURRENCY` and `MEDIA_WORKER_CONCURRENCY`. Moving a
bucket of the earlier model is in [migration](migration.md#media-model).
