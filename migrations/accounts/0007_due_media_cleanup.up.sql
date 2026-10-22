-- Account-owned media uses the same cleanup ledger as catalog-owned media,
-- in its own shared account schema.
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

INSERT INTO content_media_cleanup (tenant_id, folder_prefix, content_kind, due_at)
SELECT tenant_id, folder_prefix, min(content_kind), 'infinity'::timestamptz
FROM content_media_commits GROUP BY tenant_id, folder_prefix;
