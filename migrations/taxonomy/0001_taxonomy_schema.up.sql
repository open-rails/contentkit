-- parent: root
-- ContentKit taxonomy lineage (contentkit #7/#8): generic catalog records,
-- localized names and aliases, typed node relationships and typed
-- content-to-taxonomy assignments, all tenant-scoped. Apply after the keyword
-- profile in the same schema: names normalize through
-- contentkit_keyword_normalize and counts/browse read content_search_documents.
-- Four catalog tables, 31 columns, plus the derived content_node_counts.
DO $$ BEGIN PERFORM 'contentkit_keyword_normalize(text)'::regprocedure; END $$;

CREATE TABLE content_nodes (
    tenant_id text NOT NULL,
    taxonomy_id text NOT NULL,
    kind text NOT NULL,
    slug text NOT NULL,
    state text NOT NULL DEFAULT 'active',
    source_revision bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, taxonomy_id),
    CHECK (tenant_id <> '' AND taxonomy_id <> '' AND kind <> '' AND slug <> ''),
    CHECK (state IN ('active', 'merged', 'deleted'))
);
-- One live slug per tenant and kind; merged and deleted nodes release theirs.
CREATE UNIQUE INDEX content_nodes_slug ON content_nodes (tenant_id, kind, slug) WHERE state = 'active';
CREATE INDEX content_nodes_kind ON content_nodes (tenant_id, kind, state, taxonomy_id);

CREATE TABLE content_node_names (
    tenant_id text NOT NULL,
    taxonomy_id text NOT NULL,
    language text NOT NULL,
    kind text NOT NULL,
    name text NOT NULL,
    normalized text NOT NULL GENERATED ALWAYS AS (contentkit_keyword_normalize(name)) STORED,
    source_revision bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, taxonomy_id, language, normalized),
    FOREIGN KEY (tenant_id, taxonomy_id) REFERENCES content_nodes (tenant_id, taxonomy_id) ON DELETE CASCADE,
    CHECK (kind IN ('name', 'alias')),
    CHECK (language <> '' AND btrim(name) <> '')
);
CREATE UNIQUE INDEX content_node_names_canonical ON content_node_names (tenant_id, taxonomy_id, language) WHERE kind = 'name';
CREATE INDEX content_node_names_lookup ON content_node_names (tenant_id, language, normalized);

-- (from, relation, to) reads "from is <relation> of to".
CREATE TABLE content_edges (
    tenant_id text NOT NULL,
    from_taxonomy_id text NOT NULL,
    relation text NOT NULL,
    to_taxonomy_id text NOT NULL,
    source_revision bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, from_taxonomy_id, relation, to_taxonomy_id),
    FOREIGN KEY (tenant_id, from_taxonomy_id) REFERENCES content_nodes (tenant_id, taxonomy_id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, to_taxonomy_id) REFERENCES content_nodes (tenant_id, taxonomy_id) ON DELETE CASCADE,
    CHECK (relation IN ('alias_of', 'member_of', 'artist_of', 'voice_of', 'parent', 'child', 'synonym')),
    CHECK (from_taxonomy_id <> to_taxonomy_id)
);
CREATE INDEX content_edges_to ON content_edges (tenant_id, to_taxonomy_id, relation);

-- content_version_id NULL scopes the assignment to the work; a version row
-- describes that version only and never a sibling.
CREATE TABLE content_assignments (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    content_version_id text,
    taxonomy_id text NOT NULL,
    relation text NOT NULL,
    source_revision bigint NOT NULL DEFAULT 0,
    state text NOT NULL DEFAULT 'active',
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, taxonomy_id, relation),
    FOREIGN KEY (tenant_id, taxonomy_id) REFERENCES content_nodes (tenant_id, taxonomy_id) ON DELETE CASCADE,
    CHECK (content_kind <> '' AND content_id <> '' AND relation <> '' AND content_version_id IS DISTINCT FROM ''),
    CHECK (state IN ('active', 'proposed'))
);
CREATE INDEX content_assignments_node ON content_assignments (tenant_id, taxonomy_id, content_kind, content_id);

-- Derived: distinct works per node, content kind and document language with
-- an eligible document whose effective assignments include the node.
-- Recomputed on write and by RebuildCounts, never by trigger.
CREATE TABLE content_node_counts (
    tenant_id text NOT NULL,
    taxonomy_id text NOT NULL,
    content_kind text NOT NULL,
    language text NOT NULL,
    content_count integer NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, taxonomy_id, content_kind, language),
    FOREIGN KEY (tenant_id, taxonomy_id) REFERENCES content_nodes (tenant_id, taxonomy_id) ON DELETE CASCADE
);
CREATE INDEX content_node_counts_rank ON content_node_counts (tenant_id, content_kind, language, content_count DESC);
