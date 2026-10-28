-- ContentKit's shared account schema: tenant bindings, account media and
-- erasure records. Without Config.Accounts it is the catalog schema.
-- Functions may precede the tables they read, as in a pg_dump.
SET LOCAL check_function_bodies = false;

-- Media journal, processing and event-log storage. A catalog in this same
-- schema already created identical objects, so they are created only once.
DO $ckguard$
BEGIN
    IF to_regclass(format('%I.content_media_commits', current_schema())) IS NOT NULL THEN
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

CREATE SEQUENCE "__contentkit_schema__".content_event_ids
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

CREATE TABLE "__contentkit_schema__".content_media_allocations (
    tenant_id text NOT NULL,
    folder_prefix text NOT NULL,
    object_key text NOT NULL,
    incarnation uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    retired_at timestamp with time zone,
    CONSTRAINT content_media_allocations_check CHECK (((tenant_id <> ''::text) AND (folder_prefix <> ''::text) AND (object_key <> ''::text)))
);

CREATE TABLE "__contentkit_schema__".content_media_cleanup (
    tenant_id text NOT NULL,
    folder_prefix text NOT NULL,
    content_kind text NOT NULL,
    due_at timestamp with time zone NOT NULL,
    revision uuid DEFAULT gen_random_uuid() NOT NULL,
    deletion_operations uuid[] DEFAULT '{}'::uuid[] NOT NULL
);

CREATE TABLE "__contentkit_schema__".content_media_commits (
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

CREATE TABLE "__contentkit_schema__".content_media_processing (
    tenant_id text NOT NULL,
    folder_prefix text NOT NULL,
    fingerprint bytea NOT NULL,
    observed_at timestamp with time zone NOT NULL
);

CREATE TABLE "__contentkit_schema__".content_media_publications (
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

CREATE TABLE "__contentkit_schema__".content_media_releases (
    tenant_id text NOT NULL,
    operation_id text NOT NULL,
    folder_prefix text NOT NULL,
    owner_id text NOT NULL,
    bytes bigint NOT NULL,
    applied boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_media_releases_bytes_check CHECK ((bytes >= 0))
);

CREATE TABLE "__contentkit_schema__".content_media_reservations (
    tenant_id text NOT NULL,
    object_key text NOT NULL,
    owner_id text NOT NULL,
    bytes bigint NOT NULL,
    expires_at timestamp with time zone NOT NULL
);

CREATE TABLE "__contentkit_schema__".content_media_upload_rates (
    tenant_id text NOT NULL,
    uploader text NOT NULL,
    hour timestamp with time zone NOT NULL,
    files integer NOT NULL,
    bytes bigint NOT NULL
);

CREATE TABLE "__contentkit_schema__".content_media_usage (
    tenant_id text NOT NULL,
    owner_id text NOT NULL,
    used_bytes bigint DEFAULT 0 NOT NULL
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
    data jsonb NOT NULL
);

CREATE TABLE "__contentkit_schema__".eventlog_migrations (
    version integer NOT NULL
);

CREATE TABLE "__contentkit_schema__".eventlog_pending (
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

ALTER TABLE "__contentkit_schema__".eventlog_pending ALTER COLUMN ordinal ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME "__contentkit_schema__".eventlog_pending_ordinal_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE "__contentkit_schema__".eventlog_streams (
    source text NOT NULL,
    scope text NOT NULL,
    epoch text DEFAULT (gen_random_uuid())::text NOT NULL,
    "position" bigint DEFAULT 0 NOT NULL,
    head_id text DEFAULT ''::text NOT NULL,
    pruned_through bigint DEFAULT 0 NOT NULL,
    pruned_id text DEFAULT ''::text NOT NULL,
    recorded_at timestamp with time zone DEFAULT '-infinity'::timestamp with time zone NOT NULL
);

ALTER TABLE ONLY "__contentkit_schema__".content_media_allocations
    ADD CONSTRAINT content_media_allocations_pkey PRIMARY KEY (tenant_id, object_key);

ALTER TABLE ONLY "__contentkit_schema__".content_media_cleanup
    ADD CONSTRAINT content_media_cleanup_pkey PRIMARY KEY (tenant_id, folder_prefix);

ALTER TABLE ONLY "__contentkit_schema__".content_media_commits
    ADD CONSTRAINT content_media_commits_pkey PRIMARY KEY (tenant_id, operation_id);

ALTER TABLE ONLY "__contentkit_schema__".content_media_processing
    ADD CONSTRAINT content_media_processing_pkey PRIMARY KEY (tenant_id, folder_prefix);

ALTER TABLE ONLY "__contentkit_schema__".content_media_publications
    ADD CONSTRAINT content_media_publications_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, upload_name, upload_path, preset);

ALTER TABLE ONLY "__contentkit_schema__".content_media_releases
    ADD CONSTRAINT content_media_releases_pkey PRIMARY KEY (tenant_id, operation_id);

ALTER TABLE ONLY "__contentkit_schema__".content_media_reservations
    ADD CONSTRAINT content_media_reservations_pkey PRIMARY KEY (tenant_id, object_key);

ALTER TABLE ONLY "__contentkit_schema__".content_media_upload_rates
    ADD CONSTRAINT content_media_upload_rates_pkey PRIMARY KEY (tenant_id, uploader, hour);

ALTER TABLE ONLY "__contentkit_schema__".content_media_usage
    ADD CONSTRAINT content_media_usage_pkey PRIMARY KEY (tenant_id, owner_id);

ALTER TABLE ONLY "__contentkit_schema__".eventlog_events
    ADD CONSTRAINT eventlog_events_pkey PRIMARY KEY (source, scope, "position");

ALTER TABLE ONLY "__contentkit_schema__".eventlog_events
    ADD CONSTRAINT eventlog_events_source_scope_id_key UNIQUE (source, scope, id);

ALTER TABLE ONLY "__contentkit_schema__".eventlog_migrations
    ADD CONSTRAINT eventlog_migrations_pkey PRIMARY KEY (version);

ALTER TABLE ONLY "__contentkit_schema__".eventlog_pending
    ADD CONSTRAINT eventlog_pending_pkey PRIMARY KEY (ordinal);

ALTER TABLE ONLY "__contentkit_schema__".eventlog_streams
    ADD CONSTRAINT eventlog_streams_pkey PRIMARY KEY (source, scope);

CREATE INDEX content_media_allocations_folder ON "__contentkit_schema__".content_media_allocations USING btree (tenant_id, folder_prefix);

CREATE INDEX content_media_cleanup_due ON "__contentkit_schema__".content_media_cleanup USING btree (tenant_id, content_kind, due_at, folder_prefix);

CREATE INDEX content_media_commits_notify ON "__contentkit_schema__".content_media_commits USING btree (tenant_id, updated_at, operation_id) WHERE ((state = 'applied'::text) AND ((effects ->> 'notify'::text) = 'true'::text));

CREATE INDEX content_media_commits_pending ON "__contentkit_schema__".content_media_commits USING btree (tenant_id, updated_at, operation_id) WHERE (state = ANY (ARRAY['open'::text, 'prepared'::text, 'frozen'::text]));

CREATE UNIQUE INDEX content_media_commits_pending_folder ON "__contentkit_schema__".content_media_commits USING btree (tenant_id, folder_prefix) WHERE (state = ANY (ARRAY['open'::text, 'prepared'::text, 'frozen'::text]));

CREATE UNIQUE INDEX content_media_releases_pending_folder ON "__contentkit_schema__".content_media_releases USING btree (tenant_id, folder_prefix) WHERE (NOT applied);

CREATE INDEX content_media_reservations_owner ON "__contentkit_schema__".content_media_reservations USING btree (tenant_id, owner_id, expires_at);

CREATE INDEX eventlog_events_source_scope_event_type_position_idx ON "__contentkit_schema__".eventlog_events USING btree (source, scope, event_type, "position");

CREATE INDEX eventlog_events_source_scope_recorded_at_position_idx ON "__contentkit_schema__".eventlog_events USING btree (source, scope, recorded_at, "position");

CREATE INDEX eventlog_pending_transaction_id_idx ON "__contentkit_schema__".eventlog_pending USING btree (transaction_id);

CREATE CONSTRAINT TRIGGER eventlog_flush_at_commit AFTER INSERT ON "__contentkit_schema__".eventlog_pending DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION "__contentkit_schema__".eventlog_flush();

CREATE TRIGGER eventlog_immutable BEFORE UPDATE ON "__contentkit_schema__".eventlog_events FOR EACH ROW EXECUTE FUNCTION "__contentkit_schema__".eventlog_immutable();

ALTER TABLE ONLY "__contentkit_schema__".eventlog_events
    ADD CONSTRAINT eventlog_events_source_scope_fkey FOREIGN KEY (source, scope) REFERENCES "__contentkit_schema__".eventlog_streams(source, scope) ON DELETE CASCADE;


--

INSERT INTO "__contentkit_schema__".eventlog_migrations (version) VALUES (1);
$ckshared$, '"__contentkit_schema__"', quote_ident(current_schema()));
END
$ckguard$;

CREATE TABLE content_account_avatar_placement (
    tenant_id uuid NOT NULL,
    primary_app_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_account_avatar_placement_primary_app_id_check CHECK ((primary_app_id <> '00000000-0000-0000-0000-000000000000'::uuid))
);

CREATE TABLE content_account_bindings (
    account_namespace text NOT NULL,
    tenant_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_account_bindings_account_namespace_check CHECK ((account_namespace <> ''::text)),
    CONSTRAINT content_account_bindings_tenant_id_check CHECK ((tenant_id <> '00000000-0000-0000-0000-000000000000'::uuid))
);

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
END)
);

