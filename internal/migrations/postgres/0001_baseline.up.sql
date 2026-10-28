-- ContentKit's catalog schema. migratekit selects the host schema; extensions
-- live in public so several schemas share them. A version id is NULL for the
-- work itself. Functions that name their own schema are created through
-- the schema placeholder.
-- Functions may precede the tables they read, as in a pg_dump.
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


SET default_tablespace = '';

SET default_table_access_method = heap;

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
    CONSTRAINT content_entities_ids_check CHECK (((tenant_id <> ''::text) AND (id <> ''::text) AND (kind <> ''::text) AND (slug <> ''::text))),
    CONSTRAINT content_entities_state_check CHECK ((state = ANY (ARRAY['active'::text, 'merged'::text, 'deleted'::text])))
);

CREATE TABLE content_memberships (
    tenant_id text NOT NULL,
    entity_id text CONSTRAINT content_memberships_group_id_not_null NOT NULL,
    content_id text NOT NULL,
    version_id text,
    role text DEFAULT ''::text NOT NULL,
    "position" integer,
    state text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_memberships_state_check CHECK ((state = ANY (ARRAY['active'::text, 'proposed'::text])))
);

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

CREATE TABLE content_access_history (
    id bigint NOT NULL,
    tenant_id text NOT NULL,
    content_id text NOT NULL,
    version_id text NOT NULL,
    occurred_at timestamp with time zone NOT NULL,
    snapshot jsonb NOT NULL
);

ALTER TABLE content_access_history ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME content_access_history_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE content_access_history_start (
    tenant_id text NOT NULL,
    started_at timestamp with time zone NOT NULL
);

CREATE TABLE content_account_lifecycle (
    tenant_id text NOT NULL,
    actor_id text NOT NULL,
    state text NOT NULL,
    anonymous_key uuid,
    CONSTRAINT content_account_lifecycle_check CHECK (((state = 'purging'::text) = (anonymous_key IS NOT NULL))),
    CONSTRAINT content_account_lifecycle_state_check CHECK ((state = ANY (ARRAY['deleted'::text, 'purging'::text, 'purged'::text])))
);

