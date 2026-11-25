-- The preference export's position per tenant: every settled revision up to
-- it has been sent. content_preference_sync keeps samples of the revision
-- sequence (its value at taken_at), which bound what is settled.
CREATE TABLE content_preference_cursor (
    tenant_id text NOT NULL,
    revision bigint NOT NULL,
    PRIMARY KEY (tenant_id)
);

-- The oldest retained checkpoint was sent, overlap included.
INSERT INTO content_preference_cursor (tenant_id, revision)
SELECT tenant_id, min(revision) FROM content_preference_sync GROUP BY tenant_id;
