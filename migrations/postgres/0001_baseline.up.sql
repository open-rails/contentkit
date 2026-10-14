-- ContentKit's schema: a fresh baseline (#117; pre-launch, no upgrade path from
-- older installs). migratekit selects the host schema; extensions live in public
-- so several schemas share them. A version id is NULL for the work itself.
CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS pgroonga WITH SCHEMA public;

CREATE FUNCTION contentkit_bump_dirty_revision() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    NEW.revision := nextval(pg_get_serial_sequence(
        format('%I.%I', TG_TABLE_SCHEMA, TG_TABLE_NAME), 'revision'));
    RETURN NEW;
END;
$$;

CREATE FUNCTION contentkit_content_code() RETURNS text
    LANGUAGE plpgsql PARALLEL SAFE
    AS $$
DECLARE
    b bytea;
    c text;
    i int;
BEGIN
    LOOP
        b := uuid_send(gen_random_uuid());
        c := '';
        -- Bytes 6 and 8 carry the UUID version and variant; the other nine are random.
        FOREACH i IN ARRAY ARRAY[0, 1, 2, 3, 4, 5, 9, 10, 11] LOOP
            c := c || substr('0123456789ABCDEFGHJKMNPQRSTVWXYZ', (get_byte(b, i) & 31) + 1, 1);
        END LOOP;
        IF c ~ '[A-Z]' THEN
            RETURN c;
        END IF;
    END LOOP;
END;
$$;

-- A version's code: four Crockford characters, at least one a letter, unique
-- within its content; a code an insert names (an import's) stays.
CREATE FUNCTION contentkit_version_code() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    b bytea;
    c text;
    i int;
    taken boolean;
BEGIN
    IF NEW.code IS NOT NULL THEN
        RETURN NEW;
    END IF;
    LOOP
        b := uuid_send(gen_random_uuid());
        c := '';
        FOREACH i IN ARRAY ARRAY[0, 1, 2, 3] LOOP
            c := c || substr('0123456789ABCDEFGHJKMNPQRSTVWXYZ', (get_byte(b, i) & 31) + 1, 1);
        END LOOP;
        CONTINUE WHEN c !~ '[A-Z]';
        EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.%I WHERE tenant_id = $1 AND content_id = $2 AND code = $3)', TG_TABLE_SCHEMA, TG_TABLE_NAME)
            INTO taken USING NEW.tenant_id, NEW.content_id, c;
        IF NOT taken THEN
            NEW.code := c;
            RETURN NEW;
        END IF;
    END LOOP;
END;
$$;

CREATE FUNCTION contentkit_first_published() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    NEW.first_published_at := coalesce(NEW.first_published_at, NEW.published_at);
    RETURN NEW;
END;
$$;

CREATE FUNCTION contentkit_group_deleted() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.state <> 'deleted' THEN
        NEW.deleted_at := NULL;
        NEW.media_purged_at := NULL;
    ELSIF TG_OP = 'INSERT' OR OLD.state <> 'deleted' THEN
        NEW.deleted_at := coalesce(NEW.deleted_at, now());
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION contentkit_keyword_normalize(value text) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN TRIM(BOTH FROM regexp_replace(lower(NORMALIZE(regexp_replace(NORMALIZE(COALESCE(value, ''::text), NFKD), '[̀-ͯ]'::text, ''::text, 'g'::text), NFC)), '\s+'::text, ' '::text, 'g'::text));

CREATE FUNCTION contentkit_keyword_terms(title text, aliases text[], keywords text[], legacy text) RETURNS text[]
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN (SELECT COALESCE(array_agg(contentkit_keyword_normalize(input_terms.term) ORDER BY input_terms.ordinal), '{}'::text[]) AS "coalesce" FROM unnest(((ARRAY[COALESCE(NULLIF(contentkit_keyword_terms.title, ''::text), contentkit_keyword_terms.legacy, ''::text)] || contentkit_keyword_terms.aliases) || contentkit_keyword_terms.keywords)) WITH ORDINALITY input_terms(term, ordinal));

CREATE FUNCTION contentkit_keyword_text(title text, aliases text[], keywords text[], legacy text) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN contentkit_keyword_normalize(concat_ws(' '::text, COALESCE(NULLIF(title, ''::text), legacy, ''::text), array_to_string(aliases, ' '::text), array_to_string(keywords, ' '::text)));


SET default_table_access_method = heap;

-- Content: items, per-language text, creators, versions, their text and segments, and version publications.

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
    comments text DEFAULT 'open'::text NOT NULL,
    first_published_at timestamp with time zone,
    deleted_with_group text,
    view_baseline bigint DEFAULT 0 NOT NULL,
    CONSTRAINT content_items_pkey PRIMARY KEY (tenant_id, id),
    CONSTRAINT content_items_access_check CHECK ((jsonb_typeof(access) = 'object'::text)),
    CONSTRAINT content_items_attributes_check CHECK ((jsonb_typeof(attributes) = 'object'::text)),
    CONSTRAINT content_items_comments_check CHECK ((comments = ANY (ARRAY['open'::text, 'closed'::text, 'hidden'::text]))),
    CONSTRAINT content_items_ids_check CHECK (((tenant_id <> ''::text) AND (id <> ''::text) AND (kind <> ''::text))),
    CONSTRAINT content_items_listing_check CHECK ((listing = ANY (ARRAY['listed'::text, 'members'::text, 'unlisted'::text]))),
    CONSTRAINT content_items_moderation_check CHECK ((moderation = ANY (ARRAY['approved'::text, 'held'::text, 'rejected'::text]))),
    CONSTRAINT content_items_state_check CHECK ((state = ANY (ARRAY['draft'::text, 'published'::text, 'deleted'::text]))),
    CONSTRAINT content_items_view_baseline_check CHECK ((view_baseline >= 0))
);

