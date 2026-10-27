# Media jobs

ContentKit uses two media processing job types and one release job:

| Operation | River kind | Queue |
|---|---|---|
| Validate/unpack/attach uploads | `contentkit_media_ingest` | `media_ingest` |
| Generate required image/video outputs | `contentkit_media_derivatives` | `media_derivatives` |
| Deliver and activate a complete edition | `contentkit_version_release` | catalog-scoped versions queue |

A version can contain images and video. Its derivative job uses the configured
image presets and video codecs/renditions, including soundtracks. Original audio
is pass-through. Chunking and encoder checkpoints are internal progress, not
separate River job types. Completed work survives retries; source/incarnation
checks prevent stale work from replacing newer uploads. A force request is
consumed once. Ingest and derivative concurrency are configured independently.

## Processing and publication are separate

Ingest validates the archive before attaching its files. The derivative attempt
prepares page order and automatic cover inputs, then produces missing required
outputs. Drafts receive the same preparation without becoming published.
Generated edition outputs remain in `internal/`.

Publish records intent for now or a future date. The existing release request
waits until every required output is complete, copies the selected output set
into `public/` or `private/`, verifies it, then activates the reader snapshot.
An existing completed release remains readable while a replacement processes.
A video becoming partially playable does not complete publication.

There is no standalone playable flag, daily playable scan, version preparation
worker, body-image-copy worker or version-fork operation. New versions have their
own text and media. The media gateway retains its simple rule: files actually in
`public/` are public; `private/` requires the appropriate authorization token.

## Progress and retries

Processors update canonical manifests/checkpoints and append retained processing
facts. Admin JSON, Go and SSE observers can show partial progress without exposing
partially complete releases. Normal processing completion wakes an accepted release
request directly; no readiness relay or additional preparation job is necessary.

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
Content/media retention deletion belongs to the existing content-purge sweep;
there is no separate media reaper or bucket-discovery scanner.

## Adoption

Use matching host libraries, media-worker images and KEDA triggers. Configure
`IngestWorkers`, `DerivativeWorkers` and `JobTimeout`; stock workers use
`MEDIA_WORKER_INGEST_CONCURRENCY` and `MEDIA_WORKER_CONCURRENCY`.

Before the hard cut, finish accepted old image/video/preparation/copy work using
the previous implementation, then stop old producers and workers before migration.
Preserve existing job history, original files and published migration bytes.
No compatibility handler or new obsolete-job repair guard is introduced.

Qualify actual PostgreSQL/S3/FFmpeg behavior: mixed media, complete-output release,
last-good reader snapshots, interruption/retry, force consumption, stale sources,
codec outputs, per-version access, progress observation and retained deletion.
