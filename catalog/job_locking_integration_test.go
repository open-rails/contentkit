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

func TestPurgeVersionsSerializesBeforeFirstOfferIntegration(t *testing.T) {
	b := newCatalogJobBarrier(t, `SELECT v.id, v.content_id, c.kind`, true)
	f := catalogJobFixture(t, b)
	id := f.create("staff", gallery("Purge before first offer", "en", "ja"))
	vid := f.read("staff", "main.test", id)[0].Versions[1].ID
	if err := f.s.DeleteVersions(f.ctx, f.as("staff"), []VersionKey{{ContentID: id, VersionID: vid}}); err != nil {
		t.Fatal(err)
	}
	f.advance(DefaultDeletedRetention + time.Hour)
	seller, err := offers.New(offers.Options{Pool: f.s.pool, Schema: f.schema, Tenant: tenant, Kinds: config(),
		Billing: catalogOfferBilling{}, Gate: f.s.accessG})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	purged := make(chan error, 1)
	go func() {
		_, err := f.s.eraseVersions(context.WithValue(ctx, catalogJobTraceKey{}, "job"), f.now().Add(-DefaultDeletedRetention))
		purged <- err
	}()
	select {
	case <-b.reached:
	case <-ctx.Done():
		t.Fatal("purger did not lock its selected version")
	}
	offered := make(chan error, 1)
	go func() {
		_, err := seller.Put(context.WithValue(ctx, catalogJobTraceKey{}, "writer"), []offers.Put{{
			Target: offers.Target{Kind: offers.ContentTarget, ID: id}, Terms: offers.Terms{Amount: 1_000_000, Currency: "USD"},
		}})
		offered <- err
	}()
	committed := waitCatalogWriter(t, f, b, offered)
	b.release()
	if err := <-purged; err != nil {
		t.Fatal(err)
	}
	if !committed {
		if err := <-offered; err != nil {
			t.Fatal(err)
		}
	}
	if committed {
		t.Error("first offer committed before the purger released its parent lock")
	}
	var sold bool
	var versions, activeOffers int
	if err := f.pool.QueryRow(ctx, `SELECT ever_sold,
 (SELECT count(*) FROM `+f.table("content_versions")+` WHERE tenant_id=$1 AND id=$3),
 (SELECT count(*) FROM `+f.table("content_offers")+` WHERE tenant_id=$1 AND target_id=$2 AND state='active')
 FROM `+f.table("content_items")+` WHERE tenant_id=$1 AND id=$2`, tenant, id, vid).Scan(&sold, &versions, &activeOffers); err != nil {
		t.Fatal(err)
	}
	if !sold || versions != 0 || activeOffers != 1 {
		t.Fatalf("purge-first serialization: sold=%t retained versions=%d active offers=%d", sold, versions, activeOffers)
	}
}

