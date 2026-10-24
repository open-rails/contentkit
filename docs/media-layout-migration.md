# Tenant and app media layout

A scoped engine derives object paths from its immutable `Config.Scope`:

- App content, versions, group covers and defaults: `tenant/<tenant-id>/app/<app-id>/<kind>/<item-id>[/<version-id>]/…`.
- Tenant account avatars: `tenant/<tenant-id>/account/<kind>/<account-id>/…`.

`private`, `download`, `public`, `temp` and `manifest.json` retain their meanings inside the item folder. The media registry carries the scope into signed uploads, private/download access tokens, work items, quotas, manifests and public-image rules. The internal database partition includes both tenant and app IDs and is not an object path. Different apps may reuse a kind or item ID without sharing media.

Scoped config needs no `Namespace`. Set `Accounts.Schema` and, when enabled, `Accounts.Avatars`; legacy unscoped configurations remain supported. `MigrationOptions.LegacyAccountKey` explicitly associates an existing account inbox key with the tenant during provisioning. Runtime startup reads that immutable association, preserving existing notification IDs, read/archive state and erasure fences. It never reassigns the key.

The avatar primary owns the canonical bucket writes. Its journal, allocations, public-image projection and quota state live in `Accounts.Schema`; its processing queue is tenant-owned. `Config.AccountMedia()` returns the schema and worker schema for `worker.Config.AccountSchema` and `AccountWorkerSchema`. Explicit migration records one primary AppID as deployment placement for the tenant; matching replicas may start, and another app must forward. A long-running primary worker drains both its app queue and this account queue. A secondary forwards authenticated writes and owns neither avatar state nor account processing jobs. App-only erasure leaves these tenant account avatars intact.

For one-shot workers, start a separate account worker with `ContentSchema` and `Schema` set to the returned account storage and queue, and omit the additional account fields. Worker processes apply no migrations. The explicit host migration command prepares both queues.

## Gateway deployment

`gateway.Config.Routes` authorizes each exact `{Host, Scope, Bucket, Origins}` combination. Account scopes omit `AppID`; app scopes include it. Legacy `Hosts`/`Buckets` continue to authorize only legacy paths, so enabling a legacy route cannot expose scoped objects accidentally. The stock gateway accepts the routes as JSON in `MEDIA_GATEWAY_ROUTES` (or `-routes`). Use each route's exact site origins for CORS. Scoped routes check a reserved per-item erasure fence before serving bytes. The gateway read credential must be able to HEAD this public object and distinguish a missing object (404) from forbidden access; a forbidden fence read fails closed. This normally includes ListBucket permission for the served prefixes. The fence prevents a late, previously signed upload from becoming readable under an old token after account erasure. Deploy gateway routes and the new worker before switching writers.

## Copy and cutover

Stop the affected writers and drain their media jobs. Keep the original buckets and registry configuration available. The reusable operations are:

1. Build explicit `media.LayoutMapping{From, To, Owner}` entries from catalog/account ownership. Preserve a charged app/group quota owner in `Owner`; account kinds default to the account ID. Call `media.PlanLayoutMigration(ctx, sourceManifests, destinationRegistry, mappings)` and save the JSON plan outside the bucket. This reads and hashes every referenced blob, editor view and public rendition. It rejects unresolved commits, staged uploads and pending producers. Migrate pre-v0.68 manifests with ContentKit v0.119.1 before planning.
2. Call `media.ApplyLayoutMigration(ctx, sourceManifests, destinationJobs, plan)`. It compares source ETags and content, copies only missing objects with conditional writes, verifies SHA-256 bytes, and adopts each destination manifest through the normal journal protocol. Existing different bytes are refused. It is safe to repeat the same plan after interruption. Originals are never deleted.
3. Check the resulting API and gateway reads, then switch the scoped writer to the new layout. Retain the plan and originals throughout the rollback window.

A legacy versionless cover needs explicit mappings to the preexisting versions that inherit its cover metadata. The planner supports a versionless legacy source even when the current kind uses version folders. The operator must determine the correct versions from catalog records; the media library does not guess. Complete and verify the copy before clearing legacy item-cover metadata or finishing its database migration.

Before upgrading to metadata-only reader snapshots (0.114.x), an app with an
existing scope binding must have completed this verified scope copy. The binding
alone does not prove its objects moved. If only the legacy root exists, finish
the copy and cutover above with the previous release, then retry the upgrade.
The reader migration selects the exact bound app root and preserves legacy
journal history; it refuses missing, foreign or malformed bound ownership.
Unbound installations retain their existing unique-source layout migration path.
Already-applied reader migrations keep their original recorded bytes and digest.

## Rollback

`Media.LegacyLayout` is temporary migration compatibility: ownership and authorization remain scoped while object paths use the retained legacy catalog/account keys. It is not a safe rollback operation by itself.

Keep the saved pre-migration media worker registry for rollback; a pure scoped `Config.MediaKinds()` cannot infer an adopted legacy avatar key from PostgreSQL and refuses to guess it. The scoped API engine resolves that key at startup.

With all affected writers stopped, call `media.VerifyLayoutRollback(ctx, sourceManifests, destinationJobs, plan)` before switching readers/writers back. It requires unchanged originals and destination manifests, validates copied bytes, and rejects additional canonical objects, outstanding commits or upload reservations. This catches post-cutover edits and new uploads that a simple flag change would hide. If verification fails, build and verify a reverse-copy plan for the new state instead. Keep originals; do not delete either layout as part of cutover or rollback.
