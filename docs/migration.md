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

## Content model configuration

Content model files use `content_kinds` alongside `entity_kinds`. Rename the old
`kinds` configuration key before loading this release; YAML and JSON decoding
reject it rather than silently ignoring it or accepting a legacy alias. The Go
member remains `kinds.Config.Kinds`. This does not rename the `kinds` field in the
public `GET /kinds` discovery response or any HTTP query parameters.

## First-class channel upgrade

Move channel configuration out of `entity_kinds` into `channels`. Replace a
content kind's `owner_entity: {kind: channel, slug: blog}` with
`channel: {fixed: blog}`; creator-selected kinds use `channel: {required: true}`
or an empty optional policy. Metadata kinds no longer declare owner or
permission-group policies. Content HTTP ownership is `channel_id`, and channels
have their own `/channels` interface. Existing channel IDs, media locations,
AuthKit group identities and opaque OpenRails membership keys are retained.

Channel create/delete/restore now calls AuthKit synchronously. A failed call
returns an error and the caller retries against the same identity. No
`contentkit_entity_sync` or replacement lifecycle worker remains. Before upgrading,
finish actionable old channel lifecycle work using the prior version, stop old
writers, and run the normal explicit migration. Preflight refuses unfinished,
running or ambiguous legacy obligations. It retires only no-op queued rows and
preserves terminal history and foreign-app work. Do not delete jobs to bypass
that check. The content-purge sweep still owns physical retention cleanup.

New migration `0054_channels` adds descriptive channel metadata/repost references
and moves imported channel identifiers into their own record category. It does
not move source media or rewrite published SQL. Reposts reference their original
item and retain its access, versions and editorial authority.

## Authored content belongs to versions

Migration `0055_version_content` moves retained parent-localized text and source
titles into existing version records, preserving already-authored version titles
and summaries. Existing IDs, media and files stay in place. The parent text table
and original-title columns are then removed. This one-time adoption preserves old
translations; it does not create automatic copying between future editions.

Write initial text through `ContentInput.versions[].text` and update an edition
through `PATCH /contents/{id}/versions/{vid}`. The former parent-text routes and
version-fork endpoint are removed. Hidden-version post kinds still create one
version; callers may supply that initial version explicitly. Additional versions
start empty. The content reader's title/body/image fields are read-only projections
from its `selected_version_id`, with no parent or sibling content fallback.

Finish any accepted old body-copy/fork work using the previous version before
cutover. No replacement copy job or obsolete-job scan is introduced. Existing
copied files are preserved, not deleted merely because the feature was removed.

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
rebuildable. The baseline contains only current signal tables, including
`subject_content_state.last_view_at`; it does not install retired signal
formats or convert historical rows. See [restore.md](restore.md).