CREATE TABLE content_app_bindings (
    tenant_id uuid NOT NULL,
    app_id uuid NOT NULL,
    catalog_key text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_app_bindings_app_id_check CHECK ((app_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT content_app_bindings_catalog_key_check CHECK ((catalog_key ~ '^[a-z0-9_-]{1,64}$'::text)),
    CONSTRAINT content_app_bindings_tenant_id_check CHECK ((tenant_id <> '00000000-0000-0000-0000-000000000000'::uuid))
);

CREATE TABLE content_banned_words (
    tenant_id text NOT NULL,
    word text NOT NULL,
    action text DEFAULT 'mask'::text NOT NULL,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_banned_words_action_check CHECK ((action = ANY (ARRAY['mask'::text, 'reject'::text]))),
    CONSTRAINT content_banned_words_word_check CHECK (((btrim(word) <> ''::text) AND (word = lower(word))))
);

CREATE TABLE content_channel_entities (
    tenant_id text NOT NULL,
    channel_id text NOT NULL,
    entity_id text NOT NULL,
    CONSTRAINT content_channel_entities_check CHECK ((channel_id <> entity_id))
);

CREATE TABLE content_channel_reposts (
    tenant_id text NOT NULL,
    channel_id text NOT NULL,
    content_id text NOT NULL,
    actor_id text,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE content_code_aliases (
    tenant_id text NOT NULL,
    source text NOT NULL,
    legacy_kind text NOT NULL,
    legacy_key text NOT NULL,
    code text NOT NULL,
    locator text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_code_aliases_ck CHECK (((source ~ '^[a-z0-9][a-z0-9_.-]{0,63}$'::text) AND (legacy_kind ~ '^[a-z0-9][a-z0-9_.-]{0,63}$'::text) AND (legacy_key <> ''::text) AND (length(legacy_key) <= 512) AND (locator <> ''::text)))
);

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
    CONSTRAINT content_codes_slugs_ck CHECK ((jsonb_typeof(slugs) = 'object'::text))
);

CREATE TABLE content_comment_bans (
    tenant_id text NOT NULL,
    user_id text NOT NULL,
    scope text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    until timestamp with time zone,
    banned_by text NOT NULL,
    banned_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_comment_bans_scope_ck CHECK (((scope = 'global'::text) OR ((scope ~~ 'owner:%'::text) AND (length(scope) > 6)) OR ((scope ~~ 'group:%'::text) AND (length(scope) > 6))))
);

CREATE TABLE content_comment_notifications (
    tenant_id text NOT NULL,
    comment_id uuid NOT NULL,
    recipient text NOT NULL,
    source_id text NOT NULL,
    kind text NOT NULL,
    revision bigint DEFAULT 1 NOT NULL,
    withdrawn boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT content_comment_notifications_revision_check CHECK ((revision > 0))
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
    content_version_id text,
    moderation text DEFAULT 'approved'::text NOT NULL,
    moderation_reason text,
    moderation_verdict jsonb,
    moderated_by text,
    moderated_at timestamp with time zone,
    moderation_revision bigint DEFAULT 1 NOT NULL,
    published_body text,
    language text DEFAULT ''::text NOT NULL,
    CONSTRAINT content_comments_actor_ck CHECK (((user_id IS NOT NULL) OR (anon_name IS NOT NULL))),
    CONSTRAINT content_comments_content_id_ck CHECK ((content_id = lower(content_id))),
    CONSTRAINT content_comments_moderation_ck CHECK ((moderation = ANY (ARRAY['approved'::text, 'held'::text, 'rejected'::text]))),
    CONSTRAINT content_comments_moderation_revision_check CHECK ((moderation_revision > 0)),
    CONSTRAINT content_comments_version_check CHECK ((content_version_id <> ''::text))
);

CREATE TABLE content_creators (
    tenant_id text NOT NULL,
    content_id text NOT NULL,
    user_id text NOT NULL,
    role text DEFAULT 'author'::text NOT NULL,
    "position" integer DEFAULT 0 NOT NULL
);

CREATE TABLE content_entity_edges (
    tenant_id text CONSTRAINT content_group_edges_tenant_id_not_null NOT NULL,
    from_id text CONSTRAINT content_group_edges_from_id_not_null NOT NULL,
    relation text CONSTRAINT content_group_edges_relation_not_null NOT NULL,
    to_id text CONSTRAINT content_group_edges_to_id_not_null NOT NULL,
    created_at timestamp with time zone DEFAULT now() CONSTRAINT content_group_edges_created_at_not_null NOT NULL,
    CONSTRAINT content_entity_edges_check CHECK ((from_id <> to_id))
);

CREATE TABLE content_entity_names (
    tenant_id text CONSTRAINT content_group_names_tenant_id_not_null NOT NULL,
    entity_id text CONSTRAINT content_group_names_group_id_not_null NOT NULL,
    language text CONSTRAINT content_group_names_language_not_null NOT NULL,
    kind text CONSTRAINT content_group_names_kind_not_null NOT NULL,
    name text CONSTRAINT content_group_names_name_not_null NOT NULL,
    normalized text GENERATED ALWAYS AS (contentkit_keyword_normalize(name)) STORED CONSTRAINT content_group_names_normalized_not_null NOT NULL,
    created_at timestamp with time zone DEFAULT now() CONSTRAINT content_group_names_created_at_not_null NOT NULL,
    CONSTRAINT content_entity_names_check CHECK (((language <> ''::text) AND (btrim(name) <> ''::text))),
    CONSTRAINT content_entity_names_kind_check CHECK ((kind = ANY (ARRAY['name'::text, 'alias'::text])))
);

CREATE TABLE content_entity_text (
    tenant_id text CONSTRAINT content_group_text_tenant_id_not_null NOT NULL,
    entity_id text CONSTRAINT content_group_text_group_id_not_null NOT NULL,
    language text CONSTRAINT content_group_text_language_not_null NOT NULL,
    description text DEFAULT ''::text CONSTRAINT content_group_text_description_not_null NOT NULL,
    sort_key text DEFAULT ''::text CONSTRAINT content_group_text_sort_key_not_null NOT NULL
);

CREATE TABLE content_erased_subjects (
    tenant_id text NOT NULL,
    actor_id text NOT NULL,
    erased_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    engine_cleanup_complete boolean DEFAULT false NOT NULL
);

CREATE TABLE content_event_epoch (
    singleton boolean DEFAULT true NOT NULL,
    epoch uuid DEFAULT gen_random_uuid() NOT NULL,
    CONSTRAINT content_event_epoch_singleton_check CHECK (singleton)
);

CREATE SEQUENCE content_event_ids
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

CREATE TABLE content_external_ids (
    tenant_id text NOT NULL,
    source text NOT NULL,
    entity text NOT NULL,
    external_id text NOT NULL,
    id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

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
    CONSTRAINT content_facets_access_releases_check CHECK ((jsonb_typeof(access_releases) = 'array'::text))
);

CREATE TABLE content_favorites (
    user_id text,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id text NOT NULL,
    content_version_id text,
    value smallint NOT NULL,
    revision bigint NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    anonymous_actor_id uuid,
    CONSTRAINT content_favorites_actor_ck CHECK ((((anonymous_actor_id IS NOT NULL) AND (user_id IS NULL)) OR ((anonymous_actor_id IS NULL) AND (user_id IS NOT NULL)))),
    CONSTRAINT content_favorites_content_id_ck CHECK ((content_id = lower(content_id))),
    CONSTRAINT content_favorites_value_ck CHECK ((value = ANY (ARRAY[0, 1]))),
    CONSTRAINT content_favorites_version_check CHECK ((content_version_id <> ''::text))
);

CREATE TABLE content_filter_prefs (
    tenant_id text NOT NULL,
    user_id text NOT NULL,
    overrides jsonb DEFAULT '{}'::jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
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
    content_version_id text,
    CONSTRAINT content_interaction_counts_content_id_ck CHECK ((content_id = lower(content_id))),
    CONSTRAINT content_interaction_counts_version_check CHECK ((content_version_id <> ''::text))
);

CREATE TABLE content_ip_bans (
    tenant_id text NOT NULL,
    network cidr NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    until timestamp with time zone,
    banned_by text NOT NULL,
    banned_at timestamp with time zone DEFAULT now() NOT NULL
);

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
    CONSTRAINT content_items_state_check CHECK ((state = ANY (ARRAY['draft'::text, 'published'::text, 'deleted'::text])))
);

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
    body_excerpt text GENERATED ALWAYS AS (contentkit_body_excerpt(body)) STORED
);

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
    release_id uuid,
    original_title text,
    original_language text,
    CONSTRAINT content_versions_access_check CHECK (((access IS NULL) OR (jsonb_typeof(access) = 'object'::text))),
    CONSTRAINT content_versions_code_check CHECK (((code ~ '^[0-9A-HJKMNP-TV-Z]{4}$'::text) AND (code ~ '[A-Z]'::text))),
    CONSTRAINT content_versions_cover_check CHECK (((cover IS NULL) OR (jsonb_typeof(cover) = 'object'::text))),
    CONSTRAINT content_versions_languages_check CHECK ((cardinality(languages) > 0)),
    CONSTRAINT content_versions_original_language_length CHECK ((char_length(original_language) <= 128)),
    CONSTRAINT content_versions_original_title_length CHECK ((char_length(original_title) <= 512)),
    CONSTRAINT content_versions_released_access_check CHECK (((NOT released) OR (released_access IS NOT NULL))),
    CONSTRAINT content_versions_state_check CHECK ((state = ANY (ARRAY['draft'::text, 'published'::text, 'deleted'::text])))
);

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

CREATE TABLE content_media_allocations (
    tenant_id text NOT NULL,
    folder_prefix text NOT NULL,
    object_key text NOT NULL,
    incarnation uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    retired_at timestamp with time zone,
    CONSTRAINT content_media_allocations_check CHECK (((tenant_id <> ''::text) AND (folder_prefix <> ''::text) AND (object_key <> ''::text)))
);

CREATE TABLE content_media_cleanup (
    tenant_id text NOT NULL,
    folder_prefix text NOT NULL,
    content_kind text NOT NULL,
    due_at timestamp with time zone NOT NULL,
    revision uuid DEFAULT gen_random_uuid() NOT NULL,
    deletion_operations uuid[] DEFAULT '{}'::uuid[] NOT NULL
);

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
    CONSTRAINT content_media_commits_attempt_id_check CHECK (((attempt_id IS NULL) OR (attempt_id <> '00000000-0000-0000-0000-000000000000'::uuid))),
    CONSTRAINT content_media_commits_check CHECK (((tenant_id <> ''::text) AND (content_kind <> ''::text) AND (content_id <> ''::text) AND (folder_prefix <> ''::text))),
    CONSTRAINT content_media_commits_check1 CHECK (((operation_id <> '00000000-0000-0000-0000-000000000000'::uuid) AND (lease_id <> '00000000-0000-0000-0000-000000000000'::uuid))),
    CONSTRAINT content_media_commits_check2 CHECK (((state <> 'prepared'::text) OR (attempt_id IS NOT NULL))),
    CONSTRAINT content_media_commits_check3 CHECK (((charged_bytes >= 0) AND (jsonb_typeof(effects) = 'object'::text))),
    CONSTRAINT content_media_commits_fingerprint_check CHECK ((octet_length(fingerprint) = 32)),
    CONSTRAINT content_media_commits_state_check CHECK ((state = ANY (ARRAY['open'::text, 'prepared'::text, 'frozen'::text, 'applied'::text, 'absent'::text]))),
    CONSTRAINT content_media_commits_version_check CHECK ((content_version_id <> ''::text))
);