func TestPublishDueSkipsConcurrentVersionEditIntegration(t *testing.T) {
	b := newCatalogJobBarrier(t, `SELECT languages, traits, label, state`, false)
	f := catalogJobFixture(t, b)
	id := f.create("staff", gallery("Editing scheduled version", "en"))
	f.publish("staff", id, nil)
	at := f.now().Add(time.Hour)
	vids, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionInput: VersionInput{
		Languages: []string{"ja"}, State: StatePublished, PublishAt: &at,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	f.advance(2 * time.Hour)
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	edited := make(chan error, 1)
	actor := f.as("staff")
	go func() {
		_, err := f.s.PutVersions(context.WithValue(ctx, catalogJobTraceKey{}, "job"), actor,
			[]VersionPut{{ContentID: id, VersionID: vids[0], VersionInput: VersionInput{Label: ptr("edited")}}})
		edited <- err
	}()
	select {
	case <-b.reached:
	case <-ctx.Done():
		t.Fatal("version edit did not reach the parent-before-version boundary")
	}
	if n, err := f.s.PublishDue(ctx); err != nil || n != 0 {
		t.Fatalf("publisher did not skip the parent owned by a version edit: %d %v", n, err)
	}
	b.release()
	if err := <-edited; err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.PublishDue(ctx); err != nil || n != 1 {
		t.Fatalf("publisher permanently skipped the edited version: %d %v", n, err)
	}
}

func TestPurgeVersionsSkipsLockedRowsIntegration(t *testing.T) {
	f := newFixture(t, nil)
	var parents, vids []string
	for range 3 {
		id := f.create("staff", gallery("Concurrent purge", "en", "ja"))
		vid := f.read("staff", "main.test", id)[0].Versions[1].ID
		if err := f.s.DeleteVersions(f.ctx, f.as("staff"), []VersionKey{{ContentID: id, VersionID: vid}}); err != nil {
			t.Fatal(err)
		}
		parents, vids = append(parents, id), append(vids, vid)
	}
	f.advance(DefaultDeletedRetention + time.Hour)
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	parent, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Rollback(f.ctx)
	if _, err := parent.Exec(ctx, `SELECT id FROM `+f.table("content_items")+` WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenant, parents[0]); err != nil {
		t.Fatal(err)
	}
	version, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer version.Rollback(f.ctx)
	if _, err := version.Exec(ctx, `SELECT id FROM `+f.table("content_versions")+` WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenant, vids[1]); err != nil {
		t.Fatal(err)
	}
	cutoff := f.now().Add(-DefaultDeletedRetention)
	if n, err := f.s.eraseVersions(ctx, cutoff); err != nil || n != 1 {
		t.Fatalf("purge must skip both locked rows while processing another parent: %d %v", n, err)
	}
	if err := parent.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := version.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.eraseVersions(ctx, cutoff); err != nil || n != 2 {
		t.Fatalf("purge permanently skipped released locks: %d %v", n, err)
	}
}

func TestPurgeVersionsBoundsBatchIntegration(t *testing.T) {
	f := newFixture(t, nil)
	id := f.create("staff", gallery("Version backlog", "en"))
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO `+f.table("content_versions")+` (tenant_id,id,content_id,languages,state,deleted_at)
 SELECT $1,'purge-' || n,$2,ARRAY['ja'],'deleted',$3::timestamptz FROM generate_series(1,$4) n`, tenant, id, f.now().Add(-time.Hour), purgeBatch+1); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.eraseVersions(f.ctx, f.now()); err != nil || n != purgeBatch {
		t.Fatalf("first version purge batch: %d %v", n, err)
	}
	if n, err := f.s.eraseVersions(f.ctx, f.now()); err != nil || n != 1 {
		t.Fatalf("remaining version purge batch: %d %v", n, err)
	}
}

func TestCatalogJobsSkipLockedVersionParentBatchesIntegration(t *testing.T) {
	for _, test := range []struct {
		name  string
		limit int
		state string
	}{
		{name: "publish", limit: 500, state: "published"},
		{name: "purge", limit: purgeBatch, state: "deleted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, nil)
			at := f.now().Add(-DefaultDeletedRetention - time.Hour)
			if _, err := f.pool.Exec(f.ctx, `INSERT INTO `+f.table("content_items")+` (tenant_id,id,kind,state,publish_at,published_at)
 SELECT $1,'batch-parent-' || lpad(n::text,4,'0'),'gallery','published',$2,$2 FROM generate_series(1,$3) n`, tenant, at, test.limit+1); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(f.ctx, `INSERT INTO `+f.table("content_versions")+` (tenant_id,id,content_id,languages,state,publish_at,deleted_at)
 SELECT $1,'batch-version-' || lpad(n::text,4,'0'),'batch-parent-' || lpad(n::text,4,'0'),ARRAY['en'],$2,$3,$3
 FROM generate_series(1,$4) n`, tenant, test.state, at, test.limit+1); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
			defer cancel()
			locked, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer locked.Rollback(f.ctx)
			// Lock exactly one full candidate-parent batch's versions while
			// keeping the next parent's work available.
			if _, err := locked.Exec(ctx, `SELECT id FROM `+f.table("content_versions")+`
 WHERE tenant_id=$1 AND id IN (SELECT 'batch-version-' || lpad(n::text,4,'0') FROM generate_series(1,$2) n) FOR UPDATE`, tenant, test.limit); err != nil {
				t.Fatal(err)
			}
			var n int
			if test.name == "publish" {
				n, err = f.s.PublishDue(ctx)
			} else {
				n, err = f.s.eraseVersions(ctx, f.now().Add(-DefaultDeletedRetention))
			}
			if err != nil || n != 1 {
				t.Fatalf("locked versions in the first %d parents hid later available work: processed=%d err=%v", test.limit, n, err)
			}
		})
	}
}

func TestReconcileFacetsRepairsStaleAccessIntegration(t *testing.T) {
	f := newFixture(t, nil)
	inputs := make([]ContentInput, 201)
	for i := range inputs {
		inputs[i] = gallery("Reconcile access", "en")
		inputs[i].Access = &access.Rule{Level: access.PPV}
	}
	ids, err := f.s.Create(f.ctx, f.as("staff"), inputs)
	if err != nil {
		t.Fatal(err)
	}
	// Seed the exact stale projection left by the old job, including the
	// incorrect latest history row. No ordinary due publication repairs it.
	if _, err := f.pool.Exec(f.ctx, `UPDATE `+f.table("content_facets")+` SET access_level='public' WHERE tenant_id=$1`, tenant); err != nil {
		t.Fatal(err)
	}
	if err := f.s.recordAccessHistory(f.ctx, f.pool, ids); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.PublishDue(f.ctx); err != nil || n != 0 {
		t.Fatalf("unexpected due publication: %d %v", n, err)
	}
	if n, err := f.s.ReconcileFacets(f.ctx); err != nil || n != len(ids) {
		t.Fatalf("reconcile all keyset batches: %d %v", n, err)
	}
	var stale, before, after int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.table("content_facets")+` f
 JOIN LATERAL (SELECT snapshot FROM `+f.table("content_access_history")+` h WHERE h.tenant_id=f.tenant_id AND h.version_id=f.version_id ORDER BY occurred_at DESC,id DESC LIMIT 1) h ON true
 WHERE f.tenant_id=$1 AND (f.access_level <> 'ppv' OR h.snapshot->>'level' <> 'ppv')`, tenant).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("reconciliation left stale facets/history: %d %v", stale, err)
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.table("content_access_history")).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ReconcileFacets(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.table("content_access_history")).Scan(&after); err != nil || after != before {
		t.Fatalf("reconciliation split unchanged access history: before=%d after=%d err=%v", before, after, err)
	}
}
