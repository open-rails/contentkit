-- Publishing receipts outlive a caller connection and never carry bearer tokens.
CREATE TABLE content_publishing_operations (
    tenant_id text NOT NULL,
    id uuid NOT NULL,
    owner_key text NOT NULL,
    request_hash text NOT NULL,
    request jsonb NOT NULL,
    authority jsonb,
    content_id text NOT NULL,
    version_id text NOT NULL,
    content_revision bigint NOT NULL,
    version_revision bigint NOT NULL,
    group_ids text[] NOT NULL DEFAULT '{}',
    entity_resolutions jsonb NOT NULL DEFAULT '[]',
    commit_id uuid NOT NULL,
    upload jsonb,
    upload_expires_at timestamptz,
    commit_started boolean NOT NULL DEFAULT false,
    state text NOT NULL DEFAULT 'awaiting_upload',
    progress jsonb NOT NULL DEFAULT '{"stage":"upload","completed":0,"total":0}',
    result jsonb,
    failure jsonb,
    source_provider text,
    source_id text,
    publish_at timestamptz,
    lease_id uuid,
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    CHECK (state IN ('awaiting_upload','processing','publishing','scheduled','ready','review_required','published','blocked','failed')),
    CHECK (jsonb_typeof(request) = 'object'),
    CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    CHECK ((source_provider IS NULL) = (source_id IS NULL))
);
CREATE UNIQUE INDEX content_publishing_source_idx
    ON content_publishing_operations(tenant_id, source_provider, source_id)
    WHERE source_provider IS NOT NULL;
CREATE INDEX content_publishing_pending_idx
    ON content_publishing_operations(tenant_id, updated_at)
    WHERE state IN ('processing','publishing','review_required','blocked');
CREATE INDEX content_publishing_scheduled_idx
    ON content_publishing_operations(tenant_id,publish_at) WHERE state='scheduled';

-- Name resolution ignores case and separators but retains all other punctuation.
CREATE INDEX content_entity_name_lookup_idx ON content_group_names
    (tenant_id, language, (regexp_replace(normalized, '[[:space:]_‐‑‒–—―-]+', '', 'g')));
