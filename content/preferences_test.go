package content

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
)

// galleryWork is a host canonical rule: the ":<lang>" route suffix is stripped
// so every language route votes on one work; an explicit version stays; posts
// export under their own id; taxonomy stays out of the boundary entirely.
func galleryWork(r contentref.ContentRef) (contentref.ContentRef, bool) {
	switch r.ContentKind {
	case "gallery":
		id, _, _ := strings.Cut(r.ContentID, ":")
		return contentref.New(r.TenantID, "gallery", id).WithVersion(r.Version()), true
	case KindPost:
		return r, true
	}
	return contentref.ContentRef{}, false
}

func newPreferenceRuntime(t *testing.T) *Runtime {
	t.Helper()
	res := &fakeResolver{}
	for _, id := range []string{"42:en", "42:ja", "7:en", "9:en"} {
		res.set("gallery", id, true, true)
	}
	res.set("tag", "9", true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"gallery", "tag"}, Canonicalizer: ContentCanonicalizerFunc(galleryWork)})
	return rt
}

func prefKey(actor, kind, id, axis string) PreferenceKey {
	return PreferenceKey{TenantID: testTenant, ActorID: actor, ContentKind: kind, ContentID: id, Axis: axis}
}

// snapshotRows reads the table directly, in key order: the authority the API
// must agree with.
func snapshotRows(t *testing.T, rt *Runtime) []PreferenceSnapshot {
	t.Helper()
	rows, err := rt.store.pool.Query(context.Background(), `SELECT `+preferenceCols+` FROM `+rt.store.t.preferenceSnapshots+`
		WHERE tenant_id = $1 ORDER BY actor_id, content_kind, content_id, content_version_id, axis`, rt.tenant)
	if err != nil {
		t.Fatalf("read snapshots: %v", err)
	}
	defer rows.Close()
	var out []PreferenceSnapshot
	for rows.Next() {
		s, err := scanPreference(rows)
		if err != nil {
			t.Fatalf("scan snapshot: %v", err)
		}
		out = append(out, s)
	}
	return out
}

func snapshotRow(t *testing.T, rt *Runtime, key PreferenceKey) PreferenceSnapshot {
	t.Helper()
	for _, s := range snapshotRows(t, rt) {
		if s.PreferenceKey == key {
			return s
		}
	}
	t.Fatalf("no snapshot for %+v", key)
	return PreferenceSnapshot{}
}

func pending(t *testing.T, rt *Runtime) []PreferenceSnapshot {
	t.Helper()
	out, err := rt.PendingPreferences(context.Background(), PreferenceKey{}, 0)
	if err != nil {
		t.Fatalf("PendingPreferences: %v", err)
	}
	return out
}

// mustReact applies a reaction and returns the committed snapshot (nil for a
// no-op or a non-exported target).
func mustReact(t *testing.T, rt *Runtime, actor Actor, kind, id string, value int16) *PreferenceSnapshot {
	t.Helper()
	_, snap, err := rt.reactions.react(context.Background(), actor, kind, id, value)
	if err != nil {
		t.Fatalf("react %s/%s=%d: %v", kind, id, value, err)
	}
	return snap
}

func mustFavorite(t *testing.T, rt *Runtime, actor Actor, kind, id string, add bool) *PreferenceSnapshot {
	t.Helper()
	var snap *PreferenceSnapshot
	var err error
	if add {
		snap, err = rt.favorites.add(context.Background(), actor, kind, id)
	} else {
		snap, err = rt.favorites.remove(context.Background(), actor, kind, id)
	}
	if err != nil {
		t.Fatalf("favorite %s/%s add=%v: %v", kind, id, add, err)
	}
	return snap
}

