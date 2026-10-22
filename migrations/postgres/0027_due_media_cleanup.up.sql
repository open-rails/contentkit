-- Cleanup deadlines belong to storage state, not one delayed River job per folder.
-- The explicit migration resolves bootstrap deadlines using configured grace.
-- Both lineages may target the same schema. Serialize their shared DDL and
-- retain any cleanup already scheduled by the other lineage.
SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':contentkit:media-cleanup', 0));
CREATE TABLE IF NOT EXISTS content_media_cleanup (
    tenant_id text NOT NULL,
    folder_prefix text NOT NULL,
    content_kind text NOT NULL,
    due_at timestamptz NOT NULL,
    revision uuid NOT NULL DEFAULT gen_random_uuid(),
    deletion_operations uuid[] NOT NULL DEFAULT '{}',
    PRIMARY KEY (tenant_id, folder_prefix)
);
CREATE INDEX IF NOT EXISTS content_media_cleanup_due
    ON content_media_cleanup (tenant_id, content_kind, due_at, folder_prefix);

-- IF NOT EXISTS is only valid for the other lineage's identical table, never
-- for an unrelated or partially defined relation with the same name.
DO $$
DECLARE
    cleanup_table regclass := format('%I.content_media_cleanup', current_schema())::regclass;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_class WHERE oid=cleanup_table AND relkind='r')
       OR (SELECT jsonb_agg(jsonb_build_array(a.attname, format_type(a.atttypid,a.atttypmod),
                    a.attnotnull, coalesce(pg_get_expr(d.adbin,d.adrelid),'')) ORDER BY a.attnum)
           FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
           WHERE a.attrelid=cleanup_table AND a.attnum>0 AND NOT a.attisdropped)
          IS DISTINCT FROM '[
            ["tenant_id","text",true,""],
            ["folder_prefix","text",true,""],
            ["content_kind","text",true,""],
            ["due_at","timestamp with time zone",true,""],
            ["revision","uuid",true,"gen_random_uuid()"],
            ["deletion_operations","uuid[]",true,"''{}''::uuid[]"]
          ]'::jsonb
       OR NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid=cleanup_table
           AND contype='p' AND NOT condeferrable
           AND pg_get_constraintdef(oid)='PRIMARY KEY (tenant_id, folder_prefix)')
       OR NOT EXISTS (SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
           WHERE i.indrelid=cleanup_table AND c.relname='content_media_cleanup_due'
             AND i.indisvalid AND i.indisready
             AND pg_get_indexdef(i.indexrelid)=format(
                 'CREATE INDEX content_media_cleanup_due ON %I.content_media_cleanup USING btree (tenant_id, content_kind, due_at, folder_prefix)', current_schema()))
    THEN
        RAISE EXCEPTION 'contentkit: incompatible content_media_cleanup table or index in schema %', current_schema();
    END IF;
END $$;

-- Bootstrap only missing folders. Never overwrite a finite deadline, revision
-- token or deletion receipt already owned by the other migration lineage.
INSERT INTO content_media_cleanup (tenant_id, folder_prefix, content_kind, due_at)
SELECT tenant_id, folder_prefix, min(content_kind), 'infinity'::timestamptz
FROM content_media_commits GROUP BY tenant_id, folder_prefix
ON CONFLICT (tenant_id, folder_prefix) DO NOTHING;
