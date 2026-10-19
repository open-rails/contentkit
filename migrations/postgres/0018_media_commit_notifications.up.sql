-- Journal recovery reads pending ItemCommitted notifications on their own,
-- so each pass reaches them without scanning retained commit receipts.
CREATE INDEX content_media_commits_notify ON content_media_commits (tenant_id, updated_at, operation_id)
    WHERE state = 'applied' AND effects->>'notify' = 'true';
