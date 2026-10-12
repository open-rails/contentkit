
CREATE TABLE content_search_invalid (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    content_version_id text NOT NULL,
    language text NOT NULL,
    revision bigint NOT NULL,
    last_error text NOT NULL,
    failed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, content_kind, content_id, content_version_id, language),
    FOREIGN KEY (tenant_id, content_kind, content_id, content_version_id, language)
        REFERENCES content_search_dirty (tenant_id, content_kind, content_id, content_version_id, language)
        ON DELETE CASCADE
);
