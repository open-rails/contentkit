# Media ingestion

Uploads, processing and publication are separate actions. Create the content/version, presign and upload source files, then commit through the ordinary media upload API. Commit records durable work and returns without waiting for extraction or renditions. Editors can poll the ordinary media status read. Publishing remains an explicit catalog API action.

`contentkit_media_ingest` on `media_ingest` owns staged placement and ZIP expansion. It then schedules image/video processing. There is no separate host ZIP-import job or upload-and-publish operation.

## Configure ZIP sources

A media upload accepting `application/zip` can set `archive_to` to another configured `{name}` image upload. This works without a catalog or reader kind, including named body-image destinations. Existing reader `Publish.Import` / `Publish.Pages` mappings are also supported.

The existing upload authorizer must explicitly return `UploadGrant.ArchiveTo` equal to that destination pattern. Permission to store the ZIP alone does not authorize arbitrary generated filenames. The destination, quota owner and accepted quota policy are recorded by the server on the source file; clients cannot supply this admission through upload metadata. Default ContentKit version/entity editors already have full item authority and approve their configured destination. Custom path-sensitive authorizers must make that decision explicitly.

## Guarantees

- Source ZIP bytes remain untouched and retained; page originals keep their original image formats.
- Central-directory allocations, total entries, image count and actual expanded bytes are bounded before attachment. Every selected entry's CRC and accepted image type are checked.
- Decoded originals are reserved against the accepted quota, including the normal per-upload minimum charge, before page objects are written. The final manifest edit charges only new originals, once.
- New objects use the existing allocation journal. One conditional manifest edit attaches the whole archive; failed validation or storage writes cannot leave a partial new gallery. Exact cleanup owns unused allocations and uncertain writes.
- Natural ordering, filename cleanup, source hashes/entry positions and page QA metadata are retained. Existing pages reused from the same archive are not duplicated or charged again.
- Metadata edits to an unchanged ZIP preserve server-owned completion state. Reusing the same blob under another ZIP path has a separate completion receipt.
- The accepting commit identifies each admitted ZIP generation. Exact retries and metadata edits retain that identity; removing and re-adding a ZIP from a retained alias creates fresh processing work. Copies obtain the destination's own permission and quota policy rather than transferring the source's admission.
- Unattached ZIP uploads process before Save; their extracted pages remain unattached too. Attaching the ZIP rechecks destination permission and attaches its staged pages atomically. Cancelling or replacing an unattached ZIP removes only its unused pages, preserving pages already adopted by the editor and batches retained by another ZIP alias.
- Normal cancellation closes and removes local scratch files. Use an isolated worker scratch directory; pod ephemeral-storage limits/lifetime bound scratch left after a hard kill.
- Scratch-disk failures and interrupted writes remain retryable infrastructure errors; malformed archives are reported as failed source uploads.

Old unfinished ZIP uploads without a verified admission require an authorized re-commit. They are not silently processed as an exempt system user.

## Upgrade

Stop old producers and workers before worker migration0004. It converts nonterminal placement requests to ingestion while preserving their row IDs and completed history. The host's explicit migration step transfers valid old `contentkit_version_import` requests into its configured external worker schema in the same transaction that cancels their old rows. A running or ambiguous old request is an actionable refusal. Repeated handoff does not duplicate ingestion requests.

Image processing still owns pixel/frame limits, the application's original-size ceiling, page analysis and renditions. Video processing retains its existing codecs/checkpoints. Audio pass-through is a separate adoption; with that change the three processing kinds are ingest, image and video. Other metadata, readiness and cleanup jobs are not renamed to hide them.
