DROP TABLE IF EXISTS content_page_notice_dismissals;
CREATE TABLE content_page_review_events (
    tenant_id text NOT NULL,
    version_id text NOT NULL,
    page_id text NOT NULL,
    event_id text NOT NULL,
    actor_id text NOT NULL,
    decision jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, version_id, page_id, event_id)
);
CREATE INDEX content_page_review_events_version ON content_page_review_events (tenant_id, version_id, created_at);
