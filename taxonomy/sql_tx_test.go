package taxonomy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestSQLTxBorrowsAtomicCatalogTransaction(t *testing.T) {
	ctx := context.Background()
	pool, schema, s := catalogFixture(t, ctx)
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	borrowed := s.WithSQLTx(tx)
	if _, err := tx.ExecContext(ctx, "SAVEPOINT host_batch"); err != nil {
		t.Fatal(err)
	}
	nodes, err := borrowed.CreateNodes(ctx, []NodeInput{tag("sql-node", "sql-node", name("en", "SQL node"))})
	if err != nil || len(nodes) != 1 {
		t.Fatalf("create: %v %v", nodes, err)
	}
	if err := borrowed.Assign(ctx, []Assignment{assign(work(tenant, "gallery", "g1"), "sql-node", "")}, AssignOptions{}); err != nil {
		t.Fatal(err)
	}
	page, err := borrowed.ListNodes(ctx, ListOptions{Kind: "tag", Language: "en"})
	if err != nil || len(page.Nodes) == 0 {
		t.Fatalf("named args query: %+v %v", page, err)
	}
	if _, err := borrowed.Node(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no rows sentinel: %v", err)
	}
	// UUID and native PostgreSQL array arguments remain supported by pgx/stdlib.
	q := sqlQuerier{tx: tx}
	id := uuid.New()
	var gotID uuid.UUID
	var gotArray []string
	if err := q.QueryRow(ctx, "SELECT $1::uuid,$2::text[]", id, []string{"one", "two"}).Scan(&gotID, &gotArray); err != nil || gotID != id || !reflect.DeepEqual(gotArray, []string{"one", "two"}) {
		t.Fatalf("bindings: %v %v %v", gotID, gotArray, err)
	}
	report, err := borrowed.Merge(ctx, "sql-node", "alpha")
	if err != nil || report.AssignmentsMoved+report.AssignmentsMerged != 1 {
		t.Fatalf("merge affected rows: %+v %v", report, err)
	}
	if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT host_batch"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"content_nodes", "content_assignments", "content_node_counts"} {
		var n int
		if err := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s.%s WHERE taxonomy_id='sql-node'", schema, table)).Scan(&n); err != nil || n != 0 {
			t.Fatalf("rollback %s: %d %v", table, n, err)
		}
	}
	var dirty int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s.content_search_dirty WHERE content_id='sql-node'", schema)).Scan(&dirty); err != nil || dirty != 0 {
		t.Fatalf("dirty rollback: %d %v", dirty, err)
	}
	// The library never finalized the borrowed transaction; the host can write
	// more and commit after rolling back only its failed batch.
	if _, err := borrowed.CreateNodes(ctx, []NodeInput{tag("committed", "committed", name("en", "Committed"))}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Node(ctx, "committed"); err != nil {
		t.Fatal(err)
	}
	tx2, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WithSQLTx(tx2).CreateNodes(ctx, []NodeInput{tag("rolled-back", "rolled-back", name("en", "Rolled back"))}); err != nil {
		t.Fatal(err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Node(ctx, "rolled-back"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("host rollback: %v", err)
	}
	if _, err := s.WithSQLTx(nil).Node(ctx, "alpha"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil transaction must fail closed: %v", err)
	}
}
