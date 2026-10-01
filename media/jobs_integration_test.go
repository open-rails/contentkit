package media_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// later is the fixture's jobs at a clock past the grace and temp periods.
func (f *fixture) later() *media.Jobs {
	f.t.Helper()
	j, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Locker: s3test.Locker(f.t, f.env.Store),
		Processes: f.q, Now: func() time.Time { return time.Now().Add(72 * time.Hour) }})
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

func (f *fixture) exists(key string) bool {
	f.t.Helper()
	_, err := f.env.Store.Head(context.Background(), key)
	if err != nil && !errors.Is(err, media.ErrNotFound) {
		f.t.Fatal(err)
	}
	return err == nil
}

func (f *fixture) object(key, body string) {
	f.t.Helper()
	if _, err := f.env.Store.Put(context.Background(), key, strings.NewReader(body), int64(len(body)), media.PutOptions{}); err != nil {
		f.t.Fatal(err)
	}
}

// The sweep collects by manifest reference: unexpected public names at once
// (purged), unreferenced blobs and temp/ once past their periods, nothing
// the manifest keeps.
func TestSweep(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.gallery(1, 2)
	item, _ := f.reg.Item(g)
	ctx := context.Background()
	old, _ := item.Blob(blobOf(png(100)))
	f.put(g, "originals/000.png", "image/png", png(7)) // replaces page 0: its old blob is unreferenced
	f.produce(g)
	stray, _ := item.Blob(blobOf([]byte("stray")))
	f.object(stray, "stray")
	stale, _ := item.Public("cover-999.webp")
	f.object(stale, "stale")
	f.object(item.TempPrefix()+"u-1", "temp")
	cover, _ := item.Public("cover-230.webp")

	res, err := f.jobs.Sweep(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	if res.Wait <= 0 || !slices.Equal(res.Deleted, []string{stale}) || !f.exists(old) || !f.exists(cover) {
		t.Fatalf("within grace: %+v", res)
	}
	if urls := f.purges(); len(urls) != 1 || !strings.HasSuffix(urls[0], "/public/cover-999.webp") || !strings.HasPrefix(urls[0], "https://"+mediaHost+"/v1/") {
		t.Fatalf("purged %v", urls)
	}
	res, err = f.later().Sweep(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	if res.Wait != 0 || f.exists(old) || f.exists(stray) || f.exists(item.TempPrefix()+"u-1") || !f.exists(cover) {
		t.Fatalf("past grace: %+v", res)
	}
	m, _, _ := f.ms.Get(ctx, g)
	for _, b := range m.Blobs() {
		if key, _ := item.Blob(b); !f.exists(key) {
			t.Fatalf("swept referenced %s", key)
		}
	}
	// Uploads never committed are swept once past the grace period.
	other := f.ref("gallery", 2)
	_, blob := f.upload(other, "originals/1.png", "image/png", png(1))
	otherItem, _ := f.reg.Item(other)
	key, _ := otherItem.Blob(blob)
	if _, err := f.later().Sweep(ctx, other); err != nil || f.exists(key) {
		t.Fatalf("abandoned upload kept: %v", err)
	}
}

// Expose hides an item (public/ deleted and purged at once, nothing public
// pending) and unhides it (public presets pending, the worker asked).
func TestExpose(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.gallery(1, 1)
	item, _ := f.reg.Item(g)
	ctx := context.Background()
	cover, _ := item.Public("cover-460.webp")
	if !f.exists(cover) {
		t.Fatal("no public cover")
	}
	if status, body, _ := f.fetch(f.reg.PublicURL(g, "cover-460.webp")); status != 200 || body != string(png(199)) {
		t.Fatalf("public cover %d %q", status, body)
	}
	f.res.set(cid(1), access.Resolution{})
	if err := f.jobs.Expose(ctx, g); err != nil {
		t.Fatal(err)
	}
	m, _, _ := f.ms.Get(ctx, g)
	if !m.Hidden || f.exists(cover) {
		t.Fatalf("hidden %v, cover kept %v", m.Hidden, f.exists(cover))
	}
	if urls := f.purges(); len(urls) != 2 {
		t.Fatalf("purged %v", urls)
	}
	f.q.take()
	f.visible(1)
	if err := f.jobs.Expose(ctx, g); err != nil {
		t.Fatal(err)
	}
	m, _, _ = f.ms.Get(ctx, g)
	if c, _ := m.Get("cover.png"); m.Hidden || !slices.Equal(c.Pending, []string{"cover"}) {
		t.Fatalf("unhidden %v pending %v", m.Hidden, c.Pending)
	}
	if jobs := f.q.take(); len(jobs) != 1 || jobs[0].Ref != g {
		t.Fatalf("render asked %+v", jobs)
	}
	if err := f.jobs.Expose(ctx, f.ref("gallery", 2)); err != nil {
		t.Fatalf("an item without a manifest: %v", err)
	}
}

// A new item exposes nothing public before its first visibility decision
// (audit): Create writes a hidden manifest, and the first commit resolves it
// anonymously, so a draft's cover renders nothing public until Expose
// unhides it; a visible item's first commit unhides it.
func TestNewItemStartsHidden(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	g := f.ref("gallery", 1)
	item, _ := f.reg.Item(g)
	cover, _ := item.Public("cover-460.webp")
	f.res.set(cid(1), access.Resolution{}) // a draft
	if m, err := f.ms.Create(ctx, g); err != nil || !m.Hidden {
		t.Fatalf("created %+v %v", m, err)
	}
	m := f.put(g, "cover.png", "image/png", png(1))
	if c, _ := m.Get("cover.png"); !m.Hidden || c.Pending != nil {
		t.Fatalf("draft's first commit: hidden %v pending %v", m.Hidden, c.Pending)
	}
	f.produce(g)
	if f.exists(cover) {
		t.Fatal("a draft's cover is public")
	}
	f.visible(1)
	if err := f.jobs.Expose(ctx, g); err != nil {
		t.Fatal(err)
	}
	f.produce(g)
	if !f.exists(cover) {
		t.Fatal("the published item's cover is not public")
	}

	v := f.ref("gallery", 2)
	f.visible(2)
	if _, err := f.ms.Create(ctx, v); err != nil {
		t.Fatal(err)
	}
	if m := f.put(v, "cover.png", "image/png", png(2)); m.Hidden {
		t.Fatal("a visible item's first commit kept it hidden")
	}
}

// Purge deletes an item's folder now; Regenerate visits every item of a
// kind; SweepOrphans finds folders the host no longer has.
func TestPurgeRegenerateOrphans(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	f.visible(2)
	a, b := f.gallery(1, 1), f.gallery(2, 1)
	ctx := context.Background()
	f.q.take()
	if n, err := f.jobs.Regenerate(ctx, "gallery", "thumb"); err != nil || n != 2 {
		t.Fatalf("regenerate %d %v", n, err)
	}
	if jobs := f.q.take(); len(jobs) != 2 || jobs[0].Preset != "thumb" {
		t.Fatalf("regenerate jobs %+v", jobs)
	}
	rep, err := f.later().SweepOrphans(ctx, media.OrphanSweep{Kind: "gallery", Delete: true,
		Exists: func(_ context.Context, ids []string) (map[string]bool, error) {
			return map[string]bool{cid(1): true}, nil
		}})
	if err != nil || rep.Folders != 2 || len(rep.Orphans) != 1 || rep.Orphans[0].ID != cid(2) || !rep.Orphans[0].Deleted {
		t.Fatalf("orphans %+v %v", rep, err)
	}
	if _, _, err := f.ms.Get(ctx, b); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("orphan kept: %v", err)
	}
	f.purges() // the orphan's public cover
	if err := f.jobs.Purge(ctx, media.Deletion{Ref: a}); err != nil {
		t.Fatal(err)
	}
	item, _ := f.reg.Item(a)
	for o, err := range f.env.Store.List(ctx, item.Prefix()) {
		t.Fatalf("purge kept %s %v", o.Key, err)
	}
	if urls := f.purges(); len(urls) != 2 {
		t.Fatalf("purged %v", urls)
	}
}

// riverHost composes media's jobs into a host River client on a fresh
// schema and starts it.
func riverHost(t *testing.T, jobs *media.Jobs, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	cfg := &river.Config{Schema: schema, FetchPollInterval: 100 * time.Millisecond, FetchCooldown: 50 * time.Millisecond}
	client, err := riverhelpers.New(ctx, pool, cfg, jobs.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.StopAndCancel(ctx)
	})
	return schema
}

