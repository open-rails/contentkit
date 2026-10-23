-- Writers must be stopped for this coordinated storage migration. SQL0032 is
-- immutable; its purge handoff remains owned by the existing domain migration.
LOCK TABLE content_event_epoch, content_event_streams, content_events IN ACCESS EXCLUSIVE MODE;

-- BEGIN HELPERS EVENTLOG V1
DO $install_eventlog$
BEGIN
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
-- END HELPERS EVENTLOG V1

-- Retain the epoch even for old empty cursors issued before an app's first fact.
-- The shared deferred flush initializes new streams in its sorted lock order.
DO $epoch_default$
BEGIN
    EXECUTE format('CREATE FUNCTION %I.content_event_legacy_epoch() RETURNS text LANGUAGE sql STABLE SET search_path TO pg_catalog,pg_temp AS %L',
        current_schema(), format('SELECT epoch::text FROM %I.content_event_epoch WHERE singleton', current_schema()));
    EXECUTE format('ALTER TABLE %I.eventlog_streams ALTER COLUMN epoch SET DEFAULT %I.content_event_legacy_epoch()', current_schema(), current_schema());
END
$epoch_default$;

DO $validate_history$
BEGIN
    IF (SELECT count(*) FROM content_event_epoch WHERE singleton) <> 1
       OR EXISTS (SELECT 1 FROM content_event_streams s WHERE
          (s.retained_after > 0 AND s.retained_boundary IS NULL)
          OR (s.position > s.retained_after AND NOT EXISTS (
             SELECT 1 FROM content_events e WHERE e.tenant_id=s.tenant_id AND e.position=s.position)))
       OR EXISTS (SELECT 1 FROM content_events e LEFT JOIN content_event_streams s USING (tenant_id)
          WHERE s.tenant_id IS NULL OR e.position>s.position
             OR e.entity_id IS DISTINCT FROM coalesce(nullif(e.payload->>'version_id',''),e.payload->>'content_id')
             OR nullif(e.payload->>'kind','') IS NULL OR nullif(e.payload->>'content_id','') IS NULL)
    THEN
        RAISE EXCEPTION 'contentkit: event history cannot be migrated without changing its identity or boundary';
    END IF;
END
$validate_history$;

INSERT INTO eventlog_streams(source,scope,epoch,position,head_id,pruned_through,pruned_id,recorded_at)
SELECT 'contentkit',s.tenant_id,x.epoch::text,s.position,
       CASE WHEN s.position>s.retained_after THEN e.boundary::text ELSE coalesce(s.retained_boundary::text,'') END,
       s.retained_after,coalesce(s.retained_boundary::text,''),
       coalesce((SELECT max(created_at) FROM content_events WHERE tenant_id=s.tenant_id),'-infinity'::timestamptz)
FROM content_event_streams s CROSS JOIN content_event_epoch x
LEFT JOIN content_events e ON e.tenant_id=s.tenant_id AND e.position=s.position
WHERE x.singleton;

INSERT INTO eventlog_events(source,scope,position,id,boundary,event_source,event_type,version,
                           occurred_at,recorded_at,subject_type,subject_id,data)
SELECT 'contentkit',tenant_id,position,id::text,boundary::text,'contentkit',type,event_version,
       created_at,created_at,payload->>'kind',payload->>'content_id',payload
FROM content_events;

-- Keep the allocator's high water even when the highest facts were pruned.
CREATE SEQUENCE content_event_ids AS bigint;
DO $preserve_ids$
DECLARE old_sequence text; high_water bigint; was_called boolean; retained_max bigint;
BEGIN
    old_sequence := pg_get_serial_sequence('content_events','id');
    IF old_sequence IS NULL THEN RAISE EXCEPTION 'contentkit: missing legacy event ID sequence'; END IF;
    EXECUTE format('SELECT last_value,is_called FROM %s',old_sequence) INTO high_water,was_called;
    SELECT max(id) INTO retained_max FROM content_events;
    IF retained_max IS NOT NULL AND retained_max >= high_water THEN
        high_water := retained_max;
        was_called := true;
    END IF;
    PERFORM setval('content_event_ids'::regclass,high_water,was_called);
END
$preserve_ids$;

DROP TABLE content_events;
DROP TABLE content_event_streams;

-- Read compatibility only. New producers enqueue into eventlog_pending; old
-- ON CONFLICT stream writers fail instead of creating a second source log.
CREATE VIEW content_events AS
SELECT scope AS tenant_id,id::bigint AS id,event_type AS type,
       coalesce(nullif(data->>'version_id',''),data->>'content_id') AS entity_id,
       data AS payload,recorded_at AS created_at,position,boundary::uuid AS boundary,
       version AS event_version
FROM eventlog_events WHERE source='contentkit';
CREATE VIEW content_event_streams AS
SELECT scope AS tenant_id,position,pruned_through AS retained_after,
       nullif(pruned_id,'')::uuid AS retained_boundary
FROM eventlog_streams WHERE source='contentkit';

CREATE FUNCTION content_event_legacy_readonly() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'contentkit: legacy event views are read-only; stop old writers before migration';
END $$;
CREATE TRIGGER content_events_readonly INSTEAD OF INSERT OR UPDATE OR DELETE ON content_events
FOR EACH ROW EXECUTE FUNCTION content_event_legacy_readonly();
CREATE TRIGGER content_event_streams_readonly INSTEAD OF INSERT OR UPDATE OR DELETE ON content_event_streams
FOR EACH ROW EXECUTE FUNCTION content_event_legacy_readonly();
