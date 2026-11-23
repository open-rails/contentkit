CREATE TABLE content_location_rules (
    tenant_id text NOT NULL,
    id text NOT NULL,
    target text NOT NULL CHECK (target IN ('site', 'content', 'entities')),
    content_ids text[] NOT NULL DEFAULT '{}',
    entity_ids text[] NOT NULL DEFAULT '{}',
    domains text[] NOT NULL,
    enabled boolean NOT NULL,
    note text NOT NULL DEFAULT '',
    created_by text NOT NULL,
    created_at timestamp with time zone NOT NULL DEFAULT now(),
    updated_by text NOT NULL,
    updated_at timestamp with time zone NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    CHECK ((target = 'site' AND cardinality(content_ids) = 0 AND cardinality(entity_ids) = 0)
        OR (target = 'content' AND cardinality(content_ids) > 0 AND cardinality(entity_ids) = 0)
        OR (target = 'entities' AND cardinality(entity_ids) > 0 AND cardinality(content_ids) = 0))
);
CREATE INDEX content_location_rules_enabled_tenant_idx ON content_location_rules (tenant_id) WHERE enabled;
CREATE INDEX content_location_rules_domains_idx ON content_location_rules USING gin (domains) WHERE enabled;

CREATE TABLE content_location_rule_entries (
    tenant_id text NOT NULL,
    rule_id text NOT NULL,
    position integer NOT NULL,
    country text,
    subdivision text,
    network cidr,
    PRIMARY KEY (tenant_id, rule_id, position),
    FOREIGN KEY (tenant_id, rule_id) REFERENCES content_location_rules (tenant_id, id) ON DELETE CASCADE,
    CHECK (num_nonnulls(country, subdivision, network) = 1)
);

-- Deliberately no rule foreign key: deleting a rule retains its audit trail.
CREATE TABLE content_location_rule_history (
    tenant_id text NOT NULL,
    id bigint GENERATED ALWAYS AS IDENTITY,
    rule_id text NOT NULL,
    action text NOT NULL CHECK (action IN ('create', 'update', 'enable', 'disable', 'delete')),
    actor text NOT NULL,
    at timestamp with time zone NOT NULL DEFAULT now(),
    snapshot jsonb NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX content_location_rule_history_rule_idx ON content_location_rule_history (tenant_id, rule_id, id DESC);
