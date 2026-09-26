-- parent: 1 sha256:af673faa8e22e627ea8631acb6b95832593904bb3a3ee89cd4e04fd1f180fad3

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
