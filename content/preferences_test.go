package content

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/testauth"
)

// localeID is the content id of work n's language route: every language
// edition is its own route id, which the resolver maps to its work.
func localeID(n int, lang string) string {
	return cid(n*1000 + map[string]int{"en": 1, "ja": 2}[lang])
}

var localeWorks = map[string]string{
	localeID(42, "en"): cid(42), localeID(42, "ja"): cid(42), localeID(7, "en"): cid(7), localeID(9, "en"): cid(9),
}

func newPreferenceRuntime(t *testing.T) *Runtime {
	t.Helper()
	res := &fakeResolver{}
	for _, id := range []string{localeID(42, "en"), localeID(42, "ja"), localeID(7, "en"), localeID(9, "en")} {
		res.setCanonical("gallery", id, localeWorks[id])
	}
	res.set("tag", cid(9), true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"gallery", "tag"}})
	return rt
}

func mustReact(t *testing.T, rt *Runtime, actor access.Actor, kind, id string, value int16) {
	t.Helper()
	if _, err := rt.reactions.react(context.Background(), actor, kind, id, value); err != nil {
		t.Fatalf("react %s/%s=%d: %v", kind, id, value, err)
	}
}

func mustFavorite(t *testing.T, rt *Runtime, actor access.Actor, kind, id string, add bool) {
	t.Helper()
	var err error
	if add {
		err = favErr(rt.favorites.add(context.Background(), actor, kind, id))
	} else {
		err = favErr(rt.favorites.remove(context.Background(), actor, kind, id))
	}
	if err != nil {
		t.Fatalf("favorite %s/%s add=%v: %v", kind, id, add, err)
	}
}

// exported is everything a full resync sends, in revision order.
func exported(t *testing.T, rt *Runtime) []Preference {
	t.Helper()
	var out []Preference
	if _, err := rt.ResyncPreferences(context.Background(), func(_ context.Context, page []Preference) error {
		out = append(out, page...)
		return nil
	}); err != nil {
		t.Fatalf("ResyncPreferences: %v", err)
	}
	return out
}

// row reads one stored (value, revision), or ok=false.
func row(t *testing.T, rt *Runtime, table, user, id string) (value int16, revision int64, ok bool) {
	t.Helper()
	err := rt.store.pool.QueryRow(context.Background(), `SELECT value, revision FROM `+table+` WHERE tenant_id = $1 AND user_id = $2 AND content_id = $3`,
		rt.tenant, user, id).Scan(&value, &revision)
	if err != nil {
		return 0, 0, false
	}
	return value, revision, true
}

// Concurrent writes from different language routes land on one work row, and
// every committed change takes a strictly higher revision, across pools.
func TestPreferences_ConcurrentLocaleRoutesShareOneOrderedPreference(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	actor := access.Actor{ID: "u1", Kind: "user"}
	langs := []string{localeID(42, "en"), localeID(42, "ja")}
	values := []int16{1, -1, 0, 1}
	var wg sync.WaitGroup
	errs := make(chan error, 48)
	for i := 0; i < 48; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := rt.reactions.react(ctx, actor, "gallery", langs[i%2], values[i%4])
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent react: %v", err)
		}
	}
	got := exported(t, rt)
	if len(got) != 1 || got[0].ContentID != cid(42) || got[0].Version() != "" || got[0].Axis != PreferenceAxisReaction {
		t.Fatalf("exported = %+v, want one work row", got)
	}
	value, prev, _ := row(t, rt, rt.store.t.reactions, "u1", cid(42))
	if got[0].Value != value || got[0].Revision != prev {
		t.Fatalf("export %+v != stored (%d, %d)", got[0], value, prev)
	}
	c := countsOf(t, rt, ref("gallery", cid(42)))
	if c.Likes != b2i(value == 1) || c.Dislikes != b2i(value == -1) {
		t.Fatalf("work counts = %+v, want the one vote value %d implies", c, value)
	}

	// A cached sequence block would hand an older revision to the second pool.
	second, err := pgxpool.New(ctx, rt.store.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	other, err := New(ctx, Options{Pool: second, Schema: rt.schema, Tenant: testTenant, Auth: testauth.Auth{Authz: allowAll{}},
		Resolver: rt.resolver, ContentKinds: []string{"gallery"}})
	if err != nil {
		t.Fatal(err)
	}
	next := int16(1)
	if value == next {
		next = -1
	}
	for _, backend := range []*Runtime{other, rt, other, rt} {
		mustReact(t, backend, actor, "gallery", localeID(42, "en"), next)
		_, rev, _ := row(t, rt, rt.store.t.reactions, "u1", cid(42))
		if rev <= prev {
			t.Fatalf("revision %d did not increase past %d across backends", rev, prev)
		}
		prev, next = rev, -next
	}
}

