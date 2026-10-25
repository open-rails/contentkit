-- Exact obligations outlive their content rows. The existing content-deletion
-- sweep removes them only after ClickHouse acknowledges deletion.
CREATE TABLE content_signal_purges (
    tenant_id text NOT NULL,
    content_kind text NOT NULL CHECK (content_kind <> ''),
    content_id text NOT NULL CHECK (content_id <> ''),
    content_version_id text NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, content_kind, content_id, content_version_id)
);
