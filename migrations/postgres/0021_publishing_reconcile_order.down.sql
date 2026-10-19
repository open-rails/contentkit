CREATE INDEX content_publishing_pending_idx
    ON content_publishing_operations(tenant_id, updated_at)
    WHERE state IN ('processing', 'publishing', 'review_required', 'blocked');

DROP INDEX content_publishing_reconcile_idx;
ALTER TABLE content_publishing_operations DROP COLUMN reconcile_at;
