# Database initialization

Each store has a fresh-install baseline, followed by versioned upgrades:

| Store | Embedded filesystem | File | Contents |
|---|---|---|---|
| PostgreSQL | `migrations.Postgres` | `postgres/0001_baseline.up.sql` | content, versions, entities, memberships, facets, offers, the outbox, import ids, codes, interactions, polls, search, preferences, erasure and media state |
| ClickHouse | `migrations.ClickHouse` | `clickhouse/0001_baseline.up.sql` | signals, subject state, daily contributions, exposures, co-engagement and erasure fences |

The PostgreSQL migrator also recognizes the exact baseline shipped before
version-owned covers (`79d1bcc`, SHA-256 `5f1614f38022…`). Its immutable bytes
remain embedded under `migrations/compat`; installations that recorded it keep
that truthful ledger entry and apply the numbered upgrade steps. No checksum
is silently accepted or rewritten. Other historical baseline hashes require
an explicitly supported conversion; do not reset a populated store to hide a
migration error. A version id is NULL for the work itself in PostgreSQL and
`''` in ClickHouse, whose sorting keys hold it.

The cover upgrade prepares the version column in migration `0003`. Migration
`0004` refuses to remove content-level metadata while any old cover remains.
An installation with old covers must first run the explicit media migration:
inventory and copy the files, verify the destination, move the metadata to its
declared version owner, then clear the old metadata and rerun migrations.
Preparation remains applied so the operator can resume; an interrupted copy
never causes the schema migration to discard the original cover.

## ContentKit's own schema

Hosts run `MigratePostgres(ctx, pool, cfg)` and
`MigrateClickHouse(ctx, pool, cfg)` from their explicit migration command.
The PostgreSQL entrypoint prepares the configured catalog, account storage,
scope bindings and queues; `MigrationOptions{SkipQueues: true}` skips queue
DDL when the caller manages that separately. The ClickHouse entrypoint creates
its target database before applying the signal lineage.

The PostgreSQL call also realizes fixed `owner_entity` declarations from the
content model. It creates missing site-owned channels and binds eligible unowned
posts, preserving existing names, identities, grants and access. If a declared
owner has `auth_scope: true`, provide `MigrationOptions.Scopes` using the existing
runtime provider or `adapters/authkit.MigrationScopes(pool, authConfig, scope)`.
The latter needs only migrated AuthKit storage and the host's role configuration,
not a full authentication runtime. No fixed owners means no provisioning or extra
authorization dependency. See [configured channels](posts-and-channels.md#one-fixed-official-channel)
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
lifecycles vary. A host can add its own PostgreSQL foreign keys, including
cross-schema foreign keys, when those lifecycles align. Include tenant keys
where the host and ContentKit keys are tenant-scoped. ClickHouse references
are not PostgreSQL foreign keys and have no cross-store FK enforcement.

## Retained state

Back up authored content, moderation and retained-publication state,
reactions, favorites and their revision sequence, source erasure fences,
and the ClickHouse signal plane. Search documents and group counts are
rebuildable. The baseline contains only current signal tables, including
`subject_content_state.last_view_at`; it does not install retired signal
formats or convert historical rows. See [restore.md](restore.md).
