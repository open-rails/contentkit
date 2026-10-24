package migrations_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/migratekit"
)

func TestRemovePublishingPreservesDraftsAndRefusesUnfinishedWork(t *testing.T) {
	for _, tt := range []struct {
		state                 string
		commit, match, reject bool
	}{
		{state: "processing", reject: true}, {state: "publishing", reject: true}, {state: "review_required", reject: true},
		{state: "awaiting_upload"}, {state: "awaiting_upload", commit: true, reject: true},
		{state: "scheduled", reject: true}, {state: "scheduled", match: true}, {state: "ready"}, {state: "published"}, {state: "failed"},
	} {
		t.Run(tt.state+map[bool]string{true: "-active"}[tt.commit]+map[bool]string{true: "-matched"}[tt.match], func(t *testing.T) {
			ctx := t.Context()
			pool := pgtest.Pool(t, nil)
			schema := pgtest.EmptySchema(t, ctx, pool)
			pgtest.EnsureExtensions(t, ctx, pool)
			db := stdlib.OpenDBFromPool(pool)
			defer db.Close()
			steps, err := migratekit.Load(migrations.Postgres, ".")
			if err != nil {
				t.Fatal(err)
			}
			at := -1
			for i, s := range steps {
				if s.Name == "0041_remove_publishing_operations.up.sql" {
					at = i
					break
				}
			}
			if at < 0 {
				t.Fatal("missing removal")
			}
			runner := migratekit.NewPostgres(db, "contentkit").WithSchema(schema)
			if err = runner.ApplyMigrations(ctx, steps[:at]); err != nil {
				t.Fatal(err)
			}
			q := pgx.Identifier{schema}.Sanitize()
			if _, err = pool.Exec(ctx, `INSERT INTO `+q+`.content_items(tenant_id,id,kind,state,publish_at) VALUES('tenant','c','post',CASE WHEN $1 THEN 'published' ELSE 'draft' END,'2030-01-01')`, tt.match); err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, `INSERT INTO `+q+`.content_versions(tenant_id,id,content_id,languages) VALUES('tenant','v','c','{en}')`); err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, `INSERT INTO `+q+`.content_publishing_operations(tenant_id,id,owner_key,request_hash,request,content_id,version_id,content_revision,version_revision,commit_id,state,commit_started,publish_at)
 VALUES('tenant',$1,'owner',repeat('a',64),'{}','c','v',1,1,$2,$3,$4,'2030-01-01')`, uuid.NewString(), uuid.NewString(), tt.state, tt.commit); err != nil {
				t.Fatal(err)
			}
			err = runner.ApplyMigrations(ctx, steps[:at+1])
			if tt.reject && (err == nil || !strings.Contains(err.Error(), "ContentKit publishing removal:")) {
				t.Fatalf("unfinished submission=%v", err)
			}
			if !tt.reject && err != nil {
				t.Fatal(err)
			}
			var exists bool
			if err = pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+".content_publishing_operations").Scan(&exists); err != nil || exists != tt.reject {
				t.Fatalf("state table exists=%v err=%v", exists, err)
			}
			var content, version int
			if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM `+q+`.content_items WHERE id='c'),(SELECT count(*) FROM `+q+`.content_versions WHERE id='v')`).Scan(&content, &version); err != nil || content != 1 || version != 1 {
				t.Fatalf("content lost: %d %d %v", content, version, err)
			}
		})
	}
}
