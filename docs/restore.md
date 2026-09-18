# Restore

Search documents are derived data; the ClickHouse signal plane and the
erasure ledger are not. Restore them differently.

## Postgres snapshot taken before keyword 0003 / legacy 0004

1. Restore the dump into the host schema as usual. Its `migrations` ledger
   still names the old lineage state.
2. Run the host's migrate step: migratekit applies the pending content-refs
   migration (and, for the legacy lineage, 0005 drops the embedding tables —
   the export step in [migration.md](migration.md) applies to the restored
   data just as it did before).
3. The restored `search_documents` rows now sit in `content_search_documents`
   under `tenant_id = ''`. Reindex through the worker backfill, then delete the
   `''` rows. Do not try to map old rows onto tenants by hand.

If a dump of only the old search tables must be loaded into an already
converted schema, convert it on the way in:

```sql
INSERT INTO content_search_documents (tenant_id, content_kind, content_id, content_version_id, language, title, aliases, keywords, raw_document, created_at, updated_at)
SELECT '<tenant>', entity_type, entity_id, '', language, title, aliases, keywords, raw_document, created_at, updated_at
FROM old_search_documents;
```

`content_version_id` is `''` for every old row: pre-cut documents were keyed
by the version id in `entity_id` for grouped kinds, so a reindex (not this
conversion) is what restores version references.

## Postgres snapshot taken after the migrations

Nothing to convert. Reindex if the snapshot predates catalog changes.

## ClickHouse backup taken before signal 0005

1. Restore the database.
2. Apply the signal lineage: 0005 copies the restored `events`,
   `subject_state`, `subject_daily` and `item_pairs` into the content-referenced
   tables and converts `exposures`.
3. Re-erase every subject deleted since the backup (the host's deletion ledger
   is authoritative), then run `EnforceErasures` for each tenant: it clears the
   restored legacy tables too.
4. `signal.CheckSchema` must pass before the host serves analytics.

## ClickHouse backup taken after signal 0005

Steps 3 and 4 only.

## What never rolls back

Erasures. A restore that resurrects an erased subject is a defect; the
ledger restored with the backup plus the host deletion ledger must be
replayed before traffic.
