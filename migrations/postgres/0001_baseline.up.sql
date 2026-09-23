-- parent: root
-- Complete ContentKit schema; migratekit selects the host schema.
-- Extensions live in public so multiple application schemas can share them.
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

CREATE FUNCTION contentkit_keyword_normalize(value text) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN TRIM(BOTH FROM regexp_replace(lower(NORMALIZE(regexp_replace(NORMALIZE(COALESCE(value, ''::text), NFKD), '[̀-ͯ]'::text, ''::text, 'g'::text), NFC)), '\s+'::text, ' '::text, 'g'::text));

CREATE FUNCTION contentkit_keyword_terms(title text, aliases text[], keywords text[], legacy text) RETURNS text[]
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN (SELECT COALESCE(array_agg(contentkit_keyword_normalize(input_terms.term) ORDER BY input_terms.ordinal), '{}'::text[]) AS "coalesce" FROM unnest(((ARRAY[COALESCE(NULLIF(contentkit_keyword_terms.title, ''::text), contentkit_keyword_terms.legacy, ''::text)] || contentkit_keyword_terms.aliases) || contentkit_keyword_terms.keywords)) WITH ORDINALITY input_terms(term, ordinal));

CREATE FUNCTION contentkit_keyword_text(title text, aliases text[], keywords text[], legacy text) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN contentkit_keyword_normalize(concat_ws(' '::text, COALESCE(NULLIF(title, ''::text), legacy, ''::text), array_to_string(aliases, ' '::text), array_to_string(keywords, ' '::text)));

CREATE TABLE content_assignments (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    content_version_id text,
    taxonomy_id text NOT NULL,
    relation text NOT NULL,
    source_revision bigint DEFAULT 0 NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_assignments_check CHECK (((content_kind <> ''::text) AND (content_id <> ''::text) AND (relation <> ''::text) AND (content_version_id IS DISTINCT FROM ''::text))),
    CONSTRAINT content_assignments_state_check CHECK ((state = ANY (ARRAY['active'::text, 'proposed'::text])))
);

CREATE TABLE content_edges (
    tenant_id text NOT NULL,
    from_taxonomy_id text NOT NULL,
    relation text NOT NULL,
    to_taxonomy_id text NOT NULL,
    source_revision bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_edges_check CHECK ((from_taxonomy_id <> to_taxonomy_id)),
    CONSTRAINT content_edges_relation_check CHECK ((relation = ANY (ARRAY['alias_of'::text, 'member_of'::text, 'artist_of'::text, 'voice_of'::text, 'parent'::text, 'child'::text, 'synonym'::text])))
);