CREATE TABLE content_media_processing (
    tenant_id text NOT NULL,
    folder_prefix text NOT NULL,
    fingerprint bytea NOT NULL,
    observed_at timestamp with time zone NOT NULL
);

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
    CONSTRAINT content_media_publications_check CHECK (((tenant_id <> ''::text) AND (content_kind <> ''::text) AND (content_id <> ''::text) AND (upload_path <> ''::text) AND (preset <> ''::text))),
    CONSTRAINT content_media_publications_check1 CHECK (((ordinal >= 0) AND (jsonb_typeof(renditions) = 'array'::text) AND (jsonb_array_length(renditions) > 0))),
    CONSTRAINT content_media_publications_version_check CHECK ((content_version_id <> ''::text))
);

CREATE TABLE content_media_releases (
    tenant_id text NOT NULL,
    operation_id text NOT NULL,
    folder_prefix text NOT NULL,
    owner_id text NOT NULL,
    bytes bigint NOT NULL,
    applied boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_media_releases_bytes_check CHECK ((bytes >= 0))
);

CREATE TABLE content_media_reservations (
    tenant_id text NOT NULL,
    object_key text NOT NULL,
    owner_id text NOT NULL,
    bytes bigint NOT NULL,
    expires_at timestamp with time zone NOT NULL
);

CREATE TABLE content_media_upload_rates (
    tenant_id text NOT NULL,
    uploader text NOT NULL,
    hour timestamp with time zone NOT NULL,
    files integer NOT NULL,
    bytes bigint NOT NULL
);

CREATE TABLE content_media_usage (
    tenant_id text NOT NULL,
    owner_id text NOT NULL,
    used_bytes bigint DEFAULT 0 NOT NULL
);

CREATE TABLE content_page_notice_dismissals (
    tenant_id text NOT NULL,
    version_id text NOT NULL,
    notice_id text NOT NULL,
    dismissed_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE content_poll_answers (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id text NOT NULL,
    question_id uuid NOT NULL,
    actor_id text,
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
    image_name text,
    "position" integer DEFAULT 0 NOT NULL,
    vote_count integer DEFAULT 0 NOT NULL,
    tenant_id text NOT NULL
);

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
    CONSTRAINT content_poll_questions_kind_ck CHECK ((kind = ANY (ARRAY['multiple_choice'::text, 'free_text'::text])))
);

CREATE TABLE content_poll_votes (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    question_id uuid NOT NULL,
    option_id uuid NOT NULL,
    user_id text,
    ip text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id text NOT NULL,
    anonymous_actor_id uuid,
    CONSTRAINT content_poll_votes_actor_ck CHECK ((((anonymous_actor_id IS NOT NULL) AND (user_id IS NULL) AND (ip IS NULL)) OR ((anonymous_actor_id IS NULL) AND ((user_id IS NOT NULL) OR (ip IS NOT NULL)))))
);

CREATE TABLE content_popular_rankings (
    tenant_id text NOT NULL,
    kind text NOT NULL,
    period text NOT NULL,
    domain text NOT NULL,
    language text NOT NULL,
    computed_at timestamp with time zone,
    content_ids text[] DEFAULT '{}'::text[] NOT NULL,
    window_from timestamp with time zone,
    window_until timestamp with time zone,
    covered_from timestamp with time zone,
    covered_until timestamp with time zone,
    available_from timestamp with time zone
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
    revision bigint NOT NULL
);

CREATE TABLE content_publication_requests (
    tenant_id text NOT NULL,
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    content_id text NOT NULL,
    version_id text NOT NULL,
    scheduled_at timestamp with time zone NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    plan jsonb,
    result jsonb,
    current boolean DEFAULT true NOT NULL,
    error text,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    completed_at timestamp with time zone,
    CONSTRAINT content_publication_requests_plan_check CHECK (((plan IS NULL) OR (jsonb_typeof(plan) = 'object'::text))),
    CONSTRAINT content_publication_requests_result_check CHECK (((result IS NULL) OR (jsonb_typeof(result) = 'object'::text))),
    CONSTRAINT content_publication_requests_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'waiting'::text, 'running'::text, 'retiring'::text, 'completed'::text, 'failed'::text, 'cancelled'::text])))
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
    anonymous_actor_id uuid,
    CONSTRAINT content_reactions_actor_ck CHECK ((((anonymous_actor_id IS NOT NULL) AND (user_id IS NULL) AND (ip IS NULL)) OR ((anonymous_actor_id IS NULL) AND ((user_id IS NOT NULL) OR (ip IS NOT NULL))))),
    CONSTRAINT content_reactions_content_id_ck CHECK ((content_id = lower(content_id))),
    CONSTRAINT content_reactions_value_ck CHECK ((value = ANY (ARRAY['-1'::integer, 0, 1]))),
    CONSTRAINT content_reactions_version_check CHECK ((content_version_id <> ''::text))
);

CREATE TABLE content_release_receipts (
    tenant_id text NOT NULL,
    content_id text NOT NULL,
    version_id text NOT NULL,
    release_key text NOT NULL,
    live_from timestamp with time zone NOT NULL,
    occurred_at timestamp with time zone NOT NULL
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
    content_version_id text,
    CONSTRAINT content_search_dirty_version_check CHECK ((content_version_id <> ''::text))
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
    title text DEFAULT ''::text NOT NULL,
    aliases text[] DEFAULT '{}'::text[] NOT NULL,
    keywords text[] DEFAULT '{}'::text[] NOT NULL,
    tenant_id text NOT NULL,
    content_version_id text,
    terms text[] GENERATED ALWAYS AS (contentkit_search_terms(title, aliases, keywords, ''::text)) STORED,
    body text GENERATED ALWAYS AS (contentkit_search_text(title, aliases, keywords, ''::text)) STORED,
    CONSTRAINT content_search_documents_version_check CHECK ((content_version_id <> ''::text))
);

