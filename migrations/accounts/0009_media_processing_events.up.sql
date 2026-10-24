-- Shared tenant avatars keep one retained log beside their journal.
DO $install_eventlog$
BEGIN
    IF to_regclass(format('%I.eventlog_migrations', current_schema())) IS NOT NULL THEN RETURN; END IF;
    EXECUTE replace($shared_eventlog$
CREATE TABLE IF NOT EXISTS "__contentkit_eventlog_schema__".eventlog_migrations(version integer PRIMARY KEY);
CREATE TABLE "__contentkit_eventlog_schema__".eventlog_streams (
 source text NOT NULL, scope text NOT NULL,
 epoch text NOT NULL DEFAULT gen_random_uuid()::text,
 position bigint NOT NULL DEFAULT 0,
 head_id text NOT NULL DEFAULT '',
 pruned_through bigint NOT NULL DEFAULT 0,
 pruned_id text NOT NULL DEFAULT '',
 recorded_at timestamptz NOT NULL DEFAULT '-infinity',
 PRIMARY KEY(source,scope)
);
CREATE TABLE "__contentkit_eventlog_schema__".eventlog_events (
 source text NOT NULL, scope text NOT NULL, position bigint NOT NULL,
 id text NOT NULL, boundary text NOT NULL, event_source text NOT NULL, event_type text NOT NULL, version integer NOT NULL,
 occurred_at timestamptz NOT NULL, recorded_at timestamptz NOT NULL,
 subject_type text NOT NULL, subject_id text NOT NULL, data jsonb NOT NULL,
 PRIMARY KEY(source,scope,position), UNIQUE(source,scope,id),
 FOREIGN KEY(source,scope) REFERENCES "__contentkit_eventlog_schema__".eventlog_streams(source,scope) ON DELETE CASCADE
);
CREATE INDEX ON "__contentkit_eventlog_schema__".eventlog_events(source,scope,event_type,position);
CREATE INDEX ON "__contentkit_eventlog_schema__".eventlog_events(source,scope,recorded_at,position);
-- A queue row is part of the mutation transaction and is gone before commit.
-- The identity orders facts inside that transaction, never public cursors.
CREATE TABLE "__contentkit_eventlog_schema__".eventlog_pending (
 ordinal bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 transaction_id xid8 NOT NULL DEFAULT pg_current_xact_id(),
 source text NOT NULL CHECK(source<>''), scope text NOT NULL CHECK(scope<>''),
 id text NOT NULL CHECK(id<>''), boundary text, event_source text NOT NULL, event_type text NOT NULL CHECK(event_type<>''),
 version integer NOT NULL CHECK(version>0), occurred_at timestamptz NOT NULL,
 subject_type text NOT NULL DEFAULT '', subject_id text NOT NULL DEFAULT '', data jsonb NOT NULL
);
CREATE INDEX ON "__contentkit_eventlog_schema__".eventlog_pending(transaction_id);
CREATE FUNCTION "__contentkit_eventlog_schema__".eventlog_flush() RETURNS trigger LANGUAGE plpgsql SET search_path TO pg_catalog,pg_temp AS $fn$
DECLARE
 s record; e record; next_position bigint; stamp timestamptz;
BEGIN
 -- Every scope touched by this transaction is known before taking any locks.
 FOR s IN SELECT DISTINCT source,scope FROM "__contentkit_eventlog_schema__".eventlog_pending
          WHERE transaction_id=pg_current_xact_id() ORDER BY source,scope
 LOOP
  INSERT INTO "__contentkit_eventlog_schema__".eventlog_streams(source,scope) VALUES(s.source,s.scope) ON CONFLICT DO NOTHING;
  PERFORM 1 FROM "__contentkit_eventlog_schema__".eventlog_streams WHERE source=s.source AND scope=s.scope FOR UPDATE;
 END LOOP;
 FOR e IN SELECT * FROM "__contentkit_eventlog_schema__".eventlog_pending WHERE transaction_id=pg_current_xact_id() ORDER BY ordinal
 LOOP
  IF EXISTS(SELECT 1 FROM "__contentkit_eventlog_schema__".eventlog_events WHERE source=e.source AND scope=e.scope AND id=e.id) THEN
   IF EXISTS(SELECT 1 FROM "__contentkit_eventlog_schema__".eventlog_events WHERE source=e.source AND scope=e.scope AND id=e.id
     AND event_source=e.event_source AND event_type=e.event_type AND version=e.version
     AND occurred_at=e.occurred_at AND subject_type=e.subject_type AND subject_id=e.subject_id AND data=e.data) THEN
    CONTINUE;
   END IF;
   RAISE EXCEPTION 'event ID already records a different fact' USING ERRCODE='23505';
  END IF;
  UPDATE "__contentkit_eventlog_schema__".eventlog_streams SET position=position+1,head_id=coalesce(e.boundary,e.id),
   recorded_at=greatest(recorded_at,clock_timestamp())
   WHERE source=e.source AND scope=e.scope RETURNING position,recorded_at INTO next_position,stamp;
  INSERT INTO "__contentkit_eventlog_schema__".eventlog_events(source,scope,position,id,boundary,event_source,event_type,version,occurred_at,recorded_at,subject_type,subject_id,data)
   VALUES(e.source,e.scope,next_position,e.id,coalesce(e.boundary,e.id),e.event_source,e.event_type,e.version,e.occurred_at,stamp,e.subject_type,e.subject_id,e.data);
 END LOOP;
 DELETE FROM "__contentkit_eventlog_schema__".eventlog_pending WHERE transaction_id=pg_current_xact_id();
 RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER eventlog_flush_at_commit AFTER INSERT ON "__contentkit_eventlog_schema__".eventlog_pending
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION "__contentkit_eventlog_schema__".eventlog_flush();

CREATE FUNCTION "__contentkit_eventlog_schema__".eventlog_immutable() RETURNS trigger LANGUAGE plpgsql SET search_path TO pg_catalog,pg_temp AS $fn$
BEGIN
 RAISE EXCEPTION 'retained event facts are immutable';
END $fn$;
CREATE TRIGGER eventlog_immutable BEFORE UPDATE ON "__contentkit_eventlog_schema__".eventlog_events
 FOR EACH ROW EXECUTE FUNCTION "__contentkit_eventlog_schema__".eventlog_immutable();

INSERT INTO "__contentkit_eventlog_schema__".eventlog_migrations(version) VALUES(1);
$shared_eventlog$, '"__contentkit_eventlog_schema__"', quote_ident(current_schema()));
END
$install_eventlog$;
CREATE SEQUENCE IF NOT EXISTS content_event_ids;
-- Per-item observations outlive retained log rows, so restarting an old job
-- cannot append its completion again after event retention expires.
CREATE TABLE IF NOT EXISTS content_media_processing (
    tenant_id text NOT NULL,
    folder_prefix text NOT NULL,
    fingerprint bytea NOT NULL,
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, folder_prefix)
);


-- Existing account ownership claims also get stable empty-stream cursors.
INSERT INTO eventlog_streams(source,scope)
SELECT 'contentkit','tenant_' || tenant_id::text FROM content_account_bindings
ON CONFLICT DO NOTHING;
