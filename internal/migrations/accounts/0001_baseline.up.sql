-- ContentKit's shared account schema: account media, avatar placement and
-- account erasure records. Without Config.Accounts it is the catalog schema,
-- whose baseline has already created the event log and content_media_items:
-- they are created here only where absent. With bodies unchecked, a function
-- may precede the tables it reads.
SET LOCAL check_function_bodies = false;

DO $ckguard$
BEGIN
    IF to_regclass(format('%I.eventlog_events', current_schema())) IS NOT NULL THEN
        RETURN;
    END IF;
    EXECUTE replace($ckshared$
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
END $$;

CREATE FUNCTION "__contentkit_schema__".eventlog_immutable() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'pg_temp'
    AS $$
BEGIN
 RAISE EXCEPTION 'retained event facts are immutable';
END $$;

-- The account event log: account media events, one ordered stream per scope.
CREATE SEQUENCE "__contentkit_schema__".content_event_ids;

CREATE TABLE "__contentkit_schema__".eventlog_streams (
    source text NOT NULL,
    scope text NOT NULL,
    epoch text DEFAULT (gen_random_uuid())::text NOT NULL,
    "position" bigint DEFAULT 0 NOT NULL,
    head_id text DEFAULT ''::text NOT NULL,
    pruned_through bigint DEFAULT 0 NOT NULL,
    pruned_id text DEFAULT ''::text NOT NULL,
    recorded_at timestamp with time zone DEFAULT '-infinity'::timestamp with time zone NOT NULL,
    CONSTRAINT eventlog_streams_pkey PRIMARY KEY (source, scope)
);

CREATE TABLE "__contentkit_schema__".eventlog_events (
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
    CONSTRAINT eventlog_events_source_scope_fkey FOREIGN KEY (source, scope) REFERENCES "__contentkit_schema__".eventlog_streams(source, scope) ON DELETE CASCADE
);
CREATE INDEX eventlog_events_source_scope_event_type_position_idx ON "__contentkit_schema__".eventlog_events USING btree (source, scope, event_type, "position");
CREATE INDEX eventlog_events_source_scope_recorded_at_position_idx ON "__contentkit_schema__".eventlog_events USING btree (source, scope, recorded_at, "position");
CREATE TRIGGER eventlog_immutable BEFORE UPDATE ON "__contentkit_schema__".eventlog_events FOR EACH ROW EXECUTE FUNCTION "__contentkit_schema__".eventlog_immutable();

CREATE TABLE "__contentkit_schema__".eventlog_pending (
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
CREATE INDEX eventlog_pending_transaction_id_idx ON "__contentkit_schema__".eventlog_pending USING btree (transaction_id);
CREATE CONSTRAINT TRIGGER eventlog_flush_at_commit AFTER INSERT ON "__contentkit_schema__".eventlog_pending DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION "__contentkit_schema__".eventlog_flush();

CREATE TABLE "__contentkit_schema__".eventlog_migrations (
    version integer NOT NULL,
    CONSTRAINT eventlog_migrations_pkey PRIMARY KEY (version)
);
INSERT INTO "__contentkit_schema__".eventlog_migrations (version) VALUES (1);
$ckshared$, '"__contentkit_schema__"', quote_ident(current_schema()));
END
$ckguard$;

-- Each tenant's avatar primary: the app whose bucket holds account media.
CREATE TABLE content_account_avatar_placement (
    tenant_id uuid NOT NULL,
    primary_app_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_account_avatar_placement_primary_app_id_check CHECK ((primary_app_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT content_account_avatar_placement_pkey PRIMARY KEY (tenant_id)
);

-- Account erasure: requested, then completed per app.
CREATE TABLE content_account_erasure_intents (
    tenant_id uuid NOT NULL,
    app_id text NOT NULL,
    actor_id text NOT NULL,
    requested_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_account_erasure_intents_actor_id_check CHECK ((actor_id <> ''::text)),
    CONSTRAINT content_account_erasure_intents_app_id_check CHECK (
CASE
    WHEN (app_id = ''::text) THEN true
    ELSE (((app_id)::uuid <> '00000000-0000-0000-0000-000000000000'::uuid) AND (((app_id)::uuid)::text = app_id))
END),
    CONSTRAINT content_account_erasure_intents_pkey PRIMARY KEY (tenant_id, app_id, actor_id)
);
-- The pending-anonymization probe reads intents by request time.
CREATE INDEX content_account_erasure_intents_requested ON content_account_erasure_intents USING btree (tenant_id, app_id, requested_at);

CREATE TABLE content_account_erasure_receipts (
    tenant_id uuid NOT NULL,
    app_id uuid NOT NULL,
    actor_id text NOT NULL,
    completed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_account_erasure_receipts_actor_id_check CHECK ((actor_id <> ''::text)),
    CONSTRAINT content_account_erasure_receipts_app_id_check CHECK ((app_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT content_account_erasure_receipts_pkey PRIMARY KEY (tenant_id, app_id, actor_id)
);

-- An app whose signal plane was ever provisioned (MigrateClickHouse) keeps
-- account erasure pending on an engine without one.
CREATE TABLE content_signal_planes (
    tenant_id uuid NOT NULL,
    app_id uuid NOT NULL,
    provisioned_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_signal_planes_app_id_check CHECK ((app_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT content_signal_planes_pkey PRIMARY KEY (tenant_id, app_id)
);

-- Each account media item's manifest.json in the bucket is the only record of
-- its files; this is a rebuildable cache of it, one row per item.
CREATE TABLE IF NOT EXISTS content_media_items (
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

-- The chains before this baseline dropped the catalog's empty stand-in here.
DROP TABLE IF EXISTS content_media_commits;
