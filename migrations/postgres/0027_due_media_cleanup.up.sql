-- Cleanup deadlines belong to storage state, not to one delayed River job per
-- folder. Exact deletion targets remain in the existing commit journal.
CREATE TABLE content_media_cleanup (
    tenant_id text NOT NULL,
    folder_prefix text NOT NULL,
    content_kind text NOT NULL,
    due_at timestamptz NOT NULL,
    revision uuid NOT NULL DEFAULT gen_random_uuid(),
    deletion_operations uuid[] NOT NULL DEFAULT '{}',
    PRIMARY KEY (tenant_id, folder_prefix)
);
CREATE INDEX content_media_cleanup_due
    ON content_media_cleanup (tenant_id, content_kind, due_at, folder_prefix);

-- One initial reference-aware pass replaces the old bucket-wide daily scan.
-- The explicit migration resolves the initial deadline using configured grace;
-- runtime startup refuses an unresolved bootstrap.
INSERT INTO content_media_cleanup (tenant_id, folder_prefix, content_kind, due_at)
SELECT tenant_id, folder_prefix, min(content_kind), 'infinity'::timestamptz
FROM content_media_commits GROUP BY tenant_id, folder_prefix;