CREATE TABLE content_search_entity_dirty (
    tenant_id text NOT NULL,
    entity_id text NOT NULL,
    revision bigint NOT NULL,
    cursor text DEFAULT ''::text NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

CREATE SEQUENCE content_search_entity_dirty_revision_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE content_search_entity_dirty_revision_seq OWNED BY content_search_entity_dirty.revision;

CREATE TABLE content_search_invalid (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    content_version_id text,
    language text NOT NULL,
    revision bigint NOT NULL,
    last_error text NOT NULL,
    failed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_search_invalid_version_check CHECK ((content_version_id <> ''::text))
);

CREATE TABLE content_searches (
    tenant_id text NOT NULL,
    subject text NOT NULL,
    normalized text NOT NULL,
    query text NOT NULL,
    results integer DEFAULT 0 NOT NULL,
    searches integer DEFAULT 1 NOT NULL,
    searched_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE content_segments (
    tenant_id text NOT NULL,
    version_id text NOT NULL,
    "position" integer NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    start_index integer,
    end_index integer,
    release_at timestamp with time zone,
    start_page text,
    id text DEFAULT (gen_random_uuid())::text NOT NULL,
    released_at timestamp with time zone,
    release_id uuid,
    CONSTRAINT content_segments_range_check CHECK ((((start_page IS NULL) AND (start_index IS NOT NULL) AND (end_index IS NOT NULL) AND (start_index >= 0) AND (end_index > start_index)) OR ((start_page IS NOT NULL) AND ((octet_length(start_page) >= 1) AND (octet_length(start_page) <= 1024)) AND (start_page !~ '(^/|(^|/)\.\.?(/|$)|[[:cntrl:]\\])'::text) AND (start_page !~~ '%//%'::text) AND (start_page !~~ '%/'::text) AND (start_index IS NULL) AND (end_index IS NULL))))
);

CREATE TABLE content_signal_purges (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    content_version_id text DEFAULT ''::text NOT NULL,
    CONSTRAINT content_signal_purges_content_id_check CHECK ((content_id <> ''::text)),
    CONSTRAINT content_signal_purges_content_kind_check CHECK ((content_kind <> ''::text))
);

CREATE TABLE content_version_publications (
    tenant_id text NOT NULL,
    id text NOT NULL,
    version_id text NOT NULL,
    kind text NOT NULL,
    state text DEFAULT 'building'::text NOT NULL,
    source jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    ready_at timestamp with time zone,
    CONSTRAINT content_version_publications_kind_check CHECK ((kind = ANY (ARRAY['pages'::text, 'playback'::text]))),
    CONSTRAINT content_version_publications_state_check CHECK ((state = ANY (ARRAY['building'::text, 'current'::text])))
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
    data jsonb NOT NULL
);

CREATE TABLE eventlog_migrations (
    version integer NOT NULL
);

CREATE TABLE eventlog_pending (
    ordinal bigint NOT NULL,
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
    CONSTRAINT eventlog_pending_version_check CHECK ((version > 0))
);

ALTER TABLE eventlog_pending ALTER COLUMN ordinal ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME eventlog_pending_ordinal_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE eventlog_streams (
    source text NOT NULL,
    scope text NOT NULL,
    epoch text DEFAULT content_event_legacy_epoch() NOT NULL,
    "position" bigint DEFAULT 0 NOT NULL,
    head_id text DEFAULT ''::text NOT NULL,
    pruned_through bigint DEFAULT 0 NOT NULL,
    pruned_id text DEFAULT ''::text NOT NULL,
    recorded_at timestamp with time zone DEFAULT '-infinity'::timestamp with time zone NOT NULL
);

ALTER TABLE ONLY content_search_dirty ALTER COLUMN revision SET DEFAULT nextval('content_search_dirty_revision_seq'::regclass);

ALTER TABLE ONLY content_search_entity_dirty ALTER COLUMN revision SET DEFAULT nextval('content_search_entity_dirty_revision_seq'::regclass);

ALTER TABLE ONLY content_access_history
    ADD CONSTRAINT content_access_history_pkey PRIMARY KEY (tenant_id, id);

ALTER TABLE ONLY content_access_history_start
    ADD CONSTRAINT content_access_history_start_pkey PRIMARY KEY (tenant_id);

ALTER TABLE ONLY content_account_lifecycle
    ADD CONSTRAINT content_account_lifecycle_pkey PRIMARY KEY (tenant_id, actor_id);

ALTER TABLE ONLY content_app_bindings
    ADD CONSTRAINT content_app_bindings_catalog_key_key UNIQUE (catalog_key);

ALTER TABLE ONLY content_app_bindings
    ADD CONSTRAINT content_app_bindings_pkey PRIMARY KEY (app_id);

ALTER TABLE ONLY content_banned_words
    ADD CONSTRAINT content_banned_words_pkey PRIMARY KEY (tenant_id, word);

ALTER TABLE ONLY content_channel_entities
    ADD CONSTRAINT content_channel_entities_pkey PRIMARY KEY (tenant_id, channel_id, entity_id);

ALTER TABLE ONLY content_channel_reposts
    ADD CONSTRAINT content_channel_reposts_pkey PRIMARY KEY (tenant_id, channel_id, content_id);

ALTER TABLE ONLY content_code_aliases
    ADD CONSTRAINT content_code_aliases_pkey PRIMARY KEY (tenant_id, source, legacy_kind, legacy_key);

ALTER TABLE ONLY content_codes
    ADD CONSTRAINT content_codes_pkey PRIMARY KEY (tenant_id, code);

ALTER TABLE ONLY content_codes
    ADD CONSTRAINT content_codes_ref_key UNIQUE (tenant_id, content_kind, content_id);

ALTER TABLE ONLY content_comment_bans
    ADD CONSTRAINT content_comment_bans_pkey PRIMARY KEY (tenant_id, user_id, scope);

ALTER TABLE ONLY content_comment_notifications
    ADD CONSTRAINT content_comment_notifications_pkey PRIMARY KEY (tenant_id, recipient, source_id);

ALTER TABLE ONLY content_comments
    ADD CONSTRAINT content_comments_pkey PRIMARY KEY (tenant_id, id);

ALTER TABLE ONLY content_creators
    ADD CONSTRAINT content_creators_pkey PRIMARY KEY (tenant_id, content_id, user_id, role);

ALTER TABLE ONLY content_entities
    ADD CONSTRAINT content_entities_pkey PRIMARY KEY (tenant_id, id);

ALTER TABLE ONLY content_entity_edges
    ADD CONSTRAINT content_entity_edges_pkey PRIMARY KEY (tenant_id, from_id, relation, to_id);

ALTER TABLE ONLY content_entity_names
    ADD CONSTRAINT content_entity_names_pkey PRIMARY KEY (tenant_id, entity_id, language, normalized);

ALTER TABLE ONLY content_entity_text
    ADD CONSTRAINT content_entity_text_pkey PRIMARY KEY (tenant_id, entity_id, language);

ALTER TABLE ONLY content_erased_subjects
    ADD CONSTRAINT content_erased_subjects_pkey PRIMARY KEY (tenant_id, actor_id);

ALTER TABLE ONLY content_event_epoch
    ADD CONSTRAINT content_event_epoch_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY content_external_ids
    ADD CONSTRAINT content_external_ids_pkey PRIMARY KEY (tenant_id, source, entity, external_id);

ALTER TABLE ONLY content_facets
    ADD CONSTRAINT content_facets_pkey PRIMARY KEY (tenant_id, version_id);

ALTER TABLE ONLY content_filter_prefs
    ADD CONSTRAINT content_filter_prefs_pkey PRIMARY KEY (tenant_id, user_id);

ALTER TABLE ONLY content_interaction_counts
    ADD CONSTRAINT content_interaction_counts_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id);

ALTER TABLE ONLY content_ip_bans
    ADD CONSTRAINT content_ip_bans_pkey PRIMARY KEY (tenant_id, network);

ALTER TABLE ONLY content_items
    ADD CONSTRAINT content_items_pkey PRIMARY KEY (tenant_id, id);

ALTER TABLE ONLY content_media_allocations
    ADD CONSTRAINT content_media_allocations_pkey PRIMARY KEY (tenant_id, object_key);

ALTER TABLE ONLY content_media_cleanup
    ADD CONSTRAINT content_media_cleanup_pkey PRIMARY KEY (tenant_id, folder_prefix);

ALTER TABLE ONLY content_media_commits
    ADD CONSTRAINT content_media_commits_pkey PRIMARY KEY (tenant_id, operation_id);

ALTER TABLE ONLY content_media_processing
    ADD CONSTRAINT content_media_processing_pkey PRIMARY KEY (tenant_id, folder_prefix);

ALTER TABLE ONLY content_media_publications
    ADD CONSTRAINT content_media_publications_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, upload_name, upload_path, preset);

