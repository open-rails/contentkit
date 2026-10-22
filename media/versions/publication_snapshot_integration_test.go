package versions

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/internal/pgtest"
)

// A withdrawal after the query finishes cannot split its publication snapshot.
func TestPublicationReadSurvivesWithdrawal(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	s := &Service{o: Options{Tenant: "snapshot", Pool: pool}, t: tables{
		publications: pgxIdent(schema, "content_version_publications"),
	}}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, tenant := range []string{"snapshot", "other"} {
		exec(`INSERT INTO `+pgxIdent(schema, "content_items")+` (tenant_id, id, kind) VALUES ($1, 'content', 'gallery')`, tenant)
		exec(`INSERT INTO `+pgxIdent(schema, "content_versions")+` (tenant_id, id, content_id, languages) VALUES ($1, 'version', 'content', ARRAY['en'])`, tenant)
		exec(`INSERT INTO `+s.t.publications+` (tenant_id, id, version_id, kind, state, source)
 VALUES ($1, 'publication', 'version', 'pages', 'current', '{"pages":[]}')`, tenant)

	}
	q := &withdrawingQuery{Pool: pool, withdraw: func() {
		exec(`DELETE FROM ` + s.t.publications + ` WHERE tenant_id = 'snapshot' AND id = 'publication'`)
	}}
	p, err := s.current(ctx, q, Target{Content: "content", Version: "version"}, kindPages)
	if err != nil || p == nil || p.ID != "publication" {
		t.Fatalf("withdrawal split the reader snapshot: %+v %v", p, err)
	}

	if p, err := s.current(ctx, pool, Target{Content: "content", Version: "version"}, kindPages); err != nil || p != nil {
		t.Fatalf("withdrawn publication is current: %+v %v", p, err)
	}
	if _, err := s.loadPub(ctx, pool, "publication"); !errors.Is(err, errRetired) {
		t.Fatalf("retired build lookup: %v", err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+s.t.publications+` WHERE tenant_id = 'other'`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("another tenant changed: %d %v", remaining, err)
	}
}

type withdrawingQuery struct {
	*pgxpool.Pool
	once     sync.Once
	withdraw func()
}

func (q *withdrawingQuery) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return withdrawingRow{Row: q.Pool.QueryRow(ctx, sql, args...), after: func() { q.once.Do(q.withdraw) }}
}

type withdrawingRow struct {
	pgx.Row
	after func()
}

func (r withdrawingRow) Scan(dest ...any) error {
	if err := r.Row.Scan(dest...); err != nil {
		return err
	}
	r.after()
	return nil
}