// Spec case 4: concurrent writes from different language routes resolve to
// one work preference, and revisions follow the order the key lock granted.
func TestPreferences_ConcurrentLocaleRoutesShareOneOrderedPreference(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	actor := Actor{ID: "u1", Kind: "user"}
	langs := []string{"42:en", "42:ja"}
	values := []int16{1, -1, 0, 1}

	var mu sync.Mutex
	var committed []PreferenceSnapshot
	var wg sync.WaitGroup
	errs := make(chan error, 48)
	for i := 0; i < 48; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, snap, err := rt.reactions.react(ctx, actor, "gallery", langs[i%2], values[i%4])
			errs <- err
			if snap != nil {
				mu.Lock()
				committed = append(committed, *snap)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent react: %v", err)
		}
	}
	if len(committed) == 0 {
		t.Fatal("no committed transitions")
	}
	sort.Slice(committed, func(i, j int) bool { return committed[i].Revision < committed[j].Revision })
	for i, s := range committed {
		if s.ContentID != "42" || s.ContentVersionID != "" || s.Axis != PreferenceAxisReaction || s.TenantID != testTenant {
			t.Fatalf("snapshot %d is not keyed on the work: %+v", i, s)
		}
		if i > 0 {
			if s.Revision <= committed[i-1].Revision {
				t.Fatalf("revisions are not strictly increasing: %d then %d", committed[i-1].Revision, s.Revision)
			}
			if s.OccurredAt.Before(committed[i-1].OccurredAt) {
				t.Fatalf("occurred_at regressed between revisions %d and %d", committed[i-1].Revision, s.Revision)
			}
		}
	}
	last := committed[len(committed)-1]
	row := snapshotRow(t, rt, prefKey("u1", "gallery", "42", PreferenceAxisReaction))
	if row.Revision != last.Revision || row.Value != last.Value || !row.OccurredAt.Equal(last.OccurredAt) {
		t.Fatalf("snapshot %+v != highest committed transition %+v", row, last)
	}
	if got := len(snapshotRows(t, rt)); got != 1 {
		t.Fatalf("snapshots = %d, want one work row for two language routes", got)
	}
	var sourceRows int
	var storedID string
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*), min(content_id) FROM `+rt.store.t.reactions+` WHERE tenant_id = $1 AND user_id = 'u1' AND content_kind = 'gallery'`, testTenant).Scan(&sourceRows, &storedID); err != nil {
		t.Fatal(err)
	}
	if sourceRows != 1 || storedID != "42" {
		t.Fatalf("source rows = %d under %q, want one row under the work", sourceRows, storedID)
	}
	c := countsOf(t, rt, ref("gallery", "42"))
	if c.Likes != b2i(row.Value == 1) || c.Dislikes != b2i(row.Value == -1) {
		t.Fatalf("work counts = %+v, want the one vote the final value %d implies", c, row.Value)
	}

	// Alternate database backends: a cached sequence block would hand an older
	// revision to the second connection after the first committed.
	second, err := pgxpool.New(ctx, rt.store.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	other, err := New(ctx, Options{Pool: second, Schema: rt.schema, Tenant: testTenant, Identity: &fakeIdentity{}, Authz: allowAll{},
		Resolver: rt.resolver, ContentKinds: []string{"gallery"}, Canonicalizer: ContentCanonicalizerFunc(galleryWork)})
	if err != nil {
		t.Fatal(err)
	}
	prev := row.Revision
	value := int16(1)
	if row.Value == value {
		value = -1
	}
	for _, backend := range []*Runtime{other, rt, other, rt} {
		mustReact(t, backend, actor, "gallery", "42:en", value)
		next := snapshotRow(t, rt, row.PreferenceKey)
		if next.Revision <= prev {
			t.Fatalf("revision %d did not increase past %d across backends", next.Revision, prev)
		}
		prev = next.Revision
		value = -value
	}
}

// Own-state reads resolve through the same rule as the writes, so a host may
// hydrate by either the language route reference or the work reference.
func TestPreferences_OwnStateReadsUseTheSameIdentity(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	actor := Actor{ID: "u1", Kind: "user"}
	mustReact(t, rt, actor, "gallery", "42:en", 1)
	mustFavorite(t, rt, actor, "gallery", "42:ja", true)
	locale, work := ref("gallery", "42:ja"), ref("gallery", "42")
	reactions, err := rt.MyReactions(ctx, actor, []contentref.ContentRef{locale, work})
	if err != nil || reactions[locale.Key()] != 1 || reactions[work.Key()] != 1 {
		t.Fatalf("MyReactions = %+v err=%v, want the like under both keys", reactions, err)
	}
	favs, err := rt.IsFavorited(ctx, "u1", []contentref.ContentRef{locale, work})
	if err != nil || !favs[locale.Key()] || !favs[work.Key()] {
		t.Fatalf("IsFavorited = %+v err=%v, want true under both keys", favs, err)
	}
}

// Spec case 4 (second half): a no-op refreshes neither revision nor time.
func TestPreferences_NoOpAllocatesNothing(t *testing.T) {
	rt := newPreferenceRuntime(t)
	actor := Actor{ID: "u1", Kind: "user"}
	if snap := mustReact(t, rt, actor, "gallery", "42:en", 1); snap == nil {
		t.Fatal("first like committed no snapshot")
	}
	before := snapshotRow(t, rt, prefKey("u1", "gallery", "42", PreferenceAxisReaction))
	time.Sleep(10 * time.Millisecond)
	if snap := mustReact(t, rt, actor, "gallery", "42:ja", 1); snap != nil { // same value from the other route
		t.Fatalf("a no-op returned a snapshot: %+v", snap)
	}
	if after := snapshotRow(t, rt, before.PreferenceKey); !reflect.DeepEqual(before, after) {
		t.Fatalf("a no-op changed the snapshot: %+v -> %+v", before, after)
	}
}

// Spec case 1: a delayed older delivery cannot hide the newer preference.
func TestPreferences_DelayedOlderAckCannotHideNewer(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	actor := Actor{ID: "u1", Kind: "user"}
	key := prefKey("u1", "gallery", "42", PreferenceAxisReaction)

	older := mustReact(t, rt, actor, "gallery", "42:en", 1)  // like
	newer := mustReact(t, rt, actor, "gallery", "42:ja", -1) // dislike, from the other language route
	if older.Revision >= newer.Revision || newer.Value != -1 {
		t.Fatalf("revisions = %d then %d value %d", older.Revision, newer.Revision, newer.Value)
	}
	if err := rt.AcknowledgePreferences(ctx, []PreferenceAck{{PreferenceKey: key, Revision: older.Revision}}); err != nil {
		t.Fatalf("ack older: %v", err)
	}
	got := pending(t, rt)
	if len(got) != 1 || got[0].Revision != newer.Revision || got[0].Value != -1 || got[0].DeliveredRevision != older.Revision {
		t.Fatalf("after the older ack, pending = %+v, want the dislike", got)
	}
	if err := rt.AcknowledgePreferences(ctx, []PreferenceAck{{PreferenceKey: key, Revision: newer.Revision}}); err != nil {
		t.Fatalf("ack newer: %v", err)
	}
	if got := pending(t, rt); len(got) != 0 {
		t.Fatalf("after the newer ack, pending = %+v, want none", got)
	}
	if err := rt.AcknowledgePreferences(ctx, []PreferenceAck{{PreferenceKey: key, Revision: older.Revision}}); err != nil {
		t.Fatalf("late older ack: %v", err)
	}
	if row := snapshotRow(t, rt, key); row.DeliveredRevision != newer.Revision || row.Value != -1 {
		t.Fatalf("row after the late older ack = %+v", row)
	}
}

// Spec cases 2 and 7: replay keeps one identity, across a restarted process.
func TestPreferences_ReplayKeepsOneIdentity(t *testing.T) {
	rt := newPreferenceRuntime(t)
	actor := Actor{ID: "u1", Kind: "user"}
	committed := *mustReact(t, rt, actor, "gallery", "42:en", 1)

	first := pending(t, rt)
	time.Sleep(20 * time.Millisecond)
	second := pending(t, rt)
	if len(first) != 1 || len(second) != 1 || !reflect.DeepEqual(first[0], second[0]) {
		t.Fatalf("a retry minted a new identity: %+v vs %+v", first, second)
	}
	if first[0].Revision != committed.Revision || !first[0].OccurredAt.Equal(committed.OccurredAt) || first[0].Value != committed.Value {
		t.Fatalf("swept row %+v != committed snapshot %+v", first[0], committed)
	}
	restarted, err := New(context.Background(), Options{Pool: rt.store.pool, Schema: rt.schema, Tenant: testTenant, Identity: &fakeIdentity{}, Authz: allowAll{},
		Resolver: &fakeResolver{}, Canonicalizer: ContentCanonicalizerFunc(galleryWork)})
	if err != nil {
		t.Fatalf("restart runtime: %v", err)
	}
	after, err := restarted.PendingPreferences(context.Background(), PreferenceKey{}, 10)
	if err != nil || len(after) != 1 || !reflect.DeepEqual(after[0], first[0]) {
		t.Fatalf("after the restart pending = %+v err=%v, want the same snapshot", after, err)
	}
}

// Spec case 3: every transition, including removals, keeps a zero-valued
// snapshot so a delayed like cannot resurrect removed state.
func TestPreferences_RemovalsKeepZeroSnapshots(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	actor := Actor{ID: "u1", Kind: "user"}

	mustReact(t, rt, actor, "gallery", "42:en", 1)
	neutral := mustReact(t, rt, actor, "gallery", "42:en", 0) // neutral is a state, not a delete
	row := snapshotRow(t, rt, prefKey("u1", "gallery", "42", PreferenceAxisReaction))
	if row.Value != 0 || !row.Pending() || neutral == nil || neutral.Value != 0 {
		t.Fatalf("neutral snapshot = %+v (returned %+v), want a pending zero", row, neutral)
	}
	mustFavorite(t, rt, actor, "gallery", "42:ja", true)
	removed := mustFavorite(t, rt, actor, "gallery", "42:ja", false)
	fav := snapshotRow(t, rt, prefKey("u1", "gallery", "42", PreferenceAxisFavorite))
	if fav.Value != 0 || removed == nil || removed.Revision != fav.Revision {
		t.Fatalf("unfavorite snapshot = %+v, want value 0", fav)
	}
	var favRows int
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.favorites+` WHERE user_id = 'u1'`).Scan(&favRows); err != nil || favRows != 0 {
		t.Fatalf("favorite rows = %d err=%v, want the bookmark deleted while the snapshot keeps its zero", favRows, err)
	}
	mustFavorite(t, rt, actor, "gallery", "42:en", true)
	if fav = snapshotRow(t, rt, fav.PreferenceKey); fav.Value != 1 || fav.Revision <= removed.Revision {
		t.Fatalf("re-favorite snapshot = %+v, want value 1 above revision %d", fav, removed.Revision)
	}
	if c := countsOf(t, rt, ref("gallery", "42")); c.Favorites != 1 {
		t.Fatalf("favorite count = %+v, want exactly 1", c)
	}
	if got := len(pending(t, rt)); got != 2 {
		t.Fatalf("pending = %d, want both axes", got)
	}
}

// Spec case 8: a newer revision commits while the acknowledgement for the older
// one is in flight; the newer stays pending whichever order the lock grants.
func TestPreferences_UpdateRacingAcknowledgement(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	actor := Actor{ID: "u1", Kind: "user"}
	key := prefKey("u1", "gallery", "42", PreferenceAxisReaction)

	mustReact(t, rt, actor, "gallery", "42:en", 1)
	inFlight := pending(t, rt)[0]
	mustReact(t, rt, actor, "gallery", "42:en", -1) // lands while the first is in flight
	if err := rt.AcknowledgePreferences(ctx, []PreferenceAck{{PreferenceKey: key, Revision: inFlight.Revision}}); err != nil {
		t.Fatalf("ack the in-flight revision: %v", err)
	}
	got := pending(t, rt)
	if len(got) != 1 || got[0].Value != -1 || got[0].DeliveredRevision != inFlight.Revision {
		t.Fatalf("pending after the racing ack = %+v, want the newer revision still pending", got)
	}
	acked := got[0].Revision

	// True lock race: hold the snapshot row while a mutation and an
	// acknowledgement both wait on it.
	hold, err := rt.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(ctx, `SELECT 1 FROM `+rt.store.t.preferenceSnapshots+` WHERE actor_id = 'u1' AND content_id = '42' AND axis = 'reaction' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	results := make(chan error, 2)
	go func() {
		defer wg.Done()
		results <- reactErr(rt.reactions.react(ctx, actor, "gallery", "42:ja", 1))
	}()
	go func() {
		defer wg.Done()
		results <- rt.AcknowledgePreferences(ctx, []PreferenceAck{{PreferenceKey: key, Revision: acked}})
	}()
	time.Sleep(150 * time.Millisecond) // let both block on the held row
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("racing operation: %v", err)
		}
	}
	row := snapshotRow(t, rt, key)
	if row.Value != 1 || row.Revision <= acked || row.DeliveredRevision != acked {
		t.Fatalf("row after the lock race = %+v, want the newer like pending over acknowledged %d", row, acked)
	}
	if got := pending(t, rt); len(got) != 1 || got[0].Revision != row.Revision {
		t.Fatalf("pending after the lock race = %+v", got)
	}
}

