# One publication workflow

Posts, galleries, videos and other content kinds use the same `contentkit_publication` job, one per version. Scheduling does not depend on whether an item has attachments.

Accepting publication wakes the job in the content write's transaction. The job plans where each of the version's files belongs now, moves them (README "Media": a release is a move), records the release in the catalog (availability, released chapters, the access it was released under, retained publication events) and snoozes until the next chapter or audience deadline. A version without media follows the same lifecycle without storage calls. A storage error never turns a configured media release into an empty-file success. A run that finds nothing to move writes nothing.

Readers see a version from the later of its completed release and its scheduled publication. Re-scheduling a released item hides it at once, before the job withdraws the earlier release; re-accepting an open publication keeps its date.

Processing remains separate from publication. Ingest, image and video workers write manifests, and every manifest write wakes the version's release. There is no publication sweep or readiness polling job.

Jobs carry the content and version IDs and run in the catalog-scoped `contentkit_publication_<catalog hash>` queue. A job whose version is gone completes. The content-purge schedule is independent of publication.
