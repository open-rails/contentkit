-- ContentKit's catalog schema, in the schema migratekit selects. Extensions
-- live in public so several schemas share them. A version id is NULL for the
-- work itself. Functions that name their own schema are created through the
-- schema placeholder; with bodies unchecked, a function may precede the
-- tables it reads. Tables follow in foreign-key order.
SET LOCAL check_function_bodies = false;

CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS pgroonga WITH SCHEMA public;

DO $ckdo$
BEGIN
    EXECUTE replace($ckfn$
CREATE FUNCTION "__contentkit_schema__".content_event_legacy_epoch() RETURNS text
    LANGUAGE sql STABLE
    SET search_path TO 'pg_catalog', 'pg_temp'
    AS $$SELECT epoch::text FROM "__contentkit_schema__".content_event_epoch WHERE singleton$$
$ckfn$, '"__contentkit_schema__"', quote_ident(current_schema()));
END
$ckdo$;

CREATE FUNCTION content_frozen_access(source_rule jsonb, completed_at timestamp with time zone) RETURNS jsonb
    LANGUAGE sql STABLE
    AS $$
    SELECT (source_rule-'releases') || jsonb_build_object('releases',
        (SELECT coalesce(jsonb_agg(release),'[]'::jsonb)
         FROM jsonb_array_elements(CASE WHEN jsonb_typeof(source_rule->'releases')='array'
              THEN source_rule->'releases' ELSE '[]'::jsonb END) release
         WHERE (release->>'at')::timestamptz<=completed_at))
$$;

CREATE FUNCTION content_release_key(release jsonb) RETURNS text
    LANGUAGE sql STABLE
    AS $$
 SELECT jsonb_build_object('at',extract(epoch from (release->>'at')::timestamptz),'level',release->>'level',
 'tiers',coalesce((SELECT jsonb_agg(t ORDER BY t) FROM jsonb_array_elements_text(release->'tiers') t),'[]'::jsonb),
 'group_id',release->>'group_id')::text
$$;

CREATE FUNCTION contentkit_body_excerpt(value text) RETURNS text
    LANGUAGE plpgsql IMMUTABLE STRICT
    AS $$
DECLARE
    document jsonb;
    plain text := value;
    structured boolean := false;
