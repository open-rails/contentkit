# Media jobs

Media job model for ContentKit #242. Deployment requires the coordinated adoption described below.

Media processing uses three worker job kinds: ingest, image and video. Original audio is pass-through. Host integration retains known-deadline cleanup, non-version folder deletion and interrupted-commit recovery. Public exposure and readiness relay jobs are removed; see [processing events](media-processing-events.md).

## One resumable video conversion

`contentkit_media_derivatives` runs on `media_derivatives`. One River row owns image generation and video conversion for a media item through planning, encoding and rendition publication. Chunking is an internal checkpoint mechanism, not a separate River job graph.

Each attempt validates the current source/preset, completes bounded missing work and records successful outputs. Completed chunks survive interruption. Finished renditions contribute editor progress while later renditions finish; they are not released to readers until all required outputs are ready. The same job yields at chunk boundaries so shorter waiting conversions get a turn; worker resource limits still bound CPU and scratch use. A force request is consumed once, and progressive readiness is delivered even when the job yields.

Keep the existing durable encoding checkpoints and source/incarnation checks. Remove child-job fan-out, separate assembly scheduling, light/encode queue aliases and unused one-shot worker selection. Preserve cancellation, stale-source rejection, temporary-output recovery and idempotent publication.

## Remove obsolete host work

Remove the unused CDN-purge callback/relay surface. Current D/H hosts do not configure it, so retiring objects already produces no CDN invalidation through that callback. Actual storage deletion remains a durable operation. Public presets remain public independently of visibility.

Remove the legacy pre-v0.68 manifest conversion runtime and its recurring upgrade job. Current routed media kinds in DEV/PROD are fully upgraded with zero current failures. Preserve historical database records and applied SQL, including an old DEV failure outside current namespaces. Older installations must finish conversion using v0.119.1 before this upgrade; old-format refusal remains explicit.

## Keep the durable boundaries

- Placement validates and hashes staged uploads before producers consume immutable sources.
- Image and video processing retain their own resource/concurrency profiles.
- Folder deletion bridges committed database deletion to object storage and fences later writes.
- Deadline cleanup processes exact recorded obligations, including delayed uploads.
- Processors append retained progress/completion events and enqueue real version metadata work in one transaction.
- The existing version metadata worker updates facts, facets, covers and reader snapshots.
- Recovery finishes known interrupted storage commits and integration effects.

## Adoption and verification

Retire old queued video stages through a forward worker migration while preserving completed history and successful checkpoints. Do not edit applied migrations. Coordinate old producer/worker retirement, the new public library/worker images and KEDA selectors; an empty-queue snapshot alone is insufficient for a rolling cutover.

Adapt existing real PostgreSQL/S3/FFmpeg tests for checkpoint resume, progressive playback, fairness, force consumption, stale sources, missing temporary outputs, cancellation and output formats. Verify the pinned River snooze behavior and availability after worker restart. Keep declared public/private storage, explicit deletion and late-write lifecycle assertions when removing callback-only tests.

## Complete media before release

Ingest validates and attaches sources. A single derivative job handles image
presets and resumable video outputs, including mixed-media versions. Generated
version outputs remain in `future/`. Publish records intent and the existing
release worker waits for all required outputs, copies them into their public or
private destinations and activates the completed snapshot. The previous completed
release remains readable while a replacement is processing.

No separate playable state, daily playable scan or version-publish preparation
worker remains. Page order and automatic cover inputs are prepared by the
derivative attempt; normal processing completion wakes the release request.
Admin SSE progress may show partial work without exposing partial reader content.

Before adopting the hard cut, finish any accepted old image/video/preparation
jobs with the previous worker. Existing job history and source files remain; no
compatibility handler or new runtime obsolete-job guard is registered.