// A no-op refreshes no revision and creates no row.
func TestPreferences_NoOpAllocatesNothing(t *testing.T) {
	rt := newPreferenceRuntime(t)
	actor := access.Actor{ID: "u1", Kind: "user"}
	mustReact(t, rt, actor, "gallery", localeID(42, "en"), 1)
	_, before, _ := row(t, rt, rt.store.t.reactions, "u1", cid(42))
	mustReact(t, rt, actor, "gallery", localeID(42, "ja"), 1) // same value from the other route
	if _, after, _ := row(t, rt, rt.store.t.reactions, "u1", cid(42)); after != before {
		t.Fatalf("a no-op moved the revision %d -> %d", before, after)
	}
	mustReact(t, rt, actor, "gallery", localeID(7, "en"), 0)
	mustFavorite(t, rt, actor, "gallery", localeID(7, "en"), false)
	if _, _, ok := row(t, rt, rt.store.t.reactions, "u1", cid(7)); ok {
		t.Fatal("neutral with no prior reaction stored a row")
	}
	if _, _, ok := row(t, rt, rt.store.t.favorites, "u1", cid(7)); ok {
		t.Fatal("unfavorite with no prior favorite stored a row")
	}
}

// Unfavorite keeps the row at 0 with a newer revision; every read and count
// treats it as absent, and re-favoriting restarts the bookmark's age.
func TestPreferences_FavoritesAreSoftRows(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	actor := access.Actor{ID: "u1", Kind: "user"}
	work := ref("gallery", cid(42))
	mustFavorite(t, rt, actor, "gallery", localeID(42, "en"), true)
	_, r1, _ := row(t, rt, rt.store.t.favorites, "u1", cid(42))
	mustFavorite(t, rt, actor, "gallery", localeID(42, "ja"), false)
	v, r2, ok := row(t, rt, rt.store.t.favorites, "u1", cid(42))
	if !ok || v != 0 || r2 <= r1 {
		t.Fatalf("unfavorite row = (%d, %d) ok=%v, want value 0 above revision %d", v, r2, ok, r1)
	}
	if favs, _ := rt.IsFavorited(ctx, "u1", []contentref.ContentRef{work}); favs[work.Key()] {
		t.Fatal("IsFavorited reports an unfavorited row")
	}
	if items, err := rt.ListFavorites(ctx, "u1", 0, 0); err != nil || len(items) != 0 {
		t.Fatalf("ListFavorites = %+v err=%v, want empty", items, err)
	}
	if c := countsOf(t, rt, work); c.Favorites != 0 {
		t.Fatalf("counts = %+v, want no favorite", c)
	}
	mustFavorite(t, rt, actor, "gallery", localeID(42, "ja"), false)
	if _, again, _ := row(t, rt, rt.store.t.favorites, "u1", cid(42)); again != r2 {
		t.Fatal("a repeated unfavorite took a revision")
	}
	time.Sleep(10 * time.Millisecond)
	mustFavorite(t, rt, actor, "gallery", localeID(42, "en"), true)
	mustFavorite(t, rt, actor, "gallery", localeID(7, "en"), true)
	items, err := rt.ListFavorites(ctx, "u1", 0, 0)
	if err != nil || len(items) != 2 || items[0].ContentID != cid(7) {
		t.Fatalf("ListFavorites = %+v err=%v", items, err)
	}
	if c := countsOf(t, rt, work); c.Favorites != 1 {
		t.Fatalf("counts after re-favorite = %+v", c)
	}
	if err := rt.EraseSubjects(ctx, []string{"u1"}); err != nil {
		t.Fatal(err)
	}
	if c := countsOf(t, rt, work); c.Favorites != 0 {
		t.Fatalf("counts after erasure = %+v", c)
	}
}

