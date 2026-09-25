package contentkit

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/signal"
)

const prefTestCHDB = "contentkit_preference_test"

// routeResolver accepts any gallery route id ("<id>:en", "<id>:ja"); the
// canonicalizer collapses the language suffix to the work.
type routeResolver struct{}

func (routeResolver) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, r := range refs {
		if r.ContentKind == "gallery" {
			out[r.Key()] = access.Resolution{Visible: true, Accessible: true}
		}
	}
	return out, nil
}

func stripLanguage(r contentref.ContentRef) (contentref.ContentRef, bool) {
	if r.ContentKind != "gallery" {
		return contentref.ContentRef{}, false
	}
	id, _, _ := strings.Cut(r.ContentID, ":")
	return contentref.New(r.TenantID, "gallery", id).WithVersion(r.Version()), true
}

type prefEnv struct {
	pool   *pgxpool.Pool
	schema string
	conn   signal.Conn
	chDB   string
	// declined makes the canonicalizer refuse gallery "8" at sync time.
	declined *atomic.Bool
}

func newPrefEnv(t *testing.T, chDB string) prefEnv {
	t.Helper()
	ctx := context.Background()
	pool := testPG(t)
	env := signaltest.FromEnv(t)
	pgtest.EnsureExtensions(t, ctx, pool)
	schema := pgtest.EmptySchema(t, ctx, pool)
	sqlDB := stdlib.OpenDBFromPool(pool)
	if err := Migrate(ctx, MigrateConfig{DB: sqlDB, Schema: schema}); err != nil {
		t.Fatal(err)
	}
	_ = sqlDB.Close()
	return prefEnv{pool: pool, schema: schema, conn: env.Fresh(t, chDB), chDB: chDB, declined: &atomic.Bool{}}
}

