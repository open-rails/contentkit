-- Queue rotation is independent of user-visible operation changes.
ALTER TABLE content_publishing_operations
    ADD COLUMN reconcile_at timestamptz NOT NULL DEFAULT now();

CREATE INDEX content_publishing_reconcile_idx
    ON content_publishing_operations(tenant_id, reconcile_at, id)
    WHERE state IN ('processing', 'publishing', 'review_required', 'scheduled')
       OR state = 'blocked' AND failure->>'retryable' = 'true';

DROP INDEX content_publishing_pending_idx;
