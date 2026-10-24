# Public media and processing events

Public/private storage is a property of the declared media preset. Public
covers and previews remain public before release and after unpublishing.
Private originals and reader pages continue to require scoped tokens.
Publication, billing and access changes do not copy, hide or regenerate media.
Explicit replacement/removal and permanent purge still retire obsolete files.

The processor records meaningful progress and completion in the retained event
log before its processing job completes. Current status remains available
through the existing media HTTP and Go interfaces. Observers own their cursors;
retention expiry requires a fresh current-state read.

Completion also wakes the existing version metadata worker in the same database
transaction. That worker records media facts, refreshes listing facets, builds
immutable reader snapshots and applies the selected cover. It performs real
metadata work, including before unrelated processing finishes. There is no
readiness delivery job or source-managed event consumer.

This change requires a stopped-writer upgrade: stop older hosts and media
workers before applying the new migrations and restart the matching release.
Published migration files must not be rewritten. The upgrade must retire old
waiting exposure/readiness jobs without losing pending metadata work.

Implementation and verification are tracked in ContentKit #247.

## Observation interfaces

Application facts use `Client.EventReader()` and the existing events HTTP route.
Types are `media.processing_progressed` and `media.processing_completed`.
Records include kind, content/version IDs, current state, upload/archive/preset
counts, failure count and playability. Completion may be ready or a settled
failure/full state. No frame-by-frame encoding updates or delivery credentials
are appended. The normal media status endpoint still exposes current encoding
progress.

Shared avatar processing records one fact in its tenant account schema.
`Engine.AccountMediaEvents()` returns a configured `eventlog.Reader`; it never
copies these facts into each application's log. Trusted host account integrations
own their cursors. Retention is independent of observers and completion receipts
survive log expiry.

Custom embedded `Hooks.ItemReady` and `Hooks.ItemProgressed` run synchronously in
the processor's completion transaction. They must be idempotent transactional
extensions. Stock external workers need no hooks: completion directly queues
`contentkit_version_publish`, whose real metadata work updates facts/facets,
reader snapshots and the selected cover. If an earlier publisher is running,
its normal follow-up job preserves the wakeup.

Removed interfaces: `media.Jobs.Expose`, `ExposeTx` on media/version/entity
folders, `media.HostQueue`, `worker.Config.HostQueue` and `MEDIA_HOST_QUEUE`.
Keep `HostSchema` / `MEDIA_HOST_RIVER_SCHEMA` for the metadata work queue.
Standalone integrations run `media.MigrateProcessing` alongside their explicit
migration step. It refuses running old lifecycle jobs, retires only owned
waiting rows, preserves history and other namespaces, and queues one normal
image pass per known item using an existing journal receipt. No bucket discovery
or recurring backfill job is installed.

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
time with a write deadline and a heartbeat every ten seconds.

Live permission and credential checks run before subsequent observations and
heartbeats without replaying the request's sender proof. Expiry, revocation,
account membership changes or loss of editor access terminate observation.
Authenticators must provide the existing live `auth.PermissionChecker` contract;
identity-only credentials cannot open a long-lived stream. Cancellation and
response-body close release request resources, including embedded Go pipes.
