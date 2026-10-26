-- Channels retain their existing identity/media/authoring scope. Descriptive
-- metadata and repost references are separate relationships, never grants.
CREATE TABLE content_channel_entities (
    tenant_id text NOT NULL,
    channel_id text NOT NULL,
    entity_id text NOT NULL,
    PRIMARY KEY (tenant_id, channel_id, entity_id),
    FOREIGN KEY (tenant_id,channel_id) REFERENCES content_entities(tenant_id,id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id,entity_id) REFERENCES content_entities(tenant_id,id) ON DELETE CASCADE,
    CHECK (channel_id <> entity_id)
);
CREATE TABLE content_channel_reposts (
    tenant_id text NOT NULL,
    channel_id text NOT NULL,
    content_id text NOT NULL,
    actor_id text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, channel_id, content_id),
    FOREIGN KEY (tenant_id,channel_id) REFERENCES content_entities(tenant_id,id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id,content_id) REFERENCES content_items(tenant_id,id) ON DELETE CASCADE
);
CREATE INDEX content_channel_reposts_feed_idx ON content_channel_reposts(tenant_id,channel_id,created_at DESC,content_id);

-- Preserve imported external identities when the public record category changes.
-- A contradictory channel mapping fails its unique constraint rather than
-- silently replacing an existing record.
UPDATE content_external_ids x SET entity='channel'
FROM content_entities e
WHERE x.tenant_id=e.tenant_id AND x.id=e.id AND x.entity='group' AND e.kind='channel';
