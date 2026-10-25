# Media work ownership

This draft implements issue 247: ingest owns durable commit completion; image and video jobs own their writes; the existing content purge sweep owns physical deletion and retries.

A commit records its exact next manifest, admitted quota, and an ingest job in one PostgreSQL transaction before sending the conditional object-store write. The request attempts the write immediately. An interrupted request leaves that same ingest job able to finish the admitted mutation without replaying client input against a newer manifest.

Deletion intents and exact retired keys remain in PostgreSQL until storage succeeds. The content purge sweep handles versions, entity and poll folders, abandoned allocations, replaced outputs, and bounded late writes. Temporary objects and unfinished multipart uploads retain their existing object-store lifetime.

No periodic media recovery, media cleanup job, or folder-deletion queue remains in the target design. This document describes implementation in progress; validation receipts will be recorded in the pull request.
