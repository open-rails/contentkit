package catalog

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/media/versions"
)

// Keep real candidate selection, row locks, rechecks and catalog writes. The
// storage boundary fails a chosen prefix instantly, making scan starvation
// deterministic without creating hundreds of objects or relying on timeouts.
type purgeCursorStorage struct {
	VersionMedia
	failed   map[string]bool
	attempts map[string]int
}

func (*purgeCursorStorage) ResumeDeletes(context.Context, int) (int, error)           { return 0, nil }
func (*purgeCursorStorage) DeleteTx(context.Context, pgx.Tx, []versions.Target) error { return nil }
func (m *purgeCursorStorage) Delete(_ context.Context, targets []versions.Target) error {
	var result error
	for _, target := range targets {
		m.attempts[target.Version]++
		if m.failed[target.Version] {
			result = errors.Join(result, fmt.Errorf("storage unavailable for %s", target.Version))
		}
	}
	return result
}

func seedPurgeCursor(f *fixture, mode string, count int) []string {
	f.t.Helper()
	at := f.now().Add(-DefaultDeletedRetention - time.Hour)
	parents := count
	if mode == "one-parent" {
		parents = 1
	}
	state := "published"
	if mode == "parents" {
		state = "deleted"
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO `+f.table("content_items")+` (tenant_id,id,kind,state,deleted_at)
 SELECT $1,'cursor-parent-'||lpad(n::text,4,'0'),'gallery',$2,$3 FROM generate_series(1,$4)n`, tenant, state, at, parents); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO `+f.table("content_versions")+` (tenant_id,id,content_id,languages,state,deleted_at)
 SELECT $1,'cursor-version-'||lpad(n::text,4,'0'),'cursor-parent-'||lpad((CASE WHEN $4 THEN 1 ELSE n END)::text,4,'0'),ARRAY['en'],'deleted',$2
 FROM generate_series(1,$3)n`, tenant, at, count, mode == "one-parent"); err != nil {
		f.t.Fatal(err)
	}
	if mode != "parents" {
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO `+f.table("content_versions")+` (tenant_id,id,content_id,languages,state)
 SELECT $1,'live-version-'||lpad(n::text,4,'0'),'cursor-parent-'||lpad(n::text,4,'0'),ARRAY['en'],'published' FROM generate_series(1,$2)n`, tenant, parents); err != nil {
			f.t.Fatal(err)
		}
	}
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("cursor-version-%04d", i+1)
	}
	return ids
}

func TestPurgeCursorPassesMoreThanOneFailedBatch(t *testing.T) {
	for _, mode := range []string{"parents", "versions", "one-parent"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, nil)
			ids := seedPurgeCursor(f, mode, purgeBatch+2)
			storage := &purgeCursorStorage{failed: map[string]bool{}, attempts: map[string]int{}}
			for _, id := range ids[:len(ids)-1] {
				storage.failed[id] = true
			}
			if err := f.s.BindMedia(Media{Versions: storage}); err != nil {
				t.Fatal(err)
			}
			if n, err := f.s.PurgeDeleted(f.ctx); n != 1 || err == nil {
				t.Fatalf("later healthy target starved: erased=%d err=%v", n, err)
			}
			for _, id := range ids {
				if storage.attempts[id] != 1 {
					t.Fatalf("target %s attempted %d times in one scan", id, storage.attempts[id])
				}
			}
			var remaining int
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.table("content_versions")+` WHERE tenant_id=$1 AND id=ANY($2)`, tenant, ids).Scan(&remaining); err != nil || remaining != len(ids)-1 {
				t.Fatalf("failed rows were lost: remaining=%d err=%v", remaining, err)
			}
			if n, err := f.s.PurgeDeleted(f.ctx); n != 0 || err == nil {
				t.Fatalf("retry scan: erased=%d err=%v", n, err)
			}
			for _, id := range ids[:len(ids)-1] {
				if storage.attempts[id] != 2 {
					t.Fatalf("failed target %s not retried exactly once: %d", id, storage.attempts[id])
				}
			}
			if storage.attempts[ids[len(ids)-1]] != 1 {
				t.Fatal("completed target was retried")
			}
		})
	}
}

func TestPurgeVersionCursorPassesLockedPrefixInOneParent(t *testing.T) {
	f := newFixture(t, nil)
	ids := seedPurgeCursor(f, "one-parent", purgeBatch+2)
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	held, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback(f.ctx)
	if _, err = held.Exec(ctx, `SELECT id FROM `+f.table("content_versions")+` WHERE tenant_id=$1 AND id=ANY($2) ORDER BY id FOR UPDATE`, tenant, ids[:len(ids)-1]); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.PurgeDeleted(ctx); err != nil || n != 1 {
		t.Fatalf("locked version prefix blocked healthy suffix: %d %v", n, err)
	}
	if err = held.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.PurgeDeleted(ctx); err != nil || n != len(ids)-1 {
		t.Fatalf("released locks permanently skipped: %d %v", n, err)
	}
}

func TestPurgeScanDoesNotHideFatalErrorsAfterStorageFailures(t *testing.T) {
	store := &Store{}
	fatal := errors.New("catalog transaction failed")
	attempts := 0
	_, err := store.scanPurge(t.Context(), func(_ context.Context, after string) (int, string, error) {
		attempts++
		if after == "" {
			return 0, "first", &purgeStorageError{errors.New("folder unavailable")}
		}
		return 0, "second", fatal
	})
	var storage *purgeStorageError
	if attempts != 2 || !errors.Is(err, fatal) || errors.As(err, &storage) {
		t.Fatalf("fatal error treated as a recoverable storage-only scan: attempts=%d err=%v", attempts, err)
	}
}
