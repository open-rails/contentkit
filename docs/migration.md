# Database initialization

Each store has one baseline; later schema changes are numbered migrations after it:

| Store | Embedded filesystem | File | Contents |
|---|---|---|---|
| PostgreSQL catalog | `migrations.Postgres` | `postgres/0001_baseline.up.sql` | content, versions, entities, memberships, facets, the event log, import ids, codes, interactions, polls, search, preferences, erasure and media state |
| PostgreSQL accounts | `migrations.Accounts` | `accounts/0001_baseline.up.sql` | account media, avatar placement and account erasure records |
| ClickHouse | `migrations.ClickHouse` | `clickhouse/0001_baseline.up.sql` | signals, measured engagement, exposures, popularity and erasure fences |

No checksum is silently accepted or rewritten; a database recorded by an
earlier migration history is recreated, not converted. A version id is NULL for
the work itself in PostgreSQL and `''` in ClickHouse, whose sorting keys hold it.

Account keys are derived from `Config.Scope.TenantID` as `tenant_<TenantID>`.
The accounts migration `0002_remove_account_bindings.up.sql` removes
ContentKit's obsolete key-binding table and its three dependent foreign keys.
AuthKit's inbox tables are unchanged. No account-key conversion or row rewrite
runs during migration or startup.

## Content model configuration

Content model files use `content_kinds` alongside `entity_kinds`. Rename the old
`kinds` configuration key before loading this release; YAML and JSON decoding
reject it rather than silently ignoring it or accepting a legacy alias. The Go
member remains `kinds.Config.Kinds`. This does not rename the `kinds` field in the
public `GET /kinds` discovery response or any HTTP query parameters.

## Publication job

The publication job is `contentkit_publication` in the queue
`contentkit_publication_<catalog hash>`, formerly `contentkit_version_release`
in `contentkit_versions_<catalog hash>`. The stock media worker inserts these
jobs, so deploy it from the same release as the host. A database from the
earlier migration history is recreated and re-imported; the import schedules
every publication again, and old jobs name requests that no longer exist. If the
host's River schema is kept, delete them after the old workers stop:

```sql
DELETE FROM <river_schema>.river_job
WHERE kind = 'contentkit_version_release' AND finalized_at IS NULL;
```

## Event retention

Catalog events are kept fourteen days by default, not ninety (`event_retention`). The next catalog maintenance prunes the older ones. A reader whose cursor is older gets `event_cursor_expired` (`eventlog.ErrExpiredCursor`), resyncs current state and reads on without a cursor, as on any expiry.

## Favorites keys

Catalog migrations `0072`–`0074` rebuild `content_favorites` with `uuid` keys: `app_id` replaces the text `tenant_id`, and `user_id`, `content_id` and `content_version_id` become `uuid`. The five indexes keep their names, led by `app_id`. A favorite needs a UUID account id, live and imported. `0072` refuses a row whose id is not a canonical lowercase UUID and names it; correct or delete such rows and migrate again.

The table is copied, not altered, so writers keep writing during the copy:

1. `0072` checks every id, creates the new table and a trigger that notes each key written to the old one. Its scan holds only `ACCESS SHARE`; creating the trigger waits for in-flight writes (`lock_timeout` 10s).
2. `0073` runs outside a transaction: it copies the rows in batches of 4096 pages, each committed on its own, rewrites the noted keys and indexes the copy. Nothing waits on it. If it fails, resolve it with `migratekit repair resolve` and migrate again; it starts over.
3. `0074` locks the old table (`ACCESS EXCLUSIVE`, `lock_timeout` 10s), rewrites the keys written since `0073`, drops it and renames the copy. The lock lasts as long as those keys take, normally well under a second; a timeout fails only this step.

At 15.1M rows (2.7 GB of heap, 9.7 GB of indexes) expect about 30–60 s for the check, 1–3 minutes for the copy, 2–5 minutes for the indexes at the default `maintenance_work_mem` and under a second for the swap: 4–9 minutes and 6–8 GB of WAL. The table ends at about 6 GB (2 GB heap, 4 GB indexes). The previous release cannot read it, so roll the hosts forward with the migration.
## Media model

Media's bucket is now the only record of an item's files (README "Media"). Migrations `postgres/0076_bucket_media_model` and `accounts/0004_bucket_media_model` drop the media journal, allocations, cleanup, processing, publication, publication-request, quota, lock and delivery tables and create the cache `content_media_items`. Stable avatar names, MP4 download renditions, ZIP presets, upload quotas (`UploadLimits`, `Channels.Uploads`) and media restore are gone; `MEDIA_GATEWAY_DEFAULTS` is no longer read.

The bucket must never have had versioning. Move a host's media into a new one, with the old workers and engine stopped:

1. Create the new bucket without versioning, and point the worker's `MEDIA_S3_BUCKET` at it.
2. Run `media-worker move` with `MEDIA_MOVE_SOURCE_BUCKET` naming the old bucket (same endpoint and key), `MEDIA_KINDS_FILE` and `MEDIA_CONTENT_SCHEMA`. It copies each item's live files server-side under their hash names, writes its new manifest, verifies every size and fills the cache. It only reads the old bucket, and re-running it skips moved items.
3. Run `media-worker publish-defaults`, then start the release's engine, worker and gateway on the new bucket. Releases re-run from the cache.
4. Delete the old bucket once the site is verified.

A video kind's download serves its original as uploaded: drop MP4 presets from the registry.

## Time-ordered ids

