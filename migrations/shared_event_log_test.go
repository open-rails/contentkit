package migrations_test

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
	eventpg "github.com/open-rails/helpers/eventlog/postgres"
)

func TestSharedEventStorageContainsCanonicalHelpersMigration(t *testing.T) {
	body, err := fs.ReadFile(migrations.Postgres, "0033_shared_catalog_event_storage.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(body), "$shared_eventlog$")
	if len(parts) != 3 {
		t.Fatal("missing canonical shared migration segment")
	}
	want, err := eventpg.MigrationSQL("__contentkit_eventlog_schema__", 1)
	if err != nil {
		t.Fatal(err)
	}
	if parts[1] != "\n"+want {
		t.Fatal("shared SQL differs from the pinned Helpers version")
	}
}

func TestSharedEventStoragePreservesHistoryAndSequence(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, t.Context(), pool)
	q := pgx.Identifier{schema}.Sanitize()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`SET LOCAL search_path TO ` + q + `,public;
 CREATE TABLE content_events(tenant_id text NOT NULL,id bigint GENERATED ALWAYS AS IDENTITY,type text NOT NULL,entity_id text NOT NULL,payload jsonb NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),delivered_at timestamptz,PRIMARY KEY(tenant_id,id));
 CREATE INDEX content_events_undelivered_idx ON content_events(tenant_id,id) WHERE delivered_at IS NULL;
 INSERT INTO content_events(tenant_id,type,entity_id,payload,created_at) VALUES
 ('first','content.created','one','{"kind":"gallery","content_id":"one"}','2026-10-20T00:00:00Z'),
 ('first','content.updated','two','{"kind":"gallery","content_id":"two","unknown":{"preserve":true}}','2026-10-22T00:00:00Z'),
 ('first','version.purged','v3','{"kind":"gallery","content_id":"three","version_id":"v3"}','2026-10-21T00:00:00Z'),
 ('second','content.created','four','{"kind":"gallery","content_id":"four"}','2026-10-22T00:00:00Z');`)
	apply := func(name string) {
		t.Helper()
		body, err := fs.ReadFile(migrations.Postgres, name)
		if err != nil {
			t.Fatal(err)
		}
		exec(string(body))
	}
	apply("0032_catalog_event_log.up.sql")
	exec(`UPDATE content_event_streams SET retained_after=1,retained_boundary=(SELECT boundary FROM content_events WHERE tenant_id='first' AND position=1) WHERE tenant_id='first'; DELETE FROM content_events WHERE tenant_id='first' AND position=1; SELECT setval(pg_get_serial_sequence('content_events','id')::regclass,9007199254741993,true)`)
	snapshot := func(query string) string {
		t.Helper()
		var body string
		if err := tx.QueryRow(t.Context(), query).Scan(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	beforeEvents := snapshot(`SELECT jsonb_agg(to_jsonb(e) ORDER BY tenant_id,position)::text FROM content_events e`)
	beforeStreams := snapshot(`SELECT jsonb_agg(to_jsonb(s) ORDER BY tenant_id)::text FROM content_event_streams s`)
	beforeEpoch := snapshot(`SELECT epoch::text FROM content_event_epoch`)
	beforePurge := snapshot(`SELECT jsonb_agg(to_jsonb(h) ORDER BY tenant_id,event_id)::text FROM content_event_purge_handoff h`)
	apply("0033_shared_catalog_event_storage.up.sql")
	for name, pair := range map[string][2]string{
		"facts":   {beforeEvents, snapshot(`SELECT jsonb_agg(to_jsonb(e) ORDER BY tenant_id,position)::text FROM content_events e`)},
		"streams": {beforeStreams, snapshot(`SELECT jsonb_agg(to_jsonb(s) ORDER BY tenant_id)::text FROM content_event_streams s`)},
		"epoch":   {beforeEpoch, snapshot(`SELECT epoch::text FROM content_event_epoch`)},
		"purge":   {beforePurge, snapshot(`SELECT jsonb_agg(to_jsonb(h) ORDER BY tenant_id,event_id)::text FROM content_event_purge_handoff h`)},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("%s changed: before=%s after=%s", name, pair[0], pair[1])
		}
	}
	exec(`SAVEPOINT legacy_writer`)
	if _, err := tx.Exec(t.Context(), `UPDATE content_events SET created_at=now()`); err == nil {
		t.Fatal("legacy view accepted a writer")
	}
	exec(`ROLLBACK TO SAVEPOINT legacy_writer`)
	if got := snapshot(`SELECT nextval('content_event_ids')::text`); got != "9007199254741994" {
		t.Fatalf("pruned sequence high water lost: %s", got)
	}
	exec(`CREATE TEMP TABLE content_event_epoch(singleton boolean,epoch uuid); INSERT INTO pg_temp.content_event_epoch VALUES(true,gen_random_uuid())`)
	exec(`INSERT INTO eventlog_streams(source,scope) VALUES('contentkit','previously-empty')`)
	if got := snapshot(`SELECT epoch FROM eventlog_streams WHERE scope='previously-empty'`); got != beforeEpoch {
		t.Fatalf("empty cursor epoch changed or temp table shadowed it: %s != %s", got, beforeEpoch)
	}
	exec(`DROP TABLE pg_temp.content_event_epoch`)
	if got := snapshot(`SELECT count(*)::text FROM eventlog_events WHERE occurred_at<>recorded_at`); got != "0" {
		t.Fatal("historical timestamps changed")
	}
	// Existing ContentKit had no app-binding FK. Deleting one independent shared
	// scope cascades only its event rows; it never touches the other app or handoff.
	exec(`DELETE FROM eventlog_streams WHERE source='contentkit' AND scope='second'`)
	if got := snapshot(`SELECT count(*)::text FROM eventlog_events WHERE scope='first'`); got != "2" {
		t.Fatalf("other app lost records: %s", got)
	}
	if got := snapshot(`SELECT count(*)::text FROM eventlog_events WHERE scope='second'`); got != "0" {
		t.Fatalf("deleted scope retained events: %s", got)
	}
	if got := snapshot(`SELECT jsonb_agg(to_jsonb(h) ORDER BY tenant_id,event_id)::text FROM content_event_purge_handoff h`); got != beforePurge {
		t.Fatal("event removal changed purge obligations")
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestSharedAccountProcessingLogContainsCanonicalHelpersMigration(t *testing.T) {
	body, err := fs.ReadFile(migrations.Accounts, "0009_media_processing_events.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(body), "$shared_eventlog$")
	want, err := eventpg.MigrationSQL("__contentkit_eventlog_schema__", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 || parts[1] != "\n"+want {
		t.Fatal("shared account SQL differs from pinned Helpers migration")
	}
}
