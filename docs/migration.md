# Database initialization

From 0.141.0 each migration set is one baseline, and later schema changes are numbered migrations after it:

| Store | Embedded filesystem | File | Contents |
|---|---|---|---|
| PostgreSQL catalog | `migrations.Postgres` | `postgres/0001_baseline.up.sql` | content, versions, entities, memberships, facets, the event log, import ids, codes, interactions, polls, location rules, search, reading state, erasure and media state |
| PostgreSQL accounts | `migrations.Accounts` | `accounts/0001_baseline.up.sql` | account media, avatar placement, account erasure records and provisioned signal planes |
| PostgreSQL media worker | `internal/media/workqueue` | `migrations/0001_baseline.up.sql` | video conversion runs and their chunks, beside River's tables |
| ClickHouse | `migrations.ClickHouse` | `clickhouse/0001_activity.up.sql` | the activity log, `content_daily`, `audience_daily` and erasure fences |

Released migrations are immutable: a schema change is a new migration, and CI refuses a change to any migration the latest release shipped (`scripts/check-released-migrations.sh`). A version id is NULL for the work itself in PostgreSQL and `''` in ClickHouse, whose sorting keys hold it.

A database built before 0.141.0, or whose ledger records a migration this release does not ship byte for byte, is never converted: `MigratePostgres` and a `SkipMigrations` start refuse it, naming the schema and the rebuild. That refusal is not `ErrMigrationsRequired`, since migrating cannot fix it.

## Rebuilding a database from before 0.141.0

Every ContentKit database an earlier release built is rebuilt. Stop the host and its media workers, then drop ContentKit's PostgreSQL schemas (the catalog, its River schema `<catalog>_river`, the account schema, which the tenant's apps share, and each media worker schema, the tenant's account-media `ck_account_<tenant>` included) and their migration records:

```sql
DROP SCHEMA <catalog>, <catalog>_river, <accounts>, <media_worker>, ck_account_<tenant> CASCADE;
DELETE FROM public.migrations
WHERE app IN ('contentkit', 'contentkit-accounts', 'contentkit-media-worker')
  AND database = 'postgres' AND schema IN ('<catalog>', '<accounts>', '<media_worker>', 'ck_account_<tenant>');
```

Drop the ClickHouse database too, with its records (`app = 'contentkit_signal' AND database = 'clickhouse' AND schema = '<signal database>'`). Then run the host's migration command (`MigratePostgres`, then `MigrateClickHouse`), start media on a new bucket ([media model](#media-model)) and import the content again.

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

## Media model

Media's bucket is now the only record of an item's files (README "Media"). The media journal, allocations, cleanup, processing, publication, publication-request, quota, lock and delivery tables are gone; `content_media_items` caches each item's manifest. Stable avatar names, MP4 download renditions, ZIP presets, upload quotas (`UploadLimits`, `Channels.Uploads`) and media restore are gone; `MEDIA_GATEWAY_DEFAULTS` is no longer read.

The bucket must never have had versioning, and an existing bucket's media is not carried over: start the release on a new, empty bucket created without versioning (`MEDIA_S3_BUCKET`), run `media-worker publish-defaults`, then import the media again. Delete the old bucket once the site is verified.

Keys no longer carry `tenant/{tenant}/app/{app}/` or `tenant/{tenant}/account/`: the bucket is the tenancy boundary, one per app. Items are `{kind}/{id}/` and account media `_accounts/{kind}/{id}/` in the avatar primary's bucket. `media.Config.Namespace`, `media.Kind.Namespace` and `PresetRule.namespace` are gone (`Config.Scope` is required; `avatar.User()` is an `Account` kind), `layout.Scope` keeps only `Partition`, and `MEDIA_GATEWAY_ROUTES` entries are `{host, bucket, accounts, origins}`, one per host. Item tokens are bound to the bucket (`token.ItemScope(bucket, prefix)`), so tokens signed before the upgrade stop opening files. Reading an earlier bucket is the importer's own code; ContentKit parses no other layout.

A video kind's download streams its original as uploaded (its master under the identity recipe): drop MP4 presets from the registry. Manifests record each upload's `master` and each rendition's `input`; with no `master` recipe the master is the original and nothing more is stored.

## Time-ordered ids

ContentKit mints its row, operation and object ids as UUIDv7s, so new rows append to their indexes; generated ids default to `uuidv7()`. Media lifecycle ids (erasure, purge, folder retirement, delivery adoption and withdrawal, archive ingestion) are deterministic UUIDv7s instead of name-based UUIDv5s, so one retried across the upgrade derives a new id and runs again under it.

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
reactions, favorites, reading state and history, source erasure fences,
and the ClickHouse rollups (`content_daily`, `audience_daily`). Search
documents and group counts are rebuildable; the raw activity log lives 90
days. A ClickHouse database migrated by an earlier baseline fails its
migration check: recreate it. See [restore.md](restore.md).