CREATE TABLE content_node_counts (
    tenant_id text NOT NULL,
    taxonomy_id text NOT NULL,
    content_kind text NOT NULL,
    language text NOT NULL,
    content_count integer NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE content_node_names (
    tenant_id text NOT NULL,
    taxonomy_id text NOT NULL,
    language text NOT NULL,
    kind text NOT NULL,
    name text NOT NULL,
    normalized text GENERATED ALWAYS AS (contentkit_keyword_normalize(name)) STORED NOT NULL,
    source_revision bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_node_names_check CHECK (((language <> ''::text) AND (btrim(name) <> ''::text))),
    CONSTRAINT content_node_names_kind_check CHECK ((kind = ANY (ARRAY['name'::text, 'alias'::text])))
);

CREATE TABLE content_nodes (
    tenant_id text NOT NULL,
    taxonomy_id text NOT NULL,
    kind text NOT NULL,
    slug text NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    source_revision bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_nodes_check CHECK (((tenant_id <> ''::text) AND (taxonomy_id <> ''::text) AND (kind <> ''::text) AND (slug <> ''::text))),
    CONSTRAINT content_nodes_state_check CHECK ((state = ANY (ARRAY['active'::text, 'merged'::text, 'deleted'::text])))
);

CREATE TABLE content_preference_key_archive (
    archived_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id text NOT NULL,
    axis text NOT NULL,
    actor_id text,
    ip text,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    content_version_id text NOT NULL,
    value smallint NOT NULL,
    source_at timestamp with time zone NOT NULL
);

CREATE SEQUENCE content_preference_revision_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

CREATE TABLE content_preference_snapshots (
    tenant_id text NOT NULL,
    actor_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    content_version_id text DEFAULT ''::text NOT NULL,
    axis text NOT NULL,
    value smallint NOT NULL,
    revision bigint NOT NULL,
    occurred_at timestamp with time zone NOT NULL,
    delivered_revision bigint DEFAULT 0 NOT NULL,
    CONSTRAINT content_preference_snapshots_axis_ck CHECK ((axis = ANY (ARRAY['reaction'::text, 'favorite'::text]))),
    CONSTRAINT content_preference_snapshots_revision_ck CHECK (((revision > 0) AND (delivered_revision >= 0) AND (delivered_revision <= revision))),
    CONSTRAINT content_preference_snapshots_value_ck CHECK (((value = ANY (ARRAY['-1'::integer, 0, 1])) AND ((axis <> 'favorite'::text) OR (value = ANY (ARRAY[0, 1])))))
);

CREATE TABLE content_erased_subjects (
    tenant_id text NOT NULL,
    actor_id text NOT NULL,
    erased_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE content_search_backfill (
    content_kind text NOT NULL,
    language text NOT NULL,
    cursor text DEFAULT ''::text NOT NULL,
    state text DEFAULT 'running'::text NOT NULL,
    last_error text,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    tenant_id text NOT NULL
);

CREATE TABLE content_search_dirty (
    content_kind text NOT NULL,
    content_id text NOT NULL,
    language text NOT NULL,
    is_deleted boolean DEFAULT false NOT NULL,
    reason text DEFAULT 'unknown'::text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    revision bigint NOT NULL,
    tenant_id text NOT NULL,
    content_version_id text DEFAULT ''::text NOT NULL
);

CREATE SEQUENCE content_search_dirty_revision_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE content_search_dirty_revision_seq OWNED BY content_search_dirty.revision;

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
    content_version_id text DEFAULT ''::text NOT NULL
);

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
    content_version_id text DEFAULT ''::text NOT NULL,
    moderation text DEFAULT 'approved'::text NOT NULL,
    moderation_reason text,
    moderation_verdict jsonb,
    moderated_by text,
    moderated_at timestamp with time zone,
    moderation_revision bigint DEFAULT 1 NOT NULL,
    published_body text,
    CONSTRAINT content_comments_actor_ck CHECK (((user_id IS NOT NULL) OR (anon_name IS NOT NULL))),
    CONSTRAINT content_comments_moderation_ck CHECK ((moderation = ANY (ARRAY['approved'::text, 'held'::text, 'rejected'::text]))),
    CONSTRAINT content_comments_moderation_revision_check CHECK ((moderation_revision > 0))
);

CREATE TABLE content_interaction_counts (
    content_kind text NOT NULL,
    content_id text NOT NULL,
    likes integer DEFAULT 0 NOT NULL,
    dislikes integer DEFAULT 0 NOT NULL,
    favorites integer DEFAULT 0 NOT NULL,
    comment_count integer DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id text NOT NULL,
    content_version_id text DEFAULT ''::text NOT NULL
);

CREATE TABLE content_favorites (
    user_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id text NOT NULL,
    content_version_id text DEFAULT ''::text NOT NULL
);

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
    CONSTRAINT content_poll_answers_revision_check CHECK ((revision > 0))
);

CREATE TABLE content_poll_options (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    question_id uuid NOT NULL,
    label text NOT NULL,
    image_url text,
    "position" integer DEFAULT 0 NOT NULL,
    vote_count integer DEFAULT 0 NOT NULL
);

CREATE TABLE content_poll_questions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    question text NOT NULL,
    language text DEFAULT ''::text NOT NULL,
    is_active boolean DEFAULT true NOT NULL,
    image_url text,
    live_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    tenant_id text NOT NULL,
    kind text DEFAULT 'multiple_choice'::text NOT NULL,
    closes_at timestamp with time zone,
    CONSTRAINT content_poll_questions_kind_ck CHECK ((kind = ANY (ARRAY['multiple_choice'::text, 'free_text'::text])))
);

CREATE TABLE content_poll_votes (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    question_id uuid NOT NULL,
    option_id uuid NOT NULL,
    user_id text,
    ip text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_poll_votes_actor_ck CHECK (((user_id IS NOT NULL) OR (ip IS NOT NULL)))
);