ALTER TABLE ONLY content_media_releases
    ADD CONSTRAINT content_media_releases_pkey PRIMARY KEY (tenant_id, operation_id);

ALTER TABLE ONLY content_media_reservations
    ADD CONSTRAINT content_media_reservations_pkey PRIMARY KEY (tenant_id, object_key);

ALTER TABLE ONLY content_media_upload_rates
    ADD CONSTRAINT content_media_upload_rates_pkey PRIMARY KEY (tenant_id, uploader, hour);

ALTER TABLE ONLY content_media_usage
    ADD CONSTRAINT content_media_usage_pkey PRIMARY KEY (tenant_id, owner_id);

ALTER TABLE ONLY content_page_notice_dismissals
    ADD CONSTRAINT content_page_notice_dismissals_pkey PRIMARY KEY (tenant_id, version_id, notice_id);

ALTER TABLE ONLY content_poll_answers
    ADD CONSTRAINT content_poll_answers_actor_uq UNIQUE (tenant_id, question_id, actor_id);

ALTER TABLE ONLY content_poll_answers
    ADD CONSTRAINT content_poll_answers_pkey PRIMARY KEY (tenant_id, id);

ALTER TABLE ONLY content_poll_options
    ADD CONSTRAINT content_poll_options_pkey PRIMARY KEY (tenant_id, id);

ALTER TABLE ONLY content_poll_questions
    ADD CONSTRAINT content_poll_questions_pkey PRIMARY KEY (tenant_id, id);

ALTER TABLE ONLY content_poll_votes
    ADD CONSTRAINT content_poll_votes_pkey PRIMARY KEY (tenant_id, id);

ALTER TABLE ONLY content_popular_rankings
    ADD CONSTRAINT content_popular_rankings_pkey PRIMARY KEY (tenant_id, kind, period, domain, language);

ALTER TABLE ONLY content_preference_sync
    ADD CONSTRAINT content_preference_sync_pkey PRIMARY KEY (tenant_id, taken_at);

ALTER TABLE ONLY content_publication_requests
    ADD CONSTRAINT content_publication_requests_pkey PRIMARY KEY (tenant_id, id);

ALTER TABLE ONLY content_release_receipts
    ADD CONSTRAINT content_release_receipts_pkey PRIMARY KEY (tenant_id, version_id, release_key, live_from);

ALTER TABLE ONLY content_search_backfill
    ADD CONSTRAINT content_search_backfill_pkey PRIMARY KEY (tenant_id, content_kind, language);

ALTER TABLE ONLY content_search_dirty
    ADD CONSTRAINT content_search_dirty_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, language);

ALTER TABLE ONLY content_search_documents
    ADD CONSTRAINT content_search_documents_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, language);

ALTER TABLE ONLY content_search_entity_dirty
    ADD CONSTRAINT content_search_entity_dirty_pkey PRIMARY KEY (tenant_id, entity_id);

ALTER TABLE ONLY content_search_invalid
    ADD CONSTRAINT content_search_invalid_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, language);

ALTER TABLE ONLY content_searches
    ADD CONSTRAINT content_searches_pkey PRIMARY KEY (tenant_id, subject, normalized);

ALTER TABLE ONLY content_segments
    ADD CONSTRAINT content_segments_pkey PRIMARY KEY (tenant_id, version_id, "position");

ALTER TABLE ONLY content_signal_purges
    ADD CONSTRAINT content_signal_purges_pkey PRIMARY KEY (tenant_id, content_kind, content_id, content_version_id);

ALTER TABLE ONLY content_version_publications
    ADD CONSTRAINT content_version_publications_pkey PRIMARY KEY (tenant_id, id);

ALTER TABLE ONLY content_version_text
    ADD CONSTRAINT content_version_text_pkey PRIMARY KEY (tenant_id, version_id, language);

ALTER TABLE ONLY content_versions
    ADD CONSTRAINT content_versions_code_key UNIQUE (tenant_id, content_id, code);

ALTER TABLE ONLY content_versions
    ADD CONSTRAINT content_versions_pkey PRIMARY KEY (tenant_id, id);

ALTER TABLE ONLY eventlog_events
    ADD CONSTRAINT eventlog_events_pkey PRIMARY KEY (source, scope, "position");

ALTER TABLE ONLY eventlog_events
    ADD CONSTRAINT eventlog_events_source_scope_id_key UNIQUE (source, scope, id);

ALTER TABLE ONLY eventlog_migrations
    ADD CONSTRAINT eventlog_migrations_pkey PRIMARY KEY (version);

ALTER TABLE ONLY eventlog_pending
    ADD CONSTRAINT eventlog_pending_pkey PRIMARY KEY (ordinal);

ALTER TABLE ONLY eventlog_streams
    ADD CONSTRAINT eventlog_streams_pkey PRIMARY KEY (source, scope);

CREATE INDEX content_access_history_version_time ON content_access_history USING btree (tenant_id, content_id, version_id, occurred_at, id);

CREATE INDEX content_app_bindings_tenant_idx ON content_app_bindings USING btree (tenant_id, app_id);

CREATE INDEX content_channel_reposts_feed_idx ON content_channel_reposts USING btree (tenant_id, channel_id, created_at DESC, content_id);

CREATE INDEX content_codes_merged_into ON content_codes USING btree (tenant_id, merged_into) WHERE (merged_into IS NOT NULL);

