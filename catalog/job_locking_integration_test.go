package catalog

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/offers"
)

type catalogJobTraceKey struct{}
type catalogJobQueryKey struct{}

// Pause only the job's connection. The competing writer uses the same real
// PostgreSQL database and can either commit or demonstrably wait on its lock.
type catalogJobBarrier struct {
	match   string
	after   bool
	reached chan struct{}
	resume  chan struct{}
	writer  chan uint32
	once    sync.Once
	closed  sync.Once
}

func newCatalogJobBarrier(t *testing.T, match string, after bool) *catalogJobBarrier {
	b := &catalogJobBarrier{match: match, after: after, reached: make(chan struct{}), resume: make(chan struct{}), writer: make(chan uint32, 1)}
	t.Cleanup(b.release)
	return b
}

func (b *catalogJobBarrier) release() { b.closed.Do(func() { close(b.resume) }) }

func (b *catalogJobBarrier) pause(ctx context.Context) {
	b.once.Do(func() {
		close(b.reached)
		select {
		case <-b.resume:
		case <-ctx.Done():
		}
	})
}

func (b *catalogJobBarrier) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	actor, _ := ctx.Value(catalogJobTraceKey{}).(string)
	if actor == "writer" {
		select {
		case b.writer <- conn.PgConn().PID():
		default:
		}
	}
	if actor != "job" || !strings.Contains(data.SQL, b.match) {
		return ctx
	}
	if b.after {
		return context.WithValue(ctx, catalogJobQueryKey{}, true)
	}
	b.pause(ctx)
	return ctx
}

func (b *catalogJobBarrier) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if matched, _ := ctx.Value(catalogJobQueryKey{}).(bool); matched {
		b.pause(ctx)
	}
}

func catalogJobFixture(t *testing.T, b *catalogJobBarrier) *fixture {
	return newFixture(t, func(o *Options) {
		o.Pool = pgtest.Pool(t, func(c *pgxpool.Config) { c.ConnConfig.Tracer = b })
	})
}

// waitCatalogWriter resolves the interleaving from database evidence rather
// than a delay: the writer either committed or PostgreSQL reports its blocker.
func waitCatalogWriter(t *testing.T, f *fixture, b *catalogJobBarrier, done <-chan error) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	var pid uint32
	select {
	case pid = <-b.writer:
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		return true
	case <-ctx.Done():
		t.Fatal("writer did not start")
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return true
		case <-tick.C:
			var blocked bool
			if err := f.pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1::int)) > 0`, pid).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				return false
			}
		case <-ctx.Done():
			t.Fatal("writer neither committed nor waited on a PostgreSQL lock")
		}
	}
}

func TestPublishDuePreservesConcurrentAccessIntegration(t *testing.T) {
	for _, levels := range [][2]access.Level{{access.Public, access.PPV}, {access.PPV, access.Public}} {
		t.Run(string(levels[0])+"_to_"+string(levels[1]), func(t *testing.T) {
			b := newCatalogJobBarrier(t, `INSERT INTO `, false)
			// Only the facet INSERT is reached after the job reads access in Go.
			f := catalogJobFixture(t, b)
			b.match = `INSERT INTO ` + f.table("content_facets")
			in := gallery("Scheduled access", "en")
			in.Access = &access.Rule{Level: levels[0]}
			id := f.create("staff", in)
			f.publish("staff", id, nil)
			at := f.now().Add(time.Hour)
			if _, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionInput: VersionInput{
				Languages: []string{"ja"}, State: StatePublished, PublishAt: &at,
			}}}); err != nil {
				t.Fatal(err)
			}
			f.advance(2 * time.Hour)
			ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
			defer cancel()
			job := make(chan error, 1)
			go func() {
				_, err := f.s.PublishDue(context.WithValue(ctx, catalogJobTraceKey{}, "job"))
				job <- err
			}()
			select {
			case <-b.reached:
			case <-ctx.Done():
				t.Fatal("PublishDue did not reach facet write")
			}
			writer := make(chan error, 1)
			actor := f.as("staff")
			go func() {
				writer <- f.s.Update(context.WithValue(ctx, catalogJobTraceKey{}, "writer"), actor,
					[]Update{{ID: id, ContentPatch: ContentPatch{Access: &access.Rule{Level: levels[1]}}}})
			}()
			committed := waitCatalogWriter(t, f, b, writer)
			b.release()
			if err := <-job; err != nil {
				t.Fatal(err)
			}
			if !committed {
				if err := <-writer; err != nil {
					t.Fatal(err)
				}
			}
			for _, got := range f.facets(id) {
				if got.Level != levels[1] {
					t.Errorf("PublishDue overwrote newer access: facet %s = %s, want %s", got.Version, got.Level, levels[1])
				}
			}
			var inconsistent int
			if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM `+f.table("content_facets")+` f
 JOIN LATERAL (SELECT snapshot FROM `+f.table("content_access_history")+` h WHERE h.tenant_id=f.tenant_id AND h.version_id=f.version_id ORDER BY occurred_at DESC,id DESC LIMIT 1) h ON true
 WHERE f.tenant_id=$1 AND f.content_id=$2 AND (h.snapshot->>'level' <> $3 OR f.access_level <> $3)`, tenant, id, levels[1]).Scan(&inconsistent); err != nil {
				t.Fatal(err)
			}
			if inconsistent != 0 {
				t.Errorf("%d facets or latest access-history snapshots lost the newer %s rule", inconsistent, levels[1])
			}
		})
	}
}