CREATE TABLE content_posts (
    id text DEFAULT (gen_random_uuid())::text NOT NULL,
    author_id text NOT NULL,
    title text NOT NULL,
    slug text,
    body text NOT NULL,
    excerpt text,
    cover_url text,
    language text DEFAULT ''::text NOT NULL,
    is_draft boolean DEFAULT true NOT NULL,
    live_at timestamp with time zone,
    total_likes integer DEFAULT 0 NOT NULL,
    total_dislikes integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    tenant_id text NOT NULL,
    moderation text DEFAULT 'approved'::text NOT NULL,
    moderation_reason text,
    moderation_verdict jsonb,
    moderated_by text,
    moderated_at timestamp with time zone,
    moderation_revision bigint DEFAULT 1 NOT NULL,
    published_content jsonb,
    CONSTRAINT content_posts_moderation_ck CHECK ((moderation = ANY (ARRAY['approved'::text, 'held'::text, 'rejected'::text]))),
    CONSTRAINT content_posts_moderation_revision_check CHECK ((moderation_revision > 0))
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
    content_version_id text DEFAULT ''::text NOT NULL,
    CONSTRAINT content_reactions_actor_ck CHECK (((user_id IS NOT NULL) OR (ip IS NOT NULL))),
    CONSTRAINT content_reactions_value_ck CHECK ((value = ANY (ARRAY['-1'::integer, 0, 1])))
);

ALTER TABLE ONLY content_search_dirty ALTER COLUMN revision SET DEFAULT nextval('content_search_dirty_revision_seq'::regclass);

ALTER TABLE ONLY content_assignments
    ADD CONSTRAINT content_assignments_tenant_id_content_kind_content_id_conte_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, taxonomy_id, relation);

ALTER TABLE ONLY content_edges
    ADD CONSTRAINT content_edges_pkey PRIMARY KEY (tenant_id, from_taxonomy_id, relation, to_taxonomy_id);

ALTER TABLE ONLY content_node_counts
    ADD CONSTRAINT content_node_counts_pkey PRIMARY KEY (tenant_id, taxonomy_id, content_kind, language);

ALTER TABLE ONLY content_node_names
    ADD CONSTRAINT content_node_names_pkey PRIMARY KEY (tenant_id, taxonomy_id, language, normalized);

ALTER TABLE ONLY content_nodes
    ADD CONSTRAINT content_nodes_pkey PRIMARY KEY (tenant_id, taxonomy_id);

ALTER TABLE ONLY content_preference_snapshots
    ADD CONSTRAINT content_preference_snapshots_pkey PRIMARY KEY (tenant_id, actor_id, content_kind, content_id, content_version_id, axis);

ALTER TABLE ONLY content_erased_subjects
    ADD CONSTRAINT content_erased_subjects_pkey PRIMARY KEY (tenant_id, actor_id);

ALTER TABLE ONLY content_search_backfill
    ADD CONSTRAINT content_search_backfill_pkey PRIMARY KEY (tenant_id, content_kind, language);

ALTER TABLE ONLY content_search_dirty
    ADD CONSTRAINT content_search_dirty_pkey PRIMARY KEY (tenant_id, content_kind, content_id, content_version_id, language);

ALTER TABLE ONLY content_search_documents
    ADD CONSTRAINT content_search_documents_pkey PRIMARY KEY (tenant_id, content_kind, content_id, content_version_id, language);

ALTER TABLE ONLY content_comments
    ADD CONSTRAINT content_comments_pkey PRIMARY KEY (id);

ALTER TABLE ONLY content_interaction_counts
    ADD CONSTRAINT content_interaction_counts_pkey PRIMARY KEY (tenant_id, content_kind, content_id, content_version_id);

ALTER TABLE ONLY content_favorites
    ADD CONSTRAINT content_favorites_pkey PRIMARY KEY (tenant_id, user_id, content_kind, content_id, content_version_id);

ALTER TABLE ONLY content_poll_answers
    ADD CONSTRAINT content_poll_answers_actor_uq UNIQUE (tenant_id, question_id, actor_id);

ALTER TABLE ONLY content_poll_answers
    ADD CONSTRAINT content_poll_answers_pkey PRIMARY KEY (id);

ALTER TABLE ONLY content_poll_options
    ADD CONSTRAINT content_poll_options_pkey PRIMARY KEY (id);

ALTER TABLE ONLY content_poll_questions
    ADD CONSTRAINT content_poll_questions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY content_poll_votes
    ADD CONSTRAINT content_poll_votes_pkey PRIMARY KEY (id);

ALTER TABLE ONLY content_posts
    ADD CONSTRAINT content_posts_pkey PRIMARY KEY (id);

CREATE INDEX content_assignments_node ON content_assignments USING btree (tenant_id, taxonomy_id, content_kind, content_id);

