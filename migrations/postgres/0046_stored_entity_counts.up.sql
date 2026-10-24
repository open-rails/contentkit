-- Entity counts and automatic covers are stored again and rebuilt by the
-- recount job; listings read facets refreshed by the windows job. Counting per
-- request scanned every member's facets for every card. Direct access
-- decisions still call content_current_entity_ids.
DROP VIEW content_current_facets;

CREATE TABLE content_entity_counts (
    tenant_id text NOT NULL,
    entity_id text NOT NULL,
    content_kind text NOT NULL,
    language text NOT NULL,
    content_count integer NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_entity_counts_pkey PRIMARY KEY (tenant_id, entity_id, content_kind, language),
    CONSTRAINT content_entity_counts_tenant_id_entity_id_fkey FOREIGN KEY (tenant_id, entity_id) REFERENCES content_entities(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX content_entity_counts_rank_idx ON content_entity_counts USING btree (tenant_id, content_kind, language, content_count DESC);