CREATE INDEX content_comment_bans_scope_idx ON content_comment_bans USING btree (tenant_id, scope, banned_at DESC, user_id);

CREATE INDEX content_comment_notifications_comment_idx ON content_comment_notifications USING btree (tenant_id, comment_id) WHERE (NOT withdrawn);

CREATE INDEX content_comments_author_idx ON content_comments USING btree (tenant_id, user_id, created_at DESC, id DESC) WHERE (user_id IS NOT NULL);

CREATE INDEX content_comments_author_published_idx ON content_comments USING btree (tenant_id, user_id) INCLUDE (likes, dislikes) WHERE ((deleted_at IS NULL) AND (moderation = 'approved'::text) AND (user_id IS NOT NULL));

CREATE INDEX content_comments_held_idx ON content_comments USING btree (tenant_id, created_at, id) WHERE (moderation = 'held'::text);

CREATE INDEX content_comments_latest_idx ON content_comments USING btree (tenant_id, created_at DESC) WHERE (deleted_at IS NULL);

CREATE INDEX content_comments_reply_idx ON content_comments USING btree (tenant_id, reply_to_id, created_at) WHERE (reply_to_id IS NOT NULL);

CREATE INDEX content_comments_toplevel_idx ON content_comments USING btree (tenant_id, content_kind, content_id, content_version_id, language, created_at DESC) WHERE (reply_to_id IS NULL);

CREATE INDEX content_creators_user_idx ON content_creators USING btree (tenant_id, user_id);

CREATE INDEX content_entities_kind_idx ON content_entities USING btree (tenant_id, kind, state, id);

CREATE INDEX content_entities_owner_idx ON content_entities USING btree (tenant_id, owner_user_id) WHERE (owner_user_id IS NOT NULL);

CREATE INDEX content_entities_parent_idx ON content_entities USING btree (tenant_id, parent_id) WHERE (parent_id IS NOT NULL);

CREATE INDEX content_entities_purge_idx ON content_entities USING btree (tenant_id, deleted_at) WHERE ((state = 'deleted'::text) AND (media_purged_at IS NULL));

CREATE UNIQUE INDEX content_entities_slug_idx ON content_entities USING btree (tenant_id, kind, scope_id, slug) WHERE (state = 'active'::text);

CREATE INDEX content_entity_edges_to_idx ON content_entity_edges USING btree (tenant_id, to_id, relation);

CREATE INDEX content_entity_name_lookup_idx ON content_entity_names USING btree (tenant_id, language, regexp_replace(normalized, '[[:space:]_‐‑‒–—―-]+'::text, ''::text, 'g'::text));

CREATE UNIQUE INDEX content_entity_names_canonical_idx ON content_entity_names USING btree (tenant_id, entity_id, language) WHERE (kind = 'name'::text);

CREATE INDEX content_entity_names_lookup_idx ON content_entity_names USING btree (tenant_id, language, normalized);

CREATE INDEX content_entity_names_management_prefix ON content_entity_names USING pgroonga (contentkit_search_terms(name, '{}'::text[], '{}'::text[], ''::text) public.pgroonga_text_array_term_search_ops_v2);

CREATE INDEX content_entity_names_management_substring ON content_entity_names USING pgroonga (contentkit_search_names(name, '{}'::text[]) public.pgroonga_text_regexp_ops_v2);

CREATE INDEX content_external_ids_id_idx ON content_external_ids USING btree (tenant_id, entity, id);

CREATE INDEX content_facets_content_idx ON content_facets USING btree (tenant_id, content_id);

CREATE INDEX content_facets_entities_idx ON content_facets USING gin (entity_ids);

CREATE INDEX content_facets_languages_idx ON content_facets USING gin (languages);

CREATE INDEX content_facets_list_idx ON content_facets USING btree (tenant_id, kind, published_at DESC, content_id DESC) WHERE (NOT deleted);

CREATE INDEX content_facets_next_release_idx ON content_facets USING btree (tenant_id, next_release_at) WHERE (next_release_at IS NOT NULL);

CREATE INDEX content_facets_owner_idx ON content_facets USING btree (tenant_id, owner_entity_id, published_at DESC) WHERE (owner_entity_id IS NOT NULL);

CREATE INDEX content_facets_published_order_idx ON content_facets USING btree (tenant_id, kind, COALESCE(published_at, '1970-01-01 00:00:00+00'::timestamp with time zone) DESC, content_id DESC);

CREATE INDEX content_favorites_content_idx ON content_favorites USING btree (tenant_id, content_kind, content_id, content_version_id);

CREATE INDEX content_favorites_revision_idx ON content_favorites USING btree (tenant_id, revision);

CREATE INDEX content_favorites_user_created_idx ON content_favorites USING btree (tenant_id, user_id, created_at DESC) WHERE (value = 1);

CREATE UNIQUE INDEX content_favorites_user_key ON content_favorites USING btree (tenant_id, user_id, content_kind, content_id, content_version_id) NULLS NOT DISTINCT WHERE (user_id IS NOT NULL);

CREATE INDEX content_interaction_counts_best_idx ON content_interaction_counts USING btree (tenant_id, content_kind, (
CASE
    WHEN ((likes + dislikes) = 0) THEN (0)::double precision
    ELSE ((((((likes)::numeric + 1.9208) / ((likes + dislikes))::numeric))::double precision - (((1.96)::double precision * sqrt(((((likes)::double precision * (dislikes)::double precision) / ((likes + dislikes))::double precision) + (0.9604)::double precision))) / ((likes + dislikes))::double precision)) / (((1)::numeric + (3.8416 / ((likes + dislikes))::numeric)))::double precision)
END) DESC) WHERE ((likes + dislikes) > 0);

CREATE INDEX content_interaction_counts_comments_idx ON content_interaction_counts USING btree (tenant_id, content_kind, comment_count DESC) WHERE (comment_count > 0);

CREATE INDEX content_interaction_counts_favorites_idx ON content_interaction_counts USING btree (tenant_id, content_kind, favorites DESC) WHERE (favorites > 0);

CREATE INDEX content_interaction_counts_likes_idx ON content_interaction_counts USING btree (tenant_id, content_kind, likes DESC) WHERE (likes > 0);

CREATE INDEX content_items_deleted_with_entity_idx ON content_items USING btree (tenant_id, deleted_with_entity) WHERE (deleted_with_entity IS NOT NULL);

CREATE INDEX content_items_drafts_idx ON content_items USING btree (tenant_id, kind, created_at) WHERE ((state = 'draft'::text) AND (first_published_at IS NULL));

CREATE INDEX content_items_due_idx ON content_items USING btree (publish_at) WHERE ((state = 'published'::text) AND (published_at IS NULL));

CREATE INDEX content_items_kind_published_idx ON content_items USING btree (tenant_id, kind, published_at DESC, id DESC) WHERE (state = 'published'::text);

