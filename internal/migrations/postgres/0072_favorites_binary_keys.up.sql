-- Favorites move to binary keys: app_id (uuid) replaces the text tenant_id,
-- and the account, content and version ids become uuid. The table is copied,
-- not altered, so its writers never wait on the copy: this step creates the
-- new table and notes every key written from now on, 0073 copies the rows in
-- batches and builds the indexes, and 0074 settles the noted keys under a
-- short lock and swaps the tables.
SET LOCAL lock_timeout = '10s';

DO $ck$
DECLARE
    bad text;
BEGIN
    SELECT format('tenant_id %s, user_id %s, content_id %s, content_version_id %s', tenant_id, user_id, content_id, content_version_id) INTO bad
    FROM content_favorites
    WHERE tenant_id !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
       OR content_id !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
       OR user_id !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
       OR content_version_id !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    LIMIT 1;
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'content_favorites holds an id that is not a canonical UUID (%); delete or correct such rows, then migrate again', bad;
    END IF;
END
$ck$;

CREATE TABLE content_favorites_next (
    revision bigint NOT NULL,
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
    CONSTRAINT content_favorites_actor_ck CHECK ((user_id IS NULL) <> (anonymous_actor_id IS NULL)),
    CONSTRAINT content_favorites_value_ck CHECK (value IN (0, 1))
);

-- Keys written to content_favorites since the copy began, in its text form.
CREATE TABLE content_favorites_moved (
    tenant_id text NOT NULL,
    user_id text,
    anonymous_actor_id uuid,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    content_version_id text
);

CREATE FUNCTION content_favorites_note() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path FROM CURRENT
    AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        INSERT INTO content_favorites_moved VALUES (OLD.tenant_id, OLD.user_id, OLD.anonymous_actor_id, OLD.content_kind, OLD.content_id, OLD.content_version_id);
    END IF;
    IF TG_OP <> 'DELETE' THEN
        INSERT INTO content_favorites_moved VALUES (NEW.tenant_id, NEW.user_id, NEW.anonymous_actor_id, NEW.content_kind, NEW.content_id, NEW.content_version_id);
    END IF;
    RETURN NULL;
END
$$;

CREATE TRIGGER content_favorites_note AFTER INSERT OR UPDATE OR DELETE ON content_favorites
    FOR EACH ROW EXECUTE FUNCTION content_favorites_note();

-- Copies every row in batches of 4096 pages, each its own transaction. A row
-- written meanwhile may be copied twice or not at all; its key is noted, and
-- settling rewrites it.
CREATE PROCEDURE content_favorites_copy()
    LANGUAGE plpgsql
    AS $$
DECLARE
    pages bigint;
    first bigint := 0;
BEGIN
    TRUNCATE content_favorites_next;
    COMMIT;
    pages := pg_relation_size('content_favorites') / current_setting('block_size')::bigint;
    WHILE first < pages LOOP
        INSERT INTO content_favorites_next (revision, created_at, updated_at, app_id, user_id, anonymous_actor_id,
            content_id, content_version_id, value, content_kind, import_source)
        SELECT revision, created_at, updated_at, tenant_id::uuid, user_id::uuid, anonymous_actor_id,
            content_id::uuid, content_version_id::uuid, value, content_kind, import_source
        FROM content_favorites
        WHERE ctid >= format('(%s,0)', first)::tid AND ctid < format('(%s,0)', first + 4096)::tid;
        first := first + 4096;
        COMMIT;
    END LOOP;
END
$$;

-- Rewrites the noted keys from content_favorites' current rows: an account's
-- key through the unique indexes, an anonymized one through the content's.
CREATE PROCEDURE content_favorites_settle()
    LANGUAGE plpgsql
    AS $$
BEGIN
    CREATE TEMP TABLE content_favorites_settling (LIKE content_favorites_moved) ON COMMIT DROP;
    WITH noted AS (DELETE FROM content_favorites_moved RETURNING *)
    INSERT INTO content_favorites_settling SELECT DISTINCT * FROM noted;
    DELETE FROM content_favorites_next n USING content_favorites_settling k
    WHERE k.user_id IS NOT NULL AND n.app_id = k.tenant_id::uuid AND n.user_id = k.user_id::uuid
      AND n.content_kind = k.content_kind AND n.content_id = k.content_id::uuid
      AND n.content_version_id IS NOT DISTINCT FROM k.content_version_id::uuid;
    DELETE FROM content_favorites_next n USING content_favorites_settling k
    WHERE k.user_id IS NULL AND n.app_id = k.tenant_id::uuid AND n.content_kind = k.content_kind AND n.content_id = k.content_id::uuid
      AND n.content_version_id IS NOT DISTINCT FROM k.content_version_id::uuid
      AND n.user_id IS NULL AND n.anonymous_actor_id = k.anonymous_actor_id;
    INSERT INTO content_favorites_next (revision, created_at, updated_at, app_id, user_id, anonymous_actor_id,
        content_id, content_version_id, value, content_kind, import_source)
    SELECT o.revision, o.created_at, o.updated_at, o.tenant_id::uuid, o.user_id::uuid, o.anonymous_actor_id,
        o.content_id::uuid, o.content_version_id::uuid, o.value, o.content_kind, o.import_source
    FROM content_favorites o JOIN content_favorites_settling k ON k.user_id IS NOT NULL AND o.tenant_id = k.tenant_id
     AND o.user_id = k.user_id AND o.content_kind = k.content_kind AND o.content_id = k.content_id
     AND o.content_version_id IS NOT DISTINCT FROM k.content_version_id;
    INSERT INTO content_favorites_next (revision, created_at, updated_at, app_id, user_id, anonymous_actor_id,
        content_id, content_version_id, value, content_kind, import_source)
    SELECT o.revision, o.created_at, o.updated_at, o.tenant_id::uuid, NULL, o.anonymous_actor_id,
        o.content_id::uuid, o.content_version_id::uuid, o.value, o.content_kind, o.import_source
    FROM content_favorites o JOIN content_favorites_settling k ON k.user_id IS NULL AND o.tenant_id = k.tenant_id
     AND o.content_kind = k.content_kind AND o.content_id = k.content_id
     AND o.content_version_id IS NOT DISTINCT FROM k.content_version_id
     AND o.user_id IS NULL AND o.anonymous_actor_id = k.anonymous_actor_id;
    DROP TABLE content_favorites_settling;
END
$$;