// A rolled-back source transaction leaves no row to export.
func TestPreferences_RolledBackMutationExportsNothing(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	if _, err := rt.store.pool.Exec(ctx, `DROP TABLE `+rt.store.t.counts); err != nil {
		t.Fatal(err)
	}
	err := reactErr(rt.reactions.react(ctx, access.Actor{ID: "u1", Kind: "user"}, "gallery", localeID(42, "en"), 1))
	var pgErr interface{ SQLState() string }
	if err == nil || !errors.As(err, &pgErr) {
		t.Fatalf("react with a broken rollup = %v, want the transaction to fail", err)
	}
	var n int
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.reactions).Scan(&n); err != nil || n != 0 {
		t.Fatalf("reactions = %d err=%v, want none after the rollback", n, err)
	}
}

func TestPreferences_NilSenderCannotCheckpoint(t *testing.T) {
	for _, name := range []string{"sync", "resync"} {
		t.Run(name, func(t *testing.T) {
			rt := newPreferenceRuntime(t)
			send := rt.SyncPreferences
			if name == "resync" {
				send = rt.ResyncPreferences
			}
			ctx := t.Context()
			if _, err := send(ctx, nil); err == nil {
				t.Fatal("enabled export accepted a missing sender with no rows")
			}
			actor := access.Actor{ID: "u1", Kind: "user"}
			mustReact(t, rt, actor, "gallery", localeID(42, "en"), 1)
			mustFavorite(t, rt, actor, "gallery", localeID(42, "en"), true)
			if _, err := send(ctx, nil); err == nil {
				t.Fatal("enabled export accepted a missing sender with rows")
			}
			var checkpoints int
			if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.preferenceSync).Scan(&checkpoints); err != nil || checkpoints != 0 {
				t.Fatalf("missing sender recorded checkpoints=%d, error=%v", checkpoints, err)
			}
			var sent []Preference
			report, err := send(ctx, func(_ context.Context, page []Preference) error {
				sent = append(sent, page...)
				return nil
			})
			if err != nil || report.Sent != 2 || len(sent) != 2 {
				t.Fatalf("valid retry lost preferences: report=%+v, sent=%+v, error=%v", report, sent, err)
			}
		})
	}
}

// A failing send records no checkpoint: the next sync re-sends.
func TestPreferences_FailedSyncKeepsTheWatermark(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	mustReact(t, rt, access.Actor{ID: "u1"}, "gallery", localeID(42, "en"), 1)
	if _, err := rt.SyncPreferences(ctx, func(context.Context, []Preference) error { return errors.New("sink down") }); err == nil {
		t.Fatal("a failing send must surface")
	}
	var n int
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.preferenceSync).Scan(&n); err != nil || n != 0 {
		t.Fatalf("checkpoints after a failed sync = %d err=%v", n, err)
	}
	var sent int
	if _, err := rt.SyncPreferences(ctx, func(_ context.Context, page []Preference) error { sent += len(page); return nil }); err != nil || sent != 1 {
		t.Fatalf("retry sent %d err=%v", sent, err)
	}
}

