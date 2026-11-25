-- The pending-anonymization probe reads intents by request time.
CREATE INDEX content_account_erasure_intents_requested ON content_account_erasure_intents (tenant_id, app_id, requested_at);

-- An app whose signal plane was ever provisioned (MigrateClickHouse) keeps
-- account erasure pending on an engine without one.
CREATE TABLE content_signal_planes (
    tenant_id uuid NOT NULL,
    app_id uuid NOT NULL,
    provisioned_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_signal_planes_app_id_check CHECK ((app_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    PRIMARY KEY (tenant_id, app_id)
);

-- Account erasure finds the account's media commits by actor and quota owner.
CREATE INDEX IF NOT EXISTS content_media_commits_actor ON content_media_commits (tenant_id, actor_id);
CREATE INDEX IF NOT EXISTS content_media_commits_settlement_owner ON content_media_commits (tenant_id, (effects->'settlement'->>'Owner'))
WHERE effects->'settlement'->>'Owner' <> '';
CREATE INDEX IF NOT EXISTS content_media_commits_deletion_owner ON content_media_commits (tenant_id, (effects->'folder_delete'->>'owner'))
WHERE effects->'folder_delete' ? 'owner';
