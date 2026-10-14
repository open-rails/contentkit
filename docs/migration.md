# Database initialization

Each store has one baseline of the final schema (#117, pre-launch hard cut):

| Store | Embedded filesystem | File | Contents |
|---|---|---|---|
| PostgreSQL | `migrations.Postgres` | `postgres/0001_baseline.up.sql` | content, versions, groups, memberships, facets, offers, the outbox, import ids, codes, interactions, polls, search, preferences, erasure and media state |
| ClickHouse | `migrations.ClickHouse` | `clickhouse/0001_baseline.up.sql` | signals, subject state, daily contributions, exposures, co-engagement and erasure fences |

There is no upgrade path from installs of an older chain: start from empty
stores and import their data (`POST /v1/import`). A version id is NULL for the
work itself in PostgreSQL and `''` in ClickHouse, whose sorting keys hold it.

## ContentKit's own schema

`contentkit.New` applies the PostgreSQL baseline to `Config.Database.Schema`
(default `contentkit`), River's migrations to `Database.RiverSchema` and the
media worker's queue, and, with `Config.ClickHouse.Addr`, the ClickHouse
baseline to `ClickHouse.Database`. The schema is ContentKit's alone: it may
share the host's database, never the host's schema. Create the ClickHouse
database with `signal.CreateDatabase` first.

New is idempotent through MigrateKit's ledger, and replicas booting together take turns on its lock. PostgreSQL uses
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
