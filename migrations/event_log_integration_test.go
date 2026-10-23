package migrations_test

import (
	"io/fs"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
)

func TestCatalogEventLogMigrationPreservesFactsAndPurgeObligations(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, t.Context(), pool)
	q := pgx.Identifier{schema}.Sanitize()
	_, err := pool.Exec(t.Context(), `CREATE TABLE `+q+`.content_events(tenant_id text NOT NULL,id bigint GENERATED ALWAYS AS IDENTITY,type text NOT NULL,entity_id text NOT NULL,payload jsonb NOT NULL DEFAULT '{}',created_at timestamptz NOT NULL DEFAULT now(),delivered_at timestamptz,PRIMARY KEY(tenant_id,id)); CREATE INDEX content_events_undelivered_idx ON `+q+`.content_events(tenant_id,id) WHERE delivered_at IS NULL;
INSERT INTO `+q+`.content_events(tenant_id,type,entity_id,payload,delivered_at) VALUES
('first','content.purged','one','{"kind":"gallery","content_id":"one"}',NULL),
('second','content.updated','two','{"kind":"gallery","content_id":"two"}',NULL),
('first','version.purged','version','{"kind":"gallery","content_id":"parent","version_id":"version"}',now()),
('first','content.updated','three','{"kind":"gallery","content_id":"three"}',now());`)
	if err != nil {
		t.Fatal(err)
	}
	body, err := fs.ReadFile(migrations.Postgres, "0032_catalog_event_log.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SET LOCAL search_path=`+q+`,public`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), string(body)); err != nil {
		t.Fatal(err)
	}
	var count, handoff, firstHead, secondHead int
	if err := tx.QueryRow(t.Context(), `SELECT count(*),(SELECT count(*) FROM content_event_purge_handoff),(SELECT position FROM content_event_streams WHERE tenant_id='first'),(SELECT position FROM content_event_streams WHERE tenant_id='second') FROM content_events`).Scan(&count, &handoff, &firstHead, &secondHead); err != nil {
		t.Fatal(err)
	}
	if count != 4 || handoff != 2 || firstHead != 3 || secondHead != 1 {
		t.Fatalf("count=%d handoff=%d app heads=%d/%d", count, handoff, firstHead, secondHead)
	}
	var positions []int64
	rows, err := tx.Query(t.Context(), `SELECT position FROM content_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	positions, err = pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || len(positions) != 4 || positions[0] != 1 || positions[1] != 1 || positions[2] != 2 || positions[3] != 3 {
		t.Fatalf("legacy ordering %v: %v", positions, err)
	}
	var oldFlag bool
	if err := tx.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=$1 AND table_name='content_events' AND column_name='delivered_at')`, schema).Scan(&oldFlag); err != nil || oldFlag {
		t.Fatalf("delivery flag remains: %v %v", oldFlag, err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}
