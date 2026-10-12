-- ContentKit #117: ContentKit owns content. Contract for #119-#123 lanes.
-- Ids are UUIDv7 text, as every other ContentKit table. Scheduled = published
-- with publish_at in the future; listing is separate from state.

CREATE TABLE content_items (
    tenant_id text NOT NULL,
    id text NOT NULL,
    kind text NOT NULL,
    state text DEFAULT 'draft'::text NOT NULL,
    publish_at timestamp with time zone,
    published_at timestamp with time zone,
    deleted_at timestamp with time zone,
    listing text DEFAULT 'listed'::text NOT NULL,
    access jsonb DEFAULT '{"level": "public"}'::jsonb NOT NULL,
    restrictions text[] DEFAULT '{}'::text[] NOT NULL,
    attributes jsonb DEFAULT '{}'::jsonb NOT NULL,
    owner_group_id text,
    cover jsonb,
    moderation text DEFAULT 'approved'::text NOT NULL,
    moderation_reason text,
    moderation_verdict jsonb,
    moderated_by text,
    moderated_at timestamp with time zone,
    moderation_revision bigint DEFAULT 1 NOT NULL,
    published_snapshot jsonb,
    ever_sold boolean DEFAULT false NOT NULL,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    revision bigint DEFAULT 1 NOT NULL,
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT content_items_ids_check CHECK (((tenant_id <> ''::text) AND (id <> ''::text) AND (kind <> ''::text))),
    CONSTRAINT content_items_state_check CHECK ((state = ANY (ARRAY['draft'::text, 'published'::text, 'deleted'::text]))),
    CONSTRAINT content_items_listing_check CHECK ((listing = ANY (ARRAY['listed'::text, 'members'::text, 'unlisted'::text]))),
    CONSTRAINT content_items_moderation_check CHECK ((moderation = ANY (ARRAY['approved'::text, 'held'::text, 'rejected'::text]))),
    CONSTRAINT content_items_access_check CHECK ((jsonb_typeof(access) = 'object'::text)),
    CONSTRAINT content_items_attributes_check CHECK ((jsonb_typeof(attributes) = 'object'::text))
);

CREATE INDEX content_items_kind_published_idx ON content_items (tenant_id, kind, published_at DESC, id DESC) WHERE (state = 'published'::text);
CREATE INDEX content_items_owner_group_idx ON content_items (tenant_id, owner_group_id) WHERE (owner_group_id IS NOT NULL);
CREATE INDEX content_items_due_idx ON content_items (publish_at) WHERE ((state = 'published'::text) AND (published_at IS NULL));