CREATE TABLE content_account_erasure_receipts (
    tenant_id uuid NOT NULL,
    app_id uuid NOT NULL,
    actor_id text NOT NULL,
    completed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_account_erasure_receipts_actor_id_check CHECK ((actor_id <> ''::text)),
    CONSTRAINT content_account_erasure_receipts_app_id_check CHECK ((app_id <> '00000000-0000-0000-0000-000000000000'::uuid))
);

ALTER TABLE ONLY content_account_avatar_placement
    ADD CONSTRAINT content_account_avatar_placement_pkey PRIMARY KEY (tenant_id);

ALTER TABLE ONLY content_account_bindings
    ADD CONSTRAINT content_account_bindings_pkey PRIMARY KEY (account_namespace);

ALTER TABLE ONLY content_account_bindings
    ADD CONSTRAINT content_account_bindings_tenant_id_key UNIQUE (tenant_id);

ALTER TABLE ONLY content_account_erasure_intents
    ADD CONSTRAINT content_account_erasure_intents_pkey PRIMARY KEY (tenant_id, app_id, actor_id);

ALTER TABLE ONLY content_account_erasure_receipts
    ADD CONSTRAINT content_account_erasure_receipts_pkey PRIMARY KEY (tenant_id, app_id, actor_id);

ALTER TABLE ONLY content_account_avatar_placement
    ADD CONSTRAINT content_account_avatar_placement_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES content_account_bindings(tenant_id);

ALTER TABLE ONLY content_account_erasure_intents
    ADD CONSTRAINT content_account_erasure_intents_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES content_account_bindings(tenant_id);

ALTER TABLE ONLY content_account_erasure_receipts
    ADD CONSTRAINT content_account_erasure_receipts_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES content_account_bindings(tenant_id);
