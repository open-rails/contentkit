package taxonomy

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

func TestImportedTimestampsValidateWithoutDatabase(t *testing.T) {
	s := &Store{}
	zero := time.Time{}
	valid := time.Date(2010, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, tc := range []struct {
		id               TaxonomyID
		created, updated *time.Time
	}{{tid("tag"), nil, nil}, {tid("tag"), &zero, nil}, {tid("tag"), nil, &zero}, {"", &valid, nil}} {
		if err := s.SetImportedTimestamps(context.Background(), tc.id, tc.created, tc.updated); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid input: %v", err)
		}
	}
}

func TestImportedTimestampsPreserveChronologyAndTransactions(t *testing.T) {
	ctx := context.Background()
	pool, schema, s := catalogFixture(t, ctx)
	other := newStore(t, pool, schema, "other", nil)
	inputs := []NodeInput{tag("import-a", "import-a", name("en", "A")), tag("import-b", "import-b", name("en", "B"))}
	for i := range inputs {
		inputs[i].SourceRevision = 17
	}
	mustCreate(t, ctx, s, inputs...)
	mustCreate(t, ctx, other, tag("foreign-only", "foreign-only", name("en", "Foreign")))
	created := time.Date(2001, 2, 3, 4, 5, 6, 123456000, time.FixedZone("source", 9*60*60))
	updated := time.Date(2000, 1, 2, 3, 4, 5, 654321000, time.FixedZone("source", -7*60*60))
	// Source clocks can run backwards; chronology is copied, not reinterpreted.
	for _, id := range []TaxonomyID{tid("import-a"), tid("import-b")} {
		if err := s.SetImportedTimestamps(ctx, id, &created, &updated); err != nil {
			t.Fatal(err)
		}
	}
	check := func(store *Store, id TaxonomyID, wantCreated, wantUpdated time.Time) {
		t.Helper()
		node, err := store.Node(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !node.CreatedAt.Equal(wantCreated) || !node.UpdatedAt.Equal(wantUpdated) || node.SourceRevision != 17 {
			t.Fatalf("imported chronology: %+v", node.Node)
		}
	}
	check(s, tid("import-a"), created, updated)
	changed := updated.Add(time.Hour)
	if err := s.SetImportedTimestamps(ctx, tid("import-a"), nil, &changed); err != nil {
		t.Fatal(err)
	}
	check(s, tid("import-a"), created, changed)
	if err := s.SetImportedTimestamps(ctx, tid("foreign-only"), &created, &updated); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign tenant lookup: %v", err)
	}
	if err := s.SetImportedTimestamps(ctx, tid("missing"), &created, &updated); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing lookup: %v", err)
	}
	foreign, err := other.Node(ctx, tid("foreign-only"))
	if err != nil || foreign.CreatedAt.Equal(created) {
		t.Fatalf("foreign node changed: %+v %v", foreign, err)
	}
	// Clear the queue so timestamp invalidation and its rollback are observable.
	if _, err := pool.Exec(ctx, fmt.Sprintf("DELETE FROM %s.content_search_dirty WHERE tenant_id=$1 AND content_id='"+string(tid("import-b"))+"'", schema), tenant); err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	borrowed := s.WithSQLTx(tx)
	if err := borrowed.SetImportedTimestamps(ctx, tid("import-b"), nil, &changed); err != nil {
		t.Fatal(err)
	}
	check(borrowed, tid("import-b"), created, changed)
	var dirty int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s.content_search_dirty WHERE tenant_id=$1 AND content_id='"+string(tid("import-b"))+"'", schema), tenant).Scan(&dirty); err != nil || dirty != 2 {
		t.Fatalf("import invalidation: %d %v", dirty, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	check(s, tid("import-b"), created, updated)
	if err := pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s.content_search_dirty WHERE tenant_id=$1 AND content_id='"+string(tid("import-b"))+"'", schema), tenant).Scan(&dirty); err != nil || dirty != 0 {
		t.Fatalf("rollback queue: %d %v", dirty, err)
	}
	// An ordinary edit resumes the normal transaction clock, preserving creation.
	slug := "ordinary-edit"
	before := time.Now().Add(-time.Second)
	node, err := s.UpdateNode(ctx, tid("import-a"), NodeUpdate{Slug: &slug})
	if err != nil {
		t.Fatal(err)
	}
	if !node.CreatedAt.Equal(created) || node.UpdatedAt.Before(before) {
		t.Fatalf("normal edit clock: %+v", node)
	}
}