CREATE INDEX content_edges_to ON content_edges USING btree (tenant_id, to_taxonomy_id, relation);

CREATE INDEX content_node_counts_rank ON content_node_counts USING btree (tenant_id, content_kind, language, content_count DESC);

CREATE UNIQUE INDEX content_node_names_canonical ON content_node_names USING btree (tenant_id, taxonomy_id, language) WHERE (kind = 'name'::text);

CREATE INDEX content_node_names_lookup ON content_node_names USING btree (tenant_id, language, normalized);

CREATE INDEX content_nodes_kind ON content_nodes USING btree (tenant_id, kind, state, taxonomy_id);

CREATE UNIQUE INDEX content_nodes_slug ON content_nodes USING btree (tenant_id, kind, slug) WHERE (state = 'active'::text);

CREATE INDEX content_preference_snapshots_pending_idx ON content_preference_snapshots USING btree (tenant_id, actor_id, content_kind, content_id, content_version_id, axis) WHERE (delivered_revision < revision);

CREATE INDEX content_search_backfill_state ON content_search_backfill USING btree (state);

CREATE INDEX content_search_dirty_updated_at ON content_search_dirty USING btree (tenant_id, updated_at);

CREATE INDEX content_search_documents_keyword_exact ON content_search_documents USING gin (contentkit_keyword_terms(title, aliases, keywords, raw_document));

CREATE INDEX content_search_documents_keyword_fuzzy ON content_search_documents USING gist (contentkit_keyword_text(title, aliases, keywords, raw_document) public.gist_trgm_ops (siglen='64'));

CREATE INDEX content_search_documents_keyword_native ON content_search_documents USING pgroonga (contentkit_keyword_terms(title, aliases, keywords, raw_document));

CREATE INDEX content_search_documents_kind_language ON content_search_documents USING btree (tenant_id, content_kind, language);

CREATE INDEX content_comments_author_published_idx ON content_comments USING btree (tenant_id, user_id) INCLUDE (likes, dislikes) WHERE ((deleted_at IS NULL) AND (moderation = 'approved'::text) AND (user_id IS NOT NULL));

CREATE INDEX content_comments_held_idx ON content_comments USING btree (tenant_id, created_at, id) WHERE (moderation = 'held'::text);

CREATE INDEX content_comments_latest_idx ON content_comments USING btree (tenant_id, created_at DESC) WHERE (deleted_at IS NULL);

CREATE INDEX content_comments_reply_idx ON content_comments USING btree (reply_to_id, created_at) WHERE (reply_to_id IS NOT NULL);

CREATE INDEX content_comments_toplevel_idx ON content_comments USING btree (tenant_id, content_kind, content_id, content_version_id, created_at DESC) WHERE (reply_to_id IS NULL);

CREATE INDEX content_interaction_counts_best_idx ON content_interaction_counts USING btree (tenant_id, content_kind, (
CASE
    WHEN ((likes + dislikes) = 0) THEN (0)::double precision
    ELSE ((((((likes)::numeric + 1.9208) / ((likes + dislikes))::numeric))::double precision - (((1.96)::double precision * sqrt(((((likes)::double precision * (dislikes)::double precision) / ((likes + dislikes))::double precision) + (0.9604)::double precision))) / ((likes + dislikes))::double precision)) / (((1)::numeric + (3.8416 / ((likes + dislikes))::numeric)))::double precision)
END) DESC) WHERE ((likes + dislikes) > 0);

CREATE INDEX content_interaction_counts_comments_idx ON content_interaction_counts USING btree (tenant_id, content_kind, comment_count DESC) WHERE (comment_count > 0);

CREATE INDEX content_interaction_counts_favorites_idx ON content_interaction_counts USING btree (tenant_id, content_kind, favorites DESC) WHERE (favorites > 0);

CREATE INDEX content_interaction_counts_likes_idx ON content_interaction_counts USING btree (tenant_id, content_kind, likes DESC) WHERE (likes > 0);

CREATE INDEX content_favorites_content_idx ON content_favorites USING btree (tenant_id, content_kind, content_id, content_version_id);

CREATE INDEX content_favorites_user_created_idx ON content_favorites USING btree (tenant_id, user_id, created_at DESC);

CREATE INDEX content_poll_answers_pending_idx ON content_poll_answers USING btree (tenant_id, id) WHERE (classified_at IS NULL);

CREATE INDEX content_poll_options_question_idx ON content_poll_options USING btree (question_id, "position");