BEGIN
    IF left(ltrim(value, E' \t\n\r'), 1) = '{' THEN
        BEGIN
            document := value::jsonb;
        EXCEPTION WHEN data_exception THEN
            -- Ordinary prose may start with a brace without being JSON.
            document := NULL;
        END;
        IF jsonb_typeof(document->'ops') = 'array' THEN
            SELECT coalesce(string_agg(
                CASE WHEN jsonb_typeof(op->'insert') = 'string'
                    THEN op->>'insert' ELSE ' ' END, '' ORDER BY ordinal), '')
            INTO plain
            FROM jsonb_array_elements(document->'ops') WITH ORDINALITY AS entries(op, ordinal);
            structured := true;
        END IF;
    END IF;
    IF NOT structured THEN
        plain := replace(replace(replace(replace(replace(replace(
            regexp_replace(plain, '<[^>]+>', ' ', 'g'),
            '&nbsp;', ' '), '&lt;', '<'), '&gt;', '>'), '&quot;', '"'), '&#39;', ''''), '&amp;', '&');
    END IF;
    RETURN left(btrim(regexp_replace(plain, '\s+', ' ', 'g')), 400);
END
$$;

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

CREATE FUNCTION contentkit_entity_deleted() RETURNS trigger
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

CREATE FUNCTION contentkit_first_published() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    NEW.first_published_at := coalesce(NEW.first_published_at, NEW.published_at);
    RETURN NEW;
END;
$$;

CREATE FUNCTION contentkit_keyword_normalize(value text) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN TRIM(BOTH FROM regexp_replace(lower(NORMALIZE(regexp_replace(NORMALIZE(COALESCE(value, ''::text), NFKD), '[̀-ͯ]'::text, ''::text, 'g'::text), NFC)), '\s+'::text, ' '::text, 'g'::text));

DO $ckdo$
BEGIN
    EXECUTE replace($ckfn$
CREATE FUNCTION "__contentkit_schema__".contentkit_search_normalize(value text) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN btrim(regexp_replace(regexp_replace("__contentkit_schema__".contentkit_keyword_normalize(value), '\m(the|a|an)\M'::text, ''::text, 'g'::text), '\s+'::text, ' '::text, 'g'::text))
$ckfn$, '"__contentkit_schema__"', quote_ident(current_schema()));
END
$ckdo$;

DO $ckdo$
BEGIN
    EXECUTE replace($ckfn$
CREATE FUNCTION "__contentkit_schema__".contentkit_search_terms(title text, aliases text[], keywords text[], legacy text) RETURNS text[]
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN (SELECT COALESCE(array_agg("__contentkit_schema__".contentkit_search_normalize(input.term) ORDER BY input.ordinal), '{}'::text[]) AS "coalesce" FROM unnest(((ARRAY[COALESCE(NULLIF(contentkit_search_terms.title, ''::text), contentkit_search_terms.legacy, ''::text)] || COALESCE(contentkit_search_terms.aliases, '{}'::text[])) || COALESCE(contentkit_search_terms.keywords, '{}'::text[]))) WITH ORDINALITY input(term, ordinal))
$ckfn$, '"__contentkit_schema__"', quote_ident(current_schema()));
END
$ckdo$;

DO $ckdo$
BEGIN
    EXECUTE replace($ckfn$
CREATE FUNCTION "__contentkit_schema__".contentkit_search_names(title text, aliases text[]) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN array_to_string("__contentkit_schema__".contentkit_search_terms(title, aliases, '{}'::text[], ''::text), '
'::text)
$ckfn$, '"__contentkit_schema__"', quote_ident(current_schema()));
END
$ckdo$;

DO $ckdo$
BEGIN
    EXECUTE replace($ckfn$
CREATE FUNCTION "__contentkit_schema__".contentkit_search_text(title text, aliases text[], keywords text[], legacy text) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN "__contentkit_schema__".contentkit_search_normalize(concat_ws(' '::text, COALESCE(NULLIF(title, ''::text), legacy, ''::text), array_to_string(aliases, ' '::text), array_to_string(keywords, ' '::text)))
$ckfn$, '"__contentkit_schema__"', quote_ident(current_schema()));
END
$ckdo$;

CREATE FUNCTION contentkit_version_code() RETURNS trigger
    LANGUAGE plpgsql
    AS $_$
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
$_$;

DO $ckdo$
BEGIN
    EXECUTE replace($ckfn$
CREATE FUNCTION "__contentkit_schema__".eventlog_flush() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'pg_temp'
    AS $$
DECLARE
 s record; e record; next_position bigint; stamp timestamptz;
BEGIN
 -- Every scope touched by this transaction is known before taking any locks.
 FOR s IN SELECT DISTINCT source,scope FROM "__contentkit_schema__".eventlog_pending
          WHERE transaction_id=pg_current_xact_id() ORDER BY source,scope
 LOOP
  INSERT INTO "__contentkit_schema__".eventlog_streams(source,scope) VALUES(s.source,s.scope) ON CONFLICT DO NOTHING;
  PERFORM 1 FROM "__contentkit_schema__".eventlog_streams WHERE source=s.source AND scope=s.scope FOR UPDATE;
 END LOOP;
 FOR e IN SELECT * FROM "__contentkit_schema__".eventlog_pending WHERE transaction_id=pg_current_xact_id() ORDER BY ordinal
 LOOP
  IF EXISTS(SELECT 1 FROM "__contentkit_schema__".eventlog_events WHERE source=e.source AND scope=e.scope AND id=e.id) THEN
   IF EXISTS(SELECT 1 FROM "__contentkit_schema__".eventlog_events WHERE source=e.source AND scope=e.scope AND id=e.id
     AND event_source=e.event_source AND event_type=e.event_type AND version=e.version
     AND occurred_at=e.occurred_at AND subject_type=e.subject_type AND subject_id=e.subject_id AND data=e.data) THEN
    CONTINUE;
   END IF;
   RAISE EXCEPTION 'event ID already records a different fact' USING ERRCODE='23505';
  END IF;
  UPDATE "__contentkit_schema__".eventlog_streams SET position=position+1,head_id=coalesce(e.boundary,e.id),
   recorded_at=greatest(recorded_at,clock_timestamp())
   WHERE source=e.source AND scope=e.scope RETURNING position,recorded_at INTO next_position,stamp;
  INSERT INTO "__contentkit_schema__".eventlog_events(source,scope,position,id,boundary,event_source,event_type,version,occurred_at,recorded_at,subject_type,subject_id,data)
   VALUES(e.source,e.scope,next_position,e.id,coalesce(e.boundary,e.id),e.event_source,e.event_type,e.version,e.occurred_at,stamp,e.subject_type,e.subject_id,e.data);
 END LOOP;
 DELETE FROM "__contentkit_schema__".eventlog_pending WHERE transaction_id=pg_current_xact_id();
 RETURN NULL;
END $$
$ckfn$, '"__contentkit_schema__"', quote_ident(current_schema()));
END
$ckdo$;

CREATE FUNCTION eventlog_immutable() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'pg_temp'
    AS $$
BEGIN
 RAISE EXCEPTION 'retained event facts are immutable';
END $$;

-- The catalog event log: one ordered stream per source and scope. A transaction
-- queues its events in eventlog_pending; eventlog_flush appends them at commit.
CREATE TABLE content_event_epoch (
    singleton boolean DEFAULT true NOT NULL,
    epoch uuid DEFAULT gen_random_uuid() NOT NULL,
    CONSTRAINT content_event_epoch_singleton_check CHECK (singleton),
    CONSTRAINT content_event_epoch_pkey PRIMARY KEY (singleton)
);
INSERT INTO content_event_epoch (singleton) VALUES (true);

CREATE SEQUENCE content_event_ids;

CREATE TABLE eventlog_streams (
    source text NOT NULL,
    scope text NOT NULL,
    epoch text DEFAULT content_event_legacy_epoch() NOT NULL,
    "position" bigint DEFAULT 0 NOT NULL,
    head_id text DEFAULT ''::text NOT NULL,
    pruned_through bigint DEFAULT 0 NOT NULL,
    pruned_id text DEFAULT ''::text NOT NULL,
    recorded_at timestamp with time zone DEFAULT '-infinity'::timestamp with time zone NOT NULL,
    CONSTRAINT eventlog_streams_pkey PRIMARY KEY (source, scope)
);

CREATE TABLE eventlog_events (
    source text NOT NULL,
    scope text NOT NULL,
    "position" bigint NOT NULL,
    id text NOT NULL,
    boundary text NOT NULL,
    event_source text NOT NULL,
    event_type text NOT NULL,
    version integer NOT NULL,
    occurred_at timestamp with time zone NOT NULL,
    recorded_at timestamp with time zone NOT NULL,
    subject_type text NOT NULL,
    subject_id text NOT NULL,
    data jsonb NOT NULL,
    CONSTRAINT eventlog_events_pkey PRIMARY KEY (source, scope, "position"),
    CONSTRAINT eventlog_events_source_scope_id_key UNIQUE (source, scope, id),
    CONSTRAINT eventlog_events_source_scope_fkey FOREIGN KEY (source, scope) REFERENCES eventlog_streams(source, scope) ON DELETE CASCADE
);
CREATE INDEX eventlog_events_source_scope_event_type_position_idx ON eventlog_events USING btree (source, scope, event_type, "position");
CREATE INDEX eventlog_events_source_scope_recorded_at_position_idx ON eventlog_events USING btree (source, scope, recorded_at, "position");
CREATE TRIGGER eventlog_immutable BEFORE UPDATE ON eventlog_events FOR EACH ROW EXECUTE FUNCTION eventlog_immutable();

CREATE TABLE eventlog_pending (
    ordinal bigint GENERATED ALWAYS AS IDENTITY NOT NULL,
    transaction_id xid8 DEFAULT pg_current_xact_id() NOT NULL,
    source text NOT NULL,
    scope text NOT NULL,
    id text NOT NULL,
    boundary text,
    event_source text NOT NULL,
    event_type text NOT NULL,
    version integer NOT NULL,
    occurred_at timestamp with time zone NOT NULL,
    subject_type text DEFAULT ''::text NOT NULL,
    subject_id text DEFAULT ''::text NOT NULL,
    data jsonb NOT NULL,
    CONSTRAINT eventlog_pending_event_type_check CHECK ((event_type <> ''::text)),
    CONSTRAINT eventlog_pending_id_check CHECK ((id <> ''::text)),
    CONSTRAINT eventlog_pending_scope_check CHECK ((scope <> ''::text)),
    CONSTRAINT eventlog_pending_source_check CHECK ((source <> ''::text)),
    CONSTRAINT eventlog_pending_version_check CHECK ((version > 0)),
    CONSTRAINT eventlog_pending_pkey PRIMARY KEY (ordinal)
);
CREATE INDEX eventlog_pending_transaction_id_idx ON eventlog_pending USING btree (transaction_id);
CREATE CONSTRAINT TRIGGER eventlog_flush_at_commit AFTER INSERT ON eventlog_pending DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION eventlog_flush();

CREATE TABLE eventlog_migrations (
    version integer NOT NULL,
    CONSTRAINT eventlog_migrations_pkey PRIMARY KEY (version)
);
INSERT INTO eventlog_migrations (version) VALUES (1);

-- Apps bound to this catalog, and accounts deleted or being purged.
CREATE TABLE content_app_bindings (
    tenant_id uuid NOT NULL,
    app_id uuid NOT NULL,
    catalog_key text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_app_bindings_app_id_check CHECK ((app_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT content_app_bindings_catalog_key_check CHECK ((catalog_key ~ '^[a-z0-9_-]{1,64}$'::text)),
    CONSTRAINT content_app_bindings_tenant_id_check CHECK ((tenant_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT content_app_bindings_catalog_key_key UNIQUE (catalog_key),
    CONSTRAINT content_app_bindings_pkey PRIMARY KEY (app_id)
);
CREATE INDEX content_app_bindings_tenant_idx ON content_app_bindings USING btree (tenant_id, app_id);

CREATE TABLE content_account_lifecycle (
    tenant_id text NOT NULL,
    actor_id text NOT NULL,
    state text NOT NULL,
    anonymous_key uuid,
    CONSTRAINT content_account_lifecycle_check CHECK (((state = 'purging'::text) = (anonymous_key IS NOT NULL))),
    CONSTRAINT content_account_lifecycle_state_check CHECK ((state = ANY (ARRAY['deleted'::text, 'purging'::text, 'purged'::text]))),
    CONSTRAINT content_account_lifecycle_pkey PRIMARY KEY (tenant_id, actor_id)
);

-- Entities (tags, artists, series, channels, ...) and their localized names and text.
CREATE TABLE content_entities (
    tenant_id text CONSTRAINT content_groups_tenant_id_not_null NOT NULL,
    id text CONSTRAINT content_groups_id_not_null NOT NULL,
    kind text CONSTRAINT content_groups_kind_not_null NOT NULL,
    slug text CONSTRAINT content_groups_slug_not_null NOT NULL,
    scope_id text DEFAULT ''::text CONSTRAINT content_groups_scope_id_not_null NOT NULL,
    parent_id text,
    state text DEFAULT 'active'::text CONSTRAINT content_groups_state_not_null NOT NULL,
    merged_into text,
    attributes jsonb DEFAULT '{}'::jsonb CONSTRAINT content_groups_attributes_not_null NOT NULL,
    cover jsonb,
    owner_user_id text,
    auth_scope text,
    access jsonb,
    created_at timestamp with time zone DEFAULT now() CONSTRAINT content_groups_created_at_not_null NOT NULL,
    updated_at timestamp with time zone DEFAULT now() CONSTRAINT content_groups_updated_at_not_null NOT NULL,
    revision bigint DEFAULT 1 CONSTRAINT content_groups_revision_not_null NOT NULL,
    avatar text,
    deleted_at timestamp with time zone,
    media_purged_at timestamp with time zone,
    internal boolean DEFAULT false CONSTRAINT content_groups_internal_not_null NOT NULL,
    CONSTRAINT content_entities_attributes_check CHECK ((jsonb_typeof(attributes) = 'object'::text)),
    CONSTRAINT content_entities_auth_scope_is_id CHECK (((auth_scope IS NULL) OR (auth_scope = id))),
    CONSTRAINT content_entities_ids_check CHECK (((tenant_id <> ''::text) AND (id <> ''::text) AND (kind <> ''::text) AND (slug <> ''::text))),
    CONSTRAINT content_entities_state_check CHECK ((state = ANY (ARRAY['active'::text, 'merged'::text, 'deleted'::text]))),
    CONSTRAINT content_entities_pkey PRIMARY KEY (tenant_id, id)
);
CREATE INDEX content_entities_kind_idx ON content_entities USING btree (tenant_id, kind, state, id);
CREATE INDEX content_entities_owner_idx ON content_entities USING btree (tenant_id, owner_user_id) WHERE (owner_user_id IS NOT NULL);
CREATE INDEX content_entities_parent_idx ON content_entities USING btree (tenant_id, parent_id) WHERE (parent_id IS NOT NULL);
CREATE INDEX content_entities_purge_idx ON content_entities USING btree (tenant_id, deleted_at) WHERE ((state = 'deleted'::text) AND (media_purged_at IS NULL));
CREATE UNIQUE INDEX content_entities_slug_idx ON content_entities USING btree (tenant_id, kind, scope_id, slug) WHERE (state = 'active'::text);
CREATE TRIGGER content_entities_deleted BEFORE INSERT OR UPDATE OF state ON content_entities FOR EACH ROW EXECUTE FUNCTION contentkit_entity_deleted();

CREATE TABLE content_entity_names (
    tenant_id text CONSTRAINT content_group_names_tenant_id_not_null NOT NULL,
    entity_id text CONSTRAINT content_group_names_group_id_not_null NOT NULL,
    language text CONSTRAINT content_group_names_language_not_null NOT NULL,
    kind text CONSTRAINT content_group_names_kind_not_null NOT NULL,
    name text CONSTRAINT content_group_names_name_not_null NOT NULL,
    normalized text GENERATED ALWAYS AS (contentkit_keyword_normalize(name)) STORED CONSTRAINT content_group_names_normalized_not_null NOT NULL,
    created_at timestamp with time zone DEFAULT now() CONSTRAINT content_group_names_created_at_not_null NOT NULL,
    CONSTRAINT content_entity_names_check CHECK (((language <> ''::text) AND (btrim(name) <> ''::text))),
    CONSTRAINT content_entity_names_kind_check CHECK ((kind = ANY (ARRAY['name'::text, 'alias'::text]))),
    CONSTRAINT content_entity_names_pkey PRIMARY KEY (tenant_id, entity_id, language, normalized),
    CONSTRAINT content_entity_names_tenant_id_entity_id_fkey FOREIGN KEY (tenant_id, entity_id) REFERENCES content_entities(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX content_entity_name_lookup_idx ON content_entity_names USING btree (tenant_id, language, regexp_replace(normalized, '[[:space:]_‐‑‒–—―-]+'::text, ''::text, 'g'::text));
CREATE UNIQUE INDEX content_entity_names_canonical_idx ON content_entity_names USING btree (tenant_id, entity_id, language) WHERE (kind = 'name'::text);
CREATE INDEX content_entity_names_lookup_idx ON content_entity_names USING btree (tenant_id, language, normalized);
CREATE INDEX content_entity_names_management_prefix ON content_entity_names USING pgroonga (contentkit_search_terms(name, '{}'::text[], '{}'::text[], ''::text) public.pgroonga_text_array_term_search_ops_v2);
CREATE INDEX content_entity_names_management_substring ON content_entity_names USING pgroonga (contentkit_search_names(name, '{}'::text[]) public.pgroonga_text_regexp_ops_v2);

CREATE TABLE content_entity_text (
    tenant_id text CONSTRAINT content_group_text_tenant_id_not_null NOT NULL,
    entity_id text CONSTRAINT content_group_text_group_id_not_null NOT NULL,
    language text CONSTRAINT content_group_text_language_not_null NOT NULL,
    description text DEFAULT ''::text CONSTRAINT content_group_text_description_not_null NOT NULL,
    sort_key text DEFAULT ''::text CONSTRAINT content_group_text_sort_key_not_null NOT NULL,
    CONSTRAINT content_entity_text_pkey PRIMARY KEY (tenant_id, entity_id, language),
    CONSTRAINT content_entity_text_tenant_id_entity_id_fkey FOREIGN KEY (tenant_id, entity_id) REFERENCES content_entities(tenant_id, id) ON DELETE CASCADE
);

-- Content items; each has versions (editions), their text and segments, entity
-- memberships, and the facets reads list them by.
CREATE TABLE content_items (
    tenant_id text NOT NULL,
    id text NOT NULL,
    kind text NOT NULL,
    state text DEFAULT 'draft'::text NOT NULL,
    publish_at timestamp with time zone,
    published_at timestamp with time zone,
    deleted_at timestamp with time zone,
    access jsonb DEFAULT '{"level": "public"}'::jsonb NOT NULL,
    restrictions text[] DEFAULT '{}'::text[] NOT NULL,
    attributes jsonb DEFAULT '{}'::jsonb NOT NULL,
    owner_entity_id text,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    revision bigint DEFAULT 1 NOT NULL,
    comments text DEFAULT 'open'::text NOT NULL,
    first_published_at timestamp with time zone,
    deleted_with_entity text,
    deleted_without_versions boolean DEFAULT false NOT NULL,
    CONSTRAINT content_items_access_check CHECK ((jsonb_typeof(access) = 'object'::text)),
    CONSTRAINT content_items_attributes_check CHECK ((jsonb_typeof(attributes) = 'object'::text)),
    CONSTRAINT content_items_comments_check CHECK ((comments = ANY (ARRAY['open'::text, 'closed'::text, 'hidden'::text]))),
    CONSTRAINT content_items_ids_check CHECK (((tenant_id <> ''::text) AND (id <> ''::text) AND (kind <> ''::text))),
    CONSTRAINT content_items_state_check CHECK ((state = ANY (ARRAY['draft'::text, 'published'::text, 'deleted'::text]))),
    CONSTRAINT content_items_pkey PRIMARY KEY (tenant_id, id)
);
CREATE INDEX content_items_deleted_with_entity_idx ON content_items USING btree (tenant_id, deleted_with_entity) WHERE (deleted_with_entity IS NOT NULL);
CREATE INDEX content_items_drafts_idx ON content_items USING btree (tenant_id, kind, created_at) WHERE ((state = 'draft'::text) AND (first_published_at IS NULL));
CREATE INDEX content_items_due_idx ON content_items USING btree (publish_at) WHERE ((state = 'published'::text) AND (published_at IS NULL));
CREATE INDEX content_items_kind_published_idx ON content_items USING btree (tenant_id, kind, published_at DESC, id DESC) WHERE (state = 'published'::text);
CREATE INDEX content_items_management_order_idx ON content_items USING btree (tenant_id, kind, created_at DESC, id DESC);
CREATE INDEX content_items_owner_entity_idx ON content_items USING btree (tenant_id, owner_entity_id) WHERE (owner_entity_id IS NOT NULL);
CREATE TRIGGER content_items_first_published BEFORE INSERT OR UPDATE OF published_at ON content_items FOR EACH ROW EXECUTE FUNCTION contentkit_first_published();

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
    access jsonb NOT NULL,
    attributes jsonb DEFAULT '{}'::jsonb NOT NULL,
    pages integer,
    duration_ms bigint,
    width integer,
    height integer,
    has_audio boolean DEFAULT false NOT NULL,
    ordinal integer,
    cover jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    revision bigint DEFAULT 1 NOT NULL,
    pages_added_at timestamp with time zone,
    released boolean DEFAULT false NOT NULL,
    released_access jsonb,
    original_title text,
    original_language text,
    -- A purge claims the version's erasure here, under its row lock, before any
    -- storage call: a restore refuses a claimed version.
    purge_claimed_at timestamp with time zone,
    CONSTRAINT content_versions_access_check CHECK (((access IS NULL) OR (jsonb_typeof(access) = 'object'::text))),
    CONSTRAINT content_versions_code_check CHECK (((code ~ '^[0-9A-HJKMNP-TV-Z]{4}$'::text) AND (code ~ '[A-Z]'::text))),
    CONSTRAINT content_versions_cover_check CHECK (((cover IS NULL) OR (jsonb_typeof(cover) = 'object'::text))),
    CONSTRAINT content_versions_languages_check CHECK ((cardinality(languages) > 0)),
    CONSTRAINT content_versions_original_language_length CHECK ((char_length(original_language) <= 128)),
    CONSTRAINT content_versions_original_title_length CHECK ((char_length(original_title) <= 512)),
    CONSTRAINT content_versions_released_access_check CHECK (((NOT released) OR (released_access IS NOT NULL))),
    CONSTRAINT content_versions_state_check CHECK ((state = ANY (ARRAY['draft'::text, 'published'::text, 'deleted'::text]))),
    CONSTRAINT content_versions_code_key UNIQUE (tenant_id, content_id, code),
    CONSTRAINT content_versions_pkey PRIMARY KEY (tenant_id, id),
    CONSTRAINT content_versions_tenant_id_content_id_fkey FOREIGN KEY (tenant_id, content_id) REFERENCES content_items(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX content_version_original_title_management_prefix ON content_versions USING pgroonga (contentkit_search_terms(original_title, '{}'::text[], '{}'::text[], ''::text) public.pgroonga_text_array_term_search_ops_v2);
CREATE INDEX content_version_original_title_management_substring ON content_versions USING pgroonga (contentkit_search_names(original_title, '{}'::text[]) public.pgroonga_text_regexp_ops_v2);
CREATE INDEX content_versions_content_idx ON content_versions USING btree (tenant_id, content_id, "position");
CREATE INDEX content_versions_due_idx ON content_versions USING btree (publish_at) WHERE ((state = 'published'::text) AND (published_at IS NULL));
CREATE TRIGGER content_versions_code BEFORE INSERT ON content_versions FOR EACH ROW EXECUTE FUNCTION contentkit_version_code();

CREATE TABLE content_version_text (
    tenant_id text NOT NULL,
    version_id text NOT NULL,
    language text NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    slug text DEFAULT ''::text NOT NULL,
    body text DEFAULT ''::text NOT NULL,
    sort_key text DEFAULT ''::text NOT NULL,
    aliases text[] DEFAULT '{}'::text[] NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    body_excerpt text GENERATED ALWAYS AS (contentkit_body_excerpt(body)) STORED,
    CONSTRAINT content_version_text_pkey PRIMARY KEY (tenant_id, version_id, language),
    CONSTRAINT content_version_text_tenant_id_version_id_fkey FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX content_version_text_management_prefix ON content_version_text USING pgroonga (contentkit_search_terms(title, aliases, '{}'::text[], ''::text) public.pgroonga_text_array_term_search_ops_v2);
CREATE INDEX content_version_text_management_substring ON content_version_text USING pgroonga (contentkit_search_names(title, aliases) public.pgroonga_text_regexp_ops_v2);

CREATE TABLE content_segments (
    tenant_id text NOT NULL,
    version_id text NOT NULL,
    "position" integer NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    start_index integer,
    end_index integer,
    release_at timestamp with time zone,
    start_page text,
    id text DEFAULT (uuidv7())::text NOT NULL,
    released_at timestamp with time zone,
    CONSTRAINT content_segments_range_check CHECK ((((start_page IS NULL) AND (start_index IS NOT NULL) AND (end_index IS NOT NULL) AND (start_index >= 0) AND (end_index > start_index)) OR ((start_page IS NOT NULL) AND ((octet_length(start_page) >= 1) AND (octet_length(start_page) <= 1024)) AND (start_page !~ '(^/|(^|/)\.\.?(/|$)|[[:cntrl:]\\])'::text) AND (start_page !~~ '%//%'::text) AND (start_page !~~ '%/'::text) AND (start_index IS NULL) AND (end_index IS NULL)))),
    CONSTRAINT content_segments_pkey PRIMARY KEY (tenant_id, version_id, "position"),
    CONSTRAINT content_segments_tenant_id_version_id_fkey FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions(tenant_id, id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX content_segments_id_idx ON content_segments USING btree (tenant_id, version_id, id);
CREATE UNIQUE INDEX content_segments_start_page_idx ON content_segments USING btree (tenant_id, version_id, start_page) WHERE (start_page IS NOT NULL);

CREATE TABLE content_memberships (
    tenant_id text NOT NULL,
    entity_id text CONSTRAINT content_memberships_group_id_not_null NOT NULL,
    content_id text NOT NULL,
    version_id text,
    role text DEFAULT ''::text NOT NULL,
    "position" integer,
    state text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_memberships_state_check CHECK ((state = ANY (ARRAY['active'::text, 'proposed'::text]))),
    CONSTRAINT content_memberships_tenant_id_content_id_fkey FOREIGN KEY (tenant_id, content_id) REFERENCES content_items(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT content_memberships_tenant_id_entity_id_fkey FOREIGN KEY (tenant_id, entity_id) REFERENCES content_entities(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT content_memberships_tenant_id_version_id_fkey FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX content_memberships_content_idx ON content_memberships USING btree (tenant_id, content_id, version_id);
CREATE INDEX content_memberships_entity_idx ON content_memberships USING btree (tenant_id, entity_id, "position", content_id);
CREATE UNIQUE INDEX content_memberships_key_idx ON content_memberships USING btree (tenant_id, entity_id, content_id, version_id, role) NULLS NOT DISTINCT;

-- Reads content_memberships and content_entities, so follows them.
DO $ckdo$
BEGIN
    EXECUTE replace($ckfn$
CREATE FUNCTION "__contentkit_schema__".content_keyword_entity_ids(wanted_tenant text, wanted_content text) RETURNS text[]
    LANGUAGE sql STABLE
    BEGIN ATOMIC
 RETURN ( WITH RECURSIVE effective(id) AS (
                  SELECT m.entity_id
                    FROM ("__contentkit_schema__".content_memberships m
                      JOIN "__contentkit_schema__".content_entities g ON (((g.tenant_id = m.tenant_id) AND (g.id = m.entity_id) AND (g.state = 'active'::text))))
                   WHERE ((m.tenant_id = content_keyword_entity_ids.wanted_tenant) AND (m.content_id = content_keyword_entity_ids.wanted_content) AND (m.state = 'active'::text))
                 UNION
                  SELECT p.id
                    FROM ((effective e
                      JOIN "__contentkit_schema__".content_entities c ON (((c.tenant_id = content_keyword_entity_ids.wanted_tenant) AND (c.id = e.id))))
                      JOIN "__contentkit_schema__".content_entities p ON (((p.tenant_id = c.tenant_id) AND (p.id = c.parent_id) AND (p.state = 'active'::text))))
                 )
          SELECT COALESCE(array_agg(effective.id ORDER BY effective.id), '{}'::text[]) AS "coalesce"
            FROM effective);
END
$ckfn$, '"__contentkit_schema__"', quote_ident(current_schema()));
END
$ckdo$;

CREATE TABLE content_facets (
    tenant_id text NOT NULL,
    content_id text NOT NULL,
    version_id text NOT NULL,
    kind text NOT NULL,
    live_from timestamp with time zone,
    deleted boolean DEFAULT false NOT NULL,
    languages text[] NOT NULL,
    access_level text NOT NULL,
    access_tiers text[] DEFAULT '{}'::text[] NOT NULL,
    access_entity_id text,
    restrictions text[] DEFAULT '{}'::text[] NOT NULL,
    entity_ids text[] DEFAULT '{}'::text[] CONSTRAINT content_facets_group_ids_not_null NOT NULL,
    traits text[] DEFAULT '{}'::text[] NOT NULL,
    owner_entity_id text,
    "position" integer DEFAULT 0 NOT NULL,
    published_at timestamp with time zone,
    attributes jsonb DEFAULT '{}'::jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    access_releases jsonb DEFAULT '[]'::jsonb NOT NULL,
    next_release_at timestamp with time zone,
    CONSTRAINT content_facets_access_releases_check CHECK ((jsonb_typeof(access_releases) = 'array'::text)),
    CONSTRAINT content_facets_pkey PRIMARY KEY (tenant_id, version_id),
    CONSTRAINT content_facets_tenant_id_version_id_fkey FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX content_facets_content_idx ON content_facets USING btree (tenant_id, content_id);
CREATE INDEX content_facets_entities_idx ON content_facets USING gin (entity_ids);
CREATE INDEX content_facets_languages_idx ON content_facets USING gin (languages);
CREATE INDEX content_facets_list_idx ON content_facets USING btree (tenant_id, kind, published_at DESC, content_id DESC) WHERE (NOT deleted);
CREATE INDEX content_facets_next_release_idx ON content_facets USING btree (tenant_id, next_release_at) WHERE (next_release_at IS NOT NULL);
CREATE INDEX content_facets_owner_idx ON content_facets USING btree (tenant_id, owner_entity_id, published_at DESC) WHERE (owner_entity_id IS NOT NULL);
CREATE INDEX content_facets_published_order_idx ON content_facets USING btree (tenant_id, kind, COALESCE(published_at, '1970-01-01 00:00:00+00'::timestamp with time zone) DESC, content_id DESC);

-- Channels: the entities a channel owns, reposts and content creators.
CREATE TABLE content_channel_entities (
    tenant_id text NOT NULL,
    channel_id text NOT NULL,
    entity_id text NOT NULL,
    CONSTRAINT content_channel_entities_check CHECK ((channel_id <> entity_id)),
    CONSTRAINT content_channel_entities_pkey PRIMARY KEY (tenant_id, channel_id, entity_id),
    CONSTRAINT content_channel_entities_tenant_id_channel_id_fkey FOREIGN KEY (tenant_id, channel_id) REFERENCES content_entities(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT content_channel_entities_tenant_id_entity_id_fkey FOREIGN KEY (tenant_id, entity_id) REFERENCES content_entities(tenant_id, id) ON DELETE CASCADE
);

CREATE TABLE content_channel_reposts (
    tenant_id text NOT NULL,
    channel_id text NOT NULL,
    content_id text NOT NULL,
    actor_id text,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT content_channel_reposts_pkey PRIMARY KEY (tenant_id, channel_id, content_id),
    CONSTRAINT content_channel_reposts_tenant_id_channel_id_fkey FOREIGN KEY (tenant_id, channel_id) REFERENCES content_entities(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT content_channel_reposts_tenant_id_content_id_fkey FOREIGN KEY (tenant_id, content_id) REFERENCES content_items(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX content_channel_reposts_feed_idx ON content_channel_reposts USING btree (tenant_id, channel_id, created_at DESC, content_id);

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

-- Release and access history.
CREATE TABLE content_release_receipts (
    tenant_id text NOT NULL,
    content_id text NOT NULL,
    version_id text NOT NULL,
    release_key text NOT NULL,
    live_from timestamp with time zone NOT NULL,
    occurred_at timestamp with time zone NOT NULL,
    CONSTRAINT content_release_receipts_pkey PRIMARY KEY (tenant_id, version_id, release_key, live_from),
    CONSTRAINT content_release_receipts_tenant_id_content_id_fkey FOREIGN KEY (tenant_id, content_id) REFERENCES content_items(tenant_id, id) ON DELETE CASCADE
);

CREATE TABLE content_page_notice_dismissals (
    tenant_id text NOT NULL,
    version_id text NOT NULL,
    notice_id text NOT NULL,
    dismissed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_page_notice_dismissals_pkey PRIMARY KEY (tenant_id, version_id, notice_id),
    CONSTRAINT content_page_notice_dismissals_tenant_id_version_id_fkey FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions(tenant_id, id) ON DELETE CASCADE
);

CREATE TABLE content_access_history (
    id bigint GENERATED ALWAYS AS IDENTITY NOT NULL,
    tenant_id text NOT NULL,
    content_id text NOT NULL,
    version_id text NOT NULL,
    occurred_at timestamp with time zone NOT NULL,
    snapshot jsonb NOT NULL,
    CONSTRAINT content_access_history_pkey PRIMARY KEY (tenant_id, id)
);
CREATE INDEX content_access_history_version_time ON content_access_history USING btree (tenant_id, content_id, version_id, occurred_at, id);

CREATE TABLE content_access_history_start (
    tenant_id text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    CONSTRAINT content_access_history_start_pkey PRIMARY KEY (tenant_id)
);

-- Short public codes, legacy aliases and import ids.
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
    CONSTRAINT content_codes_code_ck CHECK (((code ~ '^[0-9A-HJKMNP-TV-Z]{9}$'::text) AND (code ~ '[A-Z]'::text))),
    CONSTRAINT content_codes_merged_ck CHECK ((merged_into IS DISTINCT FROM code)),
    CONSTRAINT content_codes_ref_ck CHECK (((tenant_id <> ''::text) AND (content_kind <> ''::text) AND (content_id <> ''::text) AND (content_id = lower(content_id)))),
    CONSTRAINT content_codes_slug_ck CHECK (((slug ~ '^([a-z0-9]+(-[a-z0-9]+)*(/[a-z0-9]+(-[a-z0-9]+)*)?)?$'::text) AND (length(slug) <= 161))),
    CONSTRAINT content_codes_slugs_ck CHECK ((jsonb_typeof(slugs) = 'object'::text)),
    CONSTRAINT content_codes_pkey PRIMARY KEY (tenant_id, code),
    CONSTRAINT content_codes_ref_key UNIQUE (tenant_id, content_kind, content_id),
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
    CONSTRAINT content_code_aliases_ck CHECK (((source ~ '^[a-z0-9][a-z0-9_.-]{0,63}$'::text) AND (legacy_kind ~ '^[a-z0-9][a-z0-9_.-]{0,63}$'::text) AND (legacy_key <> ''::text) AND (length(legacy_key) <= 512) AND (locator <> ''::text))),
    CONSTRAINT content_code_aliases_pkey PRIMARY KEY (tenant_id, source, legacy_kind, legacy_key),
    CONSTRAINT content_code_aliases_code_fkey FOREIGN KEY (tenant_id, code) REFERENCES content_codes(tenant_id, code)
);

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

CREATE SEQUENCE content_import_recount_marks;
CREATE TABLE content_import_recounts (
    tenant_id text NOT NULL,
    target text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    content_version_id text,
    mark bigint DEFAULT nextval('content_import_recount_marks'::regclass) NOT NULL,
    marked_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_import_recounts_target_check CHECK ((target = ANY (ARRAY['counts'::text, 'replies'::text, 'votes'::text]))),
    CONSTRAINT content_import_recounts_key UNIQUE NULLS NOT DISTINCT (tenant_id, target, content_kind, content_id, content_version_id)
);
ALTER SEQUENCE content_import_recount_marks OWNED BY content_import_recounts.mark;
CREATE INDEX content_import_recounts_marked_idx ON content_import_recounts USING btree (tenant_id, marked_at);

-- Search documents, their rebuild queues and recent searches.
CREATE TABLE content_search_documents (
    content_kind text NOT NULL,
    content_id text NOT NULL,
    language text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    aliases text[] DEFAULT '{}'::text[] NOT NULL,
    keywords text[] DEFAULT '{}'::text[] NOT NULL,
    tenant_id text NOT NULL,
    content_version_id text,
    terms text[] GENERATED ALWAYS AS (contentkit_search_terms(title, aliases, keywords, ''::text)) STORED,
    body text GENERATED ALWAYS AS (contentkit_search_text(title, aliases, keywords, ''::text)) STORED,
    CONSTRAINT content_search_documents_version_check CHECK ((content_version_id <> ''::text)),
    CONSTRAINT content_search_documents_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, language)
);
CREATE INDEX content_search_documents_body_substring ON content_search_documents USING pgroonga (body public.pgroonga_text_regexp_ops_v2);
CREATE INDEX content_search_documents_kind_language ON content_search_documents USING btree (tenant_id, content_kind, language);
CREATE INDEX content_search_documents_names_prefix ON content_search_documents USING pgroonga (contentkit_search_terms(title, aliases, '{}'::text[], ''::text) public.pgroonga_text_array_term_search_ops_v2);
CREATE INDEX content_search_documents_names_substring ON content_search_documents USING pgroonga (contentkit_search_names(title, aliases) public.pgroonga_text_regexp_ops_v2);
CREATE INDEX content_search_documents_terms ON content_search_documents USING gin (terms);
CREATE INDEX content_search_documents_terms_native ON content_search_documents USING pgroonga (terms);

CREATE SEQUENCE content_search_dirty_revision_seq;
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
    CONSTRAINT content_search_dirty_version_check CHECK ((content_version_id <> ''::text)),
    CONSTRAINT content_search_dirty_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, language)
);
ALTER SEQUENCE content_search_dirty_revision_seq OWNED BY content_search_dirty.revision;
CREATE INDEX content_search_dirty_updated_at ON content_search_dirty USING btree (tenant_id, updated_at);
CREATE TRIGGER content_search_dirty_revision BEFORE INSERT OR UPDATE ON content_search_dirty FOR EACH ROW EXECUTE FUNCTION contentkit_bump_dirty_revision();

CREATE SEQUENCE content_search_entity_dirty_revision_seq;
CREATE TABLE content_search_entity_dirty (
    tenant_id text NOT NULL,
    entity_id text NOT NULL,
    revision bigint DEFAULT nextval('content_search_entity_dirty_revision_seq'::regclass) NOT NULL,
    cursor text DEFAULT ''::text NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT content_search_entity_dirty_pkey PRIMARY KEY (tenant_id, entity_id)
);
ALTER SEQUENCE content_search_entity_dirty_revision_seq OWNED BY content_search_entity_dirty.revision;
CREATE INDEX content_search_entity_dirty_updated_at ON content_search_entity_dirty USING btree (tenant_id, updated_at);

CREATE TABLE content_search_invalid (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    content_version_id text,
    language text NOT NULL,
    revision bigint NOT NULL,
    last_error text NOT NULL,
    failed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_search_invalid_version_check CHECK ((content_version_id <> ''::text)),
    CONSTRAINT content_search_invalid_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, language)
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

-- Reactions, favorites and their counts. A signed-out reaction keeps the
-- browser's anonymous id beside its address, so signing in re-keys it.
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
    anonymous_actor_id uuid,
    author_hidden boolean DEFAULT false NOT NULL,
    -- NULL is unknown history; '' marks native writes; nonempty values name import sources.
    import_source text,
    anonymous_subject uuid,
    CONSTRAINT content_reactions_actor_ck CHECK ((((anonymous_actor_id IS NOT NULL) AND (user_id IS NULL) AND (ip IS NULL)) OR ((anonymous_actor_id IS NULL) AND ((user_id IS NOT NULL) OR (ip IS NOT NULL))))),
    CONSTRAINT content_reactions_content_id_ck CHECK ((content_id = lower(content_id))),
    CONSTRAINT content_reactions_value_ck CHECK ((value = ANY (ARRAY['-1'::integer, 0, 1]))),
    CONSTRAINT content_reactions_version_check CHECK ((content_version_id <> ''::text))
);
CREATE INDEX content_reactions_anonymous_idx ON content_reactions USING btree (tenant_id, anonymous_subject) WHERE (anonymous_subject IS NOT NULL);
CREATE INDEX content_reactions_content_idx ON content_reactions USING btree (tenant_id, content_kind, content_id, content_version_id);
CREATE UNIQUE INDEX content_reactions_ip_uq ON content_reactions USING btree (tenant_id, content_kind, content_id, content_version_id, ip) NULLS NOT DISTINCT WHERE ((user_id IS NULL) AND (ip IS NOT NULL));
CREATE INDEX content_reactions_user_idx ON content_reactions USING btree (tenant_id, user_id) WHERE (user_id IS NOT NULL);
CREATE UNIQUE INDEX content_reactions_user_uq ON content_reactions USING btree (tenant_id, content_kind, content_id, content_version_id, user_id) NULLS NOT DISTINCT WHERE (user_id IS NOT NULL);

CREATE TABLE content_favorites (
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    app_id uuid NOT NULL,
    user_id uuid,
    anonymous_actor_id uuid,
    content_id uuid NOT NULL,
    content_version_id uuid,
    value smallint NOT NULL,
    content_kind text NOT NULL,
    -- NULL is unknown history; '' marks native writes; nonempty values name import sources.
    import_source text,
    CONSTRAINT content_favorites_actor_ck CHECK (((user_id IS NULL) <> (anonymous_actor_id IS NULL))),
    CONSTRAINT content_favorites_value_ck CHECK ((value = ANY (ARRAY[0, 1])))
);
CREATE INDEX content_favorites_content_idx ON content_favorites USING btree (app_id, content_kind, content_id, content_version_id);
CREATE INDEX content_favorites_user_created_idx ON content_favorites USING btree (app_id, user_id, created_at DESC) WHERE (value = 1);
CREATE UNIQUE INDEX content_favorites_user_key ON content_favorites USING btree (app_id, user_id, content_kind, content_id, content_version_id) NULLS NOT DISTINCT WHERE (user_id IS NOT NULL);

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
    CONSTRAINT content_interaction_counts_content_id_ck CHECK ((content_id = lower(content_id))),
    CONSTRAINT content_interaction_counts_version_check CHECK ((content_version_id <> ''::text)),
    CONSTRAINT content_interaction_counts_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id)
);
CREATE INDEX content_interaction_counts_best_idx ON content_interaction_counts USING btree (tenant_id, content_kind, (
CASE
    WHEN ((likes + dislikes) = 0) THEN (0)::double precision
    ELSE ((((((likes)::numeric + 1.9208) / ((likes + dislikes))::numeric))::double precision - (((1.96)::double precision * sqrt(((((likes)::double precision * (dislikes)::double precision) / ((likes + dislikes))::double precision) + (0.9604)::double precision))) / ((likes + dislikes))::double precision)) / (((1)::numeric + (3.8416 / ((likes + dislikes))::numeric)))::double precision)
END) DESC) WHERE ((likes + dislikes) > 0);
CREATE INDEX content_interaction_counts_comments_idx ON content_interaction_counts USING btree (tenant_id, content_kind, comment_count DESC) WHERE (comment_count > 0);
CREATE INDEX content_interaction_counts_favorites_idx ON content_interaction_counts USING btree (tenant_id, content_kind, favorites DESC) WHERE (favorites > 0);
CREATE INDEX content_interaction_counts_likes_idx ON content_interaction_counts USING btree (tenant_id, content_kind, likes DESC) WHERE (likes > 0);

-- A feedback prompt offered after a like, favorite or dislike: one row per
-- person and content item, completed when answered. The person is the account
-- (user_id) or a signed-out browser (anonymous_actor_id, its anonymous
-- subject). Account erasure and anonymous expiry delete the rows; text never
-- leaves this table.
CREATE TABLE content_reaction_feedback (
    app_id uuid NOT NULL,
    user_id uuid,
    anonymous_actor_id uuid,
    content_id uuid NOT NULL,
    reaction text NOT NULL,
    offered_at timestamp with time zone NOT NULL,
    answered_at timestamp with time zone,
    reason text,
    -- The tag, artist, series or character the answer names.
    entity_id text,
    -- The tag a not_into answer suggested hiding, and whether it was hidden
    -- (NULL until decided).
    hide_entity_id text,
    hide_taken boolean,
    text text,
    CONSTRAINT content_reaction_feedback_answered_ck CHECK (((answered_at IS NULL) = (reason IS NULL))),
    CONSTRAINT content_reaction_feedback_entity_ck CHECK (
CASE
    WHEN (reason = ANY (ARRAY['tag'::text, 'not_into'::text])) THEN (entity_id IS NOT NULL)
    WHEN (reason = ANY (ARRAY['artist'::text, 'series'::text])) THEN true
    ELSE (entity_id IS NULL)
END),
    CONSTRAINT content_reaction_feedback_hide_ck CHECK (((hide_taken IS NULL) OR (hide_entity_id IS NOT NULL))),
    CONSTRAINT content_reaction_feedback_reaction_ck CHECK ((reaction = ANY (ARRAY['like'::text, 'favorite'::text, 'dislike'::text]))),
    CONSTRAINT content_reaction_feedback_reason_ck CHECK (((reason IS NULL) OR ((reaction = 'dislike'::text) AND (reason = ANY (ARRAY['bad_art'::text, 'bad_story'::text, 'not_into'::text, 'misleading'::text, 'other'::text]))) OR ((reaction <> 'dislike'::text) AND (reason = ANY (ARRAY['artwork'::text, 'artist'::text, 'story'::text, 'series'::text, 'tag'::text, 'other'::text]))))),
    CONSTRAINT content_reaction_feedback_subject_ck CHECK (((user_id IS NULL) <> (anonymous_actor_id IS NULL))),
    CONSTRAINT content_reaction_feedback_text_ck CHECK (((text IS NULL) OR ((reason = 'other'::text) AND ((char_length(text) >= 1) AND (char_length(text) <= 280)))))
);
CREATE UNIQUE INDEX content_reaction_feedback_anonymous_key ON content_reaction_feedback USING btree (app_id, anonymous_actor_id, content_id) WHERE (anonymous_actor_id IS NOT NULL);
CREATE INDEX content_reaction_feedback_content_idx ON content_reaction_feedback USING btree (app_id, content_id);
CREATE UNIQUE INDEX content_reaction_feedback_user_key ON content_reaction_feedback USING btree (app_id, user_id, content_id) WHERE (user_id IS NOT NULL);

-- Comments and their moderation.
CREATE TABLE content_comments (
    id uuid DEFAULT uuidv7() NOT NULL,
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
    language text DEFAULT ''::text NOT NULL,
    author_hidden boolean DEFAULT false NOT NULL,
    CONSTRAINT content_comments_actor_ck CHECK (((user_id IS NOT NULL) OR (anon_name IS NOT NULL))),
    CONSTRAINT content_comments_content_id_ck CHECK ((content_id = lower(content_id))),
    CONSTRAINT content_comments_moderation_ck CHECK ((moderation = ANY (ARRAY['approved'::text, 'held'::text, 'rejected'::text]))),
    CONSTRAINT content_comments_moderation_revision_check CHECK ((moderation_revision > 0)),
    CONSTRAINT content_comments_version_check CHECK ((content_version_id <> ''::text)),
    CONSTRAINT content_comments_pkey PRIMARY KEY (tenant_id, id),
    CONSTRAINT content_comments_reply_to_id_fkey FOREIGN KEY (tenant_id, reply_to_id) REFERENCES content_comments(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX content_comments_admin_idx ON content_comments USING btree (tenant_id, created_at DESC, id DESC);
CREATE INDEX content_comments_author_idx ON content_comments USING btree (tenant_id, user_id, created_at DESC, id DESC) WHERE (user_id IS NOT NULL);
CREATE INDEX content_comments_author_published_idx ON content_comments USING btree (tenant_id, user_id) INCLUDE (likes, dislikes) WHERE ((deleted_at IS NULL) AND (moderation = 'approved'::text) AND (user_id IS NOT NULL));
CREATE INDEX content_comments_content_idx ON content_comments USING btree (tenant_id, content_kind, content_id, content_version_id);
CREATE INDEX content_comments_held_idx ON content_comments USING btree (tenant_id, created_at, id) WHERE (moderation = 'held'::text);
CREATE INDEX content_comments_latest_idx ON content_comments USING btree (tenant_id, created_at DESC) WHERE (deleted_at IS NULL);
CREATE INDEX content_comments_reply_idx ON content_comments USING btree (tenant_id, reply_to_id, created_at) WHERE (reply_to_id IS NOT NULL);
CREATE INDEX content_comments_toplevel_idx ON content_comments USING btree (tenant_id, content_kind, content_id, content_version_id, language, created_at DESC) WHERE (reply_to_id IS NULL);

CREATE TABLE content_comment_notifications (
    tenant_id text NOT NULL,
    comment_id uuid NOT NULL,
    recipient text NOT NULL,
    source_id text NOT NULL,
    kind text NOT NULL,
    revision bigint DEFAULT 1 NOT NULL,
    withdrawn boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT content_comment_notifications_revision_check CHECK ((revision > 0)),
    CONSTRAINT content_comment_notifications_pkey PRIMARY KEY (tenant_id, recipient, source_id),
    CONSTRAINT content_comment_notifications_tenant_id_comment_id_fkey FOREIGN KEY (tenant_id, comment_id) REFERENCES content_comments(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX content_comment_notifications_comment_idx ON content_comment_notifications USING btree (tenant_id, comment_id) WHERE (NOT withdrawn);

CREATE TABLE content_comment_bans (
    tenant_id text NOT NULL,
    user_id text NOT NULL,
    scope text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    until timestamp with time zone,
    banned_by text NOT NULL,
    banned_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_comment_bans_scope_ck CHECK (((scope = 'global'::text) OR ((scope ~~ 'owner:%'::text) AND (length(scope) > 6)) OR ((scope ~~ 'group:%'::text) AND (length(scope) > 6)))),
    CONSTRAINT content_comment_bans_pkey PRIMARY KEY (tenant_id, user_id, scope)
);
CREATE INDEX content_comment_bans_scope_idx ON content_comment_bans USING btree (tenant_id, scope, banned_at DESC, user_id);

CREATE TABLE content_banned_words (
    tenant_id text NOT NULL,
    word text NOT NULL,
    action text DEFAULT 'mask'::text NOT NULL,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_banned_words_action_check CHECK ((action = ANY (ARRAY['mask'::text, 'reject'::text]))),
    CONSTRAINT content_banned_words_word_check CHECK (((btrim(word) <> ''::text) AND (word = lower(word)))),
    CONSTRAINT content_banned_words_pkey PRIMARY KEY (tenant_id, word)
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

CREATE TABLE content_hidden_authors (
    tenant_id text NOT NULL,
    actor_id text NOT NULL,
    CONSTRAINT content_hidden_authors_pkey PRIMARY KEY (tenant_id, actor_id)
);

-- Polls with predefined choices.
CREATE TABLE content_poll_questions (
    id uuid DEFAULT uuidv7() NOT NULL,
    question text NOT NULL,
    language text DEFAULT ''::text NOT NULL,
    is_active boolean DEFAULT true NOT NULL,
    image_name text,
    live_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    tenant_id text NOT NULL,
    closes_at timestamp with time zone,
    CONSTRAINT content_poll_questions_pkey PRIMARY KEY (tenant_id, id)
);
CREATE INDEX content_poll_questions_live_idx ON content_poll_questions USING btree (tenant_id, language, live_at DESC) WHERE ((deleted_at IS NULL) AND is_active);

CREATE TABLE content_poll_options (
    id uuid DEFAULT uuidv7() NOT NULL,
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
    id uuid DEFAULT uuidv7() NOT NULL,
    question_id uuid NOT NULL,
    option_id uuid NOT NULL,
    user_id text,
    ip text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id text NOT NULL,
    anonymous_actor_id uuid,
    CONSTRAINT content_poll_votes_actor_ck CHECK ((((anonymous_actor_id IS NOT NULL) AND (user_id IS NULL) AND (ip IS NULL)) OR ((anonymous_actor_id IS NULL) AND ((user_id IS NOT NULL) OR (ip IS NOT NULL))))),
    CONSTRAINT content_poll_votes_pkey PRIMARY KEY (tenant_id, id),
    CONSTRAINT content_poll_votes_option_id_fkey FOREIGN KEY (tenant_id, option_id) REFERENCES content_poll_options(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT content_poll_votes_question_id_fkey FOREIGN KEY (tenant_id, question_id) REFERENCES content_poll_questions(tenant_id, id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX content_poll_votes_ip_uq ON content_poll_votes USING btree (tenant_id, question_id, ip) WHERE ((user_id IS NULL) AND (ip IS NOT NULL));
CREATE INDEX content_poll_votes_option_idx ON content_poll_votes USING btree (tenant_id, option_id);
CREATE UNIQUE INDEX content_poll_votes_user_uq ON content_poll_votes USING btree (tenant_id, question_id, user_id) WHERE (user_id IS NOT NULL);

-- Location rules: what a rule hides, where, and from which countries,
-- subdivisions or networks.
CREATE TABLE content_location_rules (
    tenant_id text NOT NULL,
    id text NOT NULL,
    target text NOT NULL,
    enabled boolean NOT NULL,
    note text DEFAULT ''::text NOT NULL,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_by text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_location_rules_target_check CHECK ((target = ANY (ARRAY['site'::text, 'content'::text, 'entities'::text]))),
    CONSTRAINT content_location_rules_pkey PRIMARY KEY (tenant_id, id)
);
CREATE INDEX content_location_rules_enabled_tenant_idx ON content_location_rules USING btree (tenant_id, id) WHERE enabled;

CREATE TABLE content_location_rule_domains (
    tenant_id text NOT NULL,
    rule_id text NOT NULL,
    domain text NOT NULL,
    CONSTRAINT content_location_rule_domains_pkey PRIMARY KEY (tenant_id, rule_id, domain),
    CONSTRAINT content_location_rule_domains_tenant_id_rule_id_fkey FOREIGN KEY (tenant_id, rule_id) REFERENCES content_location_rules(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX content_location_rule_domains_domain_idx ON content_location_rule_domains USING btree (tenant_id, domain, rule_id);

CREATE TABLE content_location_rule_contents (
    tenant_id text NOT NULL,
    rule_id text NOT NULL,
    content_id text NOT NULL,
    CONSTRAINT content_location_rule_contents_pkey PRIMARY KEY (tenant_id, rule_id, content_id),
    CONSTRAINT content_location_rule_contents_tenant_id_content_id_fkey FOREIGN KEY (tenant_id, content_id) REFERENCES content_items(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT content_location_rule_contents_tenant_id_rule_id_fkey FOREIGN KEY (tenant_id, rule_id) REFERENCES content_location_rules(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX content_location_rule_contents_content_idx ON content_location_rule_contents USING btree (tenant_id, content_id);

CREATE TABLE content_location_rule_entities (
    tenant_id text NOT NULL,
    rule_id text NOT NULL,
    entity_id text NOT NULL,
    CONSTRAINT content_location_rule_entities_pkey PRIMARY KEY (tenant_id, rule_id, entity_id),
    CONSTRAINT content_location_rule_entities_tenant_id_entity_id_fkey FOREIGN KEY (tenant_id, entity_id) REFERENCES content_entities(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT content_location_rule_entities_tenant_id_rule_id_fkey FOREIGN KEY (tenant_id, rule_id) REFERENCES content_location_rules(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX content_location_rule_entities_entity_idx ON content_location_rule_entities USING btree (tenant_id, entity_id);

CREATE TABLE content_location_rule_entries (
    tenant_id text NOT NULL,
    rule_id text NOT NULL,
    "position" integer NOT NULL,
    country text,
    subdivision text,
    network cidr,
    CONSTRAINT content_location_rule_entries_check CHECK ((num_nonnulls(country, subdivision, network) = 1)),
    CONSTRAINT content_location_rule_entries_pkey PRIMARY KEY (tenant_id, rule_id, "position"),
    CONSTRAINT content_location_rule_entries_tenant_id_rule_id_fkey FOREIGN KEY (tenant_id, rule_id) REFERENCES content_location_rules(tenant_id, id) ON DELETE CASCADE
);

-- Deliberately no rule foreign key: deleting a rule retains its audit trail.
CREATE TABLE content_location_rule_history (
    tenant_id text NOT NULL,
    id bigint GENERATED ALWAYS AS IDENTITY NOT NULL,
    rule_id text NOT NULL,
    action text NOT NULL,
    actor text NOT NULL,
    at timestamp with time zone DEFAULT now() NOT NULL,
    snapshot jsonb NOT NULL,
    CONSTRAINT content_location_rule_history_action_check CHECK ((action = ANY (ARRAY['create'::text, 'update'::text, 'enable'::text, 'disable'::text, 'delete'::text]))),
    CONSTRAINT content_location_rule_history_pkey PRIMARY KEY (tenant_id, id)
);
CREATE INDEX content_location_rule_history_rule_idx ON content_location_rule_history USING btree (tenant_id, rule_id, id DESC);

-- An account's content-filter overrides.
CREATE TABLE content_filter_prefs (
    tenant_id text NOT NULL,
    user_id text NOT NULL,
    overrides jsonb DEFAULT '{}'::jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_filter_prefs_pkey PRIMARY KEY (tenant_id, user_id)
);

-- Per-person reading state, history and identity links, written by the
-- analytics job from ClickHouse's activity log. A subject is the account id
-- or the anonymous id the engine derives from the browser's.
--
-- Each gallery a person opened or hid. The job merges by maximum and latest,
-- so repeating it is harmless; hide and unhide write hidden at once.
-- best_outcome: 0 none, 1 bounce, 2 neutral, 3 good, 4 complete; position is
-- the last reported point, NULL until one is.
CREATE TABLE reading_state (
    app_id uuid NOT NULL,
    subject uuid NOT NULL,
    content_id uuid NOT NULL,
    first_day date,
    last_seen_at timestamp with time zone,
    best_outcome smallint DEFAULT 0 NOT NULL,
    furthest integer DEFAULT 0 NOT NULL,
    "position" integer,
    hidden boolean DEFAULT false NOT NULL,
    CONSTRAINT reading_state_kept_check CHECK ((hidden OR (last_seen_at IS NOT NULL))),
    CONSTRAINT reading_state_opened_check CHECK (((first_day IS NULL) = (last_seen_at IS NULL))),
    CONSTRAINT reading_state_outcome_check CHECK (((best_outcome >= 0) AND (best_outcome <= 4))),
    CONSTRAINT reading_state_points_check CHECK (((furthest >= 0) AND ("position" >= 0))),
    CONSTRAINT reading_state_pkey PRIMARY KEY (app_id, subject, content_id)
);
CREATE INDEX reading_state_content_idx ON reading_state USING btree (app_id, content_id);
CREATE INDEX reading_state_recent_idx ON reading_state USING btree (app_id, subject, last_seen_at DESC) WHERE (last_seen_at IS NOT NULL);

-- One row per person and UTC day: the day's actions in time order, packed.
-- A version byte (2), then per action: a header byte (kind in bits 0-1:
-- 0 gallery, 1 search, 2 follow; bit 2 marks a session start), the UTC offset
-- in 15-minute steps (int8), the uvarint minute of the UTC day, then for a
-- gallery its 16-byte id, a byte (fraction read in tenths, bits 0-3; the
-- reaction, bits 4-6: 0 none, 1 like, 2 dislike, 3 favorite, 4 hidden), an
-- active-time byte (0 none, n for [2^(n-1), 2^n) seconds) and the surface of
-- the day's first open (readingstore.Surface: 0 outside, 1 home, 2 list,
-- 3 popular, 4 similar, 5 search, 6 entity, 7 recommended, 8 continue or
-- history, 9 favorites, 15 other); for a search a uvarint length and the
-- UTF-8 query; for a follow the 16-byte entity id.
CREATE TABLE reading_history (
    app_id uuid NOT NULL,
    subject uuid NOT NULL,
    day date NOT NULL,
    actions bytea NOT NULL,
    CONSTRAINT reading_history_actions_check CHECK ((octet_length(actions) > 1)),
    CONSTRAINT reading_history_pkey PRIMARY KEY (app_id, subject, day)
);

-- An anonymous id signed in as this account, sent once by the browser at
-- sign-in. The job folds the anonymous history into the account; later rows
-- from the anonymous id count as the account's.
CREATE TABLE anonymous_links (
    app_id uuid NOT NULL,
    anonymous_id uuid NOT NULL,
    account_id uuid NOT NULL,
    linked_at timestamp with time zone DEFAULT now() NOT NULL,
    folded_at timestamp with time zone,
    CONSTRAINT anonymous_links_distinct_check CHECK ((anonymous_id <> account_id)),
    CONSTRAINT anonymous_links_pkey PRIMARY KEY (app_id, anonymous_id)
);
CREATE INDEX anonymous_links_account_idx ON anonymous_links USING btree (app_id, account_id);
CREATE INDEX anonymous_links_unfolded_idx ON anonymous_links USING btree (app_id, linked_at) WHERE (folded_at IS NULL);

-- A signed-out subject's latest activity: the analytics job records what it
-- folds from the log, and signed-out searches, reactions and hides record
-- themselves, so it holds without ClickHouse too. After
-- Config.AnonymousRetention without activity the subject's reading state,
-- history and recent searches are deleted and its reactions unlinked.
CREATE TABLE anonymous_activity (
    app_id uuid NOT NULL,
    subject uuid NOT NULL,
    last_active_at timestamp with time zone NOT NULL,
    CONSTRAINT anonymous_activity_pkey PRIMARY KEY (app_id, subject)
);
CREATE INDEX anonymous_activity_idle_idx ON anonymous_activity USING btree (app_id, last_active_at);

-- The value of an outcome: P(sequel) per segment and outcome, and q, its
-- position from -1 to +1 around the segment's base rate. Starting guesses
-- (fitted_at NULL) until the monthly refit writes measured values.
CREATE TABLE outcome_values (
    app_id uuid NOT NULL,
    segment text NOT NULL,
    outcome text NOT NULL,
    p_sequel double precision NOT NULL,
    q double precision NOT NULL,
    fitted_at timestamp with time zone,
    CONSTRAINT outcome_values_p_check CHECK (((p_sequel >= (0)::double precision) AND (p_sequel <= (1)::double precision))),
    CONSTRAINT outcome_values_q_check CHECK (((q >= ('-1'::integer)::double precision) AND (q <= (1)::double precision))),
    CONSTRAINT outcome_values_pkey PRIMARY KEY (app_id, segment, outcome)
);

-- The analytics job's progress: the activity ingested so far, when today's
-- rollups and the last closed day's were rebuilt, and when the ClickHouse
-- erasure last ran.
CREATE TABLE analytics_job (
    app_id uuid NOT NULL,
    ingested_through timestamp with time zone,
    rolled_up_at timestamp with time zone,
    closed_at timestamp with time zone,
    erased_at timestamp with time zone,
    erasures_through timestamp with time zone,
    CONSTRAINT analytics_job_pkey PRIMARY KEY (app_id)
);

-- Erasure fences.
CREATE TABLE content_erased_subjects (
    tenant_id text NOT NULL,
    actor_id text NOT NULL,
    erased_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    engine_cleanup_complete boolean DEFAULT false NOT NULL,
    CONSTRAINT content_erased_subjects_pkey PRIMARY KEY (tenant_id, actor_id)
);

CREATE TABLE content_signal_purges (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    content_version_id text DEFAULT ''::text NOT NULL,
    CONSTRAINT content_signal_purges_content_id_check CHECK ((content_id <> ''::text)),
    CONSTRAINT content_signal_purges_content_kind_check CHECK ((content_kind <> ''::text)),
    CONSTRAINT content_signal_purges_pkey PRIMARY KEY (tenant_id, content_kind, content_id, content_version_id)
);

-- Each item's manifest.json in the bucket is the only record of its files;
-- this is a rebuildable cache of it, one row per item (media-worker rebuild).
CREATE TABLE content_media_items (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    content_version_id text DEFAULT ''::text NOT NULL,
    rev bigint NOT NULL,
    etag text NOT NULL,
    state text NOT NULL,
    status jsonb NOT NULL,
    images jsonb NOT NULL,
    deleted boolean DEFAULT false NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_media_items_check CHECK (((tenant_id <> ''::text) AND (content_kind <> ''::text) AND (content_id <> ''::text) AND (rev > 0) AND (etag <> ''::text))),
    CONSTRAINT content_media_items_images_check CHECK ((jsonb_typeof(images) = 'array'::text)),
    CONSTRAINT content_media_items_status_check CHECK ((jsonb_typeof(status) = 'object'::text)),
    CONSTRAINT content_media_items_pkey PRIMARY KEY (tenant_id, content_kind, content_id, content_version_id)
);

-- Empty: the chains before this baseline left it in a catalog-only schema.
CREATE TABLE content_media_commits (
    tenant_id text,
    actor_id text,
    effects jsonb
);

-- Management search reads names through these views.
CREATE VIEW content_management_content_names AS
 SELECT c.tenant_id,
    c.kind AS content_kind,
    c.id AS content_id,
    v.id AS content_version_id,
    t.language,
    t.title,
    t.aliases
   FROM ((content_items c
     JOIN content_versions v ON (((v.tenant_id = c.tenant_id) AND (v.content_id = c.id))))
     JOIN content_version_text t ON (((t.tenant_id = v.tenant_id) AND (t.version_id = v.id))))
  WHERE ((c.state <> 'deleted'::text) AND (v.state <> 'deleted'::text))
UNION ALL
 SELECT c.tenant_id,
    c.kind AS content_kind,
    c.id AS content_id,
    v.id AS content_version_id,
    l.language,
    v.original_title AS title,
    '{}'::text[] AS aliases
   FROM ((content_items c
     JOIN content_versions v ON (((v.tenant_id = c.tenant_id) AND (v.content_id = c.id))))
     CROSS JOIN LATERAL unnest(v.languages) l(language))
  WHERE ((c.state <> 'deleted'::text) AND (v.state <> 'deleted'::text) AND (COALESCE(v.original_title, ''::text) <> ''::text));

CREATE VIEW content_management_entity_names AS
 SELECT g.tenant_id,
    g.kind AS content_kind,
    g.id AS content_id,
    NULL::text AS content_version_id,
    n.language,
    n.name AS title,
    '{}'::text[] AS aliases
   FROM (content_entities g
     JOIN content_entity_names n ON (((n.tenant_id = g.tenant_id) AND (n.entity_id = g.id))))
  WHERE (g.state = 'active'::text);
