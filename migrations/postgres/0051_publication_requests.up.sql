-- The request is durable desired work. A completed request can remain the
-- reader's active snapshot while a newer current request is pending.
CREATE FUNCTION content_frozen_access(source_rule jsonb, completed_at timestamptz)
RETURNS jsonb LANGUAGE sql STABLE AS $$
    SELECT (source_rule-'releases') || jsonb_build_object('releases',
        (SELECT coalesce(jsonb_agg(release),'[]'::jsonb)
         FROM jsonb_array_elements(CASE WHEN jsonb_typeof(source_rule->'releases')='array'
              THEN source_rule->'releases' ELSE '[]'::jsonb END) release
         WHERE (release->>'at')::timestamptz<=completed_at))
$$;

ALTER TABLE content_versions ADD COLUMN released_access jsonb;
UPDATE content_versions SET released_access=content_frozen_access(access,statement_timestamp()) WHERE released;
ALTER TABLE content_versions ADD CONSTRAINT content_versions_released_access_check
    CHECK (NOT released OR released_access IS NOT NULL);

-- Lists and direct readers must agree with the policy actually released.
UPDATE content_facets f
SET access_level=v.released_access->>'level',
    access_tiers=ARRAY(SELECT jsonb_array_elements_text(coalesce(v.released_access->'tiers','[]'::jsonb))),
    access_entity_id=nullif(v.released_access->>'group_id',''),
    access_releases=coalesce(v.released_access->'releases','[]'::jsonb),
    next_release_at=NULL
FROM content_versions v WHERE v.tenant_id=f.tenant_id AND v.id=f.version_id AND v.released;

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
    CHECK (state IN ('pending','waiting','running','retiring','completed','failed','cancelled')),
    CHECK (plan IS NULL OR jsonb_typeof(plan)='object'),
    CHECK (result IS NULL OR jsonb_typeof(result)='object')
);
CREATE UNIQUE INDEX content_publication_requests_current_idx
    ON content_publication_requests(tenant_id,version_id) WHERE current;

ALTER TABLE content_versions ADD COLUMN release_id uuid;
ALTER TABLE content_versions ADD CONSTRAINT content_versions_release_id_fkey
    FOREIGN KEY (tenant_id,release_id) REFERENCES content_publication_requests(tenant_id,id)
    ON DELETE SET NULL (release_id);
