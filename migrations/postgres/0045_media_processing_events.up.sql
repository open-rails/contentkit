-- Per-item observations outlive retained log rows, so restarting an old job
-- cannot append its completion again after event retention expires.
CREATE TABLE IF NOT EXISTS content_media_processing (
    tenant_id text NOT NULL,
    folder_prefix text NOT NULL,
    fingerprint bytea NOT NULL,
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, folder_prefix)
);