ContentKit mints its row, operation and object ids as UUIDv7s, so new rows append to their indexes. Catalog migration `0075` and media-worker migration `0005` change the remaining `gen_random_uuid()` column defaults to `uuidv7()`; existing rows keep their ids. Media lifecycle ids (erasure, purge, folder retirement, delivery adoption and withdrawal, archive ingestion) are deterministic UUIDv7s instead of name-based UUIDv5s, so one retried across the upgrade derives a new id and runs again under it.

The media journal keys a commit by a hash of its `operation_id`, never the id as sent, so no caller can claim an id a lifecycle operation derives. Send a UUIDv7 (`contentref.NewID`, Postgres `uuidv7()`) to keep that index ordered; a UUIDv4 is still accepted.

## Channels

Channels are configured under `channels`. A content kind declares
`channel: {fixed: blog}`, `channel: {required: true}` or an empty optional
policy; metadata kinds declare no owner or permission-group policies. Content
HTTP ownership is `channel_id`, and channels have their own `/channels`
interface.

Channel create/delete/restore calls AuthKit synchronously. A failed call
returns an error and the caller retries against the same identity. No
`contentkit_entity_sync` or lifecycle worker exists. The content-purge sweep
owns physical retention cleanup. Imported channel identifiers have their own
record category. Reposts reference their original item and retain its access,
versions and editorial authority.

## Authored content belongs to versions

Write initial text through `ContentInput.versions[].text` and update an edition
through `PATCH /contents/{id}/versions/{vid}`. The former parent-text routes and
version-fork endpoint are removed. Hidden-version post kinds still create one
version; callers may supply that initial version explicitly. Additional versions
start empty. The content reader's title/body/image fields are read-only projections
from its `selected_version_id`, with no parent or sibling content fallback.

## ContentKit's own schema

Hosts run `MigratePostgres(ctx, pool, cfg)` and
`MigrateClickHouse(ctx, pool, cfg)` from their explicit migration command.
The PostgreSQL entrypoint prepares the configured catalog, account storage,
scope bindings and queues; `MigrationOptions{SkipQueues: true}` skips queue
DDL when the caller manages that separately. The ClickHouse entrypoint creates
its target database before applying the signal lineage.

The PostgreSQL call also realizes `content_kinds.<kind>.channel.fixed` declarations
from the content model. It creates missing staff channels and binds eligible
unowned posts, preserving existing names, identities, grants and access. Every
channel has an AuthKit group; provide `MigrationOptions.Scopes` using the existing
runtime provider or `adapters/authkit.MigrationScopes(pool, authConfig, scope)`.
The latter needs only migrated AuthKit storage and the host's role configuration,
not a full authentication runtime. No fixed channels means this migration performs no configured-channel provisioning. See [configured channels](posts-and-channels.md#one-fixed-official-channel)
for retry and conflict behavior. This provisioning runs only through the explicit
migration call, never ordinary runtime initialization.


Set `Config.Database.SkipMigrations = true` for application startup. `New`
then checks prepared storage without applying migrations, including when the
signal plane reconnects. Missing migrations produce an actionable error; an
unavailable optional signal plane retains its existing degraded behavior.
The zero value keeps automatic migration for older embedded callers. Doujins
and Hentai0 use explicit migration commands and validation-only startup.

The catalog schema is ContentKit's alone: it may share the host's database,
never the host's schema. Account storage can use a separate schema shared by
the tenant's apps; it does not merge their catalogs.

Explicit migration is idempotent through MigrateKit's ledger, and concurrent
migration commands take turns on its lock. PostgreSQL uses
app identity `contentkit`; ClickHouse defaults to `contentkit_signal`. The
ClickHouse ledger also binds the explicit `Database`, so two databases may
share the same app identity and PostgreSQL tracker. Each PostgreSQL tracker
represents one logical ClickHouse deployment.
MigrateKit owns tracking and locks in PostgreSQL `public.migrations`; this is
migration metadata, separate from ContentKit's application tables.

PostgreSQL commits its migrations before ClickHouse begins. There
is no cross-store transaction, and ClickHouse DDL can partially succeed.
MigrateKit records the exact filename and source digest before starting the
ClickHouse migration; rerunning the identical baseline retries its idempotent
DDL. A changed source or an old unbound ClickHouse ledger is rejected. A
failed ClickHouse step does not roll back PostgreSQL migrations.

## Extensions and runtime privileges

The PostgreSQL baseline requires `pg_trgm` and PGroonga, installed explicitly
in `public` so several host schemas can share them. Provision extensions with
an administrative role if the migration role cannot install them. No vector
extension is required. Migrations use the selected schema followed by `public`
in the transaction's search path; function and index dependencies resolve to
the shared extensions.

Hosts own database roles and grants. Give the runtime role `USAGE` on its
schema, required table privileges and sequence usage; keep DDL privileges on
the migration role. ContentKit does not create application roles or grant
access to another application's schema. Migration SQL contains the complete
indexes, checks, internal foreign keys and triggers. The runtime role also needs
EXECUTE on `contentkit_content_code()` (PUBLIC by default).

## Host relationships

ContentKit enforces its internal foreign keys. Host-owned content and actor
IDs are opaque because host table names, identifier types and deletion
lifecycles vary. Hosts add no foreign keys, grants or queries on ContentKit's
schema: ContentKit can run on its own database. Host rows keep ContentKit ids
as plain values and repair them from reads and events
([host references](../HOST_INTEGRATION.md#host-references)). ClickHouse
references have no cross-store FK enforcement.

## Retained state

Back up authored content, moderation and retained-publication state,
reactions, favorites and their revision sequence, source erasure fences,
and the ClickHouse signal plane. Search documents and group counts are
rebuildable. The baseline contains only current signal tables. See [restore.md](restore.md).