CREATE INDEX content_items_management_order_idx ON content_items USING btree (tenant_id, kind, created_at DESC, id DESC);

CREATE INDEX content_items_owner_entity_idx ON content_items USING btree (tenant_id, owner_entity_id) WHERE (owner_entity_id IS NOT NULL);

CREATE INDEX content_media_allocations_folder ON content_media_allocations USING btree (tenant_id, folder_prefix);

CREATE INDEX content_media_cleanup_due ON content_media_cleanup USING btree (tenant_id, content_kind, due_at, folder_prefix);

CREATE INDEX content_media_commits_folder_owner ON content_media_commits USING btree (tenant_id, folder_prefix) INCLUDE (content_kind, content_id, content_version_id);

CREATE INDEX content_media_commits_notify ON content_media_commits USING btree (tenant_id, updated_at, operation_id) WHERE ((state = 'applied'::text) AND ((effects ->> 'notify'::text) = 'true'::text));

CREATE INDEX content_media_commits_pending ON content_media_commits USING btree (tenant_id, updated_at, operation_id) WHERE (state = ANY (ARRAY['open'::text, 'prepared'::text, 'frozen'::text]));

CREATE UNIQUE INDEX content_media_commits_pending_folder ON content_media_commits USING btree (tenant_id, folder_prefix) WHERE (state = ANY (ARRAY['open'::text, 'prepared'::text, 'frozen'::text]));

CREATE UNIQUE INDEX content_media_releases_pending_folder ON content_media_releases USING btree (tenant_id, folder_prefix) WHERE (NOT applied);

CREATE INDEX content_media_reservations_owner ON content_media_reservations USING btree (tenant_id, owner_id, expires_at);

CREATE INDEX content_memberships_content_idx ON content_memberships USING btree (tenant_id, content_id, version_id);

CREATE INDEX content_memberships_entity_idx ON content_memberships USING btree (tenant_id, entity_id, "position", content_id);

CREATE UNIQUE INDEX content_memberships_key_idx ON content_memberships USING btree (tenant_id, entity_id, content_id, version_id, role) NULLS NOT DISTINCT;

CREATE INDEX content_poll_answers_pending_idx ON content_poll_answers USING btree (tenant_id, id) WHERE (classified_at IS NULL);

CREATE INDEX content_poll_options_question_idx ON content_poll_options USING btree (tenant_id, question_id, "position");

CREATE INDEX content_poll_questions_live_idx ON content_poll_questions USING btree (tenant_id, language, live_at DESC) WHERE ((deleted_at IS NULL) AND is_active);

CREATE UNIQUE INDEX content_poll_votes_ip_uq ON content_poll_votes USING btree (tenant_id, question_id, ip) WHERE ((user_id IS NULL) AND (ip IS NOT NULL));

CREATE INDEX content_poll_votes_option_idx ON content_poll_votes USING btree (tenant_id, option_id);

CREATE UNIQUE INDEX content_poll_votes_user_uq ON content_poll_votes USING btree (tenant_id, question_id, user_id) WHERE (user_id IS NOT NULL);

CREATE UNIQUE INDEX content_publication_requests_current_idx ON content_publication_requests USING btree (tenant_id, version_id) WHERE current;

CREATE INDEX content_reactions_content_idx ON content_reactions USING btree (tenant_id, content_kind, content_id, content_version_id);

CREATE UNIQUE INDEX content_reactions_ip_uq ON content_reactions USING btree (tenant_id, content_kind, content_id, content_version_id, ip) NULLS NOT DISTINCT WHERE ((user_id IS NULL) AND (ip IS NOT NULL));

CREATE INDEX content_reactions_revision_idx ON content_reactions USING btree (tenant_id, revision);

CREATE UNIQUE INDEX content_reactions_user_uq ON content_reactions USING btree (tenant_id, content_kind, content_id, content_version_id, user_id) NULLS NOT DISTINCT WHERE (user_id IS NOT NULL);

CREATE INDEX content_search_backfill_state ON content_search_backfill USING btree (state);

CREATE INDEX content_search_dirty_updated_at ON content_search_dirty USING btree (tenant_id, updated_at);

CREATE INDEX content_search_documents_body_substring ON content_search_documents USING pgroonga (body public.pgroonga_text_regexp_ops_v2);

CREATE INDEX content_search_documents_kind_language ON content_search_documents USING btree (tenant_id, content_kind, language);

CREATE INDEX content_search_documents_names_prefix ON content_search_documents USING pgroonga (contentkit_search_terms(title, aliases, '{}'::text[], ''::text) public.pgroonga_text_array_term_search_ops_v2);

CREATE INDEX content_search_documents_names_substring ON content_search_documents USING pgroonga (contentkit_search_names(title, aliases) public.pgroonga_text_regexp_ops_v2);

CREATE INDEX content_search_documents_terms ON content_search_documents USING gin (terms);

CREATE INDEX content_search_documents_terms_native ON content_search_documents USING pgroonga (terms);

CREATE INDEX content_search_entity_dirty_updated_at ON content_search_entity_dirty USING btree (tenant_id, updated_at);

CREATE INDEX content_searches_age_idx ON content_searches USING btree (tenant_id, searched_at);

CREATE INDEX content_searches_recent_idx ON content_searches USING btree (tenant_id, subject, searched_at DESC);

CREATE UNIQUE INDEX content_segments_id_idx ON content_segments USING btree (tenant_id, version_id, id);

CREATE UNIQUE INDEX content_segments_start_page_idx ON content_segments USING btree (tenant_id, version_id, start_page) WHERE (start_page IS NOT NULL);

CREATE INDEX content_version_original_title_management_prefix ON content_versions USING pgroonga (contentkit_search_terms(original_title, '{}'::text[], '{}'::text[], ''::text) public.pgroonga_text_array_term_search_ops_v2);

CREATE INDEX content_version_original_title_management_substring ON content_versions USING pgroonga (contentkit_search_names(original_title, '{}'::text[]) public.pgroonga_text_regexp_ops_v2);

CREATE INDEX content_version_publications_building_idx ON content_version_publications USING btree (tenant_id, created_at) WHERE (state = 'building'::text);

CREATE UNIQUE INDEX content_version_publications_state_idx ON content_version_publications USING btree (tenant_id, version_id, kind, state);

CREATE INDEX content_version_text_management_prefix ON content_version_text USING pgroonga (contentkit_search_terms(title, aliases, '{}'::text[], ''::text) public.pgroonga_text_array_term_search_ops_v2);

CREATE INDEX content_version_text_management_substring ON content_version_text USING pgroonga (contentkit_search_names(title, aliases) public.pgroonga_text_regexp_ops_v2);

CREATE INDEX content_versions_content_idx ON content_versions USING btree (tenant_id, content_id, "position");

