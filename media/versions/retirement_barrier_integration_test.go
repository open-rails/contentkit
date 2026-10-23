package versions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

func TestRetirementWaitsForConcurrentPublisher(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	s := &Service{o: Options{Tenant: "retirement", Pool: pool}, t: tables{items: pgxIdent(schema, "content_items"), versions: pgxIdent(schema, "content_versions"), publications: pgxIdent(schema, "content_version_publications")}}
	for _, sql := range []string{
		`INSERT INTO ` + s.t.items + `(tenant_id,id,kind) VALUES('retirement','content','gallery')`,
		`INSERT INTO ` + s.t.versions + `(tenant_id,id,content_id,languages) VALUES('retirement','version','content',ARRAY['en'])`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	v := Target{Content: "content", Version: "version"}
	current := &media.Manifest{Files: []media.File{{Path: "originals/001.png", Blob: "original", Type: "image/png"}, {Path: "low/001.webp", Blob: "new-render", From: "originals/001.png", Preset: "low", Type: "image/webp"}}}
	source := current.Files[0].Key()
	body, _ := json.Marshal(pagesSnapshot{Pages: []page{{Path: "originals/001.png", Source: source, Files: []rendition{{Blob: "old-render"}}}}})
	publisher, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Rollback(context.Background())
	if err := s.lockVersion(ctx, publisher, v); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Exec(ctx, `INSERT INTO `+s.t.publications+`(tenant_id,id,version_id,kind,state,source) VALUES('retirement','publication','version','pages','current',$1)`, body); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	pid := conn.Conn().PgConn().PID()
	done := make(chan error, 1)
	go func() {
		done <- pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if err := s.lockTarget(ctx, tx, v); err != nil {
				return err
			}
			_, err := s.withdrawStaleTx(ctx, tx, &media.Kind{}, v, current)
			return err
		})
	}()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
waitLock:
	for {
		select {
		case err := <-done:
			t.Fatalf("retirement skipped publisher lock: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
			var blocked bool
			if err := pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1::int))>0`, pid).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				break waitLock
			}
		}
	}
	if err := publisher.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if p, err := s.current(ctx, pool, v, kindPages); err != nil || p != nil {
		t.Fatalf("retired rendition remained published: %+v %v", p, err)
	}
}

func TestRetirementBarrierRequiresExactScopedBinding(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	ctx := t.Context()
	schema := pgtest.Schema(t, ctx, pool)
	scope := layout.Scope{TenantID: contentref.NewID(), AppID: contentref.NewID()}
	reg, err := media.NewRegistry(media.Config{Scope: scope, Kinds: []media.Kind{{Name: "gallery", Versions: true, KeepOriginals: true, Uploads: []media.Upload{{Path: "originals/{name}", Types: []string{"image/png"}, MaxBytes: 1024}}, Private: []media.Private{{Name: "low", From: "originals/{name}", To: "low/{name}.webp", Image: &media.Image{Width: 10}}}, Publish: &media.Publish{Pages: "originals/{name}", Renditions: []string{"low"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	cid, vid := contentref.NewID(), contentref.NewID()
	ref, err := reg.VersionRef("gallery", cid, vid)
	if err != nil {
		t.Fatal(err)
	}
	m := &media.Manifest{Files: []media.File{{Path: "originals/001.png", Blob: "original", Type: "image/png"}}}
	source, _ := json.Marshal(pagesSnapshot{Pages: []page{{Path: "originals/001.png", Source: m.Files[0].Key(), Files: []rendition{{Blob: "old-render"}}}}})
	for _, tenant := range []string{"catalog", "unrelated"} {
		for _, q := range []string{
			`INSERT INTO ` + pgxIdent(schema, "content_items") + `(tenant_id,id,kind) VALUES($1,$2,'gallery')`,
			`INSERT INTO ` + pgxIdent(schema, "content_versions") + `(tenant_id,id,content_id,languages) VALUES($1,$3,$2,ARRAY['en'])`,
			`INSERT INTO ` + pgxIdent(schema, "content_version_publications") + `(tenant_id,id,version_id,kind,state,source) VALUES($1,$2,$3,'pages','current',$4)`,
		} {
			var args []any
			switch {
			case strings.Contains(q, "content_items"):
				args = []any{tenant, cid}
			case strings.Contains(q, "content_versions"):
				args = []any{tenant, cid, vid}
			default:
				args = []any{tenant, cid, vid, source}
			}
			if _, err := pool.Exec(ctx, q, args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	barrier := RetirementBarrier(pool, schema, reg)
	if err := barrier(ctx, ref, m); err == nil {
		t.Fatal("missing catalog binding was accepted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO `+pgxIdent(schema, "content_app_bindings")+`(tenant_id,app_id,catalog_key) VALUES($1,$2,'catalog')`, scope.TenantID, scope.AppID); err != nil {
		t.Fatal(err)
	}
	foreign := ref
	foreign.TenantID = layout.Scope{TenantID: scope.TenantID, AppID: contentref.NewID()}.Partition()
	if err := barrier(ctx, foreign, m); err == nil {
		t.Fatal("foreign app reference was accepted")
	}
	if err := barrier(ctx, ref, m); err != nil {
		t.Fatal(err)
	}
	var own, other int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE tenant_id='catalog'),count(*) FILTER(WHERE tenant_id='unrelated') FROM `+pgxIdent(schema, "content_version_publications")).Scan(&own, &other); err != nil || own != 0 || other != 1 {
		t.Fatalf("wrong app affected: own=%d other=%d err=%v", own, other, err)
	}
}
