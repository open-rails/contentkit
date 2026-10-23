-- Preserve ownership receipts before removing per-chapter storage items.
-- All old published destinations belong to a source version. Derive the
-- storage root from that version's journal, never from an arbitrary file key.
DO $$
DECLARE
    item record;
    source_count integer;
    source_namespace text;
    source_folder text;
    source_suffix text;
    retired_folder text;
    operation uuid;
    objects text[];
    previous_due timestamptz;
BEGIN
    IF EXISTS (SELECT 1 FROM content_media_commits WHERE content_kind LIKE '%.published'
               AND state IN ('open','prepared','frozen')) THEN
        RAISE EXCEPTION 'reader retirement needs every legacy publication operation settled before schema migration';
    END IF;
    FOR item IN
        SELECT p.tenant_id, c.kind, v.content_id, v.id AS version_id, i.id AS item_id
        FROM content_version_publication_items i
        JOIN content_version_publications p ON p.tenant_id=i.tenant_id AND p.id=i.publication_id
        JOIN content_versions v ON v.tenant_id=p.tenant_id AND v.id=p.version_id
        JOIN content_items c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
        UNION
        SELECT p.tenant_id, c.kind, v.content_id, v.id, p.id
        FROM content_version_publications p
        JOIN content_versions v ON v.tenant_id=p.tenant_id AND v.id=p.version_id
        JOIN content_items c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
        WHERE p.kind='playback'
    LOOP
        source_suffix := item.kind || '/' || item.content_id || '/' || item.version_id || '/';
        SELECT count(*), min(tenant_id), min(folder_prefix)
        INTO source_count, source_namespace, source_folder
        FROM (
            SELECT DISTINCT tenant_id, folder_prefix FROM content_media_commits
            WHERE content_kind=item.kind AND content_id=item.content_id
              AND content_version_id=item.version_id AND state='applied'
              AND right(folder_prefix,length(source_suffix))=source_suffix
        ) source;
        IF source_count <> 1 THEN
            RAISE EXCEPTION 'reader retirement cannot prove unique source ownership for version %', item.version_id;
        END IF;
        retired_folder := left(source_folder,length(source_folder)-length(source_suffix)) || item.kind || '.published/' || item.item_id || '/';
        -- Existing durable journal delivery performs the one-time removal of
        -- old preview uploads; no new recurring migration worker is needed.
        UPDATE content_media_commits SET effects=jsonb_set(effects,'{notify}','true'),updated_at=now()
        WHERE tenant_id=source_namespace AND operation_id=(
            SELECT operation_id FROM content_media_commits
            WHERE tenant_id=source_namespace AND folder_prefix=source_folder AND content_kind=item.kind
              AND content_id=item.content_id AND content_version_id=item.version_id AND state='applied'
            ORDER BY updated_at DESC,operation_id DESC LIMIT 1);
        IF EXISTS (SELECT 1 FROM content_media_commits WHERE tenant_id=source_namespace
                   AND folder_prefix=retired_folder AND state IN ('open','prepared','frozen')) THEN
            RAISE EXCEPTION 'reader retirement needs settled legacy media folder %', retired_folder;
        END IF;
        SELECT array_agg(key ORDER BY key) INTO objects FROM (
            SELECT object_key AS key FROM content_media_allocations
            WHERE tenant_id=source_namespace AND folder_prefix=retired_folder
            UNION SELECT retired_folder || 'manifest.json'
        ) targets;
        SELECT due_at INTO previous_due FROM content_media_cleanup
        WHERE tenant_id=source_namespace AND folder_prefix=retired_folder AND isfinite(due_at);
        operation := gen_random_uuid();
        INSERT INTO content_media_commits
            (tenant_id,operation_id,content_kind,content_id,folder_prefix,actor_id,fingerprint,lease_id,state,effects)
        VALUES (source_namespace,operation,item.kind || '.published',item.item_id,retired_folder,
            'migration:reader-metadata',sha256(convert_to(retired_folder,'UTF8')),gen_random_uuid(),'applied',
            jsonb_build_object('cleanup',true,'private',to_jsonb(objects),'legacy_publication',jsonb_build_object(
                'kind',item.kind,'content',item.content_id,'version',item.version_id,'source',source_folder,
                'previous_due',previous_due,'initialized',false,'final',false)));
        INSERT INTO content_media_cleanup AS target
            (tenant_id,folder_prefix,content_kind,due_at,deletion_operations)
        VALUES(source_namespace,retired_folder,item.kind || '.published','infinity',ARRAY[operation])
        ON CONFLICT(tenant_id,folder_prefix) DO UPDATE SET due_at='infinity',revision=gen_random_uuid(),
            deletion_operations=ARRAY(SELECT DISTINCT op FROM unnest(target.deletion_operations || EXCLUDED.deletion_operations) op ORDER BY op);
    END LOOP;
END $$;

-- Snapshots retain exact source/rendition identities, never chapter layout.
UPDATE content_version_publications SET source=source-'spans' WHERE kind='pages';
-- A former building snapshot already names ready source files. Keep the most
-- recent snapshot; the new writer commits metadata in one transaction.
DELETE FROM content_version_publications older USING content_version_publications newer
WHERE older.tenant_id=newer.tenant_id AND older.version_id=newer.version_id AND older.kind=newer.kind
  AND (older.created_at,older.id)<(newer.created_at,newer.id);
UPDATE content_version_publications SET state='current',ready_at=coalesce(ready_at,now());
DROP TABLE content_version_publication_items;
ALTER TABLE content_version_publications DROP COLUMN previews, DROP COLUMN previews_at;