// The worker's relays reach the host's hooks through its media queue:
// ItemReady in a host transaction once the item settles, PurgePublic with
// URLs; the host deletes items from its own transaction.
func TestHostRelays(t *testing.T) {
	env := s3test.Open(t)
	pool := pgtest.Pool(t, nil)
	var mu sync.Mutex
	var ready []media.Readiness
	f := newFixtureOn(t, env, func(c *media.Config) {
		c.Hooks.ItemReady = func(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef, r media.Readiness) error {
			var one int
			if err := tx.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
				return err
			}
			mu.Lock()
			ready = append(ready, r)
			mu.Unlock()
			return nil
		}
	})
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Registry: f.reg, Locker: s3test.Locker(t, env.Store), Pool: pool, Processes: f.q})
	if err != nil {
		t.Fatal(err)
	}
	schema := riverHost(t, jobs, pool)
	host, err := media.NewHostQueue(pool, f.reg, schema, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	f.visible(1)
	g := f.ref("gallery", 1)
	f.put(g, "originals/1.png", "image/png", png(1))
	ctx := context.Background()
	if err := host.Ready(ctx, g); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	mu.Lock()
	if len(ready) != 0 {
		t.Fatalf("a processing item was reported ready: %+v", ready)
	}
	mu.Unlock()
	f.produce(g)
	if err := host.Ready(ctx, g); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "ItemReady", func() bool { mu.Lock(); defer mu.Unlock(); return len(ready) == 1 && ready[0].Ready() })
	item, _ := f.reg.Item(g)
	key, _ := item.Public("cover-460.webp")
	if err := host.Purge(ctx, []string{key}); err != nil {
		t.Fatal(err)
	}
	if urls := f.purges(); len(urls) != 1 || urls[0] != f.reg.PublicURL(g, "cover-460.webp") {
		t.Fatalf("purge relay %v", urls)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return jobs.DeleteItemsTx(ctx, tx, media.Deletion{Ref: g}) }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "folder deletion", func() bool {
		_, _, err := f.ms.Get(ctx, g)
		return errors.Is(err, media.ErrNotFound)
	})
}