CREATE INDEX content_versions_due_idx ON content_versions USING btree (publish_at) WHERE ((state = 'published'::text) AND (published_at IS NULL));

CREATE INDEX eventlog_events_source_scope_event_type_position_idx ON eventlog_events USING btree (source, scope, event_type, "position");

CREATE INDEX eventlog_events_source_scope_recorded_at_position_idx ON eventlog_events USING btree (source, scope, recorded_at, "position");

CREATE INDEX eventlog_pending_transaction_id_idx ON eventlog_pending USING btree (transaction_id);

CREATE TRIGGER content_entities_deleted BEFORE INSERT OR UPDATE OF state ON content_entities FOR EACH ROW EXECUTE FUNCTION contentkit_entity_deleted();

CREATE TRIGGER content_items_first_published BEFORE INSERT OR UPDATE OF published_at ON content_items FOR EACH ROW EXECUTE FUNCTION contentkit_first_published();

CREATE TRIGGER content_search_dirty_revision BEFORE INSERT OR UPDATE ON content_search_dirty FOR EACH ROW EXECUTE FUNCTION contentkit_bump_dirty_revision();

CREATE TRIGGER content_versions_code BEFORE INSERT ON content_versions FOR EACH ROW EXECUTE FUNCTION contentkit_version_code();

CREATE CONSTRAINT TRIGGER eventlog_flush_at_commit AFTER INSERT ON eventlog_pending DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION eventlog_flush();

CREATE TRIGGER eventlog_immutable BEFORE UPDATE ON eventlog_events FOR EACH ROW EXECUTE FUNCTION eventlog_immutable();

ALTER TABLE ONLY content_channel_entities
    ADD CONSTRAINT content_channel_entities_tenant_id_channel_id_fkey FOREIGN KEY (tenant_id, channel_id) REFERENCES content_entities(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_channel_entities
    ADD CONSTRAINT content_channel_entities_tenant_id_entity_id_fkey FOREIGN KEY (tenant_id, entity_id) REFERENCES content_entities(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_channel_reposts
    ADD CONSTRAINT content_channel_reposts_tenant_id_channel_id_fkey FOREIGN KEY (tenant_id, channel_id) REFERENCES content_entities(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_channel_reposts
    ADD CONSTRAINT content_channel_reposts_tenant_id_content_id_fkey FOREIGN KEY (tenant_id, content_id) REFERENCES content_items(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_code_aliases
    ADD CONSTRAINT content_code_aliases_code_fkey FOREIGN KEY (tenant_id, code) REFERENCES content_codes(tenant_id, code);

ALTER TABLE ONLY content_codes
    ADD CONSTRAINT content_codes_merged_into_fkey FOREIGN KEY (tenant_id, merged_into) REFERENCES content_codes(tenant_id, code);

ALTER TABLE ONLY content_comment_notifications
    ADD CONSTRAINT content_comment_notifications_tenant_id_comment_id_fkey FOREIGN KEY (tenant_id, comment_id) REFERENCES content_comments(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_comments
    ADD CONSTRAINT content_comments_reply_to_id_fkey FOREIGN KEY (tenant_id, reply_to_id) REFERENCES content_comments(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_creators
    ADD CONSTRAINT content_creators_tenant_id_content_id_fkey FOREIGN KEY (tenant_id, content_id) REFERENCES content_items(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_entity_edges
    ADD CONSTRAINT content_entity_edges_tenant_id_from_id_fkey FOREIGN KEY (tenant_id, from_id) REFERENCES content_entities(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_entity_edges
    ADD CONSTRAINT content_entity_edges_tenant_id_to_id_fkey FOREIGN KEY (tenant_id, to_id) REFERENCES content_entities(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_entity_names
    ADD CONSTRAINT content_entity_names_tenant_id_entity_id_fkey FOREIGN KEY (tenant_id, entity_id) REFERENCES content_entities(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_entity_text
    ADD CONSTRAINT content_entity_text_tenant_id_entity_id_fkey FOREIGN KEY (tenant_id, entity_id) REFERENCES content_entities(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_facets
    ADD CONSTRAINT content_facets_tenant_id_version_id_fkey FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_memberships
    ADD CONSTRAINT content_memberships_tenant_id_content_id_fkey FOREIGN KEY (tenant_id, content_id) REFERENCES content_items(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_memberships
    ADD CONSTRAINT content_memberships_tenant_id_entity_id_fkey FOREIGN KEY (tenant_id, entity_id) REFERENCES content_entities(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_memberships
    ADD CONSTRAINT content_memberships_tenant_id_version_id_fkey FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_page_notice_dismissals
    ADD CONSTRAINT content_page_notice_dismissals_tenant_id_version_id_fkey FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_poll_answers
    ADD CONSTRAINT content_poll_answers_question_id_fkey FOREIGN KEY (tenant_id, question_id) REFERENCES content_poll_questions(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_poll_options
    ADD CONSTRAINT content_poll_options_question_id_fkey FOREIGN KEY (tenant_id, question_id) REFERENCES content_poll_questions(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_poll_votes
    ADD CONSTRAINT content_poll_votes_option_id_fkey FOREIGN KEY (tenant_id, option_id) REFERENCES content_poll_options(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_poll_votes
    ADD CONSTRAINT content_poll_votes_question_id_fkey FOREIGN KEY (tenant_id, question_id) REFERENCES content_poll_questions(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_publication_requests
    ADD CONSTRAINT content_publication_requests_tenant_id_content_id_fkey FOREIGN KEY (tenant_id, content_id) REFERENCES content_items(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_publication_requests
    ADD CONSTRAINT content_publication_requests_tenant_id_version_id_fkey FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_release_receipts
    ADD CONSTRAINT content_release_receipts_tenant_id_content_id_fkey FOREIGN KEY (tenant_id, content_id) REFERENCES content_items(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_segments
    ADD CONSTRAINT content_segments_tenant_id_version_id_fkey FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_version_publications
    ADD CONSTRAINT content_version_publications_tenant_id_version_id_fkey FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_version_text
    ADD CONSTRAINT content_version_text_tenant_id_version_id_fkey FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY content_versions
    ADD CONSTRAINT content_versions_release_id_fkey FOREIGN KEY (tenant_id, release_id) REFERENCES content_publication_requests(tenant_id, id) ON DELETE SET NULL (release_id);

ALTER TABLE ONLY content_versions
    ADD CONSTRAINT content_versions_tenant_id_content_id_fkey FOREIGN KEY (tenant_id, content_id) REFERENCES content_items(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE ONLY eventlog_events
    ADD CONSTRAINT eventlog_events_source_scope_fkey FOREIGN KEY (source, scope) REFERENCES eventlog_streams(source, scope) ON DELETE CASCADE;


--

INSERT INTO content_event_epoch (singleton) VALUES (true);
INSERT INTO eventlog_migrations (version) VALUES (1);
