# Database initialization

ContentKit has a PostgreSQL migration chain and a ClickHouse baseline:

| Store | Embedded filesystem | File | Contents |
|---|---|---|---|
| PostgreSQL | `migrations.Postgres` | `postgres/0001_baseline.up.sql` | interactions, moderation, polls, preferences, keyword search and taxonomy |
| PostgreSQL | `migrations.Postgres` | `postgres/0002_search_invalid.up.sql` | durable invalid search-document state |
| PostgreSQL | `migrations.Postgres` | `postgres/0003_media_releases.up.sql` | media quota release ledger |
| PostgreSQL | `migrations.Postgres` | `postgres/0004_media_slots.up.sql` | slot index (`content_media_slots`) |
| PostgreSQL | `migrations.Postgres` | `postgres/0005_media_slot_backfill.up.sql` | slot index backfill progress (`content_media_slot_backfill`) |
| PostgreSQL | `migrations.Postgres` | `postgres/0006_drop_media_slots.up.sql` | drops the slot index and its backfill (one media model) |
| PostgreSQL | `migrations.Postgres` | `postgres/0007_inline_image_names.up.sql` | renames post and poll image URL columns to image names (refuses legacy URLs; coordinated cutover, see HOST_INTEGRATION) |
| PostgreSQL | `migrations.Postgres` | `postgres/0008_canonical_interaction_ids.up.sql` | merges case spellings of interaction content ids and refuses them (`CHECK content_id = lower(content_id)`) |
| PostgreSQL | `migrations.Postgres` | `postgres/0009_comment_bans.up.sql` | comment bans (`content_comment_bans`) |
| PostgreSQL | `migrations.Postgres` | `postgres/0010_content_codes.up.sql` | content codes and legacy aliases (`content_codes`, `content_code_aliases`, `contentkit_content_code()`); backfills codes for existing taxonomy nodes and posts |
| ClickHouse | `migrations.ClickHouse` | `clickhouse/0001_baseline.up.sql` | signals, subject state, daily contributions, exposures, co-engagement and erasure fences |

The baselines initialize fresh stores; PostgreSQL `0002` and later also upgrade
an existing installation of the current ContentKit lineage. The baselines replace
older feature-specific migration chains; they are not an in-place upgrade of
those retired ledgers. Restore old installations with the matching library
version and use a host-owned, verified data import when moving their data into
fresh stores.

## One PostgreSQL schema

The host selects one required `Schema` for all ContentKit PostgreSQL tables.
It can be the application's existing schema; no dedicated library schema is
required. Names contain letters, numbers or underscores and are quoted, so
mixed-case names are preserved. Different applications can select different
schemas in the same database.

```go
err := contentkit.Migrate(ctx, contentkit.MigrateConfig{
    DB: ddlDB,
    Schema: "my_app",
    ClickHouse: &chmigrate.Config{
        ClientAddr: "localhost:9000", Database: "content_signals",
        Username: user, Password: password, Cluster: cluster,
    },
})
```

Create the ClickHouse database with `signal.CreateDatabase` first, or leave
`ClickHouse` nil for PostgreSQL only. `migrations.ApplyPostgres(ctx, ddlDB,
"my_app")` is the PostgreSQL-only initializer. Taxonomy tables are always
installed; construction of a taxonomy store and its host-specific policies
remains optional.

Set `EmbeddedConfig.PGSchema`, `content.Options.Schema`, and the standalone
search, taxonomy and worker schema options to this same schema. `NewRuntime`
fills an omitted content schema from `PGSchema` and rejects a mismatch. Post
writes with a language queue their keyword documents in that schema.

The initializer is idempotent through MigrateKit's ledger. PostgreSQL uses
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
indexes, checks, internal foreign keys and dirty-queue trigger.
Before deploying a worker that uses `0002`, apply the migration and grant its
runtime role read/write access to `content_search_invalid`. Before deploying
code that uses `0004` and `0005`, apply them and grant the host's runtime role
read/write access to `content_media_slots` and `content_media_slot_backfill`
(the media worker needs none). Existing slots enter the index at the first
start: binding the media jobs to River schedules a one-time backfill.
`0010` adds `content_codes` and `content_code_aliases`. The host's runtime
role needs read/write access to both and EXECUTE on `contentkit_content_code()` (PUBLIC by default).
Taxonomy and post writes register codes from then on, so grant the access
before deploying code that uses `0010`.

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
and the ClickHouse signal plane. Search documents and taxonomy counts are
rebuildable. The baseline contains only current signal tables, including
`subject_content_state.last_view_at`; it does not install retired signal
formats or convert historical rows. See [restore.md](restore.md).
