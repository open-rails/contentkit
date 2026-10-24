# Media jobs

Media job model for ContentKit #242. Deployment requires the coordinated adoption described below.

Media processing uses four worker job kinds: placement, image, audio and video. Host integration uses five job kinds: known-deadline cleanup, folder deletion, public exposure, readiness and interrupted-commit recovery. This reduces the current thirteen kinds to nine.

## One resumable video conversion

`contentkit_media_video` runs on `media_video`. One River row owns a video conversion request through planning, encoding and rendition publication. Chunking is an internal checkpoint mechanism, not a separate River job graph.

Each attempt validates the current source/preset, completes bounded missing work and records successful outputs. Completed chunks survive interruption. Finished lower renditions become playable before later renditions finish. The same job yields at chunk boundaries so shorter waiting conversions get a turn; worker resource limits still bound CPU and scratch use. A force request is consumed once, and progressive readiness is delivered even when the job yields.

Keep the existing durable encoding checkpoints and source/incarnation checks. Remove child-job fan-out, separate assembly scheduling, light/encode queue aliases and unused one-shot worker selection. Preserve cancellation, stale-source rejection, temporary-output recovery and idempotent publication.

## Remove obsolete host work

Remove the unused CDN-purge callback/relay surface. Current D/H hosts do not configure it, so retiring objects already produces no CDN invalidation through that callback. Actual storage deletion and public exposure remain separate durable operations; cache policy is unchanged.

Remove the legacy pre-v0.68 manifest conversion runtime and its recurring upgrade job. Current routed media kinds in DEV/PROD are fully upgraded with zero current failures. Preserve historical database records and applied SQL, including an old DEV failure outside current namespaces. Older installations must finish conversion using v0.119.1 before this upgrade; old-format refusal remains explicit.

## Keep the durable boundaries

- Placement validates and hashes staged uploads before producers consume immutable sources.
- Image and audio processing retain their own resource/concurrency profiles.
- Folder deletion bridges committed database deletion to object storage and fences later writes.
- Deadline cleanup processes exact recorded obligations, including delayed uploads.
- Exposure reconciles actual public objects with canonical visibility.
- Readiness hands worker output to catalog facts/import/publication.
- Recovery finishes known interrupted storage commits and integration effects.

## Adoption and verification

Retire old queued video stages through a forward worker migration while preserving completed history and successful checkpoints. Do not edit applied migrations. Coordinate old producer/worker retirement, the new public library/worker images and KEDA selectors; an empty-queue snapshot alone is insufficient for a rolling cutover.

Adapt existing real PostgreSQL/S3/FFmpeg tests for checkpoint resume, progressive playback, fairness, force consumption, stale sources, missing temporary outputs, cancellation and output formats. Verify the pinned River snooze behavior and availability after worker restart. Keep physical visibility/deletion and late-write lifecycle assertions when removing callback-only tests.
