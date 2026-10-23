package entities

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/contenturl"
)

// hostReferences models a host-owned relation, using only the transaction
// supplied by ContentKit. Failure after its writes verifies atomic rollback.
type hostReferences struct {
	table     string
	kind      string
	failMerge bool
}

func (h *hostReferences) CheckUnavailable(ctx context.Context, q Querier, tenant string, refs []Ref) error {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		if h.kind == "" || ref.Kind == h.kind {
			ids = append(ids, string(ref.ID))
		}
	}
	var used bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+h.table+` WHERE tenant_id=$1 AND entity_id=ANY($2::text[]))`, tenant, ids).Scan(&used); err != nil {
		return err
	}
	if used {
		return fmt.Errorf("%w: host references remain", ErrConflict)
	}
	return nil
}

func (h *hostReferences) Merge(ctx context.Context, q Querier, tenant string, from, into Ref) error {
	if _, err := q.Exec(ctx, `INSERT INTO `+h.table+` (tenant_id, candidate, entity_id)
 SELECT tenant_id, candidate, $3 FROM `+h.table+` WHERE tenant_id=$1 AND entity_id=$2 ON CONFLICT DO NOTHING`, tenant, string(from.ID), string(into.ID)); err != nil {
		return err
	}
	if _, err := q.Exec(ctx, `DELETE FROM `+h.table+` WHERE tenant_id=$1 AND entity_id=$2`, tenant, string(from.ID)); err != nil {
		return err
	}
	if h.failMerge {
		return fmt.Errorf("%w: host write failed", ErrConflict)
	}
	return nil
}

func (f *fixture) hostReferences() *hostReferences {
	f.t.Helper()
	f.exec(`CREATE TABLE {s}.host_links (tenant_id text NOT NULL, candidate text NOT NULL, entity_id text NOT NULL,
 PRIMARY KEY (tenant_id,candidate,entity_id), FOREIGN KEY (tenant_id,entity_id) REFERENCES {s}.content_entities(tenant_id,id))`)
	h := &hostReferences{table: f.s.table("host_links")}
	f.s.references = h
	return h
}

func (f *fixture) waitEntityReferenceBlocked(ctx context.Context, pid uint32) {
	f.t.Helper()
	timer := time.NewTicker(10 * time.Millisecond)
	defer timer.Stop()
	for {
		var blocked bool
		if err := f.pool.QueryRow(ctx, "SELECT cardinality(pg_blocking_pids($1)) > 0", pid).Scan(&blocked); err != nil {
			f.t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			f.t.Fatal("competing transaction never blocked")
		case <-timer.C:
		}
	}
}