// PutTx does not call billing until the offer sync worker runs.
type catalogOfferBilling struct{ access.Billing }

func TestPurgeVersionsPreservesFirstOfferIntegration(t *testing.T) {
	b := newCatalogJobBarrier(t, `SELECT v.id, v.content_id, c.kind`, true)
	f := catalogJobFixture(t, b)
	id := f.create("staff", gallery("First offer", "en", "ja"))
	f.publish("staff", id, nil)
	vid := f.read("staff", "main.test", id)[0].Versions[1].ID
	if err := f.s.DeleteVersions(f.ctx, f.as("staff"), []VersionKey{{ContentID: id, VersionID: vid}}); err != nil {
		t.Fatal(err)
	}
	f.advance(DefaultDeletedRetention + time.Hour)
	seller, err := offers.New(offers.Options{Pool: f.pool, Schema: f.schema, Tenant: tenant, Kinds: config(),
		Billing: catalogOfferBilling{}, Gate: f.s.accessG})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if err := seller.PutTx(ctx, tx, []offers.Put{{Target: offers.Target{Kind: offers.ContentTarget, ID: id},
		Terms: offers.Terms{Amount: 1_000_000, Currency: "USD"}}}); err != nil {
		t.Fatal(err)
	}
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := f.s.eraseVersions(context.WithValue(ctx, catalogJobTraceKey{}, "job"), f.now().Add(-DefaultDeletedRetention))
		done <- result{n, err}
	}()
	var got result
	select {
	case <-b.reached:
		// The old query read ever_sold=false and locked only the version.
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		b.release()
		got = <-done
	case got = <-done:
		// Parent-first SKIP LOCKED can leave the in-flight sale alone.
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("purger blocked behind the first offer instead of skipping it")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	var remaining int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM `+f.table("content_versions")+` WHERE tenant_id=$1 AND id=$2`, tenant, vid).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if got.n != 0 || remaining != 1 {
		t.Fatalf("first offer lost its retained version: erased=%d remaining=%d", got.n, remaining)
	}
	if n, err := f.s.eraseVersions(ctx, f.now().Add(-DefaultDeletedRetention)); err != nil || n != 0 {
		t.Fatalf("committed first offer must retain versions: erased=%d err=%v", n, err)
	}
}
