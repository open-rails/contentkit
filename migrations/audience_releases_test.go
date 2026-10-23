package migrations_test

import (
	"io/fs"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
)

func TestAudienceReleaseMigrationPreservesLegacyRights(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, t.Context(), pool)
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
	exec(`SET LOCAL search_path TO ` + pgx.Identifier{schema}.Sanitize() + `,public;
 CREATE TABLE content_items(tenant_id text,id text,access jsonb,PRIMARY KEY(tenant_id,id));
 CREATE TABLE content_versions(tenant_id text,id text,content_id text,access jsonb);
 CREATE TABLE content_groups(access jsonb);
 CREATE TABLE content_access_history(snapshot jsonb);
 CREATE TABLE content_facets(tenant_id text,version_id text,access_free_after timestamptz,free_applied boolean,live_from timestamptz,deleted boolean default false,unplayable boolean default false);
 CREATE INDEX content_facets_free_idx ON content_facets(access_free_after);
 INSERT INTO content_items VALUES('app','one','{"level":"tier","tiers":["premium"],"free_after":"2027-01-01T00:00:00+00:00"}');
 INSERT INTO content_versions VALUES('app','v1','one',null),('app','v2','one','{"level":"ppv","free_after":"2027-02-01T00:00:00Z"}');
 INSERT INTO content_groups VALUES('{"level":"group","group_id":"g","free_after":"2027-03-01T00:00:00Z"}');
 INSERT INTO content_access_history VALUES('{"level":"tier","tiers":["old"],"free_after":"2026-01-01T00:00:00Z","live_from":"2025-01-01T00:00:00Z"}');
 INSERT INTO content_facets VALUES('app','v1','2027-01-01',false,'2026-01-01',false,false),('app','v2','2027-02-01',true,'2026-01-01',false,false);`)
	body, err := fs.ReadFile(migrations.Postgres, "0034_audience_releases.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(body))
	var good bool
	err = tx.QueryRow(t.Context(), `SELECT
 (SELECT access->>'level'='tier' AND access->'tiers'='["premium"]'::jsonb AND NOT access?'free_after' AND access->'releases'->0->>'level'='public' FROM content_items)
 AND (SELECT access->'releases'->0->>'at'='2027-02-01T00:00:00Z' FROM content_versions WHERE id='v2')
 AND (SELECT access->>'group_id'='g' AND access->'releases'->0->>'level'='public' FROM content_groups)
 AND (SELECT snapshot->'tiers'='["old"]'::jsonb AND snapshot->>'live_from'='2025-01-01T00:00:00Z' AND snapshot->'releases'->0->>'at'='2026-01-01T00:00:00Z' FROM content_access_history)
 AND (SELECT bool_and(jsonb_array_length(access_releases)=1) FROM content_facets)
 AND content_release_key('{"at":"2027-01-01T00:00:00Z","level":"tier","tiers":["b","a"]}')=content_release_key('{"level":"tier","tiers":["a","b"],"at":"2026-12-31T19:00:00-05:00"}')`).Scan(&good)
	if err != nil || !good {
		t.Fatalf("migration changed rights or release identity: %t %v", good, err)
	}
}
