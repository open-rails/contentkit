-- Access history starts at adoption. Do not invent past rules from today's
-- catalog: that would reclassify old paid/free readership after an edit.
CREATE TABLE content_access_history (
    id bigint GENERATED ALWAYS AS IDENTITY,
    tenant_id text NOT NULL,
    content_id text NOT NULL,
    version_id text NOT NULL,
    occurred_at timestamptz NOT NULL,
    snapshot jsonb NOT NULL,
    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX content_access_history_version_time
    ON content_access_history (tenant_id, content_id, version_id, occurred_at, id);

CREATE TABLE content_access_history_start (
    tenant_id text PRIMARY KEY,
    started_at timestamptz NOT NULL
);

INSERT INTO content_access_history_start (tenant_id, started_at)
SELECT DISTINCT tenant_id, statement_timestamp() FROM content_facets;

INSERT INTO content_access_history (tenant_id, content_id, version_id, occurred_at, snapshot)
SELECT tenant_id, content_id, version_id, statement_timestamp(),
       jsonb_build_object('kind', kind, 'live_from', live_from, 'deleted', deleted,
                          'listing', listing, 'languages', languages,
                          'level', access_level, 'tiers', access_tiers,
                          'group_id', access_group_id, 'free_after', access_free_after)
FROM content_facets;
