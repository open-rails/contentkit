-- Automatic page changes become dismissible notices derived from the manifest;
-- only dismissals persist, and they leave with their version.
DROP TABLE IF EXISTS content_page_review_events;
CREATE TABLE content_page_notice_dismissals (
    tenant_id text NOT NULL,
    version_id text NOT NULL,
    notice_id text NOT NULL,
    dismissed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, version_id, notice_id),
    FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions (tenant_id, id) ON DELETE CASCADE
);
