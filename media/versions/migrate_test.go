package versions

import (
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	riverhelpers "github.com/open-rails/helpers/river"
	"testing"
)

func TestReaderMigrationRetiresRecoveryAndQueuesMissingSnapshot(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	riverSchema := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, riverSchema); err != nil {
		t.Fatal(err)
	}
	reg, err := media.NewRegistry(media.Config{Namespace: "site", Kinds: []media.Kind{{Name: "gallery", Versions: true,
		Uploads: []media.Upload{{Path: "pages/{name}", Types: []string{"image/png"}, MaxBytes: 1024}},
		Private: []media.Private{{Name: "low", From: "pages/{name}", To: "low/{name}.webp", Image: &media.Image{Width: 100}}},
		Publish: &media.Publish{Pages: "pages/{name}", Renditions: []string{"low"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	q, rt := pgx.Identifier{schema}.Sanitize(), pgx.Identifier{riverSchema, "river_job"}.Sanitize()
	content, version := contentref.NewID(), contentref.NewID()
	if _, err := pool.Exec(ctx, `INSERT INTO `+q+`.content_items(tenant_id,id,kind) VALUES('site',$1,'gallery')`, content); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO `+q+`.content_versions(tenant_id,id,content_id,pages,languages) VALUES('site',$1,$2,1,'{en}')`, version, content); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO `+rt+`(kind,args,queue,max_attempts,state) VALUES
('contentkit_version_publish_recovery','{}','contentkit_versions',3,'scheduled'),
('contentkit_version_publish_recovery','{}','contentkit_versions',3,'running')`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateReaderSnapshots(ctx, pool, schema, "site", riverSchema, reg); err == nil {
		t.Fatal("migration consumed a running old recovery")
	}
	var cancelled int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+rt+` WHERE state='cancelled'`).Scan(&cancelled); err != nil || cancelled != 0 {
		t.Fatalf("failed migration partially cancelled work: %d %v", cancelled, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE `+rt+` SET state='completed',finalized_at=now() WHERE state='running'`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := MigrateReaderSnapshots(ctx, pool, schema, "site", riverSchema, reg); err != nil {
			t.Fatal(err)
		}
	}
	var queued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+rt+` WHERE kind='contentkit_version_publish' AND args->>'version'=$1`, version).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("missing snapshot repair was lost or duplicated: %d %v", queued, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+rt+` WHERE kind='contentkit_version_publish_recovery' AND state='cancelled'`).Scan(&cancelled); err != nil || cancelled != 1 {
		t.Fatalf("queued old recovery not retired: %d %v", cancelled, err)
	}
}