CREATE INDEX content_items_deleted_with_group_idx ON content_items USING btree (tenant_id, deleted_with_group) WHERE (deleted_with_group IS NOT NULL);
CREATE INDEX content_items_drafts_idx ON content_items USING btree (tenant_id, kind, created_at) WHERE ((state = 'draft'::text) AND (first_published_at IS NULL));
CREATE INDEX content_items_due_idx ON content_items USING btree (publish_at) WHERE ((state = 'published'::text) AND (published_at IS NULL));
CREATE INDEX content_items_kind_published_idx ON content_items USING btree (tenant_id, kind, published_at DESC, id DESC) WHERE (state = 'published'::text);
CREATE INDEX content_items_owner_group_idx ON content_items USING btree (tenant_id, owner_group_id) WHERE (owner_group_id IS NOT NULL);
CREATE TRIGGER content_items_first_published BEFORE INSERT OR UPDATE OF published_at ON content_items FOR EACH ROW EXECUTE FUNCTION contentkit_first_published();

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
    CONSTRAINT content_text_pkey PRIMARY KEY (tenant_id, content_id, language),
    CONSTRAINT content_text_language_check CHECK ((language <> ''::text)),
    CONSTRAINT content_text_tenant_id_content_id_fkey FOREIGN KEY (tenant_id, content_id) REFERENCES content_items(tenant_id, id) ON DELETE CASCADE
);

