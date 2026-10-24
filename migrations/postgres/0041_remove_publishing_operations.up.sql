-- Stop old API/coordinator workers before applying this migration. Uploads,
-- extracted pages and catalog publication dates remain owned by their existing
-- media manifests and content/version records.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM content_publishing_operations
        WHERE state IN ('processing','publishing','review_required')
           OR (state='blocked' AND failure->>'retryable'='true')
           OR (state='awaiting_upload' AND commit_started)
    ) THEN
        RAISE EXCEPTION 'ContentKit publishing removal: active submissions remain. Finish or explicitly resolve them with the previous version, stop its workers, and retry.';
    END IF;
    IF EXISTS (
        SELECT 1 FROM content_publishing_operations o
        WHERE o.state='scheduled' AND NOT EXISTS (
            SELECT 1 FROM content_items c
            JOIN content_versions v ON v.tenant_id=c.tenant_id AND v.content_id=c.id
            WHERE c.tenant_id=o.tenant_id AND c.id=o.content_id AND v.id=o.version_id
              AND c.state='published' AND c.publish_at=o.publish_at
        )
    ) THEN
        RAISE EXCEPTION 'ContentKit publishing removal: a scheduled submission has no matching catalog publication date. Resolve its publication intent before retrying.';
    END IF;
END
$$;

-- The original sources, manifests, editions, entities and scheduled release
-- dates are deliberately not changed. Future publication is a separate API
-- operation. The existing media import and rendition workers continue working.
DROP TABLE content_publishing_operations;
