# Migrations

ContentKit owns four migratekit lineages. Every file is immutable once
applied; `migratekit relink --check` verifies the parent links.

| Lineage | Embedded FS | Store | Ledger app id |
|---|---|---|---|
| Keyword profile | `migrations.Postgres` | host Postgres schema | new installations: `contentkit`; existing keyword installations keep theirs |
| Legacy combined | `migrations.LegacyPostgres` | host Postgres schema | existing installations only, under the ledger they already have (`searchkit`) |
| Taxonomy | `migrations.Taxonomy` | host Postgres schema, after the keyword profile | `contentkit_taxonomy` (see [taxonomy-migration.md](taxonomy-migration.md)) |
| Signal plane | `migrations.SignalClickHouse` | dedicated ClickHouse database | existing: `searchkit_signal`; new: `contentkit_signal` |

Never switch a populated ledger between lineages and never use the legacy
lineage for a new installation. Both Postgres lineages end on the same schema
(the `keyword`/`legacy` profile test proves the fingerprints equal).

## Keyword profile 0003 / legacy 0004: content references

Tables become `content_search_documents`, `content_search_dirty` and
`content_search_backfill`, keyed by `(tenant_id, content_kind, content_id,
content_version_id, language)` (`content_version_id = ''` is the work). The
FTS/trigram projections (`document`, `tsv`) and the raw CJK index are dropped;
the keyword functions are renamed `contentkit_*`.

Rows that existed before the migration keep `tenant_id = ''`. They are
unreachable by any tenant and never served. Adoption steps per host:

1. Apply the migration in the host's migrate step (DDL credentials).
2. Run the worker backfill for the host's tenant and kinds; every document is
   rebuilt from host content through `BuildKeywordDocuments`.
3. After the backfill reports `done`, remove the converted rows:

```sql
DELETE FROM content_search_documents WHERE tenant_id = '';
DELETE FROM content_search_dirty     WHERE tenant_id = '';
DELETE FROM content_search_backfill  WHERE tenant_id = '';
```

Hosts write the queue through `search.MarkDirty` (or the same SQL) with the
tenant, kind, id, optional version and language of every changed document.

## Legacy 0005 drops the embedding tables — export first

`embedding_models`, `embedding_tasks`, `embedding_vectors`,
`embedding_vectors_backfill_state` and `embedding_dead_letters` leave the host
schema. They are rebuildable and User Intelligence owns its own index, but
the design requires an export, a checksum and a restore qualification before
any rebuildable table is dropped. Before applying legacy 0005:

```sh
# 1. Export (schema name as installed, e.g. doujins).
pg_dump "$DSN" --data-only --format=custom \
  -t doujins.embedding_models -t doujins.embedding_tasks -t doujins.embedding_vectors \
  -t doujins.embedding_vectors_backfill_state -t doujins.embedding_dead_letters \
  > embeddings-$(date -u +%Y%m%dT%H%M%SZ).dump
sha256sum embeddings-*.dump > embeddings.sha256

# 2. Checksum the live rows (record the output with the export).
psql "$DSN" -Atc "
  SELECT 'models', count(*), md5(string_agg(model||':'||dims, ',' ORDER BY model)) FROM doujins.embedding_models
  UNION ALL SELECT 'vectors', count(*), md5(string_agg(entity_type||'/'||entity_id||'/'||model||'/'||language||':'||md5(embedding::text), ',' ORDER BY entity_type, entity_id, model, language)) FROM doujins.embedding_vectors
  UNION ALL SELECT 'tasks', count(*), md5(string_agg(entity_type||'/'||entity_id||'/'||model||'/'||language, ',' ORDER BY 1)) FROM doujins.embedding_tasks"

# 3. Qualify the restore: load the dump into a scratch database with the same
#    schema and rerun the checksum query; the counts and digests must match.
createdb embeddings_qualify && pg_restore -d embeddings_qualify embeddings-*.dump
```

Only after step 3 matches, apply 0005 (legacy 0004 alone lets ContentKit run on the converted keyword tables; migratekit applies both when both are pending, so export before the migrate step that carries 0005). The `vector` extension itself is
left installed (dropping it needs superuser and is not ContentKit's to decide).

## Signal plane 0005: content references

ClickHouse cannot rename sorting-key columns, so 0005 creates `signals`,
`subject_content_state`, `subject_content_daily` and `content_pairs` and
copies `events`, `subject_state`, `subject_daily` and `item_pairs` into them
(`content_version_id = ''`); `exposures` converts in place. Every statement is
idempotent. No repair is required afterwards; `RepairProjections{Rebuild:
true}` and `RefreshCoEngagement` are still recommended once per tenant.
Gate startup on `signal.CheckSchema` as before.

The pre-ContentKit tables stay (they are neither read nor written; erasure
still clears them). Before a later migration drops them, checksum old against
new:

```sql
SELECT count(), sum(cityHash64(tenant, entity_type, entity_id, subject_kind, subject, signal_type, event_id, revision, occurred_at))
FROM events FINAL;
SELECT count(), sum(cityHash64(tenant, content_kind, content_id, subject_kind, subject, signal_type, event_id, revision, occurred_at))
FROM signals FINAL WHERE content_version_id = '';
```

The two rows must agree (version rows only exist for signals recorded after
the cut). Record the result; the follow-up drop migration is
`DROP TABLE IF EXISTS events|subject_state|subject_daily|item_pairs {{ON_CLUSTER}} SYNC`
plus the legacy `signal_events` and `search_impressions` tables, which
predate the canonical schema and are covered by the same checksum discipline.

## Restore

See [restore.md](restore.md) for snapshots taken before these migrations.
