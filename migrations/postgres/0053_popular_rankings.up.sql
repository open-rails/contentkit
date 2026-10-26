-- Popular lists read rankings built by a background job: content ids, best
-- first. A row without computed_at is requested and not yet built.
CREATE TABLE content_popular_rankings (
    tenant_id text NOT NULL,
    kind text NOT NULL,
    period text NOT NULL,
    domain text NOT NULL,
    language text NOT NULL,
    preset boolean NOT NULL,
    requested_at timestamptz NOT NULL,
    computed_at timestamptz,
    content_ids text[] NOT NULL DEFAULT '{}',
    PRIMARY KEY (tenant_id, kind, period, domain, language)
);
