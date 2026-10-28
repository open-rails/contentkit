# One publication workflow

Posts, galleries, videos and other content kinds use the same durable
`content_publication_requests` lifecycle and `contentkit_publication` worker.
Scheduling does not depend on whether an item has attachments.

Accepting publication commits its request and River job in the content write's
transaction. The worker plans the current edition, performs any required file
copies and verification, activates the completed reader state, records retained
publication events and schedules the next chapter or audience deadline.
An empty file plan follows the same lifecycle without storage calls. A storage
error never turns a configured media release into an empty-file success.

Processing remains separate from publication. Ingest, image and video workers
prepare files and wake the publication request when prerequisites are ready.
There is no five-minute publication sweep or readiness polling job.

Jobs carry the request ID, due time and follow-up and run in the catalog-scoped
`contentkit_publication_<catalog hash>` queue. A job whose request row is gone
cancels itself. The content-purge schedule is independent of publication.

Implementation and upgrade acceptance are tracked in
[ContentKit #254](https://github.com/open-rails/tracker/blob/master/contentkit/0254-unified-content-release.md).