func TestEntityReferencesIntegration(t *testing.T) {
	f := newFixture(t)
	h := f.hostReferences()
	g := f.create(EntityInput{Kind: "artist", Slug: "old"}, EntityInput{Kind: "artist", Slug: "new"}, EntityInput{Kind: "tag", Slug: "tag"})
	from, into, tag := g[0], g[1], g[2]
	f.exec(`INSERT INTO {s}.host_links VALUES ($1,'one',$2),($1,'one',$3),($1,'two',$2)`, tenant, string(from.ID), string(into.ID))
	checkLinks := func(fromCount, intoCount int) {
		t.Helper()
		var a, b int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FILTER (WHERE entity_id=$2),count(*) FILTER (WHERE entity_id=$3) FROM `+h.table+` WHERE tenant_id=$1`, tenant, string(from.ID), string(into.ID)).Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		if a != fromCount || b != intoCount {
			t.Fatalf("host links: got %d/%d, want %d/%d", a, b, fromCount, intoCount)
		}
	}
	h.failMerge = true
	if _, err := f.s.Merge(f.ctx, []Merge{{From: from.ID, Into: into.ID}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("merge failure: %v", err)
	}
	checkLinks(2, 1)
	h.failMerge = false
	if _, err := f.s.Merge(f.ctx, []Merge{{From: from.ID, Into: into.ID}}); err != nil {
		t.Fatal(err)
	}
	checkLinks(0, 2)

	deleted := StateDeleted
	internal := true
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"delete", func() error { _, err := f.s.DeleteEntities(f.ctx, []EntityID{tag.ID, into.ID}); return err }},
		{"internal", func() error {
			_, err := f.s.UpdateEntities(f.ctx, []EntityUpdate{{ID: into.ID, Internal: &internal}})
			return err
		}},
		{"import delete", func() error {
			return pgx.BeginFunc(f.ctx, f.pool, func(tx pgx.Tx) error {
				return f.s.ImportEntities(f.ctx, tx, []ImportEntity{{ID: into.ID, Kind: into.Kind, Slug: into.Slug, State: deleted}}, false)
			})
		}},
		{"import internal", func() error {
			return pgx.BeginFunc(f.ctx, f.pool, func(tx pgx.Tx) error {
				return f.s.ImportEntities(f.ctx, tx, []ImportEntity{{ID: into.ID, Kind: into.Kind, Slug: into.Slug, Internal: &internal}}, false)
			})
		}},
		{"import deletes", func() error {
			return pgx.BeginFunc(f.ctx, f.pool, func(tx pgx.Tx) error { return f.s.ImportDeletes(f.ctx, tx, []EntityID{into.ID}) })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); !errors.Is(err, ErrConflict) {
				t.Fatalf("referenced group must be protected: %v", err)
			}
			checkLinks(0, 2)
			var state string
			var hidden bool
			if err := f.pool.QueryRow(f.ctx, `SELECT state,internal FROM `+f.s.table("content_entities")+` WHERE tenant_id=$1 AND id=$2`, tenant, string(into.ID)).Scan(&state, &hidden); err != nil {
				t.Fatal(err)
			}
			if state != "active" || hidden {
				t.Fatalf("failed mutation persisted: %s/%t", state, hidden)
			}
		})
	}

	other, err := New(Options{Pool: f.pool, Schema: f.schema, Tenant: "other", Kinds: config()})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := other.CreateEntities(f.ctx, []EntityInput{{Kind: "artist", Slug: "foreign"}})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := stdlib.OpenDBFromPool(f.pool)
	defer sqlDB.Close()
	unavailable := f.create(EntityInput{Kind: "artist", Slug: "hidden", Internal: true}, EntityInput{Kind: "artist", Slug: "deleted"})
	if _, err := f.s.DeleteEntities(f.ctx, []EntityID{unavailable[1].ID}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		refs []Ref
		want error
	}{
		{"valid", []Ref{{Kind: "artist", ID: into.ID}, {Kind: "tag", ID: tag.ID}}, nil},
		{"empty", nil, nil},
		{"wrong kind", []Ref{{Kind: "tag", ID: into.ID}}, ErrInvalid},
		{"conflicting duplicate", []Ref{{Kind: "artist", ID: into.ID}, {Kind: "tag", ID: into.ID}}, ErrInvalid},
		{"merged", []Ref{{Kind: "artist", ID: from.ID}}, ErrConflict},
		{"foreign tenant", []Ref{{Kind: "artist", ID: foreign[0].ID}}, ErrNotFound},
		{"internal", []Ref{{Kind: "artist", ID: unavailable[0].ID}}, ErrConflict},
		{"deleted", []Ref{{Kind: "artist", ID: unavailable[1].ID}}, ErrConflict},
		{"duplicate", []Ref{{Kind: "artist", ID: into.ID}, {Kind: "artist", ID: into.ID}}, nil},
		{"unknown kind", []Ref{{Kind: "missing", ID: into.ID}}, ErrInvalid},
		{"empty id", []Ref{{Kind: "artist"}}, ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := pgx.BeginFunc(f.ctx, f.pool, func(tx pgx.Tx) error { return f.s.LockReferencesTx(f.ctx, tx, tc.refs) })
			if !errors.Is(err, tc.want) {
				t.Fatalf("lock: %v, want %v", err, tc.want)
			}
			tx, err := sqlDB.BeginTx(f.ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := f.s.LockReferencesSQLTx(f.ctx, tx, tc.refs); !errors.Is(err, tc.want) {
				t.Fatalf("SQL lock: %v, want %v", err, tc.want)
			}
		})
	}
	if err := f.s.LockReferencesTx(f.ctx, nil, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil transaction: %v", err)
	}
	if err := f.s.LockReferencesSQLTx(f.ctx, nil, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil SQL transaction: %v", err)
	}
	for _, isolation := range []sql.IsolationLevel{sql.LevelRepeatableRead, sql.LevelSerializable} {
		tx, err := sqlDB.BeginTx(f.ctx, &sql.TxOptions{Isolation: isolation})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.s.LockReferencesSQLTx(f.ctx, tx, []Ref{{Kind: "artist", ID: into.ID}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("SQL isolation %v: %v", isolation, err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	f.exec(`DELETE FROM {s}.host_links`)
	if _, err := f.s.DeleteEntities(f.ctx, []EntityID{into.ID}); err != nil {
		t.Fatal(err)
	}
}

func TestEntityReferencesConcurrentBinding(t *testing.T) {
	for _, scenario := range []struct{ driver, operation string }{
		{"pgx", "merge"}, {"pgx", "delete"}, {"pgx", "import"},
		{"sql", "merge"}, {"sql", "delete"}, {"sql", "import"},
	} {
		driver, operation := scenario.driver, scenario.operation
		for _, bindFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/%s/bind-first=%t", driver, operation, bindFirst), func(t *testing.T) {
				f := newFixture(t)
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				f.ctx = ctx
				h := f.hostReferences()
				g := f.create(EntityInput{Kind: "artist", Slug: "old"}, EntityInput{Kind: "artist", Slug: "new"})
				var lock, insert, commit func() error
				var bindPID uint32
				if driver == "sql" {
					db := stdlib.OpenDBFromPool(f.pool)
					defer db.Close()
					tx, err := db.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					if err := tx.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&bindPID); err != nil {
						t.Fatal(err)
					}
					lock = func() error { return f.s.LockReferencesSQLTx(ctx, tx, []Ref{{Kind: "artist", ID: g[0].ID}}) }
					insert = func() error {
						_, err := tx.ExecContext(ctx, `INSERT INTO `+h.table+` VALUES ($1,'one',$2)`, tenant, string(g[0].ID))
						return err
					}
					commit = tx.Commit
				} else {
					tx, err := f.pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(ctx)
					bindPID = tx.Conn().PgConn().PID()
					lock = func() error { return f.s.LockReferencesTx(ctx, tx, []Ref{{Kind: "artist", ID: g[0].ID}}) }
					insert = func() error {
						_, err := tx.Exec(ctx, `INSERT INTO `+h.table+` VALUES ($1,'one',$2)`, tenant, string(g[0].ID))
						return err
					}
					commit = func() error { return tx.Commit(ctx) }
				}
				write, err := f.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer write.Rollback(ctx)
				mutate := func() error {
					if operation == "import" {
						return f.s.ImportEntities(ctx, write, []ImportEntity{{ID: g[0].ID, Kind: "artist", Slug: "old", State: StateDeleted}}, false)
					}
					if operation == "merge" {
						_, err := f.s.bound(write).Merge(ctx, []Merge{{From: g[0].ID, Into: g[1].ID}})
						return err
					}
					_, err := f.s.bound(write).DeleteEntities(ctx, []EntityID{g[0].ID})
					return err
				}
				result := make(chan error, 1)
				done := make(chan struct{})
				if bindFirst {
					if err := lock(); err != nil {
						t.Fatal(err)
					}
					pid := write.Conn().PgConn().PID()
					go func() {
						defer close(done)
						err := mutate()
						if err == nil {
							err = write.Commit(ctx)
						} else {
							_ = write.Rollback(ctx)
						}
						result <- err
					}()
					defer func() { cancel(); <-done }()
					f.waitEntityReferenceBlocked(ctx, pid)
					if err := insert(); err != nil {
						t.Fatal(err)
					}
					if err := commit(); err != nil {
						t.Fatal(err)
					}
					err = <-result
					if operation != "merge" {
						if !errors.Is(err, ErrConflict) {
							t.Fatalf("delete after bind: %v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
					want := g[0].ID
					if operation == "merge" {
						want = g[1].ID
					}
					var got string
					if err := f.pool.QueryRow(ctx, `SELECT entity_id FROM `+h.table+` WHERE candidate='one'`).Scan(&got); err != nil {
						t.Fatal(err)
					}
					if got != string(want) {
						t.Fatalf("link %s, want %s", got, want)
					}
				} else {
					if err := mutate(); err != nil {
						t.Fatal(err)
					}
					go func() { defer close(done); result <- lock() }()
					defer func() { cancel(); <-done }()
					f.waitEntityReferenceBlocked(ctx, bindPID)
					if err := write.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					if err := <-result; !errors.Is(err, ErrConflict) {
						t.Fatalf("bind after mutation: %v", err)
					}
				}
			})
		}
	}
}

func TestEntityReferencesConcurrentImportCreation(t *testing.T) {
	f := newFixture(t)
	h := f.hostReferences()
	h.kind = "tag"
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	create, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer create.Rollback(ctx)
	g, err := f.s.bound(create).CreateEntities(ctx, []EntityInput{{Kind: "tag", Slug: "new"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := create.Exec(ctx, `INSERT INTO `+h.table+` VALUES ($1,'one',$2)`, tenant, string(g[0].ID)); err != nil {
		t.Fatal(err)
	}
	importTx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer importTx.Rollback(ctx)
	pid := importTx.Conn().PgConn().PID()
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The initial SELECT cannot see the tag yet. The upsert waits for its
		// creator, retaining its actual kind rather than this input's kind.
		result <- f.s.ImportEntities(ctx, importTx, []ImportEntity{{ID: g[0].ID, Kind: "artist", Slug: "imported", State: StateDeleted}}, false)
	}()
	defer func() { cancel(); <-done }()
	f.waitEntityReferenceBlocked(ctx, pid)
	if err := create.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrently created tag must be protected: %v", err)
	}
	if err := importTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.pool.QueryRow(ctx, `SELECT state FROM `+f.s.table("content_entities")+` WHERE tenant_id=$1 AND id=$2`, tenant, string(g[0].ID)).Scan(&state); err != nil || state != "active" {
		t.Fatalf("import rollback state %q: %v", state, err)
	}
}

func TestEntityReferencesRejectStaleSnapshots(t *testing.T) {
	f := newFixture(t)
	h := f.hostReferences()
	g := f.create(EntityInput{Kind: "artist", Slug: "old"}, EntityInput{Kind: "artist", Slug: "new"})
	for _, operation := range []string{"bind", "delete", "merge", "import"} {
		t.Run(operation, func(t *testing.T) {
			f.exec(`DELETE FROM {s}.host_links`)
			tx, err := f.pool.BeginTx(f.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(f.ctx)
			// Fix this transaction's snapshot before a host link is committed.
			var count int
			if err := tx.QueryRow(f.ctx, `SELECT count(*) FROM `+h.table).Scan(&count); err != nil || count != 0 {
				t.Fatalf("initial references %d: %v", count, err)
			}
			err = pgx.BeginFunc(f.ctx, f.pool, func(bind pgx.Tx) error {
				if err := f.s.LockReferencesTx(f.ctx, bind, []Ref{{Kind: "artist", ID: g[0].ID}}); err != nil {
					return err
				}
				_, err := bind.Exec(f.ctx, `INSERT INTO `+h.table+` VALUES ($1,'one',$2)`, tenant, string(g[0].ID))
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "bind":
				err = f.s.LockReferencesTx(f.ctx, tx, []Ref{{Kind: "artist", ID: g[0].ID}})
			case "delete":
				_, err = f.s.bound(tx).DeleteEntities(f.ctx, []EntityID{g[0].ID})
			case "merge":
				_, err = f.s.bound(tx).Merge(f.ctx, []Merge{{From: g[0].ID, Into: g[1].ID}})
			case "import":
				err = f.s.ImportEntities(f.ctx, tx, []ImportEntity{{ID: g[0].ID, Kind: "artist", Slug: "old", State: StateDeleted}}, false)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("stale snapshot accepted: %v", err)
			}
		})
	}
}

func TestMatchExactNameSQLTx(t *testing.T) {
	f := newFixture(t)
	g := f.create(
		EntityInput{Kind: "artist", Slug: "artist", Names: []Name{canonical("en", "Café-Art"), canonical("es", "Café-Art"), alias("en", "Studio, West")}},
		EntityInput{Kind: "artist", Slug: "first", Names: []Name{canonical("en", "Shared")}},
		EntityInput{Kind: "artist", Slug: "second", Names: []Name{canonical("en", "Second"), alias("en", "Shared")}},
		EntityInput{Kind: "tag", Slug: "tag", Names: []Name{canonical("en", "Café-Art")}},
		EntityInput{Kind: "artist", Slug: "internal", Internal: true, Names: []Name{canonical("en", "Hidden")}},
		EntityInput{Kind: "artist", Slug: "deleted", Names: []Name{canonical("en", "Deleted")}},
		EntityInput{Kind: "series", Slug: "series-one"}, EntityInput{Kind: "series", Slug: "series-two"},
	)
	if _, err := f.s.DeleteEntities(f.ctx, []EntityID{g[5].ID}); err != nil {
		t.Fatal(err)
	}
	characters := f.create(
		EntityInput{Kind: "character", Slug: "one", ScopeID: g[6].ID, Names: []Name{canonical("en", "Character")}},
		EntityInput{Kind: "character", Slug: "two", ScopeID: g[7].ID, Names: []Name{canonical("en", "Character")}},
	)
	other, err := New(Options{Pool: f.pool, Schema: f.schema, Tenant: "other", Kinds: config()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.CreateEntities(f.ctx, []EntityInput{{Kind: "artist", Slug: "foreign", Names: []Name{canonical("en", "Café-Art"), alias("en", "Foreign")}}}); err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDBFromPool(f.pool)
	defer db.Close()
	tx, err := db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ambiguous := []string{string(g[1].ID), string(g[2].ID)}
	slices.Sort(ambiguous)
	for _, tc := range []struct {
		name, kind, scope, language, input string
		want                               []string
	}{
		{"canonical across languages", "artist", "", "", "Café-Art", []string{string(g[0].ID)}},
		{"alias", "artist", "", "en", "Studio, West", []string{string(g[0].ID)}},
		{"ambiguous", "artist", "", "", "Shared", ambiguous},
		{"correct kind", "tag", "", "", "Café-Art", []string{string(g[3].ID)}},
		{"scoped", "character", string(g[6].ID), "", "Character", []string{string(characters[0].ID)}},
		{"missing scope", "character", "", "", "Character", nil},
		{"other language", "artist", "", "es", "Studio, West", nil},
		{"case", "artist", "", "", "café-Art", nil},
		{"accent", "artist", "", "", "Cafe-Art", nil},
		{"separator", "artist", "", "", "Café Art", nil},
		{"whitespace", "artist", "", "", " Café-Art ", nil},
		{"punctuation", "artist", "", "", "Studio West", nil},
		{"no comma splitting", "artist", "", "", "Café-Art, Second", nil},
		{"unknown", "artist", "", "", "Unknown", nil},
		{"blank", "artist", "", "", "", nil},
		{"internal", "artist", "", "", "Hidden", nil},
		{"deleted", "artist", "", "", "Deleted", nil},
		{"foreign catalog", "artist", "", "", "Foreign", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.s.MatchExactNameSQLTx(f.ctx, tx, tc.kind, tc.scope, tc.language, tc.input)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v: %v", got, tc.want, err)
			}
		})
	}
	if _, err := f.s.MatchExactNameSQLTx(f.ctx, nil, "artist", "", "", "Café-Art"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil transaction: %v", err)
	}
	if _, err := f.s.MatchExactNameSQLTx(f.ctx, tx, "unknown", "", "", "Café-Art"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown kind: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.MatchExactNameSQLTx(f.ctx, tx, "artist", "", "", "Café-Art"); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("finished transaction was treated as a missing name: %v", err)
	}
}

func TestSQLEntityReferencesStayInHostTransaction(t *testing.T) {
	for _, outcome := range []string{"commit", "dry-run rollback", "failed batch"} {
		t.Run(outcome, func(t *testing.T) {
			f := newFixture(t)
			h := f.hostReferences()
			g := f.create(EntityInput{Kind: "artist", Slug: "artist"})[0]
			db := stdlib.OpenDBFromPool(f.pool)
			defer db.Close()
			tx, err := db.BeginTx(f.ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(f.ctx, `INSERT INTO `+f.s.table("content_entity_names")+` (tenant_id,entity_id,language,kind,name) VALUES ($1,$2,'en','alias','Uncommitted')`, tenant, string(g.ID)); err != nil {
				t.Fatal(err)
			}
			ids, err := f.s.MatchExactNameSQLTx(f.ctx, tx, "artist", "", "", "Uncommitted")
			if err != nil || !slices.Equal(ids, []string{string(g.ID)}) {
				t.Fatalf("transaction-local name %v: %v", ids, err)
			}
			if err := f.s.LockReferencesSQLTx(f.ctx, tx, []Ref{{Kind: "artist", ID: EntityID(ids[0])}}); err != nil {
				t.Fatal(err)
			}
			insert := `INSERT INTO ` + h.table + ` VALUES ($1,'one',$2)`
			if _, err := tx.ExecContext(f.ctx, insert, tenant, ids[0]); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+h.table).Scan(&count); err != nil || count != 0 {
				t.Fatalf("host write escaped transaction: count=%d, error=%v", count, err)
			}
			want := 0
			if outcome == "commit" {
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				want = 1
			} else {
				if outcome == "failed batch" {
					if _, err := tx.ExecContext(f.ctx, insert, tenant, ids[0]); err == nil {
						t.Fatal("expected duplicate host link to abort the batch")
					}
					if _, err := f.s.MatchExactNameSQLTx(f.ctx, tx, "artist", "", "", "Uncommitted"); err == nil {
						t.Fatal("aborted transaction treated as an unresolved name")
					}
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+h.table).Scan(&count); err != nil || count != want {
				t.Fatalf("host links after %s: count=%d, error=%v", outcome, count, err)
			}
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.s.table("content_entity_names")+` WHERE name='Uncommitted'`).Scan(&count); err != nil || count != want {
				t.Fatalf("name after %s: count=%d, error=%v", outcome, count, err)
			}
		})
	}
}

// Entities are created against their kind's rules: scope, parent, owner, auth
// scope, declared attributes and cover; slugs are unique per kind and scope.
func TestCreateEntitiesFollowsKinds(t *testing.T) {
	f := newFixture(t)
	made := f.create(
		EntityInput{Kind: "series", Slug: "fate", Names: []Name{canonical("en", "Fate"), canonical("ja", "フェイト")}, Text: []Text{{Language: "en", Description: "A saga", SortKey: "fate"}}},
		EntityInput{Kind: "series", Slug: "fate-zero", Names: []Name{canonical("en", "Fate/Zero")}},
		EntityInput{Kind: "artist", Slug: "shindol", Names: []Name{canonical("en", "Shindol"), alias("en", "Shin")}, Attributes: map[string]any{"artist_type": "circle"}},
	)
	fate, zero, shindol := made[0], made[1], made[2]
	if fate.State != StateActive || fate.Kind != "series" || len(shindol.Attributes) != 1 || shindol.Attributes["artist_type"] != "circle" {
		t.Fatalf("created %+v %+v", fate, shindol)
	}
	chars := f.create(
		EntityInput{Kind: "character", Slug: "saber", ScopeID: fate.ID, Names: []Name{canonical("en", "Saber")}},
		EntityInput{Kind: "character", Slug: "saber", ScopeID: zero.ID, Names: []Name{canonical("en", "Saber")}, Attributes: map[string]any{"external_link": "https://x.test"}},
	)
	got, err := f.s.UpdateEntities(f.ctx, []EntityUpdate{{ID: zero.ID, ParentID: &fate.ID}})
	if err != nil || got[0].ParentID != fate.ID || got[0].Revision != 2 {
		t.Fatalf("parent: %+v %v", got, err)
	}

	for name, in := range map[string]EntityInput{
		"unknown kind":          {Kind: "studio", Slug: "x"},
		"slug with a slash":     {Kind: "tag", Slug: "a/b"},
		"scoped without scope":  {Kind: "character", Slug: "rin"},
		"scope on a tag":        {Kind: "tag", Slug: "x", ScopeID: fate.ID},
		"parent on a tag":       {Kind: "tag", Slug: "x", ParentID: fate.ID},
		"site group with owner": {Kind: "tag", Slug: "x", OwnerUserID: "u1"},
		"user group no owner":   {Kind: "channel", Slug: "x"},
		"auth scope on a tag":   {Kind: "tag", Slug: "x", AuthScope: "g1"},
		"undeclared attribute":  {Kind: "tag", Slug: "x", Attributes: map[string]any{"color": "red"}},
		"enum value":            {Kind: "artist", Slug: "x", Attributes: map[string]any{"artist_type": "band"}},
		"cover image name":      {Kind: "tag", Slug: "x", Cover: &Cover{Image: "cover.png"}},
		"cover of both":         {Kind: "tag", Slug: "x", Cover: &Cover{Image: "i-0193a000-0000-7000-8000-000000000001", ContentID: cid(1)}},
		"two canonical names":   {Kind: "tag", Slug: "x", Names: []Name{canonical("en", "A"), canonical("en", "B")}},
		"scope of wrong kind":   {Kind: "character", Slug: "x", ScopeID: shindol.ID},
		"parent of wrong kind":  {Kind: "series", Slug: "x", ParentID: shindol.ID},
	} {
		if _, err := f.s.CreateEntities(f.ctx, []EntityInput{in}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, in := range map[string]EntityInput{
		"slug taken":              {Kind: "series", Slug: "fate"},
		"slug taken in its scope": {Kind: "character", Slug: "saber", ScopeID: fate.ID},
		"id taken":                {ID: fate.ID, Kind: "tag", Slug: "new"},
	} {
		if _, err := f.s.CreateEntities(f.ctx, []EntityInput{in}); !errors.Is(err, ErrConflict) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.s.CreateEntities(f.ctx, []EntityInput{{Kind: "tag", Slug: "x", Cover: &Cover{ContentID: cid(9)}}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("cover of missing content: %v", err)
	}
	if _, err := f.s.UpdateEntities(f.ctx, []EntityUpdate{{ID: fate.ID, ParentID: &zero.ID}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("parent cycle: %v", err)
	}
	if _, err := f.s.CreateEntities(f.ctx, []EntityInput{{Kind: "channel", Slug: "mine", OwnerUserID: "u1", AuthScope: "ak-group-1"}}); err != nil {
		t.Errorf("channel: %v", err)
	}

	// Codes and slugs come from the canonical names; documents are queued per language.
	urls, err := contenturl.New(contenturl.Options{Pool: f.pool, Schema: f.schema, Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	ref := contentref.New(tenant, "series", string(fate.ID))
	links, err := urls.Links(f.ctx, []contentref.ContentRef{ref, contentref.New(tenant, "character", string(chars[0].ID))})
	if err != nil || len(links) != 2 || links[ref.Key()].Slug != "fate" {
		t.Fatalf("links %+v %v", links, err)
	}
	var dirty int
	if err := f.pool.QueryRow(f.ctx, fmt.Sprintf(`SELECT count(*) FROM %q.content_search_dirty WHERE content_kind = 'series' AND content_id = $1`, f.schema), string(fate.ID)).Scan(&dirty); err != nil || dirty != 2 {
		t.Fatalf("%d dirty documents, want one per language: %v", dirty, err)
	}

	// Deleting frees the slug; restoring the old entity then conflicts.
	if _, err := f.s.DeleteEntities(f.ctx, []EntityID{shindol.ID}); err != nil {
		t.Fatal(err)
	}
	f.create(EntityInput{Kind: "artist", Slug: "shindol"})
	if _, err := f.s.UpdateEntities(f.ctx, []EntityUpdate{{ID: shindol.ID, State: ptr(StateActive)}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("restore over a taken slug: %v", err)
	}
	if _, err := f.s.UpdateEntities(f.ctx, []EntityUpdate{{ID: shindol.ID, State: ptr(StateMerged)}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("state merged is not assignable: %v", err)
	}
}

// Names: a new canonical name demotes the old one to an alias; set, add and
// remove are batches.
func TestNames(t *testing.T) {
	f := newFixture(t)
	a := f.create(EntityInput{Kind: "tag", Slug: "a", Names: []Name{canonical("en", "Colored")}})[0]
	if err := f.s.AddNames(f.ctx, []EntityNames{{ID: a.ID, Names: []Name{canonical("en", "Full Color"), alias("es", "Coloreado")}}}); err != nil {
		t.Fatal(err)
	}
	d, err := f.s.Detail(f.ctx, a.ID, "es")
	if err != nil {
		t.Fatal(err)
	}
	want := []Name{canonical("en", "Full Color"), alias("en", "Colored"), alias("es", "Coloreado")}
	if !slices.Equal(d.Names, want) || d.Name != "Full Color" || d.NameLanguage != "en" {
		t.Fatalf("names %+v (%s/%s)", d.Names, d.Name, d.NameLanguage)
	}
	if err := f.s.RemoveNames(f.ctx, []EntityNames{{ID: a.ID, Names: []Name{{Language: "en", Name: "colored"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetNames(f.ctx, []EntityNames{{ID: a.ID, Names: []Name{canonical("es", "Color")}}}); err != nil {
		t.Fatal(err)
	}
	if d, _ = f.s.Detail(f.ctx, a.ID, "en"); len(d.Names) != 1 || d.Name != "Color" || d.Slug != "a" {
		t.Fatalf("after set %+v", d)
	}
	if err := f.s.AddNames(f.ctx, []EntityNames{{ID: EntityID(cid(99)), Names: []Name{canonical("en", "x")}}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("names of a missing group: %v", err)
	}
}

// A version's effective entities: its content's and its own active memberships
// in active entities, inside their windows, with every active ancestor.
func TestEffectiveEntities(t *testing.T) {
	f := newFixture(t)
	f.content("gallery", 1, live("en"), live("es"))
	f.content("gallery", 2, live("en"))
	g := f.create(
		EntityInput{Kind: "series", Slug: "root"},
		EntityInput{Kind: "tag", Slug: "tag"},
		EntityInput{Kind: "voice_actor", Slug: "va"},
		EntityInput{Kind: "rail", Slug: "now"},
		EntityInput{Kind: "rail", Slug: "later"},
		EntityInput{Kind: "rail", Slug: "over"},
		EntityInput{Kind: "tag", Slug: "proposed"},
		EntityInput{Kind: "tag", Slug: "gone"},
	)
	root, tag, va, now, later, over, proposed, gone := g[0], g[1], g[2], g[3], g[4], g[5], g[6], g[7]
	sub := f.create(EntityInput{Kind: "series", Slug: "sub", ParentID: root.ID})[0]
	hour := time.Now().Add(time.Hour)
	ago := time.Now().Add(-time.Minute)
	f.changed = nil
	f.add(
		Membership{EntityID: sub.ID, ContentID: cid(1)},
		Membership{EntityID: tag.ID, ContentID: cid(1)},
		Membership{EntityID: va.ID, ContentID: cid(1), VersionID: vid(1, 1)},
		Membership{EntityID: now.ID, ContentID: cid(1), ActiveUntil: &hour},
		Membership{EntityID: later.ID, ContentID: cid(1), ActiveFrom: &hour},
		Membership{EntityID: over.ID, ContentID: cid(1), ActiveUntil: &ago},
		Membership{EntityID: proposed.ID, ContentID: cid(1), State: MembershipProposed},
		Membership{EntityID: gone.ID, ContentID: cid(2)},
	)
	if len(f.changed) != 1 || !slices.Equal(f.changed[0], []string{cid(1), cid(2)}) {
		t.Fatalf("ContentChanged calls %v", f.changed)
	}
	eff, err := f.s.EffectiveEntities(f.ctx, f.pool, []string{vid(1, 0), vid(1, 1), vid(2, 0), "missing"})
	if err != nil {
		t.Fatal(err)
	}
	v0 := ids(root, sub, tag, now)
	v1 := ids(root, sub, tag, va, now)
	if !slices.Equal(eff[vid(1, 0)], v0) || !slices.Equal(eff[vid(1, 1)], v1) || !slices.Equal(eff[vid(2, 0)], ids(gone)) || len(eff) != 3 {
		t.Fatalf("effective %v", eff)
	}
	if facets := f.facetEntities(); !slices.Equal(facets[vid(1, 0)], v0) || !slices.Equal(facets[vid(1, 1)], v1) {
		t.Fatalf("facets %v", facets)
	}

	// Deleting an entity, or its ancestor, takes it out of every facet.
	if _, err := f.s.DeleteEntities(f.ctx, []EntityID{gone.ID, root.ID}); err != nil {
		t.Fatal(err)
	}
	if facets := f.facetEntities(); !slices.Equal(facets[vid(1, 0)], ids(sub, tag, now)) || len(facets[vid(2, 0)]) != 0 {
		t.Fatalf("after delete %v", facets)
	}
	// A window that opened during a long outage is visible without a worker.
	f.exec(`UPDATE {s}.content_memberships SET active_from = now() - interval '2 days' WHERE entity_id = $1`, string(later.ID))
	var current []string
	if err := f.pool.QueryRow(f.ctx, `SELECT entity_ids FROM `+f.s.table("content_current_facets")+` WHERE tenant_id=$1 AND version_id=$2`, tenant, vid(1, 0)).Scan(&current); err != nil || !slices.Contains(current, string(later.ID)) {
		t.Fatalf("opened window %v: %v", current, err)
	}
	// The popularity catalog sees the same entities, per content and kind.
	byContent, err := f.s.Memberships(f.ctx, tenant, "gallery", "rail", []string{cid(1), cid(2)})
	if err != nil || !slices.Equal(byContent[cid(1)], []EntityID{now.ID, later.ID}) && !slices.Equal(byContent[cid(1)], []EntityID{later.ID, now.ID}) || len(byContent) != 1 {
		t.Fatalf("popularity memberships %v %v", byContent, err)
	}
}

// Memberships follow their entity kind: version level, roles, positions,
// windows, and the content kinds that join it.
func TestMembershipRules(t *testing.T) {
	f := newFixture(t)
	f.content("gallery", 1, live("en"), live("es"))
	f.content("gallery", 2, live("en"))
	f.content("video", 3, live("en", "ja"))
	g := f.create(EntityInput{Kind: "tag", Slug: "t"}, EntityInput{Kind: "artist", Slug: "a"}, EntityInput{Kind: "series", Slug: "s"},
		EntityInput{Kind: "rail", Slug: "r"}, EntityInput{Kind: "voice_actor", Slug: "va"})
	tag, artist, series, rail, va := g[0], g[1], g[2], g[3], g[4]
	until := time.Now().Add(time.Hour)
	for name, m := range map[string]Membership{
		"version of a content-level kind": {EntityID: artist.ID, ContentID: cid(1), VersionID: vid(1, 0)},
		"undeclared role":                 {EntityID: artist.ID, ContentID: cid(1), Role: "translator"},
		"position in an unordered group":  {EntityID: tag.ID, ContentID: cid(1), Position: ptr(1)},
		"window on a series":              {EntityID: series.ID, ContentID: cid(1), ActiveUntil: &until},
		"kind the content does not join":  {EntityID: artist.ID, ContentID: cid(3)},
		"state":                           {EntityID: tag.ID, ContentID: cid(1), State: "maybe"},
	} {
		if err := f.s.AddMemberships(f.ctx, []Membership{m}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, m := range map[string]Membership{
		"another content's version": {EntityID: va.ID, ContentID: cid(2), VersionID: vid(1, 0)},
		"missing content":           {EntityID: tag.ID, ContentID: cid(9)},
		"missing group":             {EntityID: EntityID(cid(99)), ContentID: cid(1)},
	} {
		if err := f.s.AddMemberships(f.ctx, []Membership{m}); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	f.add(Membership{EntityID: artist.ID, ContentID: cid(1)}, Membership{EntityID: artist.ID, ContentID: cid(1), Role: "publisher"},
		Membership{EntityID: rail.ID, ContentID: cid(2), ActiveUntil: &until}, Membership{EntityID: tag.ID, ContentID: cid(3)})

	// Ordered entities append, keep existing positions and take explicit ones.
	f.add(Membership{EntityID: series.ID, ContentID: cid(2)}, Membership{EntityID: series.ID, ContentID: cid(1)})
	f.add(Membership{EntityID: series.ID, ContentID: cid(2)})
	page, err := f.s.Members(f.ctx, series.ID, MemberOptions{})
	if err != nil || len(page.Members) != 2 || page.Members[0].ContentID != cid(2) || *page.Members[0].Position != 1 || *page.Members[1].Position != 2 {
		t.Fatalf("appended %+v %v", page, err)
	}
	if err := f.s.Reorder(f.ctx, []Order{{EntityID: series.ID, Members: []MemberRef{{ContentID: cid(1)}}}}); err != nil {
		t.Fatal(err)
	}
	if page, _ = f.s.Members(f.ctx, series.ID, MemberOptions{}); page.Members[0].ContentID != cid(1) || *page.Members[1].Position != 2 {
		t.Fatalf("reordered %+v", page)
	}
	if err := f.s.Reorder(f.ctx, []Order{{EntityID: tag.ID}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reorder an unordered group: %v", err)
	}
	if err := f.s.Reorder(f.ctx, []Order{{EntityID: series.ID, Members: []MemberRef{{ContentID: cid(3)}}}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reorder a non-member: %v", err)
	}

	// A content's set replaces its memberships in the kinds it names only.
	if err := f.s.SetContentEntities(f.ctx, []ContentEntities{{ContentID: cid(1), Kinds: []string{"artist", "tag", "voice_actor"}, Memberships: []Membership{
		{EntityID: tag.ID}, {EntityID: va.ID, VersionID: vid(1, 1)}, {EntityID: artist.ID, Role: "publisher"}}}}); err != nil {
		t.Fatal(err)
	}
	held, err := f.s.MembershipsOf(f.ctx, []string{cid(1)})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range held[cid(1)] {
		got = append(got, fmt.Sprintf("%s/%s/%s", m.EntityID, m.VersionID, m.Role))
	}
	want := []string{fmt.Sprintf("%s//publisher", artist.ID), fmt.Sprintf("%s//", series.ID), fmt.Sprintf("%s//", tag.ID), fmt.Sprintf("%s/%s/", va.ID, vid(1, 1))}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("after set\n %v\nwant\n %v", got, want)
	}
	if err := f.s.SetContentEntities(f.ctx, []ContentEntities{{ContentID: cid(1), Kinds: []string{"tag"}, Memberships: []Membership{{EntityID: artist.ID}}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("membership outside the kinds written: %v", err)
	}
	if err := f.s.SetContentEntities(f.ctx, []ContentEntities{{ContentID: cid(3), Kinds: []string{"artist"}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("kind the content does not join: %v", err)
	}
	// With no kinds, every kind gallery joins is replaced: the series membership goes.
	if err := f.s.SetContentEntities(f.ctx, []ContentEntities{{ContentID: cid(1)}}); err != nil {
		t.Fatal(err)
	}
	if held, _ = f.s.MembershipsOf(f.ctx, []string{cid(1)}); len(held[cid(1)]) != 0 {
		t.Fatalf("emptied %+v", held)
	}
	if err := f.s.RemoveMemberships(f.ctx, []Membership{{EntityID: rail.ID, ContentID: cid(2)}}); err != nil {
		t.Fatal(err)
	}
	if held, _ = f.s.MembershipsOf(f.ctx, []string{cid(2), cid(3)}); len(held[cid(2)]) != 1 || len(held[cid(3)]) != 1 {
		t.Fatalf("after remove %+v", held)
	}

	// An import naming one membership twice is still a change: the duplicate
	// collapses as a write keeps it, so the stored artist membership goes.
	f.add(Membership{EntityID: tag.ID, ContentID: cid(2)}, Membership{EntityID: artist.ID, ContentID: cid(2)})
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if err := f.s.ImportMemberships(f.ctx, tx, []ContentEntities{{ContentID: cid(2), Kinds: []string{"artist", "tag"},
		Memberships: []Membership{{EntityID: tag.ID}, {EntityID: tag.ID}}}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	held, _ = f.s.MembershipsOf(f.ctx, []string{cid(2)})
	kept := map[EntityID]bool{}
	for _, m := range held[cid(2)] {
		kept[m.EntityID] = true
	}
	if !kept[tag.ID] || kept[artist.ID] {
		t.Fatalf("after importing a duplicated set %+v", held[cid(2)])
	}
}

// Merging moves memberships (an accepted one wins over a proposed duplicate),
// names, edges, children and scoped entities, and redirects the code.
func TestMerge(t *testing.T) {
	f := newFixture(t)
	f.content("gallery", 1, live("en"))
	f.content("gallery", 2, live("en"))
	g := f.create(
		EntityInput{Kind: "series", Slug: "a", Names: []Name{canonical("en", "Alpha")}},
		EntityInput{Kind: "series", Slug: "b", Names: []Name{canonical("en", "Beta")}, Text: []Text{{Language: "es", Description: "b"}}},
		EntityInput{Kind: "series", Slug: "other"},
	)
	a, b, other := g[0], g[1], g[2]
	child := f.create(EntityInput{Kind: "series", Slug: "child", ParentID: b.ID})[0]
	char := f.create(EntityInput{Kind: "character", Slug: "hero", ScopeID: b.ID})[0]
	f.add(Membership{EntityID: b.ID, ContentID: cid(1)}, Membership{EntityID: a.ID, ContentID: cid(1), State: MembershipProposed}, Membership{EntityID: b.ID, ContentID: cid(2)})
	if err := f.s.AddEdges(f.ctx, []Edge{{From: b.ID, Relation: "related", To: other.ID}, {From: other.ID, Relation: "sequel_of", To: b.ID}, {From: a.ID, Relation: "related", To: b.ID}}); err != nil {
		t.Fatal(err)
	}
	reports, err := f.s.Merge(f.ctx, []Merge{{From: b.ID, Into: a.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if r := reports[0]; r.MembershipsMoved != 1 || r.NamesMoved != 1 || r.EdgesMoved != 2 || r.EntitiesMoved != 2 {
		t.Fatalf("report %+v", r)
	}
	members, _ := f.s.Members(f.ctx, a.ID, MemberOptions{})
	if len(members.Members) != 2 || members.Members[0].State != MembershipActive {
		t.Fatalf("members %+v", members)
	}
	d, err := f.s.Detail(f.ctx, b.ID, "en")
	if err != nil || d.ID != a.ID || len(d.Names) != 2 || len(d.Children) != 1 || d.Children[0].ID != child.ID || len(d.Edges) != 2 || len(d.Text) != 1 {
		t.Fatalf("merged detail resolves to the survivor: %+v %v", d, err)
	}
	moved, _ := f.s.Entities(f.ctx, []EntityID{char.ID, b.ID})
	if moved[0].ScopeID != a.ID || moved[1].State != StateMerged || moved[1].MergedInto != a.ID {
		t.Fatalf("moved %+v", moved)
	}
	if facets := f.facetEntities(); !slices.Equal(facets[vid(1, 0)], ids(a)) || !slices.Equal(facets[vid(2, 0)], ids(a)) {
		t.Fatalf("facets %v", facets)
	}
	urls, _ := contenturl.New(contenturl.Options{Pool: f.pool, Schema: f.schema, Tenant: tenant})
	links, err := urls.Links(f.ctx, []contentref.ContentRef{contentref.New(tenant, "series", string(a.ID)), contentref.New(tenant, "series", string(b.ID))})
	if err != nil || len(links) != 2 || links[contentref.New(tenant, "series", string(b.ID)).Key()].Code != links[contentref.New(tenant, "series", string(a.ID)).Key()].Code {
		t.Fatalf("code redirect %+v %v", links, err)
	}

	for name, m := range map[string]Merge{"itself": {From: a.ID, Into: a.ID}} {
		if _, err := f.s.Merge(f.ctx, []Merge{m}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	tag := f.create(EntityInput{Kind: "tag", Slug: "t"})[0]
	chans := f.create(EntityInput{Kind: "channel", Slug: "c1", OwnerUserID: "u"}, EntityInput{Kind: "channel", Slug: "c2", OwnerUserID: "u"})
	f.exec(`UPDATE {s}.content_entities SET ever_sold = true WHERE id = $1`, string(other.ID))
	for name, m := range map[string]Merge{"merged": {From: b.ID, Into: a.ID}, "other kind": {From: tag.ID, Into: a.ID},
		"user-owned": {From: chans[0].ID, Into: chans[1].ID}, "sold": {From: other.ID, Into: a.ID}} {
		if _, err := f.s.Merge(f.ctx, []Merge{m}); !errors.Is(err, ErrConflict) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Counts are distinct live, listed contents per entity, content kind and
// language ("" for all), descendants included; the automatic cover is the
// most-favorited live member anyone sees the cover of, a selected cover
// stays.
func TestRuntimeCountsAndCovers(t *testing.T) {
	f := newFixture(t)
	f.content("gallery", 1, live("en"), live("es"))
	f.content("gallery", 2, live("en"), version{languages: []string{"ja"}})
	f.content("gallery", 3, version{languages: []string{"en"}})
	f.content("video", 4, version{languages: []string{"en", "ja"}, live: &past})
	g := f.create(EntityInput{Kind: "series", Slug: "parent"}, EntityInput{Kind: "tag", Slug: "tag"}, EntityInput{Kind: "tag", Slug: "picked", Cover: &Cover{ContentID: cid(1)}})
	parent, tag, picked := g[0], g[1], g[2]
	child := f.create(EntityInput{Kind: "series", Slug: "child", ParentID: parent.ID})[0]
	f.exec(`INSERT INTO {s}.content_interaction_counts (tenant_id,content_kind,content_id,favorites) VALUES ($1,'gallery',$2,3),($1,'video',$3,7)`, tenant, cid(1), cid(4))
	// A version's reactions do not replace the content's aggregate.
	f.exec(`INSERT INTO {s}.content_interaction_counts (tenant_id,content_kind,content_id,content_version_id,favorites) VALUES ($1,'gallery',$2,$3,50)`, tenant, cid(2), vid(2, 1))
	f.add(Membership{EntityID: child.ID, ContentID: cid(1)}, Membership{EntityID: parent.ID, ContentID: cid(2)}, Membership{EntityID: parent.ID, ContentID: cid(3)},
		Membership{EntityID: tag.ID, ContentID: cid(1)}, Membership{EntityID: tag.ID, ContentID: cid(4)}, Membership{EntityID: picked.ID, ContentID: cid(2)})
	counts := func(id EntityID) string {
		d, err := f.s.Detail(f.ctx, id, "")
		if err != nil {
			t.Fatal(err)
		}
		s := ""
		for _, c := range d.Counts {
			s += fmt.Sprintf("%s/%s=%d ", c.ContentKind, c.Language, c.Count)
		}
		return s
	}
	if got := counts(parent.ID); got != "gallery/=2 gallery/en=2 gallery/es=1 " {
		t.Fatalf("parent %s", got)
	}
	if got := counts(tag.ID); got != "gallery/=1 gallery/en=1 gallery/es=1 video/=1 video/en=1 video/ja=1 " {
		t.Fatalf("tag %s", got)
	}
	covers, _ := f.s.Entities(f.ctx, []EntityID{tag.ID, picked.ID, child.ID})
	if c := covers[0].Cover; c == nil || c.ContentID != cid(4) || !c.Auto {
		t.Fatalf("auto cover %+v", c)
	}
	// Videos' locked view hides covers: a premium video's is no entity's.
	f.exec(`UPDATE {s}.content_facets SET access_level = 'tier' WHERE content_id = $1`, cid(4))
	if got, _ := f.s.Entities(f.ctx, []EntityID{tag.ID}); got[0].Cover == nil || got[0].Cover.ContentID != cid(1) {
		t.Fatalf("auto cover of a premium video %+v", got[0].Cover)
	}
	if c := covers[1].Cover; c == nil || c.ContentID != cid(1) || c.Auto {
		t.Fatalf("picked cover %+v", c)
	}
	if c := covers[2].Cover; c == nil || c.ContentID != cid(1) {
		t.Fatalf("child cover %+v", c)
	}
	// A deleted entity loses its counts; a removed member stops counting.
	if _, err := f.s.DeleteEntities(f.ctx, []EntityID{child.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RemoveMemberships(f.ctx, []Membership{{EntityID: tag.ID, ContentID: cid(4)}}); err != nil {
		t.Fatal(err)
	}
	if got := counts(parent.ID); got != "gallery/=1 gallery/en=1 " {
		t.Fatalf("parent after %s", got)
	}
	if got := counts(tag.ID); got != "gallery/=1 gallery/en=1 gallery/es=1 " {
		t.Fatalf("tag after %s", got)
	}

}

// A entity counts the content it owns as well as its members, each item once
// per language it serves and once overall, never once per version or twice
// for being both.
func TestRuntimeCountsOwnedContent(t *testing.T) {
	f := newFixture(t)
	ch := f.create(EntityInput{Kind: "channel", Slug: "studio", OwnerUserID: "owner"})[0]
	f.content("video", 1, live("en"), live("es"))
	f.content("video", 2, version{languages: []string{"en", "es"}, live: &past})
	f.content("video", 3, live("en"))
	f.exec(`UPDATE {s}.content_facets SET owner_entity_id = $1 WHERE content_id = ANY($2)`, string(ch.ID), []string{cid(1), cid(2)})
	f.add(Membership{EntityID: ch.ID, ContentID: cid(2)}, Membership{EntityID: ch.ID, ContentID: cid(3)})
	solo := f.create(EntityInput{Kind: "channel", Slug: "solo", OwnerUserID: "owner"})[0]
	f.content("video", 4, live("en"))
	f.exec(`UPDATE {s}.content_facets SET owner_entity_id = $1 WHERE content_id = $2`, string(solo.ID), cid(4))
	f.exec(`INSERT INTO {s}.content_interaction_counts (tenant_id,content_kind,content_id,favorites) VALUES ($1,'video',$2,5)`, tenant, cid(1))
	// Automatic covers pick from owned content too: a channel with no members has one.
	covers, err := f.s.Entities(f.ctx, []EntityID{ch.ID, solo.ID})
	if err != nil {
		t.Fatal(err)
	}
	if c := covers[0].Cover; c == nil || c.ContentID != cid(1) || !c.Auto {
		t.Fatalf("channel cover %+v", c)
	}
	if c := covers[1].Cover; c == nil || c.ContentID != cid(4) || !c.Auto {
		t.Fatalf("owned-only channel cover %+v", c)
	}
	d, err := f.s.Detail(f.ctx, ch.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	got := ""
	for _, c := range d.Counts {
		got += fmt.Sprintf("%s/%s=%d ", c.ContentKind, c.Language, c.Count)
	}
	if got != "video/=3 video/en=3 video/es=2 " {
		t.Fatalf("counts %s", got)
	}
	page, err := f.s.List(f.ctx, ListOptions{Kind: "channel", Slug: "studio"})
	if err != nil || len(page.Entities) != 1 || page.Entities[0].Count != 3 {
		t.Fatalf("card %+v %v", page.Entities, err)
	}
}

// Listings filter, resolve names in the request language and page every
// order by cursor without repeating or dropping an entity.
func TestList(t *testing.T) {
	f := newFixture(t)
	f.content("gallery", 1, live("en"))
	f.content("gallery", 2, live("es"))
	var in []EntityInput
	for i, n := range []string{"delta", "Alpha", "charlie", "bravo", "echo", "Écho2", "9lives"} {
		in = append(in, EntityInput{Kind: "tag", Slug: fmt.Sprintf("t%d", i), Names: []Name{canonical("en", n)}})
	}
	in = append(in, EntityInput{Kind: "tag", Slug: "spanish", Names: []Name{canonical("es", "Zeta")}, Text: []Text{{Language: "en", SortKey: "aaa"}}})
	tags := f.create(in...)
	f.add(Membership{EntityID: tags[0].ID, ContentID: cid(1)}, Membership{EntityID: tags[0].ID, ContentID: cid(2)}, Membership{EntityID: tags[4].ID, ContentID: cid(2)})
	walk := func(opts ListOptions) []string {
		t.Helper()
		var out []string
		for i := 0; ; i++ {
			page, err := f.s.List(f.ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, g := range page.Entities {
				out = append(out, g.Name)
			}
			if opts.Cursor = page.NextCursor; opts.Cursor == "" || i > 10 {
				return out
			}
		}
	}
	if got := walk(ListOptions{Kind: "tag", Language: "en", Limit: 3}); !slices.Equal(got, []string{"9lives", "Zeta", "Alpha", "bravo", "charlie", "delta", "echo", "Écho2"}) {
		t.Fatalf("by name %v", got)
	}
	if got := walk(ListOptions{Kind: "tag", Sort: SortCount, Limit: 2}); got[0] != "delta" || got[1] != "echo" || len(got) != 8 {
		t.Fatalf("by count %v", got)
	}
	if got := walk(ListOptions{Kind: "tag", Sort: SortNewest, Limit: 3}); len(got) != 8 || len(slices.Compact(slices.Sorted(slices.Values(got)))) != 8 {
		t.Fatalf("newest %v", got)
	}
	if got := walk(ListOptions{Kind: "tag", Language: "en", Letter: "e"}); !slices.Equal(got, []string{"echo", "Écho2"}) {
		t.Fatalf("letter e %v", got)
	}
	if got := walk(ListOptions{Kind: "tag", Language: "en", Letter: "#"}); !slices.Equal(got, []string{"9lives"}) {
		t.Fatalf("letter # %v", got)
	}
	page, err := f.s.List(f.ctx, ListOptions{Kind: "tag", Slug: "t0", Language: "es"})
	if err != nil || len(page.Entities) != 1 {
		t.Fatal(page, err)
	}
	if c := page.Entities[0]; c.Count != 1 || c.Code == "" || c.Path != "/es/tags/"+c.Code+"/delta" || c.NameLanguage != "en" {
		t.Fatalf("card %+v", c)
	}
	if page, _ = f.s.List(f.ctx, ListOptions{Kind: "tag", Slug: "t0"}); page.Entities[0].Count != 2 || page.Entities[0].Path != "/en/tags/"+page.Entities[0].Code+"/delta" {
		t.Fatalf("all languages %+v", page.Entities[0])
	}
	if page, _ = f.s.List(f.ctx, ListOptions{Kind: "tag", Slug: "t0", Language: "en"}); page.Entities[0].Path != "/en/tags/"+page.Entities[0].Code+"/delta" {
		t.Fatalf("the default language is prefixed too: %+v", page.Entities[0])
	}
	// Numbered pages read the same order and carry the total; a cursor page carries none.
	var numbered []string
	for n := 1; n <= 3; n++ {
		page, err := f.s.List(f.ctx, ListOptions{Kind: "tag", Language: "en", Limit: 3, Page: n})
		if err != nil || page.Total == nil || *page.Total != 8 {
			t.Fatalf("page %d: %+v %v", n, page.Total, err)
		}
		for _, g := range page.Entities {
			numbered = append(numbered, g.Name)
		}
	}
	if !slices.Equal(numbered, []string{"9lives", "Zeta", "Alpha", "bravo", "charlie", "delta", "echo", "Écho2"}) {
		t.Fatalf("numbered by name %v", numbered)
	}
	if page, _ := f.s.List(f.ctx, ListOptions{Kind: "tag", Language: "en", Letter: "e", Page: 1}); page.Total == nil || *page.Total != 2 {
		t.Fatalf("letter e total %+v", page.Total)
	}
	if page, _ := f.s.List(f.ctx, ListOptions{Kind: "tag", Limit: 3}); page.Total != nil {
		t.Fatalf("cursor page total %+v", page.Total)
	}
	for name, opts := range map[string]ListOptions{"kind": {Kind: "studio"}, "sort": {Sort: "random"}, "letter": {Letter: "ab"}, "cursor": {Cursor: "!"}, "parent": {Parent: "x"},
		"page": {Page: -1}, "page and cursor": {Page: 2, Cursor: "x"}, "deep page": {Page: 51, Limit: 200},
		"overflowing page": {Page: 92233720368547759, Limit: 100}} {
		if _, err := f.s.List(f.ctx, opts); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.s.List(f.ctx, ListOptions{Kind: "tag", Page: 50, Limit: 200}); err != nil {
		t.Errorf("last allowed page: %v", err)
	}

	// Parent and scope filters; detail carries parent, scope and children.
	series := f.create(EntityInput{Kind: "series", Slug: "fate", Names: []Name{canonical("en", "Fate")}})[0]
	sub := f.create(EntityInput{Kind: "series", Slug: "zero", ParentID: series.ID, Names: []Name{canonical("en", "Zero")}},
		EntityInput{Kind: "character", Slug: "saber", ScopeID: series.ID, Names: []Name{canonical("en", "Saber")}})
	if got := walk(ListOptions{Parent: series.ID}); !slices.Equal(got, []string{"Zero"}) {
		t.Fatalf("children %v", got)
	}
	if got := walk(ListOptions{Kind: "character", Scope: series.ID}); !slices.Equal(got, []string{"Saber"}) {
		t.Fatalf("scoped %v", got)
	}
	d, err := f.s.Detail(f.ctx, series.ID, "en")
	if err != nil || len(d.Children) != 1 || d.Children[0].ID != sub[0].ID || d.Path == "" {
		t.Fatalf("detail %+v %v", d, err)
	}
	if d, err = f.s.Detail(f.ctx, sub[1].ID, "en"); err != nil || d.Scope == nil || d.Scope.Name != "Fate" || d.Parent != nil {
		t.Fatalf("character detail %+v %v", d, err)
	}
	if _, err := f.s.DeleteEntities(f.ctx, []EntityID{sub[1].ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Detail(f.ctx, sub[1].ID, "en"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted detail: %v", err)
	}
}

func TestCountSortedEntityPages(t *testing.T) {
	f := newFixture(t)
	f.content("gallery", 1, live("en"))
	f.content("gallery", 2, live("es"))
	f.content("video", 3, live("en"))
	tags := f.create(
		EntityInput{Kind: "tag", Slug: "zulu", Names: []Name{canonical("en", "Zulu"), canonical("es", "Zeta")}, Text: []Text{{Language: "en", SortKey: "aaa"}}},
		EntityInput{Kind: "tag", Slug: "alpha", Names: []Name{canonical("es", "Alpha")}},
		EntityInput{Kind: "tag", Slug: "unnamed"},
		EntityInput{Kind: "tag", Slug: "bravo", Names: []Name{canonical("en", "Bravo")}},
	)
	f.add(
		Membership{EntityID: tags[0].ID, ContentID: cid(1)},
		Membership{EntityID: tags[0].ID, ContentID: cid(2)},
		Membership{EntityID: tags[1].ID, ContentID: cid(1)},
		Membership{EntityID: tags[1].ID, ContentID: cid(3)},
		Membership{EntityID: tags[2].ID, ContentID: cid(3)},
	)
	for _, tc := range []struct {
		name, language, kind string
	}{
		{"all", "", ""},
		{"english galleries", "en", "gallery"},
		{"spanish galleries", "es", "gallery"},
		{"english videos", "en", "video"},
		{"no matching counts", "ja", "video"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := ListOptions{Kind: "tag", Language: tc.language, ContentKind: tc.kind, Sort: SortName}
			all, err := f.s.List(f.ctx, opts)
			if err != nil || len(all.Entities) != len(tags) {
				t.Fatalf("unpaged cards: %+v, %v", all, err)
			}
			// The name-sorted query supplies complete cards independently of
			// count pagination, including localized paths and zero-count entities.
			want := all.Entities
			slices.SortFunc(want, func(a, b EntityCard) int {
				if a.Count != b.Count {
					return cmp.Compare(b.Count, a.Count)
				}
				return cmp.Compare(a.ID, b.ID)
			})
			opts.Sort, opts.Limit = SortCount, 1
			for i := 0; i <= len(want); i++ {
				opts.Cursor = ""
				opts.Page = i + 1
				got, err := f.s.List(f.ctx, opts)
				if err != nil || got.Total == nil || *got.Total != len(want) {
					t.Fatalf("numbered page %d: %+v, %v", i+1, got, err)
				}
				if !reflect.DeepEqual(got.Entities, want[i:min(i+1, len(want))]) {
					t.Fatalf("numbered page %d: %+v", i+1, got.Entities)
				}
			}
			opts.Page = 0
			for i := range want {
				got, err := f.s.List(f.ctx, opts)
				if err != nil || got.Total != nil || !reflect.DeepEqual(got.Entities, want[i:i+1]) {
					t.Fatalf("cursor page %d: %+v, %v", i+1, got, err)
				}
				if (got.NextCursor == "") != (i == len(want)-1) {
					t.Fatalf("cursor ended at page %d: %q", i+1, got.NextCursor)
				}
				opts.Cursor = got.NextCursor
			}
		})
	}
}

func TestNumberedEntityTotals(t *testing.T) {
	f := newFixture(t)
	series := f.create(EntityInput{Kind: "series", Slug: "parent"})[0]
	made := f.create(
		EntityInput{Kind: "tag", Slug: "first", Names: []Name{canonical("en", "Alpha"), canonical("es", "Alfa"), alias("en", "Needle")}},
		EntityInput{Kind: "tag", Slug: "needle-slug"},
		EntityInput{Kind: "tag", Slug: "internal", Internal: true, Names: []Name{canonical("en", "Needle")}},
		EntityInput{Kind: "tag", Slug: "deleted"},
		EntityInput{Kind: "series", Slug: "child", ParentID: series.ID},
		EntityInput{Kind: "character", Slug: "scoped", ScopeID: series.ID},
	)
	if _, err := f.s.DeleteEntities(f.ctx, []EntityID{made[3].ID}); err != nil {
		t.Fatal(err)
	}
	other, err := New(Options{Pool: f.pool, Schema: f.schema, Tenant: "other", Kinds: config()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.CreateEntities(f.ctx, []EntityInput{{Kind: "tag", Slug: "needle-foreign"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		opts ListOptions
		want []EntityID
	}{
		{"public", ListOptions{}, []EntityID{series.ID, made[0].ID, made[1].ID, made[4].ID, made[5].ID}},
		{"kind", ListOptions{Kind: "tag"}, []EntityID{made[0].ID, made[1].ID}},
		{"query", ListOptions{Kind: "tag", Query: " NEEDLE "}, []EntityID{made[0].ID, made[1].ID}},
		{"management query", ListOptions{Query: "needle", IncludeInternal: true}, []EntityID{made[0].ID, made[1].ID, made[2].ID}},
		{"slug", ListOptions{Slug: "first"}, []EntityID{made[0].ID}},
		{"parent", ListOptions{Parent: series.ID}, []EntityID{made[4].ID}},
		{"scope", ListOptions{Scope: series.ID}, []EntityID{made[5].ID}},
		{"deleted", ListOptions{Kind: "tag", Deleted: true, IncludeInternal: true}, []EntityID{made[3].ID}},
		{"language", ListOptions{Kind: "tag", Language: "es", ContentKind: "gallery"}, []EntityID{made[0].ID, made[1].ID}},
		{"blank letter", ListOptions{Kind: "tag", Letter: " "}, []EntityID{made[0].ID, made[1].ID}},
		{"letter", ListOptions{Kind: "tag", Letter: " A "}, []EntityID{made[0].ID}},
		{"nonletter", ListOptions{Kind: "tag", Letter: "#"}, []EntityID{made[1].ID}},
		{"empty", ListOptions{Query: "absent"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, sort := range []Sort{SortName, SortCount, SortNewest} {
				opts := tc.opts
				opts.Limit, opts.Sort = 1, sort
				var ids []EntityID
				for page := 1; page <= len(tc.want)+1; page++ {
					opts.Page = page
					got, err := f.s.List(f.ctx, opts)
					if err != nil || got.Total == nil || *got.Total != len(tc.want) {
						t.Fatalf("%s page %d: %+v, %v", sort, page, got, err)
					}
					wantLen := 0
					if page <= len(tc.want) {
						wantLen = 1
					}
					if len(got.Entities) != wantLen {
						t.Fatalf("%s page %d: got %d groups, want %d", sort, page, len(got.Entities), wantLen)
					}
					for _, entity := range got.Entities {
						ids = append(ids, entity.ID)
					}
				}
				if !slices.Equal(slices.Sorted(slices.Values(ids)), slices.Sorted(slices.Values(tc.want))) {
					t.Fatalf("%s: got %v, want %v", sort, ids, tc.want)
				}
			}
		})
	}
}

// Entity kinds are keyword documents: one per language, titled by the
// language's canonical name, others as aliases; backfill lists active entities.
func TestKeywordDocuments(t *testing.T) {
	f := newFixture(t)
	g := f.create(EntityInput{Kind: "tag", Slug: "a", Names: []Name{canonical("en", "Colored"), alias("en", "Colour"), canonical("es", "Coloreado")}},
		EntityInput{Kind: "tag", Slug: "b"}, EntityInput{Kind: "artist", Slug: "c", Names: []Name{canonical("en", "Shindol")}})
	if _, err := f.s.DeleteEntities(f.ctx, []EntityID{g[2].ID}); err != nil {
		t.Fatal(err)
	}
	refs := []contentref.ContentRef{contentref.New(tenant, "tag", string(g[0].ID)), contentref.New(tenant, "tag", string(g[1].ID))}
	docs, err := f.s.BuildDocuments(f.ctx, "tag", "es", refs)
	if err != nil || len(docs) != 1 || docs[0].Title != "Coloreado" || !slices.Equal(docs[0].Aliases, []string{"Colored", "Colour"}) {
		t.Fatalf("documents %+v %v", docs, err)
	}
	if _, err := f.s.BuildDocuments(f.ctx, "gallery", "en", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("content kind: %v", err)
	}
	listed, next, done, err := f.s.ListDocuments(f.ctx, "artist", "en", "", 10)
	if err != nil || len(listed) != 0 || next != "" || !done {
		t.Fatalf("deleted artist listed: %v %v", listed, err)
	}
	listed, _, done, err = f.s.ListDocuments(f.ctx, "tag", "en", "", 1)
	if err != nil || len(listed) != 1 || done {
		t.Fatalf("tag page %v %v %v", listed, done, err)
	}
}

// An edge added while a merge retires one of its entities waits for the merge
// and is refused, rather than staying attached to the retired entity.
func TestEdgeToAEntityBeingMergedIsRefused(t *testing.T) {
	f := newFixture(t)
	g := f.create(EntityInput{Kind: "series", Slug: "from"}, EntityInput{Kind: "series", Slug: "into"}, EntityInput{Kind: "series", Slug: "peer"})
	from, into, peer := g[0], g[1], g[2]
	merge, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer merge.Rollback(f.ctx)
	entities := pgx.Identifier{f.schema, "content_entities"}.Sanitize()
	if _, err := merge.Exec(f.ctx, `SELECT 1 FROM `+entities+` WHERE id = $1 FOR UPDATE`, string(from.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := merge.Exec(f.ctx, `UPDATE `+entities+` SET state = 'merged', merged_into = $2 WHERE id = $1`, string(from.ID), string(into.ID)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.s.AddEdges(f.ctx, []Edge{{From: from.ID, Relation: "related", To: peer.ID}}) }()
	select {
	case err := <-done:
		t.Fatalf("the edge did not wait for the merge: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := merge.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrNotFound) {
		t.Fatalf("edge to a merged group: %v", err)
	}
}

// raceAfter runs first in one transaction and, while it is open, second in
// another. Once second has either returned or is waiting on first's locks,
// first commits; second commits if it succeeded. It returns second's error.
func (f *fixture) raceAfter(first, second func(pgx.Tx) error) error {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	a, err := f.pool.Begin(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer a.Rollback(ctx)
	if err := first(a); err != nil {
		f.t.Fatal(err)
	}
	b, err := f.pool.Begin(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer b.Rollback(ctx)
	pid := b.Conn().PgConn().PID()
	done := make(chan error, 1)
	go func() { done <- second(b) }()
	defer func() {
		cancel()
		select {
		case <-done:
		default:
		}
	}()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	returned := false
wait:
	for {
		select {
		case err = <-done:
			returned = true
			break wait
		case <-tick.C:
			var blocked bool
			if err := f.pool.QueryRow(ctx, "SELECT cardinality(pg_blocking_pids($1)) > 0", pid).Scan(&blocked); err != nil {
				f.t.Fatal(err)
			}
			if blocked {
				break wait
			}
		case <-ctx.Done():
			f.t.Fatal("second transaction neither returned nor blocked")
		}
	}
	if err := a.Commit(ctx); err != nil {
		f.t.Fatal(err)
	}
	if !returned {
		err = <-done
	}
	if err == nil {
		if err := b.Commit(ctx); err != nil {
			f.t.Fatal(err)
		}
	}
	return err
}

// hierarchyFaults lists entities on a parent cycle and active entities under a
// parent that is not active.
func (f *fixture) hierarchyFaults() []string {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, fmt.Sprintf(`WITH RECURSIVE up(start, id) AS (
 SELECT id, parent_id FROM %[1]s WHERE parent_id IS NOT NULL
 UNION
 SELECT up.start, g.parent_id FROM up JOIN %[1]s g ON g.id = up.id WHERE g.parent_id IS NOT NULL)
SELECT 'cycle ' || start FROM up WHERE start = id
UNION ALL
SELECT 'orphan ' || c.id FROM %[1]s c JOIN %[1]s p ON p.id = c.parent_id WHERE c.state = 'active' AND p.state <> 'active'`,
		pgx.Identifier{f.schema, "content_entities"}.Sanitize()))
	if err != nil {
		f.t.Fatal(err)
	}
	faults, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		f.t.Fatal(err)
	}
	return faults
}

// Topology writers serialize: the second validates against the first's
// committed change instead of the state both started from.
func TestConcurrentHierarchyChangesStayValid(t *testing.T) {
	reparent := func(f *fixture, id, parent EntityID) func(pgx.Tx) error {
		return func(tx pgx.Tx) error {
			_, err := f.s.bound(tx).UpdateEntities(f.ctx, []EntityUpdate{{ID: id, ParentID: &parent}})
			return err
		}
	}
	tests := map[string]func(f *fixture) (first, second func(pgx.Tx) error){
		"opposite reparents": func(f *fixture) (first, second func(pgx.Tx) error) {
			g := f.create(EntityInput{Kind: "series", Slug: "a"}, EntityInput{Kind: "series", Slug: "b"})
			return reparent(f, g[0].ID, g[1].ID), reparent(f, g[1].ID, g[0].ID)
		},
		"import against a reparent": func(f *fixture) (first, second func(pgx.Tx) error) {
			g := f.create(EntityInput{Kind: "series", Slug: "a"}, EntityInput{Kind: "series", Slug: "b"})
			return reparent(f, g[0].ID, g[1].ID), func(tx pgx.Tx) error {
				return f.s.ImportEntities(f.ctx, tx, []ImportEntity{{ID: g[1].ID, Kind: "series", Slug: "b", ParentID: g[0].ID}}, false)
			}
		},
		// Merging a into b moves c under b while x, b's parent, moves under c.
		"merge against a reparent": func(f *fixture) (first, second func(pgx.Tx) error) {
			g := f.create(EntityInput{Kind: "series", Slug: "a"}, EntityInput{Kind: "series", Slug: "x"})
			a, x := g[0], g[1]
			g = f.create(EntityInput{Kind: "series", Slug: "b", ParentID: x.ID}, EntityInput{Kind: "series", Slug: "c", ParentID: a.ID})
			b, c := g[0], g[1]
			return reparent(f, x.ID, c.ID), func(tx pgx.Tx) error {
				_, err := f.s.bound(tx).Merge(f.ctx, []Merge{{From: a.ID, Into: b.ID}})
				return err
			}
		},
		"create under a parent being deleted": func(f *fixture) (first, second func(pgx.Tx) error) {
			p := f.create(EntityInput{Kind: "series", Slug: "p"})[0]
			return func(tx pgx.Tx) error {
					_, err := f.s.bound(tx).DeleteEntities(f.ctx, []EntityID{p.ID})
					return err
				}, func(tx pgx.Tx) error {
					_, err := f.s.bound(tx).CreateEntities(f.ctx, []EntityInput{{Kind: "series", Slug: "child", ParentID: p.ID}})
					return err
				}
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			first, second := setup(f)
			if err := f.raceAfter(first, second); !errors.Is(err, ErrInvalid) {
				t.Errorf("second writer: %v", err)
			}
			if faults := f.hierarchyFaults(); len(faults) > 0 {
				t.Errorf("committed hierarchy faults %v", faults)
			}
		})
	}
}

// Cycle detection has no depth cutoff.
func TestDeepHierarchy(t *testing.T) {
	f := newFixture(t)
	chain := make([]EntityInput, 100)
	for i := range chain {
		chain[i] = EntityInput{ID: EntityID(contentref.NewID()), Kind: "series", Slug: fmt.Sprintf("s%d", i)}
		if i > 0 {
			chain[i].ParentID = chain[i-1].ID
		}
	}
	f.create(chain...)
	top, bottom := chain[0].ID, chain[len(chain)-1].ID
	if _, err := f.s.UpdateEntities(f.ctx, []EntityUpdate{{ID: top, ParentID: &bottom}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("closing a 100-group cycle: %v", err)
	}
	root := f.create(EntityInput{Kind: "series", Slug: "root"})[0].ID
	if _, err := f.s.UpdateEntities(f.ctx, []EntityUpdate{{ID: top, ParentID: &root}}); err != nil {
		t.Fatalf("deepening an acyclic chain: %v", err)
	}
	if faults := f.hierarchyFaults(); len(faults) > 0 {
		t.Fatalf("hierarchy faults %v", faults)
	}
}

// A membership write and a hierarchy change racing either way leave the
// stored facets equal to the effective entities, and neither deadlocks.
func TestMembershipsRaceHierarchyChanges(t *testing.T) {
	update := func(f *fixture, u EntityUpdate) func(pgx.Tx) error {
		return func(tx pgx.Tx) error {
			_, err := f.s.bound(tx).UpdateEntities(f.ctx, []EntityUpdate{u})
			return err
		}
	}
	add := func(f *fixture, g EntityID) func(pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return f.s.bound(tx).AddMemberships(f.ctx, []Membership{{EntityID: g, ContentID: cid(1)}})
		}
	}
	merge := func(f *fixture, from, into EntityID) func(pgx.Tx) error {
		return func(tx pgx.Tx) error {
			_, err := f.s.bound(tx).Merge(f.ctx, []Merge{{From: from, Into: into}})
			return err
		}
	}
	deleted := StateDeleted
	tests := map[string]func(f *fixture) (first, second func(pgx.Tx) error){
		"ancestor deleted then membership added": func(f *fixture) (first, second func(pgx.Tx) error) {
			a := f.create(EntityInput{Kind: "series", Slug: "a"})[0]
			c := f.create(EntityInput{Kind: "series", Slug: "c", ParentID: a.ID})[0]
			return update(f, EntityUpdate{ID: a.ID, State: &deleted}), add(f, c.ID)
		},
		"membership added then ancestor deleted": func(f *fixture) (first, second func(pgx.Tx) error) {
			a := f.create(EntityInput{Kind: "series", Slug: "a"})[0]
			c := f.create(EntityInput{Kind: "series", Slug: "c", ParentID: a.ID})[0]
			return add(f, c.ID), update(f, EntityUpdate{ID: a.ID, State: &deleted})
		},
		"ancestor reparented then content groups set": func(f *fixture) (first, second func(pgx.Tx) error) {
			r := f.create(EntityInput{Kind: "series", Slug: "r1"}, EntityInput{Kind: "series", Slug: "r2"})
			a := f.create(EntityInput{Kind: "series", Slug: "a", ParentID: r[0].ID})[0]
			c := f.create(EntityInput{Kind: "series", Slug: "c", ParentID: a.ID})[0]
			return update(f, EntityUpdate{ID: a.ID, ParentID: &r[1].ID}), func(tx pgx.Tx) error {
				return f.s.bound(tx).SetContentEntities(f.ctx, []ContentEntities{{ContentID: cid(1), Kinds: []string{"series"}, Memberships: []Membership{{EntityID: c.ID}}}})
			}
		},
		"ancestor deleted then memberships imported": func(f *fixture) (first, second func(pgx.Tx) error) {
			a := f.create(EntityInput{Kind: "series", Slug: "a"})[0]
			c := f.create(EntityInput{Kind: "series", Slug: "c", ParentID: a.ID})[0]
			return update(f, EntityUpdate{ID: a.ID, State: &deleted}), func(tx pgx.Tx) error {
				return f.s.ImportMemberships(f.ctx, tx, []ContentEntities{{ContentID: cid(1), Kinds: []string{"series"}, Memberships: []Membership{{EntityID: c.ID, ContentID: cid(1)}}}})
			}
		},
		"membership on a child then its parent merged": func(f *fixture) (first, second func(pgx.Tx) error) {
			g := f.create(EntityInput{Kind: "series", Slug: "a"}, EntityInput{Kind: "series", Slug: "b"})
			c := f.create(EntityInput{Kind: "series", Slug: "c", ParentID: g[0].ID})[0]
			return add(f, c.ID), merge(f, g[0].ID, g[1].ID)
		},
		"parent merged then membership on its child": func(f *fixture) (first, second func(pgx.Tx) error) {
			g := f.create(EntityInput{Kind: "series", Slug: "a"}, EntityInput{Kind: "series", Slug: "b"})
			c := f.create(EntityInput{Kind: "series", Slug: "c", ParentID: g[0].ID})[0]
			return merge(f, g[0].ID, g[1].ID), add(f, c.ID)
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.content("gallery", 1, live("en"))
			first, second := setup(f)
			if err := f.raceAfter(first, second); err != nil {
				t.Fatalf("second writer: %v", err)
			}
			want, err := f.s.EffectiveEntities(f.ctx, f.pool, []string{vid(1, 0)})
			if err != nil {
				t.Fatal(err)
			}
			if got := f.facetEntities()[vid(1, 0)]; !slices.Equal(got, want[vid(1, 0)]) {
				t.Errorf("stored facets %v, effective %v", got, want[vid(1, 0)])
			}
		})
	}
}