func (e prefEnv) runtime(t *testing.T, conn signal.Conn, overlap time.Duration) *Runtime {
	t.Helper()
	canon := func(r contentref.ContentRef) (contentref.ContentRef, bool) {
		w, ok := stripLanguage(r)
		return w, ok && !(e.declined.Load() && w.ContentID == cid(8))
	}
	rt, err := NewRuntime(context.Background(), RuntimeConfig{
		EmbeddedConfig: EmbeddedConfig{PG: e.pool, PGSchema: e.schema, Tenant: testTenant, CH: conn, CHDatabase: e.chDB},
		Content: content.Options{Schema: e.schema, Identity: ctxIdentity{}, Authz: allowAuthz{}, Resolver: routeResolver{},
			Canonicalizer: content.ContentCanonicalizerFunc(canon), ContentKinds: []string{"gallery"}, PreferenceSyncOverlap: overlap},
	})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// galleryRoute is a gallery route whose id carries a language suffix: n's
// canonical id plus rest (":en/like").
func galleryRoute(n int, rest string) string { return "/gallery/" + cid(n) + rest }

func state(t *testing.T, rt *Runtime, user, id string) signal.State {
	t.Helper()
	states, err := rt.States(context.Background(), signal.Subject{UserID: user}, []ContentRef{gallery(id)})
	if err != nil {
		t.Fatal(err)
	}
	return states[gallery(id).Key()]
}

func metrics(t *testing.T, rt *Runtime, id string) signal.ContentMetrics {
	t.Helper()
	m, err := rt.Metrics(context.Background(), []ContentRef{gallery(id)}, signal.AllTime())
	if err != nil {
		t.Fatal(err)
	}
	return m[gallery(id).Key()]
}

func mustSync(t *testing.T, rt *Runtime) content.PreferenceSyncReport {
	t.Helper()
	rep, err := rt.SyncPreferences(context.Background())
	if err != nil {
		t.Fatalf("SyncPreferences: %v", err)
	}
	return rep
}

// The preference boundary end to end on real Postgres and ClickHouse:
// convergence across routes and toggles, unfavorite as neutral, outage
// recovery, the restore floor, resync after sink loss, sync-time
// canonicalization, anonymous rows and erasure.
func TestPreferenceBoundaryIntegration(t *testing.T) {
	ctx := context.Background()
	e := newPrefEnv(t, prefTestCHDB)
	rt := e.runtime(t, e.conn, 0)
	h := rt.Handler()
	u1 := access.Actor{ID: "u1", Kind: "user"}
	post := func(a access.Actor, method, path string) {
		t.Helper()
		if rec := do(t, h, a, method, path, nil); rec.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
	}

	// like → neutral → like, then a dislike on another language route.
	post(u1, "POST", galleryRoute(42, ":en/like"))
	mustSync(t, rt)
	post(u1, "POST", galleryRoute(42, ":ja/neutral"))
	mustSync(t, rt)
	if s := state(t, rt, "u1", cid(42)); s.NetValue != 0 || s.Feedback != 0 {
		t.Fatalf("state after neutral = %+v, want zero", s)
	}
	post(u1, "POST", galleryRoute(42, ":en/like"))
	post(u1, "POST", galleryRoute(42, ":ja/dislike"))
	mustSync(t, rt)
	mustSync(t, rt) // re-sends inside the overlap converge
	if m := metrics(t, rt, cid(42)); m.NegativeSubjects != 1 || m.PositiveSubjects != 0 || m.SignalCounts["reaction"] != 1 || m.Events != 1 {
		t.Fatalf("metrics = %+v, want one negative subject and one reaction event", m)
	}

	// favorite → unfavorite reaches the sink as neutral; re-favorite returns.
	post(u1, "POST", galleryRoute(42, ":en/neutral"))
	post(u1, "POST", galleryRoute(42, ":ja/favorite"))
	mustSync(t, rt)
	if s := state(t, rt, "u1", cid(42)); s.NetValue != 1 || s.Feedback != 1 {
		t.Fatalf("state after neutral + favorite = %+v", s)
	}
	post(u1, "DELETE", galleryRoute(42, ":en/favorite"))
	mustSync(t, rt)
	if s := state(t, rt, "u1", cid(42)); s.NetValue != 0 || s.Feedback != 0 {
		t.Fatalf("state after unfavorite = %+v, want zero", s)
	}
	post(u1, "POST", galleryRoute(42, ":ja/favorite"))
	mustSync(t, rt)
	if s := state(t, rt, "u1", cid(42)); s.NetValue != 1 || s.Feedback != 1 {
		t.Fatalf("state after re-favorite = %+v", s)
	}
	if m := metrics(t, rt, cid(42)); m.SignalCounts["favorite"] != 1 || m.SignalCounts["reaction"] != 1 {
		t.Fatalf("metrics after the favorite cycle = %+v, want one identity per axis", m)
	}

	// Anonymous rows and targets the canonicalizer declines at sync time stay out.
	post(access.Actor{IP: "10.0.0.1", Anonymous: true}, "POST", galleryRoute(7, ":en/like"))
	post(u1, "POST", galleryRoute(8, ":en/like"))
	e.declined.Store(true)
	mustSync(t, rt)
	e.declined.Store(false)
	if m := metrics(t, rt, cid(7)); m.Events != 0 {
		t.Fatalf("anonymous reaction exported: %+v", m)
	}
	if s := state(t, rt, "u1", cid(8)); s.Feedback != 0 {
		t.Fatalf("declined target exported: %+v", s)
	}

	// The sink is unreachable: the write succeeded, the sync fails without
	// advancing, and a healthy sync delivers.
	unreachable, err := clickhouse.Open(&clickhouse.Options{Addr: []string{"127.0.0.1:1"}, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	down := e.runtime(t, unreachable, 0)
	post(u1, "POST", galleryRoute(9, ":en/like"))
	if _, err := down.SyncPreferences(ctx); err == nil {
		t.Fatal("sync over a closed connection succeeded")
	}
	mustSync(t, rt)
	if s := state(t, rt, "u1", cid(9)); s.NetValue != 1 {
		t.Fatalf("after the outage: %+v", s)
	}

	// Restore floor: old revisions already in the sink lose to new ones.
	u2 := access.Actor{ID: "u2", Kind: "user"}
	old := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := rt.RecordSignals(ctx, []signal.Signal{{ContentRef: gallery(cid(9)), Subject: signal.Subject{UserID: "u2"}, Type: "reaction", EventID: PreferenceEventID,
		Revision: uint64(old.UnixMicro()), OccurredAt: old, Value: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Content.SeedPreferenceRevisionFloor(ctx, old.UnixMicro()); err != nil {
		t.Fatal(err)
	}
	post(u2, "POST", galleryRoute(9, ":en/dislike"))
	mustSync(t, rt)
	if s := state(t, rt, "u2", cid(9)); s.NetValue != -1 {
		t.Fatalf("new revision lost to the old one: %+v", s)
	}

	// Sink loss: a full resync restores current state, zeros included, and a
	// second resync adds nothing.
	if err := rt.PurgeContentKinds(ctx, []string{"gallery"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if rep, err := rt.ResyncPreferences(ctx); err != nil || rep.Sent != 5 {
			t.Fatalf("resync = %+v err=%v, want 5 rows", rep, err)
		}
	}
	if s := state(t, rt, "u1", cid(42)); s.NetValue != 1 || s.Feedback != 1 {
		t.Fatalf("state after resync = %+v, want neutral reaction + favorite", s)
	}
	if s := state(t, rt, "u1", cid(8)); s.NetValue != 1 {
		t.Fatalf("accepted-again target after resync = %+v", s)
	}
	if m := metrics(t, rt, cid(42)); m.Events != 2 || m.PositiveSubjects != 1 {
		t.Fatalf("metrics after resync = %+v", m)
	}

	// Erasure: the source rows go, the sink fences, a late send of a row read
	// before the erasure is dropped, and nothing re-ingests.
	u3 := access.Actor{ID: "u3", Kind: "user"}
	post(u3, "POST", galleryRoute(42, ":en/like"))
	var late []content.Preference
	if _, err := rt.Content.ResyncPreferences(ctx, func(_ context.Context, page []content.Preference) error {
		for _, p := range page {
			if p.ActorID == "u3" {
				late = append(late, p)
			}
		}
		return nil
	}); err != nil || len(late) != 1 {
		t.Fatalf("u3 rows = %+v err=%v", late, err)
	}
	erasure, err := rt.EraseSubjects(ctx, []signal.Subject{{UserID: "u3"}})
	if err != nil || !erasure.Complete() {
		t.Fatalf("erase = %+v err=%v", erasure, err)
	}
	if err := rt.sendPreferences(ctx, late); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, h, u3, "POST", galleryRoute(42, ":en/dislike"), nil); rec.Code != http.StatusForbidden {
		t.Fatalf("source write after erasure: %d", rec.Code)
	}
	mustSync(t, rt)
	if _, err := rt.ResyncPreferences(ctx); err != nil {
		t.Fatal(err)
	}
	if hist, err := rt.History(ctx, signal.Subject{UserID: "u3"}, signal.HistoryOptions{}); err != nil || len(hist) != 0 {
		t.Fatalf("erased subject re-ingested: %+v err=%v", hist, err)
	}
}

// A transaction that allocated its revision before a later one committed and
// synced is still delivered once it commits within the overlap, and the
// watermark advances once the overlap has passed.
func TestPreferenceSyncSlowCommitOverlapIntegration(t *testing.T) {
	ctx := context.Background()
	const overlap = 2 * time.Second
	e := newPrefEnv(t, prefTestCHDB+"_overlap")
	rt := e.runtime(t, e.conn, overlap)
	h := rt.Handler()
	u1 := access.Actor{ID: "u1", Kind: "user"}
	do(t, h, u1, "POST", galleryRoute(4, ":en/like"), nil)
	mustSync(t, rt)
	time.Sleep(overlap + 200*time.Millisecond)

	held, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback(ctx)
	var slow int64
	if err := held.QueryRow(ctx, `INSERT INTO `+e.schema+`.content_reactions (tenant_id, content_kind, content_id, content_version_id, user_id, value, revision)
		VALUES ($1, 'gallery', $2, '', 'slow', 1, nextval('`+e.schema+`.content_preference_revision_seq')) RETURNING revision`, testTenant, cid(5)).Scan(&slow); err != nil {
		t.Fatal(err)
	}
	do(t, h, u1, "POST", galleryRoute(6, ":en/like"), nil)
	mustSync(t, rt)
	if s := state(t, rt, "u1", cid(6)); s.NetValue != 1 {
		t.Fatalf("later commit not synced: %+v", s)
	}
	mustSync(t, rt)
	var newest int64
	if err := e.pool.QueryRow(ctx, `SELECT max(revision) FROM `+e.schema+`.content_preference_sync`).Scan(&newest); err != nil || newest <= slow {
		t.Fatalf("newest checkpoint %d err=%v, want past the held revision %d", newest, err, slow)
	}
	if err := held.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if rep := mustSync(t, rt); rep.From >= slow {
		t.Fatalf("sync started at %d, past the held revision %d", rep.From, slow)
	}
	if s := state(t, rt, "slow", cid(5)); s.NetValue != 1 {
		t.Fatalf("slow commit lost: %+v", s)
	}

	time.Sleep(overlap + 200*time.Millisecond)
	mustSync(t, rt)
	time.Sleep(overlap + 200*time.Millisecond)
	if rep := mustSync(t, rt); rep.From < slow || rep.Sent != 0 {
		t.Fatalf("watermark did not advance: %+v (held revision %d)", rep, slow)
	}
}