// Spec case 9: a key that never gets acknowledged does not strand the rest.
func TestPreferences_FailingKeyDoesNotStarveOthers(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	for _, actor := range []string{"u1", "u2", "u3"} {
		mustReact(t, rt, Actor{ID: actor, Kind: "user"}, "gallery", "42:en", 1)
	}
	for sweep := 0; sweep < 2; sweep++ {
		var after PreferenceKey
		for {
			page, err := rt.PendingPreferences(ctx, after, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			after = page[0].PreferenceKey
			if page[0].ActorID == "u1" {
				continue // delivery keeps failing for this key
			}
			if err := rt.AcknowledgePreferences(ctx, []PreferenceAck{{PreferenceKey: page[0].PreferenceKey, Revision: page[0].Revision}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := pending(t, rt); len(got) != 1 || got[0].ActorID != "u1" {
		t.Fatalf("pending = %+v, want only the failing key", got)
	}
}

// Spec case 5: a rolled-back source transaction leaves no snapshot and no
// source row; sequence gaps are harmless.
func TestPreferences_RolledBackMutationExportsNothing(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	if _, err := rt.store.pool.Exec(ctx, `DROP TABLE `+rt.store.t.counts); err != nil {
		t.Fatal(err)
	}
	err := reactErr(rt.reactions.react(ctx, Actor{ID: "u1", Kind: "user"}, "gallery", "42:en", 1))
	var pgErr interface{ SQLState() string }
	if err == nil || !errors.As(err, &pgErr) {
		t.Fatalf("react with a broken rollup = %v, want the transaction to fail", err)
	}
	for _, table := range []string{rt.store.t.reactions, rt.store.t.preferenceSnapshots} {
		var n int
		if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s has %d rows err=%v, want none after the rollback", table, n, err)
		}
	}
	if _, err := rt.store.pool.Exec(ctx, `CREATE TABLE `+rt.store.t.counts+` (
		tenant_id text NOT NULL, content_kind text NOT NULL, content_id text NOT NULL, content_version_id text NOT NULL DEFAULT '',
		likes int NOT NULL DEFAULT 0, dislikes int NOT NULL DEFAULT 0, favorites int NOT NULL DEFAULT 0, comment_count int NOT NULL DEFAULT 0,
		updated_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (tenant_id, content_kind, content_id, content_version_id))`); err != nil {
		t.Fatal(err)
	}
	mustReact(t, rt, Actor{ID: "u1", Kind: "user"}, "gallery", "42:en", 1)
	if row := snapshotRow(t, rt, prefKey("u1", "gallery", "42", PreferenceAxisReaction)); row.Revision < 1 || row.Value != 1 {
		t.Fatalf("snapshot after recovery = %+v", row)
	}
}

// Spec case 12: anonymous IP reactions never become analytics subjects,
// declined targets stay out, another tenant sees nothing, and the source APIs
// work with export disabled.
func TestPreferences_ExportScopeAndOptOut(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	if snap := mustReact(t, rt, Actor{IP: "10.0.0.1", Anonymous: true}, "gallery", "42:en", 1); snap != nil {
		t.Fatalf("an anonymous reaction exported %+v", snap)
	}
	if snap := mustReact(t, rt, Actor{ID: "u1", Kind: "user"}, "tag", "9", 1); snap != nil {
		t.Fatalf("a declined target exported %+v", snap)
	}
	if got := snapshotRows(t, rt); len(got) != 0 {
		t.Fatalf("snapshots = %+v, want none", got)
	}
	var storedID string
	if err := rt.store.pool.QueryRow(ctx, `SELECT content_id FROM `+rt.store.t.reactions+` WHERE ip = '10.0.0.1'`).Scan(&storedID); err != nil || storedID != "42" {
		t.Fatalf("anonymous reaction stored under %q err=%v, want the work", storedID, err)
	}
	mustReact(t, rt, Actor{ID: "u1", Kind: "user"}, "gallery", "42:en", 1)
	other, err := New(ctx, Options{Pool: rt.store.pool, Schema: rt.schema, Tenant: "other", Identity: &fakeIdentity{}, Authz: allowAll{},
		Resolver: rt.resolver, Canonicalizer: ContentCanonicalizerFunc(galleryWork)})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := other.PendingPreferences(ctx, PreferenceKey{}, 10); len(got) != 0 {
		t.Fatalf("another tenant sees pending rows: %+v", got)
	}
	if err := other.AcknowledgePreferences(ctx, []PreferenceAck{{PreferenceKey: prefKey("u1", "gallery", "42", PreferenceAxisReaction), Revision: 1}}); !errors.Is(err, ErrTenant) {
		t.Fatalf("foreign-tenant ack: want ErrTenant, got %v", err)
	}

	// Export disabled (no Canonicalizer): reactions and favorites still work and
	// keep their resolver reference; nothing is exported.
	res := &fakeResolver{}
	res.set("gallery", "42:en", true, true)
	plain, pool := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"gallery"}})
	actor := Actor{ID: "u1", Kind: "user"}
	if snap := mustReact(t, plain, actor, "gallery", "42:en", 1); snap != nil {
		t.Fatalf("export disabled returned %+v", snap)
	}
	mustFavorite(t, plain, actor, "gallery", "42:en", true)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+plain.store.t.preferenceSnapshots).Scan(&n); err != nil || n != 0 {
		t.Fatalf("snapshots with export disabled = %d err=%v, want 0", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+plain.store.t.reactions+` WHERE content_id = '42:en'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("reaction rows under the resolver reference = %d err=%v, want 1", n, err)
	}
	if got, err := plain.PendingPreferences(ctx, PreferenceKey{}, 10); err != nil || len(got) != 0 {
		t.Fatalf("PendingPreferences with export disabled = %+v err=%v", got, err)
	}
	if _, err := plain.MigratePreferences(ctx, PreferenceMigrationOptions{}); err == nil {
		t.Fatal("MigratePreferences without a canonical rule was accepted")
	}
}

func TestPreferences_ScanEqualsSnapshotsAndPages(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	u1, u2 := Actor{ID: "u1", Kind: "user"}, Actor{ID: "u2", Kind: "user"}

	mustReact(t, rt, u1, "gallery", "42:en", 1)
	mustReact(t, rt, u1, "gallery", "42:en", 0)
	mustReact(t, rt, u1, "gallery", "7:en", -1)
	mustReact(t, rt, u2, "gallery", "42:ja", 1)
	for _, a := range []Actor{u1, u2} {
		mustFavorite(t, rt, a, "gallery", "7:en", true)
	}
	mustFavorite(t, rt, u2, "gallery", "7:en", false)
	truth := snapshotRows(t, rt)
	if len(truth) != 5 {
		t.Fatalf("snapshots = %d, want 5", len(truth))
	}
	for _, s := range truth[:2] {
		if err := rt.AcknowledgePreferences(ctx, []PreferenceAck{{PreferenceKey: s.PreferenceKey, Revision: s.Revision}}); err != nil {
			t.Fatal(err)
		}
	}
	truth = snapshotRows(t, rt)
	page := func(list func(context.Context, PreferenceKey, int) ([]PreferenceSnapshot, error), limit int) []PreferenceSnapshot {
		var all []PreferenceSnapshot
		var after PreferenceKey
		for {
			got, err := list(ctx, after, limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) > limit {
				t.Fatalf("page of %d exceeds the limit %d", len(got), limit)
			}
			all = append(all, got...)
			if len(got) < limit {
				return all
			}
			after = got[len(got)-1].PreferenceKey
		}
	}
	if scanned := page(rt.ScanPreferences, 2); !reflect.DeepEqual(scanned, truth) {
		t.Fatalf("ScanPreferences pages != snapshot table:\n%+v\n%+v", scanned, truth)
	}
	var wantPending []PreferenceSnapshot
	for _, s := range truth {
		if s.Pending() {
			wantPending = append(wantPending, s)
		}
	}
	if len(wantPending) != 3 {
		t.Fatalf("pending in truth = %d, want 3", len(wantPending))
	}
	if got := page(rt.PendingPreferences, 1); !reflect.DeepEqual(got, wantPending) {
		t.Fatalf("PendingPreferences pages != pending rows:\n%+v\n%+v", got, wantPending)
	}
}

func TestPreferences_AcknowledgeRejectsUnissuedRevisions(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	actor := Actor{ID: "u1", Kind: "user"}
	committed := mustReact(t, rt, actor, "gallery", "42:en", 1).Revision
	key := prefKey("u1", "gallery", "42", PreferenceAxisReaction)

	if err := rt.AcknowledgePreferences(ctx, nil); err != nil {
		t.Fatalf("empty ack: %v", err)
	}
	for name, ack := range map[string]PreferenceAck{
		"unknown actor": {PreferenceKey: prefKey("ghost", "gallery", "42", PreferenceAxisReaction), Revision: committed},
		"future rev":    {PreferenceKey: key, Revision: committed + 1},
		"zero rev":      {PreferenceKey: key, Revision: 0},
		"route key":     {PreferenceKey: prefKey("u1", "gallery", "42:en", PreferenceAxisReaction), Revision: committed},
	} {
		if err := rt.AcknowledgePreferences(ctx, []PreferenceAck{ack}); err == nil {
			t.Fatalf("%s: ack accepted, want an error", name)
		}
	}
	if row := snapshotRow(t, rt, key); row.DeliveredRevision != 0 {
		t.Fatalf("rejected acks moved delivered_revision to %d", row.DeliveredRevision)
	}
	newest := mustReact(t, rt, actor, "gallery", "42:en", -1).Revision
	if err := rt.AcknowledgePreferences(ctx, []PreferenceAck{{PreferenceKey: key, Revision: newest}, {PreferenceKey: key, Revision: committed}}); err != nil {
		t.Fatalf("batch ack: %v", err)
	}
	if row := snapshotRow(t, rt, key); row.DeliveredRevision != newest || row.Value != -1 {
		t.Fatalf("row after the batch ack = %+v", row)
	}
}

// Cutover step 3: the sequence can be lifted above a sink's timestamp-derived
// revisions, only ever advances, and fails closed outside the safe range.
func TestPreferences_RevisionFloorOnlyAdvances(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	actor := Actor{ID: "u1", Kind: "user"}
	mustReact(t, rt, actor, "gallery", "42:en", 1)

	floor := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC).UnixMicro() // the old clock domain
	got, err := rt.SeedPreferenceRevisionFloor(ctx, floor)
	if err != nil || got < floor {
		t.Fatalf("seed floor = %d err=%v, want >= %d", got, err, floor)
	}
	row := *mustReact(t, rt, actor, "gallery", "42:en", -1)
	if row.Revision <= floor {
		t.Fatalf("revision %d does not exceed the old domain floor %d", row.Revision, floor)
	}
	if again, err := rt.SeedPreferenceRevisionFloor(ctx, floor-1_000_000); err != nil || again < row.Revision {
		t.Fatalf("re-seeding lowered the floor to %d err=%v", again, err)
	}
	if next := mustReact(t, rt, actor, "gallery", "42:en", 1); next.Revision <= row.Revision {
		t.Fatalf("revision went backwards after a re-seed: %d then %d", row.Revision, next.Revision)
	}
	if _, err := rt.SeedPreferenceRevisionFloor(ctx, maxPreferenceRevisionFloor+1); err == nil {
		t.Fatal("an unsafe floor was accepted")
	}
}

func TestPreferences_PurgeSubjectsRemovesTheObligation(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	mustReact(t, rt, Actor{ID: "u1", Kind: "user"}, "gallery", "42:en", 1)
	mustReact(t, rt, Actor{ID: "u2", Kind: "user"}, "gallery", "42:en", 1)
	if n, err := rt.PurgePreferenceSubjects(ctx, []string{"u1"}); err != nil || n != 1 {
		t.Fatalf("purge = %d err=%v, want 1", n, err)
	}
	if got := pending(t, rt); len(got) != 1 || got[0].ActorID != "u2" {
		t.Fatalf("pending after the purge = %+v", got)
	}
}

// Cutover step 2 / case 13: language-scoped history collapses onto canonical
// references with the documented rule, archives the source, rebuilds counts,
// seeds snapshots (zeros for keys a previous exporter sent included), is
// idempotent and never overwrites a newer live preference.
func TestPreferences_MigrateCollapsesLocaleHistory(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	tn := testTenant

	if _, err := rt.store.pool.Exec(ctx, `INSERT INTO `+rt.store.t.reactions+`
		(tenant_id, content_kind, content_id, content_version_id, user_id, ip, value, created_at, updated_at) VALUES
		($2, 'gallery', '42:en', '', 'u1', NULL,  1, $1::timestamptz, $1::timestamptz),
		($2, 'gallery', '42:ja', '', 'u1', NULL, -1, $1::timestamptz, $1::timestamptz + interval '1 day'),
		($2, 'gallery', '7:en',  '', 'u1', NULL,  0, $1::timestamptz, $1::timestamptz),
		($2, 'gallery', '9:en',  '', 'u2', NULL,  1, $1::timestamptz, $1::timestamptz),
		($2, 'gallery', '42:en', '', NULL, '10.0.0.1', 1, $1::timestamptz, $1::timestamptz),
		($2, 'gallery', '42:ja', '', NULL, '10.0.0.1', 1, $1::timestamptz, $1::timestamptz),
		($2, 'tag',     '9',     '', 'u1', NULL,  1, $1::timestamptz, $1::timestamptz),
		($2, 'comment', 'c1',    '', 'u1', NULL,  1, $1::timestamptz, $1::timestamptz)`, base, tn); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.store.pool.Exec(ctx, `INSERT INTO `+rt.store.t.favorites+` (tenant_id, user_id, content_kind, content_id, content_version_id, created_at) VALUES
		($2, 'u1', 'gallery', '42:en', '', $1::timestamptz),
		($2, 'u1', 'gallery', '42:ja', '', $1::timestamptz + interval '1 hour'),
		($2, 'u2', 'gallery', '9:en',  '', $1::timestamptz)`, base, tn); err != nil {
		t.Fatal(err)
	}
	// A localized comment thread must survive untouched.
	if _, err := rt.store.pool.Exec(ctx, `INSERT INTO `+rt.store.t.comments+`
		(tenant_id, content_kind, content_id, content_version_id, user_id, body) VALUES ($1, 'gallery', '42:ja', '', 'u1', 'hi')`, tn); err != nil {
		t.Fatal(err)
	}
	// Stale rollup rows under language keys (what the old writers maintained).
	if _, err := rt.store.pool.Exec(ctx, `INSERT INTO `+rt.store.t.counts+`
		(tenant_id, content_kind, content_id, content_version_id, likes, dislikes, favorites, comment_count) VALUES
		($1, 'gallery', '42:en', '', 2, 0, 1, 0), ($1, 'gallery', '42:ja', '', 0, 1, 1, 1)`, tn); err != nil {
		t.Fatal(err)
	}
	// u3 has no source row but a previous exporter sent a like for the work.
	exported := []PreferenceKey{prefKey("u3", "gallery", "42", PreferenceAxisReaction)}

	dry, err := rt.MigratePreferences(ctx, PreferenceMigrationOptions{ExportedKeys: exported, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !dry.DryRun || dry.ArchivedRows == 0 || dry.Seeded == 0 {
		t.Fatalf("dry run report = %+v", dry)
	}
	if got := len(snapshotRows(t, rt)); got != 0 || archiveCount(t, rt) != 0 {
		t.Fatalf("the dry run wrote %d snapshots / %d archive rows", got, archiveCount(t, rt))
	}

	report, err := rt.MigratePreferences(ctx, PreferenceMigrationOptions{ExportedKeys: exported})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// 6 gallery reaction rows (tag/comment declined) + 3 favorites archived.
	if report.ArchivedRows != 9 || report.Tombstoned != 1 {
		t.Fatalf("report = %+v, want 9 archived and 1 tombstone", report)
	}
	if len(report.Conflicts) != 3 {
		t.Fatalf("conflicts = %+v, want the three collapsed groups", report.Conflicts)
	}
	for _, c := range report.Conflicts {
		if c.Axis == PreferenceAxisReaction && c.ActorID == "u1" && c.Resolved != -1 {
			t.Fatalf("u1's conflicting group resolved to %d, want the dislike", c.Resolved)
		}
	}
	got := reactionInventory(t, rt)
	want := []string{"comment/c1/u1=1", "gallery/42/10.0.0.1=1", "gallery/42/u1=-1", "gallery/7/u1=0", "gallery/9/u2=1", "tag/9/u1=1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collapsed reactions =\n%v\nwant\n%v", got, want)
	}
	var favKeys []string
	frows, err := rt.store.pool.Query(ctx, `SELECT user_id, content_id, created_at FROM `+rt.store.t.favorites+` ORDER BY user_id, content_id`)
	if err != nil {
		t.Fatal(err)
	}
	for frows.Next() {
		var user, id string
		var at time.Time
		if err := frows.Scan(&user, &id, &at); err != nil {
			t.Fatal(err)
		}
		if user == "u1" && !at.Equal(base) {
			t.Fatalf("the collapsed favorite kept %v, want the oldest bookmark time", at)
		}
		favKeys = append(favKeys, user+"/"+id)
	}
	frows.Close()
	if !reflect.DeepEqual(favKeys, []string{"u1/42", "u2/9"}) {
		t.Fatalf("collapsed favorites = %v", favKeys)
	}
	if c := countsOf(t, rt, ref("gallery", "42")); c.Likes != 1 || c.Dislikes != 1 || c.Favorites != 1 {
		t.Fatalf("work counts = %+v, want the anonymous like, u1's dislike and one favorite", c)
	}
	if c := countsOf(t, rt, ref("gallery", "42:ja")); c.Likes != 1 || c.Dislikes != 1 || c.Favorites != 1 || c.CommentCount != 1 {
		t.Fatalf("language counts = %+v, want canonical preferences and localized comments", c)
	}
	var raw Counts
	if err := rt.store.pool.QueryRow(ctx, `SELECT likes, dislikes, favorites, comment_count FROM `+rt.store.t.counts+` WHERE tenant_id=$1 AND content_kind='gallery' AND content_id='42:ja'`, rt.tenant).Scan(&raw.Likes, &raw.Dislikes, &raw.Favorites, &raw.CommentCount); err != nil {
		t.Fatal(err)
	}
	if raw.Likes != 0 || raw.Dislikes != 0 || raw.Favorites != 0 || raw.CommentCount != 1 {
		t.Fatalf("physical locale rollup = %+v", raw)
	}
	var threads int
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.comments+` WHERE content_id = '42:ja'`).Scan(&threads); err != nil || threads != 1 {
		t.Fatalf("the localized comment thread = %d rows err=%v, want it untouched", threads, err)
	}
	work := snapshotRow(t, rt, prefKey("u1", "gallery", "42", PreferenceAxisReaction))
	if work.Value != -1 || !work.OccurredAt.Equal(base.Add(24*time.Hour)) {
		t.Fatalf("seeded work snapshot = %+v, want the dislike at its source time", work)
	}
	if ghost := snapshotRow(t, rt, exported[0]); ghost.Value != 0 || !ghost.Pending() {
		t.Fatalf("the exported-but-absent key = %+v, want a pending zero", ghost)
	}
	if n := len(snapshotRows(t, rt)); n != 6 {
		t.Fatalf("snapshots = %+v, want 6", snapshotRows(t, rt))
	}
	before := snapshotRows(t, rt)
	if _, err := rt.MigratePreferences(ctx, PreferenceMigrationOptions{ExportedKeys: exported}); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if after := snapshotRows(t, rt); !reflect.DeepEqual(before, after) {
		t.Fatalf("the second migrate changed the snapshots:\n%+v\n%+v", before, after)
	}
	if got := reactionInventory(t, rt); !reflect.DeepEqual(got, want) {
		t.Fatalf("the second migrate changed the source rows:\n%v", got)
	}
	// Spec case 10: a newer live mutation outranks a later reseed.
	mustReact(t, rt, Actor{ID: "u1", Kind: "user"}, "gallery", "42:en", 1)
	live := snapshotRow(t, rt, work.PreferenceKey)
	if live.Value != 1 || live.Revision <= work.Revision {
		t.Fatalf("live mutation = %+v", live)
	}
	if _, err := rt.MigratePreferences(ctx, PreferenceMigrationOptions{}); err != nil {
		t.Fatal(err)
	}
	if again := snapshotRow(t, rt, work.PreferenceKey); !reflect.DeepEqual(again, live) {
		t.Fatalf("a reseed overwrote the newer live preference: %+v -> %+v", live, again)
	}
	if n := archiveCount(t, rt); n != 9+6+6 {
		t.Fatalf("archive rows = %d, want every pre-collapse row of all three runs", n)
	}
}

// reactionInventory is every reaction row as "kind/id/actor=value", in key order.
func reactionInventory(t *testing.T, rt *Runtime) []string {
	t.Helper()
	rows, err := rt.store.pool.Query(context.Background(), `SELECT content_kind, content_id, COALESCE(user_id, ip), value
		FROM `+rt.store.t.reactions+` WHERE tenant_id = $1 ORDER BY content_kind, content_id, COALESCE(user_id, ip)`, rt.tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, id, who string
		var v int16
		if err := rows.Scan(&kind, &id, &who, &v); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s/%s/%s=%d", kind, id, who, v))
	}
	return out
}

func archiveCount(t *testing.T, rt *Runtime) int {
	t.Helper()
	var n int
	if err := rt.store.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+rt.store.t.preferenceArchive).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPreferences_PostReactionsExportUnderTheirOwnReference(t *testing.T) {
	rt, pool := newPostRuntime(t, Options{Canonicalizer: ContentCanonicalizerFunc(galleryWork)})
	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO `+rt.store.t.posts+` (tenant_id, author_id, title, body, is_draft) VALUES ($1, 'a', 't', 'b', false) RETURNING id`, testTenant).Scan(&id); err != nil {
		t.Fatal(err)
	}
	actor := Actor{ID: "u1", Kind: "user"}
	for _, v := range []int16{1, 1, -1} {
		if err := rt.posts.react(ctx, actor, id, v); err != nil {
			t.Fatalf("post react %d: %v", v, err)
		}
	}
	row := snapshotRow(t, rt, prefKey("u1", KindPost, id, PreferenceAxisReaction))
	if row.Value != -1 {
		t.Fatalf("post snapshot = %+v", row)
	}
	v, err := rt.posts.loadByID(ctx, pool, id)
	if err != nil || v.TotalLikes != 0 || v.TotalDislikes != 1 {
		t.Fatalf("post counters = (%d,%d) err=%v, want (0,1)", v.TotalLikes, v.TotalDislikes, err)
	}
	var cid string
	if err := pool.QueryRow(ctx, `INSERT INTO `+rt.store.t.comments+` (tenant_id, content_kind, content_id, content_version_id, user_id, body) VALUES ($1, 'post', $2, '', 'a', 'hi') RETURNING id`, testTenant, id).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.comments.reactTx(ctx, actor, cid, 1); err != nil {
		t.Fatalf("comment react: %v", err)
	}
	if got := len(snapshotRows(t, rt)); got != 1 {
		t.Fatalf("snapshots after a comment like = %d, want 1 (a comment reaction is never a preference)", got)
	}
}

// deliverySink answers with a scripted disposition per actor.
type deliverySink struct {
	verdicts map[string]PreferenceDisposition
	fail     error
	seen     [][]PreferenceSnapshot
}

func (s *deliverySink) DeliverPreferences(_ context.Context, snaps []PreferenceSnapshot) ([]PreferenceDisposition, error) {
	s.seen = append(s.seen, snaps)
	if s.fail != nil {
		return nil, s.fail
	}
	out := make([]PreferenceDisposition, len(snaps))
	for i, snap := range snaps {
		out[i] = s.verdicts[snap.ActorID]
	}
	return out, nil
}

// Spec cases 6, 7, 9 and 11: an unavailable sink keeps the obligation, accepted
// rows are acknowledged at exactly their revision, a failing key does not block
// the others, an erased subject is a terminal fence, and the full replay
// re-delivers acknowledged rows and resumes from its cursor.
func TestPreferences_DeliverySweepAndReplay(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	for _, actor := range []string{"u1", "u2", "u3"} {
		mustReact(t, rt, Actor{ID: actor, Kind: "user"}, "gallery", "42:en", 1)
	}
	down := &deliverySink{fail: errors.New("clickhouse unavailable")}
	if _, err := rt.DeliverPreferences(ctx, down, PreferenceKey{}, 10, 0); err == nil {
		t.Fatal("a failing sink must surface its error")
	}
	if got := len(pending(t, rt)); got != 3 {
		t.Fatalf("pending after the outage = %d, want 3", got)
	}
	sink := &deliverySink{verdicts: map[string]PreferenceDisposition{"u1": PreferenceRetry, "u2": PreferenceAccepted, "u3": PreferenceSubjectErased}}
	report, err := rt.DeliverPreferences(ctx, sink, PreferenceKey{}, 2, 0)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.Delivered != 3 || report.Acknowledged != 1 || report.Retried != 1 || report.Erased != 1 {
		t.Fatalf("report = %+v", report)
	}
	if len(sink.seen) != 2 || len(sink.seen[0]) != 2 || len(sink.seen[1]) != 1 {
		t.Fatalf("pages = %v, want the sweep bounded to 2 rows per page", sink.seen)
	}
	got := pending(t, rt)
	if len(got) != 1 || got[0].ActorID != "u1" {
		t.Fatalf("pending after the sweep = %+v, want only the retried key", got)
	}
	if rows := snapshotRows(t, rt); len(rows) != 2 {
		t.Fatalf("snapshots = %+v, want the erased subject purged", rows)
	}
	acked := snapshotRow(t, rt, prefKey("u2", "gallery", "42", PreferenceAxisReaction))
	if acked.DeliveredRevision != acked.Revision {
		t.Fatalf("accepted row = %+v, want it acknowledged at exactly its revision", acked)
	}
	// A crash between acceptance and acknowledgement replays the same snapshot.
	replay := &deliverySink{verdicts: map[string]PreferenceDisposition{"u1": PreferenceAccepted}}
	if _, err := rt.DeliverPreferences(ctx, replay, PreferenceKey{}, 10, 0); err != nil {
		t.Fatal(err)
	}
	if len(replay.seen) != 1 || replay.seen[0][0].PreferenceKey != got[0].PreferenceKey || replay.seen[0][0].Revision != got[0].Revision {
		t.Fatalf("replayed snapshot = %+v, want the same identity", replay.seen)
	}
	if n := len(pending(t, rt)); n != 0 {
		t.Fatalf("pending after the replay = %d, want 0", n)
	}
	// Full replay: every row (acknowledged ones included), bounded and resumable.
	full := &deliverySink{verdicts: map[string]PreferenceDisposition{"u1": PreferenceAccepted, "u2": PreferenceAccepted}}
	first, err := rt.ReplayPreferences(ctx, full, PreferenceKey{}, 1, 1)
	if err != nil || first.Delivered != 1 || first.Next == (PreferenceKey{}) {
		t.Fatalf("bounded replay = %+v err=%v", first, err)
	}
	rest, err := rt.ReplayPreferences(ctx, full, first.Next, 10, 0)
	if err != nil || rest.Delivered != 1 || rest.Next != (PreferenceKey{}) {
		t.Fatalf("resumed replay = %+v err=%v", rest, err)
	}
	if len(full.seen) != 2 || full.seen[0][0].ActorID != "u1" || full.seen[1][0].ActorID != "u2" {
		t.Fatalf("replay pages = %v", full.seen)
	}
}