// The restore floor lifts the sequence above a retained sink's revisions,
// only ever advances, and fails closed outside the safe range.
func TestPreferences_RevisionFloorOnlyAdvances(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	actor := access.Actor{ID: "u1", Kind: "user"}
	mustReact(t, rt, actor, "gallery", localeID(42, "en"), 1)
	floor := int64(1) << 40
	got, err := rt.SeedPreferenceRevisionFloor(ctx, floor)
	if err != nil || got < floor {
		t.Fatalf("seed floor = %d err=%v, want >= %d", got, err, floor)
	}
	mustReact(t, rt, actor, "gallery", localeID(42, "en"), -1)
	_, rev, _ := row(t, rt, rt.store.t.reactions, "u1", cid(42))
	if rev <= floor {
		t.Fatalf("revision %d does not exceed the floor %d", rev, floor)
	}
	if again, err := rt.SeedPreferenceRevisionFloor(ctx, floor-1_000_000); err != nil || again < rev {
		t.Fatalf("re-seeding lowered the floor to %d err=%v", again, err)
	}
	if _, err := rt.SeedPreferenceRevisionFloor(ctx, maxPreferenceRevisionFloor+1); err == nil {
		t.Fatal("an unsafe floor was accepted")
	}
}

// A sync holds no connection across send, so a sender writing through the
// runtime's only connection (an outbox in the same database) completes.
func TestPreferences_SyncSendsWithoutHoldingAConnection(t *testing.T) {
	base := newPreferenceRuntime(t)
	mustReact(t, base, access.Actor{ID: "u1"}, "gallery", localeID(42, "en"), 1)
	cfg := base.store.pool.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	rt, err := New(context.Background(), Options{Pool: pool, Schema: base.schema, Tenant: base.tenant, Auth: testauth.Auth{Authz: allowAll{}},
		Resolver: &fakeResolver{}, ContentKinds: []string{"gallery", "tag"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sent := 0
	send := func(ctx context.Context, page []Preference) error {
		if n := pool.Stat().AcquiredConns(); n != 0 {
			t.Errorf("sync holds %d connections across send", n)
		}
		sent += len(page)
		_, err := pool.Exec(ctx, "SELECT 1")
		return err
	}
	for i := 0; i < 2; i++ {
		if _, err := rt.SyncPreferences(ctx, send); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}
	if sent != 2 {
		t.Fatalf("sent %d, want the row on both syncs (overlap window)", sent)
	}
}

// Anonymous rows and comment threads never export; another tenant sees
// nothing.
func TestPreferences_ExportScope(t *testing.T) {
	res := &fakeResolver{}
	res.set("gallery", cid(42), true, true)
	rt, pool := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"gallery"}})
	ctx := context.Background()
	u1 := access.Actor{ID: "u1", Kind: "user"}
	mustReact(t, rt, access.Actor{IP: "10.0.0.1", Anonymous: true}, "gallery", cid(42), 1)
	commentID := cid(501)
	if _, err := pool.Exec(ctx, `INSERT INTO `+rt.store.t.comments+` (id, tenant_id, content_kind, content_id, content_version_id, user_id, body) VALUES ($3, $1, 'gallery', $2, '', 'a', 'hi')`, testTenant, cid(42), commentID); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.comments.reactTx(ctx, u1, commentID, 1); err != nil {
		t.Fatal(err)
	}
	if got := exported(t, rt); len(got) != 0 {
		t.Fatalf("exported = %+v, want nothing", got)
	}
	mustReact(t, rt, u1, "gallery", cid(42), -1)
	if got := exported(t, rt); len(got) != 1 || got[0].ContentID != cid(42) || got[0].Value != -1 || got[0].ActorID != "u1" {
		t.Fatalf("exported = %+v, want the work dislike", got)
	}
	other, err := New(ctx, Options{Pool: pool, Schema: rt.schema, Tenant: "other", Auth: testauth.Auth{Authz: allowAll{}}, Resolver: rt.resolver})
	if err != nil {
		t.Fatal(err)
	}
	if got := exported(t, other); len(got) != 0 {
		t.Fatalf("another tenant exports %+v", got)
	}
}
