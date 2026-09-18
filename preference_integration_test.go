package contentkit

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/signal"
)

const prefTestCHDB = "contentkit_preference_test"

// routeResolver accepts any gallery route id ("42:en", "42:ja"); the
// canonicalizer collapses the language suffix to the work.
type routeResolver struct{}

func (routeResolver) Resolve(_ context.Context, r contentref.ContentRef, _ content.Actor) (content.Resolution, error) {
	if r.ContentKind != "gallery" {
		return content.Resolution{}, content.ErrNotFound
	}
	return content.Resolution{Visible: true, Accessible: true}, nil
}

func stripLanguage(r contentref.ContentRef) (contentref.ContentRef, bool) {
	if r.ContentKind != "gallery" {
		return contentref.ContentRef{}, false
	}
	id, _, _ := strings.Cut(r.ContentID, ":")
	return contentref.New(r.TenantID, "gallery", id).WithVersion(r.Version()), true
}

func newPreferenceRuntime(t *testing.T, pool *pgxpool.Pool, hostSchema, searchSchema string, conn signal.Conn) *Runtime {
	t.Helper()
	rt, err := NewRuntime(context.Background(), RuntimeConfig{
		EmbeddedConfig: EmbeddedConfig{PG: pool, PGSchema: searchSchema, Tenant: testTenant, CH: conn, CHDatabase: prefTestCHDB},
		Content: content.Options{Schema: hostSchema, Identity: ctxIdentity{}, Authz: allowAuthz{}, Resolver: routeResolver{},
			Canonicalizer: content.ContentCanonicalizerFunc(stripLanguage), ContentKinds: []string{"gallery"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func pendingRows(t *testing.T, rt *Runtime) []content.PreferenceSnapshot {
	t.Helper()
	rows, err := rt.Content.PendingPreferences(context.Background(), content.PreferenceKey{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

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

// The preference boundary end to end through ContentKit's own worker on real
// Postgres and ClickHouse: the two reviewer regressions (a delayed older
// delivery cannot overwrite a newer commit; a replay keeps its revision), the
// spec's outage / crash-before-ack / erasure / floor / replay cases, and zero
// snapshots for removals.
func TestPreferenceBoundaryIntegration(t *testing.T) {
	ctx := context.Background()
	pool := testPG(t)
	env := signaltest.FromEnv(t)
	pgtest.EnsureExtensions(t, ctx, pool)
	hostSchema := pgtest.EmptySchema(t, ctx, pool)
	searchSchema := pgtest.Schema(t, ctx, pool)
	sqlDB := stdlib.OpenDBFromPool(pool)
	if err := content.Migrate(ctx, sqlDB, hostSchema); err != nil {
		t.Fatal(err)
	}
	_ = sqlDB.Close()
	conn := env.Fresh(t, prefTestCHDB)
	rt := newPreferenceRuntime(t, pool, hostSchema, searchSchema, conn)
	h := rt.Handler()
	u1 := content.Actor{ID: "u1", Kind: "user"}

	// Regression 1: the like committed first, then the dislike on another
	// language route; the like's delivery is delayed past the dislike's.
	if rec := do(t, h, u1, "POST", "/gallery/42:en/like", nil); rec.Code != http.StatusOK {
		t.Fatalf("like: %d %s", rec.Code, rec.Body.String())
	}
	older := pendingRows(t, rt)[0] // the committed like, held back
	if rec := do(t, h, u1, "POST", "/gallery/42:ja/dislike", nil); rec.Code != http.StatusOK {
		t.Fatalf("dislike: %d %s", rec.Code, rec.Body.String())
	}
	newer := pendingRows(t, rt)[0]
	if older.Revision >= newer.Revision || older.Value != 1 || newer.Value != -1 || newer.ContentID != "42" || older.PreferenceKey != newer.PreferenceKey {
		t.Fatalf("delayed old like can overwrite newer dislike: like=%+v dislike=%+v", older, newer)
	}
	report, err := rt.DeliverPreferences(ctx, 10, 0)
	if err != nil || report.Acknowledged != 1 {
		t.Fatalf("deliver newer = %+v err=%v", report, err)
	}
	sink, _ := rt.preferenceSink()
	if disp, err := sink.DeliverPreferences(ctx, []content.PreferenceSnapshot{older}); err != nil || disp[0] != content.PreferenceAccepted {
		t.Fatalf("delayed older delivery = %v err=%v", disp, err)
	}
	if s := state(t, rt, "u1", "42"); s.NetValue != -1 || s.Feedback != 1 {
		t.Fatalf("sink state after the delayed older delivery = %+v, want the dislike", s)
	}
	if m := metrics(t, rt, "42"); m.NegativeSubjects != 1 || m.PositiveSubjects != 0 || m.SignalCounts["reaction"] != 1 {
		t.Fatalf("metrics = %+v, want one negative subject and one reaction event", m)
	}

	// Regression 2: replaying one committed snapshot, before and after a
	// process restart, keeps its revision and time and adds no vote.
	restarted := newPreferenceRuntime(t, pool, hostSchema, searchSchema, conn)
	if rep, err := restarted.ReplayPreferences(ctx, content.PreferenceKey{}, 10, 0); err != nil || rep.Delivered != 1 {
		t.Fatalf("replay = %+v err=%v", rep, err)
	}
	if again := pendingRows(t, rt); len(again) != 0 {
		t.Fatalf("replay left rows pending: %+v", again)
	}
	if m := metrics(t, rt, "42"); m.NegativeSubjects != 1 || m.SignalCounts["reaction"] != 1 || m.Events != 1 {
		t.Fatalf("metrics after replay = %+v, want no duplicate vote", m)
	}

	// Spec 3: neutral and favorite/unfavorite/refavorite converge without
	// resurrection or double counting.
	do(t, h, u1, "POST", "/gallery/42:en/neutral", nil)
	do(t, h, u1, "POST", "/gallery/42:ja/favorite", nil)
	if _, err := rt.DeliverPreferences(ctx, 10, 0); err != nil {
		t.Fatal(err)
	}
	if s := state(t, rt, "u1", "42"); s.NetValue != 1 || s.Feedback != 1 {
		t.Fatalf("state after neutral + favorite = %+v, want favorite 1 and reaction 0", s)
	}
	do(t, h, u1, "DELETE", "/gallery/42:en/favorite", nil)
	if _, err := rt.DeliverPreferences(ctx, 10, 0); err != nil {
		t.Fatal(err)
	}
	if s := state(t, rt, "u1", "42"); s.NetValue != 0 || s.Feedback != 0 {
		t.Fatalf("state after unfavorite = %+v, want zero", s)
	}
	do(t, h, u1, "POST", "/gallery/42:ja/favorite", nil)
	if _, err := rt.DeliverPreferences(ctx, 10, 0); err != nil {
		t.Fatal(err)
	}
	if s := state(t, rt, "u1", "42"); s.NetValue != 1 || s.Feedback != 1 {
		t.Fatalf("state after re-favorite = %+v", s)
	}
	if m := metrics(t, rt, "42"); m.SignalCounts["favorite"] != 1 || m.SignalCounts["reaction"] != 1 {
		t.Fatalf("metrics after the favorite cycle = %+v, want one identity per axis", m)
	}

	// Spec 6: the sink is unavailable (nothing listens on the address); the
	// user action succeeded and the obligation waits for a healthy worker.
	unreachable, err := clickhouse.Open(&clickhouse.Options{Addr: []string{"127.0.0.1:1"}, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	down := newPreferenceRuntime(t, pool, hostSchema, searchSchema, unreachable)
	do(t, h, u1, "POST", "/gallery/7:en/like", nil)
	if _, err := down.DeliverPreferences(ctx, 10, 0); err == nil {
		t.Fatal("delivery over a closed connection succeeded")
	}
	if p := pendingRows(t, rt); len(p) != 1 || p[0].ContentID != "7" {
		t.Fatalf("pending after the outage = %+v", p)
	}
	// Spec 7: the sink accepted but the worker died before acknowledging.
	if disp, err := sink.DeliverPreferences(ctx, pendingRows(t, rt)); err != nil || disp[0] != content.PreferenceAccepted {
		t.Fatalf("direct delivery = %v err=%v", disp, err)
	}
	if rep, err := rt.DeliverPreferences(ctx, 10, 0); err != nil || rep.Acknowledged != 1 {
		t.Fatalf("recovery sweep = %+v err=%v", rep, err)
	}
	if s, m := state(t, rt, "u1", "7"), metrics(t, rt, "7"); s.NetValue != 1 || m.PositiveSubjects != 1 || m.Events != 1 {
		t.Fatalf("after the crash replay: state=%+v metrics=%+v", s, m)
	}

	// Spec 10: old clock-domain revisions already in the sink; the seeded floor
	// makes every new revision win.
	u2 := content.Actor{ID: "u2", Kind: "user"}
	oldClock := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixMicro()
	if err := rt.RecordSignals(ctx, []signal.Signal{{ContentRef: gallery("9"), Subject: signal.Subject{UserID: "u2"}, Type: "reaction", EventID: PreferenceEventID,
		Revision: uint64(oldClock), OccurredAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Value: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Content.SeedPreferenceRevisionFloor(ctx, oldClock); err != nil {
		t.Fatal(err)
	}
	do(t, h, u2, "POST", "/gallery/9:en/dislike", nil)
	if p := pendingRows(t, rt); len(p) != 1 || p[0].Revision <= oldClock {
		t.Fatalf("revision after the floor = %+v, want above %d", p, oldClock)
	}
	if _, err := rt.DeliverPreferences(ctx, 10, 0); err != nil {
		t.Fatal(err)
	}
	if s := state(t, rt, "u2", "9"); s.NetValue != -1 {
		t.Fatalf("new revision lost to the old clock domain: %+v", s)
	}

	// Spec 14: sink projections are lost after every row was acknowledged; the
	// full replay restores current state, removals included.
	if err := rt.PurgeContentKinds(ctx, []string{"gallery"}); err != nil {
		t.Fatal(err)
	}
	if s := state(t, rt, "u1", "42"); s.Feedback != 0 {
		t.Fatalf("purge left state %+v", s)
	}
	if rep, err := rt.ReplayPreferences(ctx, content.PreferenceKey{}, 2, 0); err != nil || rep.Delivered != 4 {
		t.Fatalf("full replay = %+v err=%v", rep, err)
	}
	if s := state(t, rt, "u1", "42"); s.NetValue != 1 || s.Feedback != 1 {
		t.Fatalf("state after the full replay = %+v, want neutral reaction + favorite", s)
	}
	if s := state(t, rt, "u2", "9"); s.NetValue != -1 {
		t.Fatalf("u2 after the full replay = %+v", s)
	}
	if m := metrics(t, rt, "42"); m.Events != 2 || m.PositiveSubjects != 1 {
		t.Fatalf("metrics after the full replay = %+v", m)
	}

	// Spec 11: account erasure races a pending delivery: the fence is terminal.
	u3 := content.Actor{ID: "u3", Kind: "user"}
	do(t, h, u3, "POST", "/gallery/42:en/like", nil)
	held := pendingRows(t, rt)[0]
	if held.ActorID != "u3" {
		t.Fatalf("pending = %+v", held)
	}
	erasure, err := rt.EraseSubjects(ctx, []signal.Subject{{UserID: "u3"}})
	if err != nil || !erasure.Complete() {
		t.Fatalf("erase = %+v err=%v", erasure, err)
	}
	if p := pendingRows(t, rt); len(p) != 0 {
		t.Fatalf("erased subject still owes a delivery: %+v", p)
	}
	if disp, err := sink.DeliverPreferences(ctx, []content.PreferenceSnapshot{held}); err != nil || disp[0] != content.PreferenceSubjectErased {
		t.Fatalf("late delivery of an erased subject = %v err=%v, want the terminal disposition", disp, err)
	}
	if hist, err := rt.History(ctx, signal.Subject{UserID: "u3"}, signal.HistoryOptions{}); err != nil || len(hist) != 0 {
		t.Fatalf("erased subject re-ingested: %+v err=%v", hist, err)
	}
	if rec := do(t, h, u3, "POST", "/gallery/42:en/dislike", nil); rec.Code != http.StatusOK {
		t.Fatalf("source write after erasure: %d", rec.Code)
	}
	if rep, err := rt.DeliverPreferences(ctx, 10, 0); err != nil || rep.Erased != 1 || rep.Acknowledged != 0 {
		t.Fatalf("sweep after erasure = %+v err=%v, want the obligation purged, nothing ingested", rep, err)
	}
	if hist, _ := rt.History(ctx, signal.Subject{UserID: "u3"}, signal.HistoryOptions{}); len(hist) != 0 {
		t.Fatalf("erased subject re-ingested by the sweep: %+v", hist)
	}
}