CREATE INDEX content_poll_questions_live_idx ON content_poll_questions USING btree (tenant_id, language, live_at DESC) WHERE ((deleted_at IS NULL) AND is_active);

CREATE UNIQUE INDEX content_poll_votes_ip_uq ON content_poll_votes USING btree (question_id, ip) WHERE ((user_id IS NULL) AND (ip IS NOT NULL));

CREATE UNIQUE INDEX content_poll_votes_user_uq ON content_poll_votes USING btree (question_id, user_id) WHERE (user_id IS NOT NULL);

CREATE INDEX content_posts_held_idx ON content_posts USING btree (tenant_id, created_at, id) WHERE (moderation = 'held'::text);

CREATE INDEX content_posts_published_idx ON content_posts USING btree (tenant_id, language, live_at DESC) WHERE ((deleted_at IS NULL) AND (is_draft = false));

CREATE UNIQUE INDEX content_posts_slug_uq ON content_posts USING btree (tenant_id, slug) WHERE ((slug IS NOT NULL) AND (deleted_at IS NULL));

CREATE INDEX content_reactions_content_idx ON content_reactions USING btree (tenant_id, content_kind, content_id, content_version_id);

CREATE UNIQUE INDEX content_reactions_ip_uq ON content_reactions USING btree (tenant_id, content_kind, content_id, content_version_id, ip) WHERE ((user_id IS NULL) AND (ip IS NOT NULL));

CREATE UNIQUE INDEX content_reactions_user_uq ON content_reactions USING btree (tenant_id, content_kind, content_id, content_version_id, user_id) WHERE (user_id IS NOT NULL);

CREATE TRIGGER content_search_dirty_revision BEFORE INSERT OR UPDATE ON content_search_dirty FOR EACH ROW EXECUTE FUNCTION contentkit_bump_dirty_revision();

ALTER TABLE ONLY content_assignments
    ADD CONSTRAINT content_assignments_tenant_id_taxonomy_id_fkey FOREIGN KEY (tenant_id, taxonomy_id) REFERENCES content_nodes(tenant_id, taxonomy_id) ON DELETE CASCADE;

ALTER TABLE ONLY content_edges
    ADD CONSTRAINT content_edges_tenant_id_from_taxonomy_id_fkey FOREIGN KEY (tenant_id, from_taxonomy_id) REFERENCES content_nodes(tenant_id, taxonomy_id) ON DELETE CASCADE;

ALTER TABLE ONLY content_edges
    ADD CONSTRAINT content_edges_tenant_id_to_taxonomy_id_fkey FOREIGN KEY (tenant_id, to_taxonomy_id) REFERENCES content_nodes(tenant_id, taxonomy_id) ON DELETE CASCADE;

ALTER TABLE ONLY content_node_counts
    ADD CONSTRAINT content_node_counts_tenant_id_taxonomy_id_fkey FOREIGN KEY (tenant_id, taxonomy_id) REFERENCES content_nodes(tenant_id, taxonomy_id) ON DELETE CASCADE;

ALTER TABLE ONLY content_node_names
    ADD CONSTRAINT content_node_names_tenant_id_taxonomy_id_fkey FOREIGN KEY (tenant_id, taxonomy_id) REFERENCES content_nodes(tenant_id, taxonomy_id) ON DELETE CASCADE;

ALTER TABLE ONLY content_comments
    ADD CONSTRAINT content_comments_reply_to_id_fkey FOREIGN KEY (reply_to_id) REFERENCES content_comments(id) ON DELETE CASCADE;

ALTER TABLE ONLY content_poll_answers
    ADD CONSTRAINT content_poll_answers_question_id_fkey FOREIGN KEY (question_id) REFERENCES content_poll_questions(id) ON DELETE CASCADE;

ALTER TABLE ONLY content_poll_options
    ADD CONSTRAINT content_poll_options_question_id_fkey FOREIGN KEY (question_id) REFERENCES content_poll_questions(id) ON DELETE CASCADE;

ALTER TABLE ONLY content_poll_votes
    ADD CONSTRAINT content_poll_votes_option_id_fkey FOREIGN KEY (option_id) REFERENCES content_poll_options(id) ON DELETE CASCADE;

ALTER TABLE ONLY content_poll_votes
    ADD CONSTRAINT content_poll_votes_question_id_fkey FOREIGN KEY (question_id) REFERENCES content_poll_questions(id) ON DELETE CASCADE;
