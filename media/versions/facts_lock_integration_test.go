package versions

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
)

// factsAccessProjection implements the real Facets callback boundary with its
// two-statement access read/write. It deliberately pauses between statements;
// the editor below takes the same parent lock as catalog.Update.
type factsAccessProjection struct {
	items, facets string
	read          chan struct{}
	resume        chan struct{}
}

func (p *factsAccessProjection) RefreshTx(ctx context.Context, tx pgx.Tx, ids []string) error {
	var level string
	if err := tx.QueryRow(ctx, `SELECT access->>'level' FROM `+p.items+` WHERE tenant_id='facts' AND id='content'`).Scan(&level); err != nil {
		return err
	}
	close(p.read)
	select {
	case <-p.resume:
	case <-ctx.Done():
		return ctx.Err()
	}
	_, err := tx.Exec(ctx, `UPDATE `+p.facets+` SET access_level=$1 WHERE tenant_id='facts' AND version_id=ANY($2)`, level, ids)
	return err
}

func TestRecordFactsPreservesConcurrentAccessIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	items, versions, facets := pgxIdent(schema, "content_items"), pgxIdent(schema, "content_versions"), pgxIdent(schema, "content_facets")
	for _, sql := range []string{
		`INSERT INTO ` + items + ` (tenant_id,id,kind) VALUES ('facts','content','gallery')`,
		`INSERT INTO ` + versions + ` (tenant_id,id,content_id,languages) VALUES ('facts','version','content',ARRAY['en'])`,
		`INSERT INTO ` + facets + ` (tenant_id,content_id,version_id,kind,listing,languages,access_level) VALUES ('facts','content','version','gallery','listed',ARRAY['en'],'public')`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	projection := &factsAccessProjection{items: items, facets: facets, read: make(chan struct{}), resume: make(chan struct{})}
	s := &Service{o: Options{Tenant: "facts", Facets: projection}, t: tables{items: items, versions: versions}}
	job := make(chan error, 1)
	go func() {
		job <- pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			return s.recordFacts(ctx, tx, &media.Kind{}, Target{Content: "content", Version: "version"},
				&media.Manifest{Files: []media.File{{Path: "source.mp4", Type: "video/mp4", Blob: "video", W: 100, H: 200, Dur: 1}}})
		})
	}()
	select {
	case <-projection.read:
	case err := <-job:
		t.Fatalf("recordFacts did not refresh access: %v", err)
	case <-ctx.Done():
		t.Fatal("recordFacts did not reach its facet callback")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	pid := conn.Conn().PgConn().PID()
	writer := make(chan error, 1)
	go func() {
		writer <- pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE `+items+` SET access='{"level":"ppv"}' WHERE tenant_id='facts' AND id='content'`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE `+facets+` SET access_level='ppv' WHERE tenant_id='facts' AND version_id='version'`)
			return err
		})
	}()
	committed := false
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
waitWriter:
	for {
		select {
		case err := <-writer:
			if err != nil {
				t.Fatal(err)
			}
			committed = true
			break waitWriter
		case <-tick.C:
			var blocked bool
			if err := pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1::int)) > 0`, pid).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				break waitWriter
			}
		case <-ctx.Done():
			t.Fatal("editor neither committed nor blocked on the facts writer")
		}
	}
	close(projection.resume)
	if err := <-job; err != nil {
		t.Fatal(err)
	}
	if !committed {
		if err := <-writer; err != nil {
			t.Fatal(err)
		}
	}
	var source, projected string
	var width int
	if err := pool.QueryRow(ctx, `SELECT c.access->>'level', f.access_level, v.width FROM `+items+` c JOIN `+versions+` v USING (tenant_id)
 JOIN `+facets+` f ON f.tenant_id=v.tenant_id AND f.version_id=v.id WHERE c.tenant_id='facts' AND c.id='content'`).Scan(&source, &projected, &width); err != nil {
		t.Fatal(err)
	}
	if source != "ppv" || projected != source || width != 100 {
		t.Fatalf("facts refresh overwrote newer access: source=%s facet=%s width=%d", source, projected, width)
	}
}
