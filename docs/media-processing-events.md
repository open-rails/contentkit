# Public media and processing events

Each gallery edition owns one media folder and one audience access decision.
Uploads and prepared reader outputs wait in `internal`. The release job moves
released files into `public` or `private`, writes the manifest naming them and
retires the `internal` copies. A private token opens the exact edition's whole
private folder, never a sibling edition. See
[chapter publication](chapter-publication.md) for ordered chapter markers.

The item cache records meaningful progress and completion in the retained event
log in the transaction that records the manifest write. Current status remains available
through the existing media HTTP and Go interfaces. Observers own their cursors;
retention expiry requires a fresh current-state read.

Every manifest write also wakes the version's release. Derivative preparation
belongs to the processing attempt; there is no separate version metadata worker.
Only a release's manifest write changes reader snapshots and release-dependent
catalog state. Processing remains independent of an editor's publication
instruction. There is no source-managed event consumer.

## Observation interfaces

Application facts use `Client.EventReader()` and the existing events HTTP route.
Types are `media.processing_progressed` and `media.processing_completed`.
Records include kind, content/version IDs, current state, upload/archive/preset
counts, failure count and playability. Completion may be ready or a settled
failure. No frame-by-frame encoding updates or delivery credentials
are appended. The normal media status endpoint still exposes current encoding
progress.

Shared avatar processing records one fact in its tenant account schema.
`Engine.AccountMediaEvents()` returns a configured `eventlog.Reader`; it never
copies these facts into each application's log. Trusted host account integrations
own their cursors. Retention is independent of observers and completion receipts
survive log expiry.

The stock worker needs no host callbacks. Each manifest write it makes wakes the
version's `contentkit_publication` job. A release is all or nothing: while the
version's uploads process it changes nothing, and the write that settles them
releases every ready page. If a release is running, its follow-up preserves the
wakeup. Queues are scoped to the exact catalog key, including when several apps
share one River schema.

Removed interfaces: `media.Jobs.Expose`, `ExposeTx` on media/version/entity
folders, `media.HostQueue`, `worker.Config.HostQueue` and `MEDIA_HOST_QUEUE`.
Keep `HostSchema` / `MEDIA_HOST_RIVER_SCHEMA` for the publication queue. The stock
worker inserts these jobs itself, so its image must use the host's job kind and
queue: deploy it from the same release when either changes.

## Live editor status

The existing `GET /v1/media/{kind}/{id}?status=1` and version route support
`Accept: text/event-stream`. Ordinary JSON and `GetMediaVersion(...Status:true)`
remain current-state queries. Go `Client.WatchMedia` streams the same snapshots
through the configured HTTP transport, including the embedded engine client.

Each connection emits an authorized `snapshot`. Item/version-matching retained
facts trigger `progress` snapshots; other owners' facts never leave the handler.
The engine selects its app log or shared account log from the registered media
kind. Snapshot payloads contain no signed URLs, cookies or new work.

SSE IDs are retained-log cursors. Reconnect with `Last-Event-ID` or `after`;
retention expiry emits `resync` with a current snapshot and new cursor. Reading
the boundary before the snapshot prevents lost concurrent completions. An
invalid or differently scoped cursor is refused. Each observer owns its cursor;
there is no registry, delivery job, acknowledgment or global poll coordinator.
The request reads bounded retained pages, and writes one bounded snapshot at a
time with a write deadline and a heartbeat every ten seconds. While processing,
the heartbeat also reads canonical status so existing encoder segment/phase
checkpoints can update the UI before a manifest rendition is complete. A
timestamp-only encoder change does not emit progress; no producer or delivery
job is added. Write deadlines are cleared between messages, including while
catching up retained pages.

Live permission and credential checks run before subsequent observations and
heartbeats without replaying the request's sender proof. Expiry, revocation,
account membership changes or loss of editor access terminate observation.
Authenticators must provide the existing live `auth.PermissionChecker` contract;
identity-only credentials cannot open a long-lived stream. Cancellation and
response-body close release request resources, including embedded Go pipes.
