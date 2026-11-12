-- Each entity's place in its kind's popularity ranking, per bucket and scope.
CREATE TABLE content_entity_popular_rankings (
    tenant_id text NOT NULL,
    entity_kind text NOT NULL,
    period text NOT NULL,
    domain text NOT NULL,
    language text NOT NULL,
    entity_id text NOT NULL,
    place bigint NOT NULL,
    computed_at timestamp with time zone NOT NULL,
    PRIMARY KEY (tenant_id, entity_kind, period, domain, language, entity_id)
);