CREATE TABLE content_creators (
    tenant_id text NOT NULL,
    content_id text NOT NULL,
    user_id text NOT NULL,
    role text DEFAULT 'author'::text NOT NULL,
    "position" integer DEFAULT 0 NOT NULL,
    CONSTRAINT content_creators_pkey PRIMARY KEY (tenant_id, content_id, user_id, role),
    CONSTRAINT content_creators_tenant_id_content_id_fkey FOREIGN KEY (tenant_id, content_id) REFERENCES content_items(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX content_creators_user_idx ON content_creators USING btree (tenant_id, user_id);

CREATE TABLE content_versions (
    tenant_id text NOT NULL,
    id text NOT NULL,
    content_id text NOT NULL,
    code text NOT NULL,
    "position" integer DEFAULT 0 NOT NULL,
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
    CONSTRAINT content_versions_pkey PRIMARY KEY (tenant_id, id),
    CONSTRAINT content_versions_code_key UNIQUE (tenant_id, content_id, code),
    CONSTRAINT content_versions_code_check CHECK (((code ~ '^[0-9A-HJKMNP-TV-Z]{4}$'::text) AND (code ~ '[A-Z]'::text))),
    CONSTRAINT content_versions_access_check CHECK (((access IS NULL) OR (jsonb_typeof(access) = 'object'::text))),
    CONSTRAINT content_versions_languages_check CHECK ((cardinality(languages) > 0)),
    CONSTRAINT content_versions_state_check CHECK ((state = ANY (ARRAY['draft'::text, 'published'::text, 'deleted'::text]))),
    CONSTRAINT content_versions_tenant_id_content_id_fkey FOREIGN KEY (tenant_id, content_id) REFERENCES content_items(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX content_versions_content_idx ON content_versions USING btree (tenant_id, content_id, "position");
CREATE INDEX content_versions_due_idx ON content_versions USING btree (publish_at) WHERE ((state = 'published'::text) AND (published_at IS NULL));
CREATE TRIGGER content_versions_code BEFORE INSERT ON content_versions FOR EACH ROW EXECUTE FUNCTION contentkit_version_code();

CREATE TABLE content_version_text (
    tenant_id text NOT NULL,
    version_id text NOT NULL,
    language text NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    CONSTRAINT content_version_text_pkey PRIMARY KEY (tenant_id, version_id, language),
    CONSTRAINT content_version_text_tenant_id_version_id_fkey FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions(tenant_id, id) ON DELETE CASCADE
);

CREATE TABLE content_segments (
    tenant_id text NOT NULL,
    version_id text NOT NULL,
    "position" integer NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    start_index integer NOT NULL,
    end_index integer NOT NULL,
    release_at timestamp with time zone,
    CONSTRAINT content_segments_pkey PRIMARY KEY (tenant_id, version_id, "position"),
    CONSTRAINT content_segments_range_check CHECK (((start_index >= 0) AND (end_index > start_index))),
    CONSTRAINT content_segments_tenant_id_version_id_fkey FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions(tenant_id, id) ON DELETE CASCADE
);

CREATE TABLE content_version_publications (
    tenant_id text NOT NULL,
    id text NOT NULL,
    version_id text NOT NULL,
    kind text NOT NULL,
    state text DEFAULT 'building'::text NOT NULL,
    source jsonb NOT NULL,
    previews integer,
    previews_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    ready_at timestamp with time zone,
    CONSTRAINT content_version_publications_pkey PRIMARY KEY (tenant_id, id),
    CONSTRAINT content_version_publications_kind_check CHECK ((kind = ANY (ARRAY['pages'::text, 'playback'::text]))),
    CONSTRAINT content_version_publications_state_check CHECK ((state = ANY (ARRAY['building'::text, 'current'::text]))),
    CONSTRAINT content_version_publications_tenant_id_version_id_fkey FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX content_version_publications_building_idx ON content_version_publications USING btree (tenant_id, created_at) WHERE (state = 'building'::text);
CREATE UNIQUE INDEX content_version_publications_state_idx ON content_version_publications USING btree (tenant_id, version_id, kind, state);

CREATE TABLE content_version_publication_items (
    tenant_id text NOT NULL,
    id text NOT NULL,
    publication_id text NOT NULL,
    segment integer,
    start_index integer NOT NULL,
    end_index integer NOT NULL,
    CONSTRAINT content_version_publication_items_pkey PRIMARY KEY (tenant_id, id),
    CONSTRAINT content_version_publication_items_range_check CHECK (((start_index >= 0) AND (end_index > start_index))),
    CONSTRAINT content_version_publication_items_tenant_id_publication_id_fkey FOREIGN KEY (tenant_id, publication_id) REFERENCES content_version_publications(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX content_version_publication_items_publication_idx ON content_version_publication_items USING btree (tenant_id, publication_id, start_index);

-- Groups: names, text, relations, member counts and memberships (content- or version-level).

CREATE TABLE content_groups (
    tenant_id text NOT NULL,
    id text NOT NULL,
    kind text NOT NULL,
    slug text NOT NULL,
    scope_id text DEFAULT ''::text NOT NULL,
    parent_id text,
    state text DEFAULT 'active'::text NOT NULL,
    merged_into text,
    attributes jsonb DEFAULT '{}'::jsonb NOT NULL,
    cover jsonb,
    owner_user_id text,
    auth_scope text,
    access jsonb,
    ever_sold boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    revision bigint DEFAULT 1 NOT NULL,
    avatar text,
    deleted_at timestamp with time zone,
    media_purged_at timestamp with time zone,
    CONSTRAINT content_groups_pkey PRIMARY KEY (tenant_id, id),
    CONSTRAINT content_groups_attributes_check CHECK ((jsonb_typeof(attributes) = 'object'::text)),
    CONSTRAINT content_groups_ids_check CHECK (((tenant_id <> ''::text) AND (id <> ''::text) AND (kind <> ''::text) AND (slug <> ''::text))),
    CONSTRAINT content_groups_state_check CHECK ((state = ANY (ARRAY['active'::text, 'merged'::text, 'deleted'::text])))
);

CREATE INDEX content_groups_kind_idx ON content_groups USING btree (tenant_id, kind, state, id);
CREATE INDEX content_groups_owner_idx ON content_groups USING btree (tenant_id, owner_user_id) WHERE (owner_user_id IS NOT NULL);
CREATE INDEX content_groups_parent_idx ON content_groups USING btree (tenant_id, parent_id) WHERE (parent_id IS NOT NULL);
CREATE INDEX content_groups_purge_idx ON content_groups USING btree (tenant_id, deleted_at) WHERE ((state = 'deleted'::text) AND (media_purged_at IS NULL));
CREATE UNIQUE INDEX content_groups_slug_idx ON content_groups USING btree (tenant_id, kind, scope_id, slug) WHERE (state = 'active'::text);
CREATE TRIGGER content_groups_deleted BEFORE INSERT OR UPDATE OF state ON content_groups FOR EACH ROW EXECUTE FUNCTION contentkit_group_deleted();

CREATE TABLE content_group_names (
    tenant_id text NOT NULL,
    group_id text NOT NULL,
    language text NOT NULL,
    kind text NOT NULL,
    name text NOT NULL,
    normalized text GENERATED ALWAYS AS (contentkit_keyword_normalize(name)) STORED NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_group_names_pkey PRIMARY KEY (tenant_id, group_id, language, normalized),
    CONSTRAINT content_group_names_check CHECK (((language <> ''::text) AND (btrim(name) <> ''::text))),
    CONSTRAINT content_group_names_kind_check CHECK ((kind = ANY (ARRAY['name'::text, 'alias'::text]))),
    CONSTRAINT content_group_names_tenant_id_group_id_fkey FOREIGN KEY (tenant_id, group_id) REFERENCES content_groups(tenant_id, id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX content_group_names_canonical_idx ON content_group_names USING btree (tenant_id, group_id, language) WHERE (kind = 'name'::text);
CREATE INDEX content_group_names_lookup_idx ON content_group_names USING btree (tenant_id, language, normalized);

CREATE TABLE content_group_text (
    tenant_id text NOT NULL,
    group_id text NOT NULL,
    language text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    sort_key text DEFAULT ''::text NOT NULL,
    CONSTRAINT content_group_text_pkey PRIMARY KEY (tenant_id, group_id, language),
    CONSTRAINT content_group_text_tenant_id_group_id_fkey FOREIGN KEY (tenant_id, group_id) REFERENCES content_groups(tenant_id, id) ON DELETE CASCADE
);

CREATE TABLE content_group_edges (
    tenant_id text NOT NULL,
    from_id text NOT NULL,
    relation text NOT NULL,
    to_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_group_edges_pkey PRIMARY KEY (tenant_id, from_id, relation, to_id),
    CONSTRAINT content_group_edges_check CHECK ((from_id <> to_id)),
    CONSTRAINT content_group_edges_tenant_id_from_id_fkey FOREIGN KEY (tenant_id, from_id) REFERENCES content_groups(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT content_group_edges_tenant_id_to_id_fkey FOREIGN KEY (tenant_id, to_id) REFERENCES content_groups(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX content_group_edges_to_idx ON content_group_edges USING btree (tenant_id, to_id, relation);

CREATE TABLE content_group_counts (
    tenant_id text NOT NULL,
    group_id text NOT NULL,
    content_kind text NOT NULL,
    language text NOT NULL,
    content_count integer NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_group_counts_pkey PRIMARY KEY (tenant_id, group_id, content_kind, language),
    CONSTRAINT content_group_counts_tenant_id_group_id_fkey FOREIGN KEY (tenant_id, group_id) REFERENCES content_groups(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX content_group_counts_rank_idx ON content_group_counts USING btree (tenant_id, content_kind, language, content_count DESC);

CREATE TABLE content_memberships (
    tenant_id text NOT NULL,
    group_id text NOT NULL,
    content_id text NOT NULL,
    version_id text,
    role text DEFAULT ''::text NOT NULL,
    "position" integer,
    active_from timestamp with time zone,
    active_until timestamp with time zone,
    state text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_memberships_state_check CHECK ((state = ANY (ARRAY['active'::text, 'proposed'::text]))),
    CONSTRAINT content_memberships_window_check CHECK (((active_from IS NULL) OR (active_until IS NULL) OR (active_until > active_from))),
    CONSTRAINT content_memberships_tenant_id_content_id_fkey FOREIGN KEY (tenant_id, content_id) REFERENCES content_items(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT content_memberships_tenant_id_group_id_fkey FOREIGN KEY (tenant_id, group_id) REFERENCES content_groups(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT content_memberships_tenant_id_version_id_fkey FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX content_memberships_content_idx ON content_memberships USING btree (tenant_id, content_id, version_id);
CREATE INDEX content_memberships_group_idx ON content_memberships USING btree (tenant_id, group_id, "position", content_id);
CREATE UNIQUE INDEX content_memberships_key_idx ON content_memberships USING btree (tenant_id, group_id, content_id, version_id, role) NULLS NOT DISTINCT;
CREATE INDEX content_memberships_window_idx ON content_memberships USING btree (tenant_id, active_from, active_until) WHERE ((active_from IS NOT NULL) OR (active_until IS NOT NULL));

-- Listing, offers, filters, the outbox, import ids and content codes.

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
    access_free_after timestamp with time zone,
    restrictions text[] DEFAULT '{}'::text[] NOT NULL,
    group_ids text[] DEFAULT '{}'::text[] NOT NULL,
    traits text[] DEFAULT '{}'::text[] NOT NULL,
    owner_group_id text,
    "position" integer DEFAULT 0 NOT NULL,
    published_at timestamp with time zone,
    attributes jsonb DEFAULT '{}'::jsonb NOT NULL,
    likes integer DEFAULT 0 NOT NULL,
    favorites integer DEFAULT 0 NOT NULL,
    rating double precision DEFAULT 0 NOT NULL,
    popularity jsonb DEFAULT '{}'::jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    dislikes integer DEFAULT 0 NOT NULL,
    free_applied boolean DEFAULT false NOT NULL,
    view_baseline bigint DEFAULT 0 NOT NULL,
    CONSTRAINT content_facets_pkey PRIMARY KEY (tenant_id, version_id),
    CONSTRAINT content_facets_tenant_id_version_id_fkey FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX content_facets_content_idx ON content_facets USING btree (tenant_id, content_id);
CREATE INDEX content_facets_free_idx ON content_facets USING btree (tenant_id, access_free_after) WHERE ((NOT free_applied) AND (access_free_after IS NOT NULL));
CREATE INDEX content_facets_groups_idx ON content_facets USING gin (group_ids);
CREATE INDEX content_facets_languages_idx ON content_facets USING gin (languages);
CREATE INDEX content_facets_list_idx ON content_facets USING btree (tenant_id, kind, published_at DESC, content_id DESC) WHERE (NOT deleted);
CREATE INDEX content_facets_owner_idx ON content_facets USING btree (tenant_id, owner_group_id, published_at DESC) WHERE (owner_group_id IS NOT NULL);

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
    CONSTRAINT content_offers_pkey PRIMARY KEY (tenant_id, id),
    CONSTRAINT content_offers_state_check CHECK ((state = ANY (ARRAY['active'::text, 'withdrawn'::text]))),
    CONSTRAINT content_offers_target_check CHECK ((target_kind = ANY (ARRAY['content'::text, 'version'::text, 'group'::text])))
);

CREATE UNIQUE INDEX content_offers_live_target_idx ON content_offers USING btree (tenant_id, target_kind, target_id) WHERE (state = 'active'::text);
CREATE INDEX content_offers_unsynced_idx ON content_offers USING btree (tenant_id) WHERE (synced_revision < revision);

CREATE TABLE content_filter_prefs (
    tenant_id text NOT NULL,
    user_id text NOT NULL,
    overrides jsonb DEFAULT '{}'::jsonb NOT NULL,
    include_deleted boolean DEFAULT false NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_filter_prefs_pkey PRIMARY KEY (tenant_id, user_id)
);

CREATE TABLE content_events (
    tenant_id text NOT NULL,
    id bigint GENERATED ALWAYS AS IDENTITY NOT NULL,
    type text NOT NULL,
    entity_id text NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    delivered_at timestamp with time zone,
    CONSTRAINT content_events_pkey PRIMARY KEY (tenant_id, id)
);

CREATE INDEX content_events_undelivered_idx ON content_events USING btree (tenant_id, id) WHERE (delivered_at IS NULL);

CREATE TABLE content_external_ids (
    tenant_id text NOT NULL,
    source text NOT NULL,
    entity text NOT NULL,
    external_id text NOT NULL,
    id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_external_ids_pkey PRIMARY KEY (tenant_id, source, entity, external_id)
);

CREATE INDEX content_external_ids_id_idx ON content_external_ids USING btree (tenant_id, entity, id);

CREATE TABLE content_codes (
    tenant_id text NOT NULL,
    code text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    slug text DEFAULT ''::text NOT NULL,
    slugs jsonb DEFAULT '{}'::jsonb NOT NULL,
    merged_into text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_codes_pkey PRIMARY KEY (tenant_id, code),
    CONSTRAINT content_codes_ref_key UNIQUE (tenant_id, content_kind, content_id),
    CONSTRAINT content_codes_code_ck CHECK (((code ~ '^[0-9A-HJKMNP-TV-Z]{9}$'::text) AND (code ~ '[A-Z]'::text))),
    CONSTRAINT content_codes_merged_ck CHECK ((merged_into IS DISTINCT FROM code)),
    CONSTRAINT content_codes_ref_ck CHECK (((tenant_id <> ''::text) AND (content_kind <> ''::text) AND (content_id <> ''::text) AND (content_id = lower(content_id)))),
    CONSTRAINT content_codes_slug_ck CHECK (((slug ~ '^([a-z0-9]+(-[a-z0-9]+)*)?$'::text) AND (length(slug) <= 80))),
    CONSTRAINT content_codes_slugs_ck CHECK ((jsonb_typeof(slugs) = 'object'::text)),
    CONSTRAINT content_codes_merged_into_fkey FOREIGN KEY (tenant_id, merged_into) REFERENCES content_codes(tenant_id, code)
);

CREATE INDEX content_codes_merged_into ON content_codes USING btree (tenant_id, merged_into) WHERE (merged_into IS NOT NULL);

CREATE TABLE content_code_aliases (
    tenant_id text NOT NULL,
    source text NOT NULL,
    legacy_kind text NOT NULL,
    legacy_key text NOT NULL,
    code text NOT NULL,
    locator text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_code_aliases_pkey PRIMARY KEY (tenant_id, source, legacy_kind, legacy_key),
    CONSTRAINT content_code_aliases_ck CHECK (((source ~ '^[a-z0-9][a-z0-9_.-]{0,63}$'::text) AND (legacy_kind ~ '^[a-z0-9][a-z0-9_.-]{0,63}$'::text) AND (legacy_key <> ''::text) AND (length(legacy_key) <= 512) AND (locator <> ''::text))),
    CONSTRAINT content_code_aliases_code_fkey FOREIGN KEY (tenant_id, code) REFERENCES content_codes(tenant_id, code)
);

-- Interactions: comments, bans, moderation rules, reactions, favorites, counts and polls.

CREATE TABLE content_comments (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    reply_to_id uuid,
    user_id text,
    anon_name text,
    body text NOT NULL,
    likes integer DEFAULT 0 NOT NULL,
    dislikes integer DEFAULT 0 NOT NULL,
    reply_count integer DEFAULT 0 NOT NULL,
    deleted_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id text NOT NULL,
    content_version_id text,
    moderation text DEFAULT 'approved'::text NOT NULL,
    moderation_reason text,
    moderation_verdict jsonb,
    moderated_by text,
    moderated_at timestamp with time zone,
    moderation_revision bigint DEFAULT 1 NOT NULL,
    published_body text,
    language text DEFAULT ''::text NOT NULL,
    CONSTRAINT content_comments_pkey PRIMARY KEY (tenant_id, id),
    CONSTRAINT content_comments_actor_ck CHECK (((user_id IS NOT NULL) OR (anon_name IS NOT NULL))),
    CONSTRAINT content_comments_content_id_ck CHECK ((content_id = lower(content_id))),
    CONSTRAINT content_comments_moderation_ck CHECK ((moderation = ANY (ARRAY['approved'::text, 'held'::text, 'rejected'::text]))),
    CONSTRAINT content_comments_moderation_revision_check CHECK ((moderation_revision > 0)),
    CONSTRAINT content_comments_version_check CHECK ((content_version_id <> ''::text)),
    CONSTRAINT content_comments_reply_to_id_fkey FOREIGN KEY (tenant_id, reply_to_id) REFERENCES content_comments(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX content_comments_author_idx ON content_comments USING btree (tenant_id, user_id, created_at DESC, id DESC) WHERE (user_id IS NOT NULL);
CREATE INDEX content_comments_author_published_idx ON content_comments USING btree (tenant_id, user_id) INCLUDE (likes, dislikes) WHERE ((deleted_at IS NULL) AND (moderation = 'approved'::text) AND (user_id IS NOT NULL));
CREATE INDEX content_comments_held_idx ON content_comments USING btree (tenant_id, created_at, id) WHERE (moderation = 'held'::text);
CREATE INDEX content_comments_latest_idx ON content_comments USING btree (tenant_id, created_at DESC) WHERE (deleted_at IS NULL);
CREATE INDEX content_comments_reply_idx ON content_comments USING btree (tenant_id, reply_to_id, created_at) WHERE (reply_to_id IS NOT NULL);
CREATE INDEX content_comments_toplevel_idx ON content_comments USING btree (tenant_id, content_kind, content_id, content_version_id, language, created_at DESC) WHERE (reply_to_id IS NULL);

CREATE TABLE content_comment_bans (
    tenant_id text NOT NULL,
    user_id text NOT NULL,
    scope text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    until timestamp with time zone,
    banned_by text NOT NULL,
    banned_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_comment_bans_pkey PRIMARY KEY (tenant_id, user_id, scope),
    CONSTRAINT content_comment_bans_scope_ck CHECK (((scope = 'global'::text) OR ((scope ~~ 'owner:%'::text) AND (length(scope) > 6)) OR ((scope ~~ 'group:%'::text) AND (length(scope) > 6))))
);

CREATE INDEX content_comment_bans_scope_idx ON content_comment_bans USING btree (tenant_id, scope, banned_at DESC, user_id);

CREATE TABLE content_banned_words (
    tenant_id text NOT NULL,
    word text NOT NULL,
    action text DEFAULT 'mask'::text NOT NULL,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_banned_words_pkey PRIMARY KEY (tenant_id, word),
    CONSTRAINT content_banned_words_action_check CHECK ((action = ANY (ARRAY['mask'::text, 'reject'::text]))),
    CONSTRAINT content_banned_words_word_check CHECK (((btrim(word) <> ''::text) AND (word = lower(word))))
);

CREATE TABLE content_ip_bans (
    tenant_id text NOT NULL,
    network cidr NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    until timestamp with time zone,
    banned_by text NOT NULL,
    banned_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_ip_bans_pkey PRIMARY KEY (tenant_id, network)
);

CREATE TABLE content_reactions (
    content_kind text NOT NULL,
    content_id text NOT NULL,
    user_id text,
    ip text,
    value smallint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id text NOT NULL,
    content_version_id text,
    revision bigint NOT NULL,
    CONSTRAINT content_reactions_actor_ck CHECK (((user_id IS NOT NULL) OR (ip IS NOT NULL))),
    CONSTRAINT content_reactions_content_id_ck CHECK ((content_id = lower(content_id))),
    CONSTRAINT content_reactions_value_ck CHECK ((value = ANY (ARRAY['-1'::integer, 0, 1]))),
    CONSTRAINT content_reactions_version_check CHECK ((content_version_id <> ''::text))
);

CREATE INDEX content_reactions_content_idx ON content_reactions USING btree (tenant_id, content_kind, content_id, content_version_id);
CREATE UNIQUE INDEX content_reactions_ip_uq ON content_reactions USING btree (tenant_id, content_kind, content_id, content_version_id, ip) NULLS NOT DISTINCT WHERE ((user_id IS NULL) AND (ip IS NOT NULL));
CREATE INDEX content_reactions_revision_idx ON content_reactions USING btree (tenant_id, revision);
CREATE UNIQUE INDEX content_reactions_user_uq ON content_reactions USING btree (tenant_id, content_kind, content_id, content_version_id, user_id) NULLS NOT DISTINCT WHERE (user_id IS NOT NULL);

CREATE TABLE content_favorites (
    user_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id text NOT NULL,
    content_version_id text,
    value smallint NOT NULL,
    revision bigint NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_favorites_key UNIQUE NULLS NOT DISTINCT (tenant_id, user_id, content_kind, content_id, content_version_id),
    CONSTRAINT content_favorites_content_id_ck CHECK ((content_id = lower(content_id))),
    CONSTRAINT content_favorites_value_ck CHECK ((value = ANY (ARRAY[0, 1]))),
    CONSTRAINT content_favorites_version_check CHECK ((content_version_id <> ''::text))
);

CREATE INDEX content_favorites_content_idx ON content_favorites USING btree (tenant_id, content_kind, content_id, content_version_id);
CREATE INDEX content_favorites_revision_idx ON content_favorites USING btree (tenant_id, revision);
CREATE INDEX content_favorites_user_created_idx ON content_favorites USING btree (tenant_id, user_id, created_at DESC) WHERE (value = 1);

CREATE TABLE content_interaction_counts (
    content_kind text NOT NULL,
    content_id text NOT NULL,
    likes integer DEFAULT 0 NOT NULL,
    dislikes integer DEFAULT 0 NOT NULL,
    favorites integer DEFAULT 0 NOT NULL,
    comment_count integer DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id text NOT NULL,
    content_version_id text,
    CONSTRAINT content_interaction_counts_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id),
    CONSTRAINT content_interaction_counts_content_id_ck CHECK ((content_id = lower(content_id))),
    CONSTRAINT content_interaction_counts_version_check CHECK ((content_version_id <> ''::text))
);

CREATE INDEX content_interaction_counts_best_idx ON content_interaction_counts USING btree (tenant_id, content_kind, (
CASE
    WHEN ((likes + dislikes) = 0) THEN (0)::double precision
    ELSE ((((((likes)::numeric + 1.9208) / ((likes + dislikes))::numeric))::double precision - (((1.96)::double precision * sqrt(((((likes)::double precision * (dislikes)::double precision) / ((likes + dislikes))::double precision) + (0.9604)::double precision))) / ((likes + dislikes))::double precision)) / (((1)::numeric + (3.8416 / ((likes + dislikes))::numeric)))::double precision)
END) DESC) WHERE ((likes + dislikes) > 0);
CREATE INDEX content_interaction_counts_comments_idx ON content_interaction_counts USING btree (tenant_id, content_kind, comment_count DESC) WHERE (comment_count > 0);
CREATE INDEX content_interaction_counts_favorites_idx ON content_interaction_counts USING btree (tenant_id, content_kind, favorites DESC) WHERE (favorites > 0);
CREATE INDEX content_interaction_counts_likes_idx ON content_interaction_counts USING btree (tenant_id, content_kind, likes DESC) WHERE (likes > 0);

CREATE TABLE content_poll_questions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    question text NOT NULL,
    language text DEFAULT ''::text NOT NULL,
    is_active boolean DEFAULT true NOT NULL,
    image_name text,
    live_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    tenant_id text NOT NULL,
    kind text DEFAULT 'multiple_choice'::text NOT NULL,
    closes_at timestamp with time zone,
    CONSTRAINT content_poll_questions_pkey PRIMARY KEY (tenant_id, id),
    CONSTRAINT content_poll_questions_kind_ck CHECK ((kind = ANY (ARRAY['multiple_choice'::text, 'free_text'::text])))
);

CREATE INDEX content_poll_questions_live_idx ON content_poll_questions USING btree (tenant_id, language, live_at DESC) WHERE ((deleted_at IS NULL) AND is_active);

CREATE TABLE content_poll_options (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    question_id uuid NOT NULL,
    label text NOT NULL,
    image_name text,
    "position" integer DEFAULT 0 NOT NULL,
    vote_count integer DEFAULT 0 NOT NULL,
    tenant_id text NOT NULL,
    CONSTRAINT content_poll_options_pkey PRIMARY KEY (tenant_id, id),
    CONSTRAINT content_poll_options_question_id_fkey FOREIGN KEY (tenant_id, question_id) REFERENCES content_poll_questions(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX content_poll_options_question_idx ON content_poll_options USING btree (tenant_id, question_id, "position");

CREATE TABLE content_poll_votes (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    question_id uuid NOT NULL,
    option_id uuid NOT NULL,
    user_id text,
    ip text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id text NOT NULL,
    CONSTRAINT content_poll_votes_pkey PRIMARY KEY (tenant_id, id),
    CONSTRAINT content_poll_votes_actor_ck CHECK (((user_id IS NOT NULL) OR (ip IS NOT NULL))),
    CONSTRAINT content_poll_votes_option_id_fkey FOREIGN KEY (tenant_id, option_id) REFERENCES content_poll_options(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT content_poll_votes_question_id_fkey FOREIGN KEY (tenant_id, question_id) REFERENCES content_poll_questions(tenant_id, id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX content_poll_votes_ip_uq ON content_poll_votes USING btree (tenant_id, question_id, ip) WHERE ((user_id IS NULL) AND (ip IS NOT NULL));
CREATE INDEX content_poll_votes_option_idx ON content_poll_votes USING btree (tenant_id, option_id);
CREATE UNIQUE INDEX content_poll_votes_user_uq ON content_poll_votes USING btree (tenant_id, question_id, user_id) WHERE (user_id IS NOT NULL);

CREATE TABLE content_poll_answers (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id text NOT NULL,
    question_id uuid NOT NULL,
    actor_id text NOT NULL,
    text text NOT NULL,
    group_id text,
    classified_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    revision bigint DEFAULT 1 NOT NULL,
    group_label text,
    CONSTRAINT content_poll_answers_actor_uq UNIQUE (tenant_id, question_id, actor_id),
    CONSTRAINT content_poll_answers_pkey PRIMARY KEY (tenant_id, id),
    CONSTRAINT content_poll_answers_revision_check CHECK ((revision > 0)),
    CONSTRAINT content_poll_answers_question_id_fkey FOREIGN KEY (tenant_id, question_id) REFERENCES content_poll_questions(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX content_poll_answers_pending_idx ON content_poll_answers USING btree (tenant_id, id) WHERE (classified_at IS NULL);

-- Search, recent searches, preference sync and erasure.

CREATE TABLE content_search_documents (
    content_kind text NOT NULL,
    content_id text NOT NULL,
    language text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    raw_document text,
    title text DEFAULT ''::text NOT NULL,
    aliases text[] DEFAULT '{}'::text[] NOT NULL,
    keywords text[] DEFAULT '{}'::text[] NOT NULL,
    tenant_id text NOT NULL,
    content_version_id text,
    CONSTRAINT content_search_documents_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, language),
    CONSTRAINT content_search_documents_version_check CHECK ((content_version_id <> ''::text))
);

CREATE INDEX content_search_documents_keyword_exact ON content_search_documents USING gin (contentkit_keyword_terms(title, aliases, keywords, raw_document));
CREATE INDEX content_search_documents_keyword_fuzzy ON content_search_documents USING gist (contentkit_keyword_text(title, aliases, keywords, raw_document) public.gist_trgm_ops (siglen='64'));
CREATE INDEX content_search_documents_keyword_native ON content_search_documents USING pgroonga (contentkit_keyword_terms(title, aliases, keywords, raw_document));
CREATE INDEX content_search_documents_kind_language ON content_search_documents USING btree (tenant_id, content_kind, language);

CREATE SEQUENCE content_search_dirty_revision_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

CREATE TABLE content_search_dirty (
    content_kind text NOT NULL,
    content_id text NOT NULL,
    language text NOT NULL,
    is_deleted boolean DEFAULT false NOT NULL,
    reason text DEFAULT 'unknown'::text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    revision bigint DEFAULT nextval('content_search_dirty_revision_seq'::regclass) NOT NULL,
    tenant_id text NOT NULL,
    content_version_id text,
    CONSTRAINT content_search_dirty_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, language),
    CONSTRAINT content_search_dirty_version_check CHECK ((content_version_id <> ''::text))
);

ALTER SEQUENCE content_search_dirty_revision_seq OWNED BY content_search_dirty.revision;

CREATE INDEX content_search_dirty_updated_at ON content_search_dirty USING btree (tenant_id, updated_at);
CREATE TRIGGER content_search_dirty_revision BEFORE INSERT OR UPDATE ON content_search_dirty FOR EACH ROW EXECUTE FUNCTION contentkit_bump_dirty_revision();

CREATE TABLE content_search_invalid (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    content_version_id text,
    language text NOT NULL,
    revision bigint NOT NULL,
    last_error text NOT NULL,
    failed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_search_invalid_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, language),
    CONSTRAINT content_search_invalid_version_check CHECK ((content_version_id <> ''::text))
);

CREATE TABLE content_search_backfill (
    content_kind text NOT NULL,
    language text NOT NULL,
    cursor text DEFAULT ''::text NOT NULL,
    state text DEFAULT 'running'::text NOT NULL,
    last_error text,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    tenant_id text NOT NULL,
    CONSTRAINT content_search_backfill_pkey PRIMARY KEY (tenant_id, content_kind, language)
);

CREATE INDEX content_search_backfill_state ON content_search_backfill USING btree (state);

CREATE TABLE content_searches (
    tenant_id text NOT NULL,
    subject text NOT NULL,
    normalized text NOT NULL,
    query text NOT NULL,
    results integer DEFAULT 0 NOT NULL,
    searches integer DEFAULT 1 NOT NULL,
    searched_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_searches_pkey PRIMARY KEY (tenant_id, subject, normalized)
);

CREATE INDEX content_searches_age_idx ON content_searches USING btree (tenant_id, searched_at);
CREATE INDEX content_searches_recent_idx ON content_searches USING btree (tenant_id, subject, searched_at DESC);

CREATE TABLE content_search_marks (
    tenant_id text NOT NULL,
    subject text NOT NULL,
    hidden_before timestamp with time zone NOT NULL,
    CONSTRAINT content_search_marks_pkey PRIMARY KEY (tenant_id, subject)
);

CREATE SEQUENCE content_preference_revision_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

CREATE TABLE content_preference_sync (
    tenant_id text NOT NULL,
    taken_at timestamp with time zone NOT NULL,
    revision bigint NOT NULL,
    CONSTRAINT content_preference_sync_pkey PRIMARY KEY (tenant_id, taken_at)
);

CREATE TABLE content_erased_subjects (
    tenant_id text NOT NULL,
    actor_id text NOT NULL,
    erased_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT content_erased_subjects_pkey PRIMARY KEY (tenant_id, actor_id)
);

-- Media: commits, allocations, publications, quotas and the media upgrade.

CREATE TABLE content_media_commits (
    tenant_id text NOT NULL,
    operation_id uuid NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    folder_prefix text NOT NULL,
    actor_id text NOT NULL,
    fingerprint bytea NOT NULL,
    lease_id uuid NOT NULL,
    attempt_id uuid,
    expected_etag text DEFAULT ''::text NOT NULL,
    state text DEFAULT 'open'::text NOT NULL,
    effects jsonb DEFAULT '{}'::jsonb NOT NULL,
    charged_bytes bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    content_version_id text,
    CONSTRAINT content_media_commits_pkey PRIMARY KEY (tenant_id, operation_id),
    CONSTRAINT content_media_commits_attempt_id_check CHECK (((attempt_id IS NULL) OR (attempt_id <> '00000000-0000-0000-0000-000000000000'::uuid))),
    CONSTRAINT content_media_commits_check CHECK (((tenant_id <> ''::text) AND (content_kind <> ''::text) AND (content_id <> ''::text) AND (folder_prefix <> ''::text))),
    CONSTRAINT content_media_commits_check1 CHECK (((operation_id <> '00000000-0000-0000-0000-000000000000'::uuid) AND (lease_id <> '00000000-0000-0000-0000-000000000000'::uuid))),
    CONSTRAINT content_media_commits_check2 CHECK (((state <> 'prepared'::text) OR (attempt_id IS NOT NULL))),
    CONSTRAINT content_media_commits_check3 CHECK (((charged_bytes >= 0) AND (jsonb_typeof(effects) = 'object'::text))),
    CONSTRAINT content_media_commits_fingerprint_check CHECK ((octet_length(fingerprint) = 32)),
    CONSTRAINT content_media_commits_state_check CHECK ((state = ANY (ARRAY['open'::text, 'prepared'::text, 'frozen'::text, 'applied'::text, 'absent'::text]))),
    CONSTRAINT content_media_commits_version_check CHECK ((content_version_id <> ''::text))
);

CREATE INDEX content_media_commits_pending ON content_media_commits USING btree (tenant_id, updated_at, operation_id) WHERE (state = ANY (ARRAY['open'::text, 'prepared'::text, 'frozen'::text]));
CREATE UNIQUE INDEX content_media_commits_pending_folder ON content_media_commits USING btree (tenant_id, folder_prefix) WHERE (state = ANY (ARRAY['open'::text, 'prepared'::text, 'frozen'::text]));

CREATE TABLE content_media_allocations (
    tenant_id text NOT NULL,
    folder_prefix text NOT NULL,
    object_key text NOT NULL,
    incarnation uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    retired_at timestamp with time zone,
    CONSTRAINT content_media_allocations_pkey PRIMARY KEY (tenant_id, object_key),
    CONSTRAINT content_media_allocations_check CHECK (((tenant_id <> ''::text) AND (folder_prefix <> ''::text) AND (object_key <> ''::text)))
);

CREATE INDEX content_media_allocations_folder ON content_media_allocations USING btree (tenant_id, folder_prefix);

CREATE TABLE content_media_publications (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    upload_name text NOT NULL,
    upload_path text NOT NULL,
    preset text NOT NULL,
    ordinal integer NOT NULL,
    generation uuid,
    renditions jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    content_version_id text,
    CONSTRAINT content_media_publications_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, upload_name, upload_path, preset),
    CONSTRAINT content_media_publications_check CHECK (((tenant_id <> ''::text) AND (content_kind <> ''::text) AND (content_id <> ''::text) AND (upload_path <> ''::text) AND (preset <> ''::text))),
    CONSTRAINT content_media_publications_check1 CHECK (((ordinal >= 0) AND (jsonb_typeof(renditions) = 'array'::text) AND (jsonb_array_length(renditions) > 0))),
    CONSTRAINT content_media_publications_version_check CHECK ((content_version_id <> ''::text))
);

CREATE TABLE content_media_reservations (
    tenant_id text NOT NULL,
    object_key text NOT NULL,
    owner_id text NOT NULL,
    bytes bigint NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT content_media_reservations_pkey PRIMARY KEY (tenant_id, object_key)
);

CREATE INDEX content_media_reservations_owner ON content_media_reservations USING btree (tenant_id, owner_id, expires_at);

CREATE TABLE content_media_releases (
    tenant_id text NOT NULL,
    operation_id text NOT NULL,
    folder_prefix text NOT NULL,
    owner_id text NOT NULL,
    bytes bigint NOT NULL,
    applied boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_media_releases_pkey PRIMARY KEY (tenant_id, operation_id),
    CONSTRAINT content_media_releases_bytes_check CHECK ((bytes >= 0))
);

CREATE UNIQUE INDEX content_media_releases_pending_folder ON content_media_releases USING btree (tenant_id, folder_prefix) WHERE (NOT applied);

CREATE TABLE content_media_usage (
    tenant_id text NOT NULL,
    owner_id text NOT NULL,
    used_bytes bigint DEFAULT 0 NOT NULL,
    CONSTRAINT content_media_usage_pkey PRIMARY KEY (tenant_id, owner_id)
);

CREATE TABLE content_media_upload_rates (
    tenant_id text NOT NULL,
    uploader text NOT NULL,
    hour timestamp with time zone NOT NULL,
    files integer NOT NULL,
    bytes bigint NOT NULL,
    CONSTRAINT content_media_upload_rates_pkey PRIMARY KEY (tenant_id, uploader, hour)
);

CREATE TABLE content_media_upgrades (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    after_id text DEFAULT ''::text NOT NULL,
    visited bigint DEFAULT 0 NOT NULL,
    upgraded bigint DEFAULT 0 NOT NULL,
    finished_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_media_upgrades_pkey PRIMARY KEY (tenant_id, content_kind),
    CONSTRAINT content_media_upgrades_check CHECK (((tenant_id <> ''::text) AND (content_kind <> ''::text) AND (visited >= 0) AND (upgraded >= 0)))
);

CREATE TABLE content_media_upgrade_failures (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    error text NOT NULL,
    failed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_media_upgrade_failures_pkey PRIMARY KEY (tenant_id, content_kind, content_id)
);
