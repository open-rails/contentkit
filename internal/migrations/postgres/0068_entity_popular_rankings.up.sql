-- Each ranked entity's place in its kind's popularity ranking, per domain.
CREATE TABLE content_entity_popular_rankings (
    tenant_id text NOT NULL,
    entity_kind text NOT NULL,
    domain text NOT NULL,
    entity_id text NOT NULL,
    place bigint NOT NULL,
    computed_at timestamp with time zone NOT NULL,
    PRIMARY KEY (tenant_id, entity_kind, domain, entity_id)
);

-- Recent account favourites for that ranking.
CREATE INDEX content_favorites_popular_idx ON content_favorites (tenant_id, content_kind, created_at)
    INCLUDE (user_id, content_id) WHERE value = 1 AND user_id IS NOT NULL;
