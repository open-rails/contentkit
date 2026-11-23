CREATE TABLE content_location_rules (
    tenant_id text NOT NULL,
    id text NOT NULL,
    target text NOT NULL CHECK (target IN ('site', 'content', 'entities')),
    enabled boolean NOT NULL,
    note text NOT NULL DEFAULT '',
    created_by text NOT NULL,
    created_at timestamp with time zone NOT NULL DEFAULT now(),
    updated_by text NOT NULL,
    updated_at timestamp with time zone NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX content_location_rules_enabled_tenant_idx ON content_location_rules (tenant_id, id) WHERE enabled;

CREATE TABLE content_location_rule_domains (
    tenant_id text NOT NULL,
    rule_id text NOT NULL,
    domain text NOT NULL,
    PRIMARY KEY (tenant_id, rule_id, domain),
    FOREIGN KEY (tenant_id, rule_id) REFERENCES content_location_rules (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX content_location_rule_domains_domain_idx ON content_location_rule_domains (tenant_id, domain, rule_id);

CREATE TABLE content_location_rule_contents (
    tenant_id text NOT NULL,
    rule_id text NOT NULL,
    content_id text NOT NULL,
    PRIMARY KEY (tenant_id, rule_id, content_id),
    FOREIGN KEY (tenant_id, rule_id) REFERENCES content_location_rules (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, content_id) REFERENCES content_items (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX content_location_rule_contents_content_idx ON content_location_rule_contents (tenant_id, content_id);

CREATE TABLE content_location_rule_entities (
    tenant_id text NOT NULL,
    rule_id text NOT NULL,
    entity_id text NOT NULL,
    PRIMARY KEY (tenant_id, rule_id, entity_id),
    FOREIGN KEY (tenant_id, rule_id) REFERENCES content_location_rules (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, entity_id) REFERENCES content_entities (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX content_location_rule_entities_entity_idx ON content_location_rule_entities (tenant_id, entity_id);

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
