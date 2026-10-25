-- The request is durable desired work. A completed request can remain the
-- reader's active snapshot while a newer current request is pending.
CREATE TABLE content_publication_requests (
    tenant_id text NOT NULL,
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    content_id text NOT NULL,
    version_id text NOT NULL,
    scheduled_at timestamptz NOT NULL,
    state text NOT NULL DEFAULT 'pending',
    plan jsonb,
    result jsonb,
    current boolean NOT NULL DEFAULT true,
    error text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    completed_at timestamptz,
    PRIMARY KEY (tenant_id,id),
    FOREIGN KEY (tenant_id,version_id) REFERENCES content_versions(tenant_id,id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id,content_id) REFERENCES content_items(tenant_id,id) ON DELETE CASCADE,
    CHECK (state IN ('pending','waiting','running','completed','failed','cancelled')),
    CHECK (plan IS NULL OR jsonb_typeof(plan)='object'),
    CHECK (result IS NULL OR jsonb_typeof(result)='object')
);
CREATE UNIQUE INDEX content_publication_requests_current_idx
    ON content_publication_requests(tenant_id,version_id) WHERE current;

ALTER TABLE content_versions ADD COLUMN release_id uuid;
ALTER TABLE content_versions ADD CONSTRAINT content_versions_release_id_fkey
    FOREIGN KEY (tenant_id,release_id) REFERENCES content_publication_requests(tenant_id,id)
    ON DELETE SET NULL (release_id);
