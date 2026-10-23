package catalog

import (
	"io/fs"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/helpers/eventlog"
)

func TestSharedStorageResumesLegacyAndPreviouslyEmptyCursors(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, t.Context(), pool)
	q := pgx.Identifier{schema}.Sanitize()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	exec := func(sql string) {
		t.Helper()
		if _, err := tx.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
	exec(`SET LOCAL search_path TO ` + q + `,public;
 CREATE TABLE content_events(tenant_id text NOT NULL,id bigint GENERATED ALWAYS AS IDENTITY,type text NOT NULL,entity_id text NOT NULL,payload jsonb NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),delivered_at timestamptz,PRIMARY KEY(tenant_id,id));
 CREATE INDEX content_events_undelivered_idx ON content_events(tenant_id,id) WHERE delivered_at IS NULL;
 SELECT setval(pg_get_serial_sequence('content_events','id')::regclass,9007199254740992,true);
 INSERT INTO content_events(tenant_id,type,entity_id,payload) VALUES
 ('legacy','content.created','one','{"kind":"gallery","content_id":"one"}'),
 ('legacy','content.updated','two','{"kind":"gallery","content_id":"two","unknown":42}');`)
	apply := func(name string) {
		t.Helper()
		body, err := fs.ReadFile(migrations.Postgres, name)
		if err != nil {
			t.Fatal(err)
		}
		exec(string(body))
	}
	apply("0032_catalog_event_log.up.sql")
	var epoch, boundary string
	if err := tx.QueryRow(t.Context(), `SELECT epoch::text,(SELECT boundary::text FROM content_events WHERE position=1) FROM content_event_epoch`).Scan(&epoch, &boundary); err != nil {
		t.Fatal(err)
	}
	legacy := encodeEventCursor(eventCursor{Version: 1, Scope: "legacy", Epoch: epoch, Position: "1", Boundary: boundary, Types: []string{EventContentUpdated}})
	empty := encodeEventCursor(eventCursor{Version: 1, Scope: "previously-empty", Epoch: epoch, Position: "0"})
	apply("0033_shared_catalog_event_storage.up.sql")
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	store := &Store{pool: pool, schema: schema, qs: q, tenant: "legacy", eventRetention: DefaultEventRetention, now: time.Now}
	page, err := store.EventReader().ListEvents(t.Context(), eventlog.Query{After: legacy, Types: []string{EventContentUpdated}})
	if err != nil || len(page.Events) != 1 || page.Events[0].ID != "9007199254740994" {
		t.Fatalf("legacy cursor: %+v %v", page, err)
	}
	if !page.Events[0].OccurredAt.Equal(page.Events[0].RecordedAt) {
		t.Fatal("historical times changed")
	}
	store.tenant = "previously-empty"
	page, err = store.EventReader().ListEvents(t.Context(), eventlog.Query{After: empty})
	if err != nil || len(page.Events) != 0 || page.NextCursor != empty {
		t.Fatalf("empty cursor: %+v %v", page, err)
	}
	var scopes int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM `+q+`.eventlog_streams WHERE scope='previously-empty'`).Scan(&scopes); err != nil || scopes != 0 {
		t.Fatalf("read created a stream: %d %v", scopes, err)
	}
	mutation, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer mutation.Rollback(t.Context())
	if err := store.emit(t.Context(), mutation, Event{Type: EventContentCreated, Kind: "gallery", ContentID: "new"}); err != nil {
		t.Fatal(err)
	}
	if err := mutation.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	page, err = store.EventReader().ListEvents(t.Context(), eventlog.Query{After: empty})
	if err != nil || len(page.Events) != 1 || page.Events[0].ID != "9007199254740995" {
		t.Fatalf("old empty cursor after append: %+v %v", page, err)
	}
	cursor, _, err := decodeEventCursor(page.NextCursor)
	if err != nil || cursor.Epoch != epoch || cursor.Boundary == page.Events[0].ID {
		t.Fatalf("new stream identity: %+v %v", cursor, err)
	}
}