CREATE TABLE content_text (
    tenant_id text NOT NULL,
    content_id text NOT NULL,
    language text NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    slug text DEFAULT ''::text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    body text DEFAULT ''::text NOT NULL,
    sort_key text DEFAULT ''::text NOT NULL,
    aliases text[] DEFAULT '{}'::text[] NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    PRIMARY KEY (tenant_id, content_id, language),
    FOREIGN KEY (tenant_id, content_id) REFERENCES content_items (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT content_text_language_check CHECK ((language <> ''::text))
);

CREATE TABLE content_creators (
    tenant_id text NOT NULL,
    content_id text NOT NULL,
    user_id text NOT NULL,
    role text DEFAULT 'author'::text NOT NULL,
    position integer DEFAULT 0 NOT NULL,
    PRIMARY KEY (tenant_id, content_id, user_id, role),
    FOREIGN KEY (tenant_id, content_id) REFERENCES content_items (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX content_creators_user_idx ON content_creators (tenant_id, user_id);

CREATE TABLE content_versions (
    tenant_id text NOT NULL,
    id text NOT NULL,
    content_id text NOT NULL,
    position integer DEFAULT 0 NOT NULL,
    languages text[] NOT NULL,
    traits text[] DEFAULT '{}'::text[] NOT NULL,
    label text DEFAULT ''::text NOT NULL,
    state text DEFAULT 'draft'::text NOT NULL,
    publish_at timestamp with time zone,
    published_at timestamp with time zone,
    deleted_at timestamp with time zone,
    access jsonb,
    attributes jsonb DEFAULT '{}'::jsonb NOT NULL,
    pages integer,
    duration_ms bigint,
    width integer,
    height integer,
    has_audio boolean DEFAULT false NOT NULL,
    ordinal integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    revision bigint DEFAULT 1 NOT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, content_id) REFERENCES content_items (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT content_versions_languages_check CHECK ((cardinality(languages) > 0)),
    CONSTRAINT content_versions_state_check CHECK ((state = ANY (ARRAY['draft'::text, 'published'::text, 'deleted'::text]))),
    CONSTRAINT content_versions_access_check CHECK (((access IS NULL) OR (jsonb_typeof(access) = 'object'::text)))
);

CREATE INDEX content_versions_content_idx ON content_versions (tenant_id, content_id, position);
CREATE INDEX content_versions_due_idx ON content_versions (publish_at) WHERE ((state = 'published'::text) AND (published_at IS NULL));

CREATE TABLE content_version_text (
    tenant_id text NOT NULL,
    version_id text NOT NULL,
    language text NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    PRIMARY KEY (tenant_id, version_id, language),
    FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions (tenant_id, id) ON DELETE CASCADE
);

CREATE TABLE content_segments (
    tenant_id text NOT NULL,
    version_id text NOT NULL,
    position integer NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    start_index integer NOT NULL,
    end_index integer NOT NULL,
    release_at timestamp with time zone,
    PRIMARY KEY (tenant_id, version_id, position),
    FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT content_segments_range_check CHECK (((start_index >= 0) AND (end_index > start_index)))
);

-- Listing projection: one row per version, written in the transaction that
-- changes any input. Popularity and counts are refreshed by engine jobs.
CREATE TABLE content_facets (
    tenant_id text NOT NULL,
    content_id text NOT NULL,
    version_id text NOT NULL,
    kind text NOT NULL,
    live_from timestamp with time zone,
    deleted boolean DEFAULT false NOT NULL,
    listing text NOT NULL,
    languages text[] NOT NULL,
    access_level text NOT NULL,
    access_tiers text[] DEFAULT '{}'::text[] NOT NULL,
    access_group_id text,
    restrictions text[] DEFAULT '{}'::text[] NOT NULL,
    group_ids text[] DEFAULT '{}'::text[] NOT NULL,
    traits text[] DEFAULT '{}'::text[] NOT NULL,
    owner_group_id text,
    position integer DEFAULT 0 NOT NULL,
    published_at timestamp with time zone,
    attributes jsonb DEFAULT '{}'::jsonb NOT NULL,
    likes integer DEFAULT 0 NOT NULL,
    favorites integer DEFAULT 0 NOT NULL,
    rating double precision DEFAULT 0 NOT NULL,
    popularity jsonb DEFAULT '{}'::jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    PRIMARY KEY (tenant_id, version_id),
    FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX content_facets_list_idx ON content_facets (tenant_id, kind, published_at DESC, content_id DESC) WHERE (NOT deleted);
CREATE INDEX content_facets_content_idx ON content_facets (tenant_id, content_id);
CREATE INDEX content_facets_groups_idx ON content_facets USING gin (group_ids);
CREATE INDEX content_facets_languages_idx ON content_facets USING gin (languages);

CREATE TABLE content_offers (
    tenant_id text NOT NULL,
    id text NOT NULL,
    target_kind text NOT NULL,
    target_id text NOT NULL,
    terms jsonb NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    product_id text,
    price_id text,
    synced_revision bigint DEFAULT 0 NOT NULL,
    last_error text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    revision bigint DEFAULT 1 NOT NULL,
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT content_offers_target_check CHECK ((target_kind = ANY (ARRAY['content'::text, 'version'::text, 'group'::text]))),
    CONSTRAINT content_offers_state_check CHECK ((state = ANY (ARRAY['active'::text, 'withdrawn'::text])))
);

CREATE UNIQUE INDEX content_offers_live_target_idx ON content_offers (tenant_id, target_kind, target_id) WHERE (state = 'active'::text);
CREATE INDEX content_offers_unsynced_idx ON content_offers (tenant_id) WHERE (synced_revision < revision);

CREATE TABLE content_filter_prefs (
    tenant_id text NOT NULL,
    user_id text NOT NULL,
    overrides jsonb DEFAULT '{}'::jsonb NOT NULL,
    include_deleted boolean DEFAULT false NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    PRIMARY KEY (tenant_id, user_id)
);

CREATE TABLE content_events (
    tenant_id text NOT NULL,
    id bigint GENERATED ALWAYS AS IDENTITY,
    type text NOT NULL,
    entity_id text NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    delivered_at timestamp with time zone,
    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX content_events_undelivered_idx ON content_events (tenant_id, id) WHERE (delivered_at IS NULL);

CREATE TABLE content_external_ids (
    tenant_id text NOT NULL,
    source text NOT NULL,
    entity text NOT NULL,
    external_id text NOT NULL,
    id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    PRIMARY KEY (tenant_id, source, entity, external_id)
);

CREATE INDEX content_external_ids_id_idx ON content_external_ids (tenant_id, entity, id);
