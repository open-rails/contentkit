package workqueue

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	riverhelpers "github.com/open-rails/helpers/river"
)

func TestVersionImportHandoffIsAtomicAndKeepsOtherJobs(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	catalog := pgtest.Schema(t, ctx, pool)
	host := pgtest.EmptySchema(t, ctx, pool)
	worker := pgtest.EmptySchema(t, ctx, pool)
	for _, schema := range []string{host, worker} {
		if err := riverhelpers.ApplyMigrations(ctx, pool, schema); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := media.NewRegistry(media.Config{Namespace: "site", Kinds: []media.Kind{{Name: "book", Versions: true, Uploads: []media.Upload{{Path: "source", Types: []string{"image/png"}, MaxBytes: 1 << 20}}}}})
	if err != nil {
		t.Fatal(err)
	}
	queue, err := New(pool, reg, worker)
	if err != nil {
		t.Fatal(err)
	}
	c, v := contentref.NewID(), contentref.NewID()
	cq := pgx.Identifier{catalog}.Sanitize()
	jobs := pgx.Identifier{host, "river_job"}.Sanitize()
	if _, err = pool.Exec(ctx, `INSERT INTO `+cq+`.content_items(tenant_id,id,kind) VALUES('site',$1,'book')`, c); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO `+cq+`.content_versions(tenant_id,id,content_id,languages) VALUES('site',$1,$2,'{en}')`, v, c); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"content": c, "version": v})
	insert := func(kind string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO `+jobs+`(kind,args,queue,max_attempts) VALUES($1,$2,'contentkit_versions',3) RETURNING id`, kind, args).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	target := insert("contentkit_version_import")
	unrelated := insert("contentkit_version_publish")
	if _, err = pool.Exec(ctx, `UPDATE `+jobs+` SET state='running' WHERE id=$1`, target); err != nil {
		t.Fatal(err)
	}
	if err = queue.AdoptVersionImports(ctx, host, catalog, "site"); err == nil || !strings.Contains(err.Error(), "stop old workers") {
		t.Fatalf("running old importer=%v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE `+jobs+` SET state='available' WHERE id=$1`, target); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = queue.AdoptVersionImports(ctx, host, catalog, "site"); err != nil {
			t.Fatal(err)
		}
	}
	var state, other string
	if err = pool.QueryRow(ctx, `SELECT (SELECT state FROM `+jobs+` WHERE id=$1),(SELECT state FROM `+jobs+` WHERE id=$2)`, target, unrelated).Scan(&state, &other); err != nil || state != "cancelled" || other != "available" {
		t.Fatalf("handoff states=%q/%q err=%v", state, other, err)
	}
	var count int
	var stored []byte
	if err = pool.QueryRow(ctx, `SELECT count(*),min(args::text)::jsonb FROM `+pgx.Identifier{worker, "river_job"}.Sanitize()+` WHERE kind='contentkit_media_ingest'`).Scan(&count, &stored); err != nil || count != 1 {
		t.Fatalf("ingest count=%d err=%v", count, err)
	}
	var actual IngestArgs
	if err = json.Unmarshal(stored, &actual); err != nil {
		t.Fatal(err)
	}
	if actual.Ref.ContentID != c || actual.Ref.Version() != v || actual.Ref.ContentKind != "book" {
		t.Fatalf("handoff ref=%+v", actual.Ref)
	}
}
