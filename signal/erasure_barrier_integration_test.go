package signal

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/open-rails/contentkit/internal/signaltest"
)

// gateConn pauses the first statement (Exec or Query) matching match after
// the real store has issued everything before it; dispatch, when set,
// replaces how that Exec is sent. Everything else runs on disposable
// ClickHouse unchanged.
type gateConn struct {
	Conn
	match    func(query string) bool
	dispatch func(ctx context.Context, query string, args ...any) error
	onExec   func(query string)
	onQuery  func(query string)
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func newGate(conn Conn, prefix string) *gateConn {
	return &gateConn{Conn: conn, match: func(q string) bool { return strings.HasPrefix(q, prefix) },
		entered: make(chan struct{}), release: make(chan struct{})}
}

func (c *gateConn) pause(ctx context.Context, query string) bool {
	if c.match == nil || !c.match(query) {
		return false
	}
	c.once.Do(func() {
		close(c.entered)
		select {
		case <-c.release:
		case <-ctx.Done():
		}
	})
	return true
}

func (c *gateConn) Exec(ctx context.Context, query string, args ...any) error {
	if c.onExec != nil {
		c.onExec(query)
	}
	if c.pause(ctx, query) && c.dispatch != nil {
		return c.dispatch(ctx, query, args...)
	}
	return c.Conn.Exec(ctx, query, args...)
}

func (c *gateConn) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	if c.onQuery != nil {
		c.onQuery(query)
	}
	c.pause(ctx, query)
	return c.Conn.Query(ctx, query, args...)
}

func (c *gateConn) wait(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-c.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// barrierFixture: gone viewed and liked e1 and viewed e2; keeper viewed both.
type barrierFixture struct {
	tenant       string
	gone, keeper Subject
	e1, e2       ContentRef
	at           time.Time
}

func newBarrierFixture(tenant string) barrierFixture {
	return barrierFixture{tenant: tenant, gone: Subject{UserID: "barrier-gone"}, keeper: Subject{UserID: "barrier-keeper"},
		e1: gallery(tenant, lid("b1")), e2: gallery(tenant, lid("b2")),
		at: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
}

func (f barrierFixture) view(sub Subject, ref ContentRef, id string) Signal {
	return Signal{ContentRef: ref, Subject: sub, Type: TypeView, EventID: id, OccurredAt: f.at, Progress: 2, ProgressMax: 2, Score: 50, Completed: true, DurationS: 30}
}

func (f barrierFixture) seed(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	like := f.view(f.gone, f.e1, "gone-like")
	like.Type, like.Value = "reaction", 1
	if err := st.RecordSignals(ctx, f.tenant, []Signal{
		f.view(f.gone, f.e1, "gone-1"), f.view(f.gone, f.e2, "gone-2"), like,
		f.view(f.keeper, f.e1, "keeper-1"), f.view(f.keeper, f.e2, "keeper-2"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordExposures(ctx, f.tenant, []Exposure{
		{RenderID: "gone-render", Stage: StageRendered, Subject: f.gone, Shown: []Placement{{ContentRef: f.e1, Position: 1}}, OccurredAt: f.at},
		{RenderID: "keeper-render", Stage: StageRendered, Subject: f.keeper, Shown: []Placement{{ContentRef: f.e1, Position: 1}}, OccurredAt: f.at},
	}); err != nil {
		t.Fatal(err)
	}
}

// assertInvisible checks every read surface for the erased subject and every
// aggregate for exactly the keeper's contribution.
func (f barrierFixture) assertInvisible(t *testing.T, st *Store, phase string) {
	t.Helper()
	ctx := context.Background()
	refs := []ContentRef{f.e1, f.e2}
	if s, err := st.States(ctx, f.tenant, f.gone, refs); err != nil || len(s) != 0 {
		t.Fatalf("%s: States leaked %+v %v", phase, s, err)
	}
	if h, err := st.History(ctx, f.tenant, f.gone, HistoryOptions{}); err != nil || len(h) != 0 {
		t.Fatalf("%s: History leaked %+v %v", phase, h, err)
	}
	if n, err := st.HistoryCount(ctx, f.tenant, f.gone, HistoryOptions{}); err != nil || n != 0 {
		t.Fatalf("%s: HistoryCount leaked %d %v", phase, n, err)
	}
	if ids, err := st.SeenIDs(ctx, f.tenant, f.gone, "gallery"); err != nil || len(ids) != 0 {
		t.Fatalf("%s: SeenIDs leaked %v %v", phase, ids, err)
	}
	if top, err := st.TopStates(ctx, f.tenant, f.gone, TopStatesOptions{}); err != nil || len(top) != 0 {
		t.Fatalf("%s: TopStates leaked %+v %v", phase, top, err)
	}
	if neg, err := st.NegativeIDs(ctx, f.tenant, f.gone, nil); err != nil || len(neg) != 0 {
		t.Fatalf("%s: NegativeIDs leaked %+v %v", phase, neg, err)
	}
	m, err := st.Metrics(ctx, f.tenant, refs, AllTime())
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if m[ref.Key()].Viewers != 1 || m[ref.Key()].Views != 1 || m[ref.Key()].PositiveSubjects != 0 || m[ref.Key()].Events != 1 {
			t.Fatalf("%s: Metrics[%s] counts the erased subject: %+v", phase, ref, m[ref.Key()])
		}
	}
	pop, err := st.Popular(ctx, f.tenant, "gallery", PopularOptions{Window: AllTime(), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range pop {
		if h.Viewers != 1 {
			t.Fatalf("%s: Popular counts the erased subject: %+v", phase, h)
		}
	}
	if err := st.RefreshCoEngagement(ctx, f.tenant, RefreshCoEngagementOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, skip := range []bool{true, false} {
		co, err := st.CoEngaged(ctx, f.tenant, f.e1, CoEngagedOptions{SkipRollup: skip})
		if err != nil {
			t.Fatal(err)
		}
		if len(co) != 1 || !co[0].Equal(f.e2) || co[0].Strength != 1 {
			t.Fatalf("%s: CoEngaged(skipRollup=%v) counts the erased subject: %+v", phase, skip, co)
		}
	}
	inv, err := st.Inventory(ctx, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range inv {
		if r.Subjects != 1 || r.SignalType == "reaction" {
			t.Fatalf("%s: Inventory counts the erased subject: %+v", phase, r)
		}
	}
}

func (f barrierFixture) rawRows(t *testing.T, conn Conn, table string) uint64 {
	t.Helper()
	return countWhere(t, conn, table, "tenant = ? AND subject_kind = ? AND subject = ?", f.tenant, f.gone.Kind(), f.gone.Key())
}

func (f barrierFixture) assertNoResidue(t *testing.T, conn Conn, phase string) {
	t.Helper()
	for _, table := range subjectTables {
		if n := f.rawRows(t, conn, table); n != 0 {
			t.Fatalf("%s: %s holds %d rows of the erased subject", phase, table, n)
		}
	}
	if n := countWhere(t, conn, "signals", "tenant = ? AND subject = ?", f.tenant, f.keeper.Key()); n < 2 {
		t.Fatalf("%s: keeper damaged: %d signals", phase, n)
	}
}

func erase(t *testing.T, st *Store, tenant string, sub Subject) ErasureReport {
	t.Helper()
	report, err := st.EraseSubjects(context.Background(), []string{tenant}, []Subject{sub})
	if err != nil || !report.Complete() {
		t.Fatalf("erasure: %+v %v", report, err)
	}
	return report
}

// plantResidue inserts one row per subject table for the erased subject with
// explicit ingest/version times: what a restore, a delayed writer or a stale
// projection publication leaves behind.
func plantResidue(t *testing.T, conn Conn, f barrierFixture, ingested time.Time) {
	t.Helper()
	ctx := context.Background()
	kind, key := f.gone.Kind(), f.gone.Key()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{fmt.Sprintf(`INSERT INTO %s.signals (tenant, content_kind, content_id, subject_kind, subject, signal_type, event_id, occurred_at, progress, progress_max, score, completed, ingested_at)
VALUES (?, ?, ?, ?, ?, 'view', 'residue', ?, 2, 2, 50, true, ?)`, testDB), []any{f.tenant, f.e1.ContentKind, f.e1.ContentID, kind, key, f.at, ingested}},
		{fmt.Sprintf(`INSERT INTO %s.subject_content_state (tenant, subject_kind, subject, content_kind, content_id, first_seen_at, last_signal_at, total_events, views, completions, active_s, max_progress, progress_max, completed, resume, last_score, net_value, feedback, version)
VALUES (?, ?, ?, ?, ?, ?, ?, 1, 1, 1, 0, 2, 2, true, '', 50, 0, 0, ?)`, testDB), []any{f.tenant, kind, key, f.e1.ContentKind, f.e1.ContentID, f.at, f.at, ingested}},
		{fmt.Sprintf(`INSERT INTO %s.subject_content_daily (tenant, content_kind, content_id, subject_kind, subject, day, events, views, completions, active_s, score_sum, value_sum, type_counts, version)
VALUES (?, ?, ?, ?, ?, toDate(?), 1, 1, 1, 0, 50, 0, map('view', 1), ?)`, testDB), []any{f.tenant, f.e1.ContentKind, f.e1.ContentID, kind, key, f.at, ingested}},
		{fmt.Sprintf(`INSERT INTO %s.exposures (tenant, render_id, stage, surface, subject_kind, subject, content_kinds, content_ids, positions, occurred_at, ingested_at)
VALUES (?, 'residue', 'rendered', 'search', ?, ?, [?], [?], [1], ?, ?)`, testDB), []any{f.tenant, kind, key, f.e1.ContentKind, f.e1.ContentID, f.at, ingested}},
	} {
		if err := conn.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("plant residue: %v\n%s", err, q.sql)
		}
	}
}

// A writer on its own connection reads the fence and pauses before its
// INSERT; an independent Store erases and reports Complete; the writer lands.
// The residue exists physically but no read, projection or export on a third
// connection can see it, and enforcement removes it.
func TestIntegrationErasureBarrierPausedWriterAcrossConnections(t *testing.T) {
	env := signaltest.FromEnv(t)
	_, conn := freshStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newBarrierFixture("barrier")
	gate := newGate(env.Open(t, testDB), "INSERT INTO "+testDB+".signals")
	writer, _ := NewStore(gate, testDB)
	eraser, _ := NewStore(env.Open(t, testDB), testDB)
	reader, _ := NewStore(env.Open(t, testDB), testDB)
	f.seed(t, eraser)

	done := make(chan error, 1)
	late := f.view(f.gone, f.e1, "gone-late")
	rescored := f.view(f.keeper, f.e1, "keeper-1")
	rescored.Revision, rescored.Score = 1, 60
	go func() { done <- writer.RecordSignals(ctx, f.tenant, []Signal{late, rescored}) }()
	gate.wait(t, ctx)
	erase(t, eraser, f.tenant, f.gone)
	f.assertNoResidue(t, conn, "after erasure, writer still paused")
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The INSERT re-evaluates the ledger, so nothing lands; the read barrier
	// below is still asserted with planted residue in the other tests.
	f.assertNoResidue(t, conn, "paused writer released")
	f.assertInvisible(t, reader, "paused writer released")
	if _, err := reader.RepairProjections(ctx, f.tenant, RepairOptions{Rebuild: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.RepairProjections(ctx, f.tenant, RepairOptions{IngestedSince: f.at.AddDate(-1, 0, 0)}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"subject_content_state", "subject_content_daily"} {
		if n := f.rawRows(t, conn, table); n != 0 {
			t.Fatalf("repair re-projected an erased subject into %s: %d", table, n)
		}
	}
	s, err := reader.States(ctx, f.tenant, f.keeper, []ContentRef{f.e1})
	if err != nil || s[f.e1.Key()].LastScore != 60 {
		t.Fatalf("the keeper's write in the same batch must land: %+v %v", s, err)
	}
	f.assertInvisible(t, reader, "after repair")
	enforced, err := reader.EnforceErasures(ctx, f.tenant)
	if err != nil || !enforced.Complete() {
		t.Fatalf("enforce: %+v %v", enforced, err)
	}
	f.assertNoResidue(t, conn, "after enforcement")
	f.assertInvisible(t, reader, "after enforcement")
	// Later writes for the dead key are dropped before insert.
	if err := reader.RecordSignals(ctx, f.tenant, []Signal{f.view(f.gone, f.e2, "gone-later")}); err != nil {
		t.Fatal(err)
	}
	if err := reader.RecordExposures(ctx, f.tenant, []Exposure{{RenderID: "gone-later", Stage: StageRendered, Subject: f.gone, Shown: []Placement{{ContentRef: f.e1, Position: 1}}, OccurredAt: f.at}}); err != nil {
		t.Fatal(err)
	}
	f.assertNoResidue(t, conn, "after post-erasure writes")
}

// The projection half of RecordSignals races the erasure: the event landed,
// the projection statement is paused, the erasure completes, the projection
// runs and must derive nothing. Then rows a projection would have written had
// it observed the pre-erasure events (or that a backup restored) are planted
// directly and must be invisible everywhere until enforcement removes them.
func TestIntegrationErasureBarrierProjectionRacesErasure(t *testing.T) {
	env := signaltest.FromEnv(t)
	st, conn := freshStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newBarrierFixture("barrier")
	f.seed(t, st)
	gate := newGate(env.Open(t, testDB), "INSERT INTO "+testDB+".subject_content_state")
	writer, _ := NewStore(gate, testDB)
	done := make(chan error, 1)
	go func() { done <- writer.RecordSignals(ctx, f.tenant, []Signal{f.view(f.gone, f.e2, "gone-late")}) }()
	gate.wait(t, ctx)
	if n := f.rawRows(t, conn, "signals"); n != 4 {
		t.Fatalf("precondition: the late event is durable before its projection: %d", n)
	}
	erase(t, st, f.tenant, f.gone)
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.assertInvisible(t, st, "projection after erasure")
	f.assertNoResidue(t, conn, "projection after erasure")

	plantResidue(t, conn, f, f.at.Add(-48*time.Hour))
	for _, table := range subjectTables {
		if n := f.rawRows(t, conn, table); n == 0 {
			t.Fatalf("precondition: residue planted in %s", table)
		}
	}
	f.assertInvisible(t, st, "planted residue")
	if _, err := st.RepairProjections(ctx, f.tenant, RepairOptions{Rebuild: true}); err != nil {
		t.Fatal(err)
	}
	f.assertInvisible(t, st, "planted residue after rebuild")
	if enforced, err := st.EnforceErasures(ctx, f.tenant); err != nil || !enforced.Complete() {
		t.Fatalf("enforce: %+v %v", enforced, err)
	}
	f.assertNoResidue(t, conn, "after enforcement")
}

// A delayed job read the fence in one process; that process died and a new
// one delivers its pending INSERT over a new connection after the erasure
// completed. The stale fence read must not matter.
func TestIntegrationErasureBarrierDelayedJobAcrossRestart(t *testing.T) {
	env := signaltest.FromEnv(t)
	st, conn := freshStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newBarrierFixture("barrier")
	f.seed(t, st)
	oldProcess := env.Open(t, testDB)
	gate := newGate(oldProcess, "INSERT INTO "+testDB+".signals")
	newProcess := env.Open(t, testDB)
	gate.dispatch = func(ctx context.Context, query string, args ...any) error {
		_ = oldProcess.Close()
		gate.Conn = newProcess
		return newProcess.Exec(ctx, query, args...)
	}
	job, _ := NewStore(gate, testDB)
	done := make(chan error, 1)
	go func() { done <- job.RecordSignals(ctx, f.tenant, []Signal{f.view(f.gone, f.e2, "delayed")}) }()
	gate.wait(t, ctx)
	erase(t, st, f.tenant, f.gone)
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.assertNoResidue(t, conn, "delayed job")
	f.assertInvisible(t, st, "delayed job")
	if _, err := st.RepairProjections(ctx, f.tenant, RepairOptions{Rebuild: true}); err != nil {
		t.Fatal(err)
	}
	f.assertInvisible(t, st, "delayed job after rebuild")
	if enforced, err := st.EnforceErasures(ctx, f.tenant); err != nil || !enforced.Complete() {
		t.Fatalf("enforce: %+v %v", enforced, err)
	}
	f.assertNoResidue(t, conn, "after enforcement")
}

// Enforcement has no cursor: residue of an old erasure arriving after later
// erasures and later enforcement runs is still removed, and restored rows
// with year-old ingest times are removed regardless of age.
func TestIntegrationErasureBarrierEnforcesOldErasuresAndRestores(t *testing.T) {
	st, conn := freshStore(t)
	ctx := context.Background()
	f := newBarrierFixture("barrier")
	f.seed(t, st)
	erase(t, st, f.tenant, f.gone)
	if r, err := st.EnforceErasures(ctx, f.tenant); err != nil || !r.Complete() {
		t.Fatalf("first enforcement: %+v %v", r, err)
	}
	other := Subject{UserID: "barrier-other"}
	if err := st.RecordSignals(ctx, f.tenant, []Signal{f.view(other, f.e1, "other-1")}); err != nil {
		t.Fatal(err)
	}
	erase(t, st, f.tenant, other)
	if r, err := st.EnforceErasures(ctx, f.tenant); err != nil || !r.Complete() {
		t.Fatalf("second enforcement: %+v %v", r, err)
	}
	plantResidue(t, conn, f, time.Now().UTC())
	plantResidue(t, conn, f, f.at.AddDate(-1, 0, 0))
	f.assertInvisible(t, st, "old-erasure residue")
	r, err := st.EnforceErasures(ctx, f.tenant)
	if err != nil || !r.Complete() {
		t.Fatalf("third enforcement: %+v %v", r, err)
	}
	f.assertNoResidue(t, conn, "after third enforcement")
	if n := countWhere(t, conn, "erasures", "tenant = ?", f.tenant); n < 2 {
		t.Fatalf("ledger must keep every erasure: %d", n)
	}
}

// A co-engagement build that observed a subject before its erasure re-verifies
// the ledger and rebuilds; a build that keeps racing gives up loudly.
func TestIntegrationErasureBarrierPairRebuildRacesErasure(t *testing.T) {
	env := signaltest.FromEnv(t)
	st, conn := freshStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newBarrierFixture("barrier")
	f.seed(t, st)
	third := Subject{UserID: "barrier-third"}
	if err := st.RecordSignals(ctx, f.tenant, []Signal{f.view(third, f.e1, "third-1"), f.view(third, f.e2, "third-2")}); err != nil {
		t.Fatal(err)
	}
	if err := st.RefreshCoEngagement(ctx, f.tenant, RefreshCoEngagementOptions{}); err != nil {
		t.Fatal(err)
	}
	co, err := st.CoEngaged(ctx, f.tenant, f.e1, CoEngagedOptions{})
	if err != nil || len(co) != 1 || co[0].Strength != 3 {
		t.Fatalf("precondition rollup: %+v %v", co, err)
	}

	// Pause the refresher after it published pairs built with the subject and
	// before its ledger re-check; erase meanwhile.
	const ledgerQuery = "SELECT uniqExact(subject_hash)"
	var builds atomic.Int32
	checks := 0
	gate := &gateConn{Conn: env.Open(t, testDB), entered: make(chan struct{}), release: make(chan struct{})}
	gate.match = func(q string) bool {
		if !strings.HasPrefix(q, ledgerQuery) {
			return false
		}
		checks++
		return checks == 2
	}
	gate.onExec = func(q string) {
		if strings.HasPrefix(q, "INSERT INTO "+testDB+".content_pairs") {
			builds.Add(1)
		}
	}
	refresher, _ := NewStore(gate, testDB)
	done := make(chan error, 1)
	go func() { done <- refresher.RefreshCoEngagement(ctx, f.tenant, RefreshCoEngagementOptions{}) }()
	gate.wait(t, ctx)
	if builds.Load() != 1 {
		t.Fatalf("precondition: one build published before the re-check: %d", builds.Load())
	}
	erase(t, st, f.tenant, f.gone)
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if builds.Load() != 2 {
		t.Fatalf("a build that raced an erasure must rebuild once: %d builds", builds.Load())
	}
	co, err = st.CoEngaged(ctx, f.tenant, f.e1, CoEngagedOptions{})
	if err != nil || len(co) != 1 || co[0].Strength != 2 {
		t.Fatalf("rebuilt rollup must exclude the erased subject: %+v %v", co, err)
	}
	if n := countWhere(t, conn, "content_pairs", "tenant = ? AND strength > 2", f.tenant); n != 0 {
		t.Fatalf("stale pairs remain: %d", n)
	}

	// Erasures landing during every build exhaust the attempts.
	victims := []Subject{third, f.keeper, {UserID: "barrier-nobody"}}
	calls := 0
	racing := &gateConn{Conn: env.Open(t, testDB)}
	racing.onQuery = func(q string) {
		if !strings.HasPrefix(q, ledgerQuery) {
			return
		}
		calls++
		if calls%2 == 0 && len(victims) > 0 {
			erase(t, st, f.tenant, victims[0])
			victims = victims[1:]
		}
	}
	rs, _ := NewStore(racing, testDB)
	if err := rs.RefreshCoEngagement(ctx, f.tenant, RefreshCoEngagementOptions{}); err == nil || !strings.Contains(err.Error(), "raced erasures 3 times") {
		t.Fatalf("exhausted rebuilds must fail loudly: %v", err)
	}
}

// Erasure is per tenant and per subject kind: the same key in another tenant
// or as the other kind stays readable, and the barrier never hides it.
func TestIntegrationErasureBarrierTenantAndKindIsolation(t *testing.T) {
	st, conn := freshStore(t)
	ctx := context.Background()
	a, b := newBarrierFixture("tenant-a"), newBarrierFixture("tenant-b")
	a.seed(t, st)
	b.seed(t, st)
	anon := Subject{AnonKey: a.gone.Key()}
	if err := st.RecordSignals(ctx, a.tenant, []Signal{a.view(anon, a.e1, "anon-1")}); err != nil {
		t.Fatal(err)
	}
	erase(t, st, a.tenant, a.gone)
	plantResidue(t, conn, a, time.Now().UTC())
	if s, err := st.States(ctx, a.tenant, a.gone, []ContentRef{a.e1, a.e2}); err != nil || len(s) != 0 {
		t.Fatalf("erased user leaked in tenant a: %+v %v", s, err)
	}
	for _, tc := range []struct {
		tenant string
		sub    Subject
		ref    ContentRef
	}{{b.tenant, b.gone, b.e1}, {a.tenant, anon, a.e1}} {
		s, err := st.States(ctx, tc.tenant, tc.sub, []ContentRef{tc.ref})
		if err != nil || !s[tc.ref.Key()].Seen {
			t.Fatalf("%s/%s must stay readable: %+v %v", tc.tenant, tc.sub.Kind(), s, err)
		}
	}
	ma := metricsByID(t, st, a.tenant, []string{a.e1.ContentID}, AllTime())
	if ma[a.e1.ContentID].Viewers != 2 || ma[a.e1.ContentID].AnonViewers != 1 {
		t.Fatalf("tenant a must count keeper and the anonymous key only: %+v", ma)
	}
	mb := metricsByID(t, st, b.tenant, []string{b.e1.ContentID}, AllTime())
	if mb[b.e1.ContentID].Viewers != 2 || mb[b.e1.ContentID].PositiveSubjects != 1 {
		t.Fatalf("tenant b metrics must be untouched: %+v", mb)
	}
	if r, err := st.EnforceErasures(ctx, b.tenant); err != nil || !r.Complete() || len(r.Remaining) != 0 {
		t.Fatalf("tenant b has no erasures to enforce: %+v %v", r, err)
	}
	if r, err := st.EnforceErasures(ctx, a.tenant); err != nil || !r.Complete() {
		t.Fatalf("enforce a: %+v %v", r, err)
	}
	a.assertNoResidue(t, conn, "tenant a enforced")
	if n := countWhere(t, conn, "signals", "tenant = ? AND subject_kind = 'anon'", a.tenant); n != 1 {
		t.Fatalf("anonymous key spelled like the erased user must survive enforcement: %d", n)
	}
	if n := countWhere(t, conn, "signals", "tenant = ? AND subject = ?", b.tenant, b.gone.Key()); n != 3 {
		t.Fatalf("tenant b rows must survive enforcement: %d", n)
	}
}

// The fence insert demands a quorum of every replica of the ledger table, so
// Complete() never precedes a replica that could still serve unfiltered
// reads; the replica count comes from system.replicas.
func TestErasureFenceRequiresEveryReplica(t *testing.T) {
	fc := &fakeConn{rowsFor: map[string][][]any{"system.replicas": {{uint32(3)}}}}
	st, _ := NewStore(fc, "hub")
	if _, err := st.EraseSubjects(context.Background(), []string{"t"}, []Subject{{UserID: "u"}}); err != nil {
		t.Fatal(err)
	}
	var fence string
	for _, c := range fc.execs {
		if strings.Contains(c.query, "INSERT INTO hub.erasures") {
			fence = c.query
		}
	}
	if !strings.Contains(fence, "SETTINGS insert_quorum = 3, insert_quorum_parallel = 1") {
		t.Fatalf("fence must wait for every replica:\n%s", fence)
	}
	for _, c := range fc.execs {
		if strings.HasPrefix(c.query, "ALTER TABLE") && !strings.Contains(c.query, "mutations_sync = 2") {
			t.Fatalf("deletes must wait for every replica:\n%s", c.query)
		}
	}
}

// Every statement that reads or writes subject rows carries the barrier; one
// without it is exactly the mutation the integration tests above catch.
func TestEveryReadCarriesTheErasureBarrier(t *testing.T) {
	fc := &fakeConn{}
	st, _ := NewStore(fc, "hub")
	ctx := context.Background()
	sub := Subject{UserID: "u"}
	ref := gallery("t", cid(1))
	_, _ = st.States(ctx, "t", sub, []ContentRef{ref})
	_, _ = st.History(ctx, "t", sub, HistoryOptions{})
	_, _ = st.HistoryCount(ctx, "t", sub, HistoryOptions{})
	_, _ = st.SeenIDs(ctx, "t", sub, "gallery")
	_, _ = st.NegativeIDs(ctx, "t", sub, nil)
	_, _ = st.TopStates(ctx, "t", sub, TopStatesOptions{})
	_, _ = st.Metrics(ctx, "t", []ContentRef{ref}, AllTime())
	_, _ = st.Popular(ctx, "t", "gallery", PopularOptions{})
	_, _ = st.CoEngaged(ctx, "t", ref, CoEngagedOptions{SkipRollup: true})
	_, _ = st.Inventory(ctx, "t")
	_, _ = st.RepairProjections(ctx, "t", RepairOptions{})
	_ = st.RefreshCoEngagement(ctx, "t", RefreshCoEngagementOptions{})
	_ = st.RecordSignals(ctx, "t", []Signal{{ContentRef: ref, Subject: sub, Type: TypeView, EventID: "e", OccurredAt: time.Now()}})
	rows := ", tenant) NOT IN (SELECT subject_hash, tenant FROM hub.erasures)"
	single := "(SELECT count() FROM hub.erasures WHERE tenant = ? AND subject_hash = sipHash128(?)) = 0"
	n := 0
	for _, c := range append(fc.queries, fc.execs...) {
		q := c.query
		if strings.Contains(q, "WHERE sipHash128(concat(t.1") {
			continue // the writer's pre-insert fence check
		}
		if !strings.Contains(q, "hub.subject_content_state") && !strings.Contains(q, "hub.subject_content_daily") && !strings.Contains(q, "hub.signals") {
			continue
		}
		if !strings.Contains(q, rows) && !strings.Contains(q, single) {
			t.Fatalf("read without the erasure barrier:\n%s", q)
		}
		n++
	}
	if n != 15 {
		t.Fatalf("expected 15 barrier-carrying statements, saw %d", n)
	}
}

// Two replicas of one shard behind one Keeper (CONTENTKIT_TEST_CH_ADDR2 is the
// second replica): the writer pauses on replica B, the erasure runs on
// replica A, and B is fenced the moment A returns Complete().
func TestIntegrationErasureBarrierAcrossReplicas(t *testing.T) {
	env := signaltest.FromEnv(t)
	envB := env
	envB.Addr = os.Getenv("CONTENTKIT_TEST_CH_ADDR2")
	if envB.Addr == "" {
		t.Skip("CONTENTKIT_TEST_CH_ADDR2 not set; single replica")
	}
	stA, connA := freshStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	connB := envB.Open(t, testDB)
	stB, _ := NewStore(connB, testDB)
	total, err := stA.erasureQuorum(ctx)
	if err != nil || total != 2 {
		t.Fatalf("expected two replicas of the ledger: %d %v", total, err)
	}
	sync := func(conn Conn) {
		t.Helper()
		for _, table := range append(append([]string{}, subjectTables...), "erasures", "content_pairs") {
			if err := conn.Exec(ctx, "SYSTEM SYNC REPLICA "+testDB+"."+table); err != nil {
				t.Fatal(err)
			}
		}
	}
	f := newBarrierFixture("barrier")
	f.seed(t, stA)
	sync(connB)
	if n := f.rawRows(t, connB, "signals"); n != 3 {
		t.Fatalf("replica B must hold the seed: %d", n)
	}
	gate := newGate(envB.Open(t, testDB), "INSERT INTO "+testDB+".signals")
	writer, _ := NewStore(gate, testDB)
	done := make(chan error, 1)
	go func() { done <- writer.RecordSignals(ctx, f.tenant, []Signal{f.view(f.gone, f.e1, "gone-late")}) }()
	gate.wait(t, ctx)
	erase(t, stA, f.tenant, f.gone)
	// Without any sync: the fence is on B by quorum and the deletes waited for B.
	if n := countWhere(t, connB, "erasures", "tenant = ?", f.tenant); n != 1 {
		t.Fatalf("fence must be on replica B when Complete() returns: %d", n)
	}
	f.assertNoResidue(t, connB, "replica B right after Complete()")
	f.assertInvisible(t, stB, "replica B right after Complete()")
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.assertNoResidue(t, connB, "paused writer released on B")
	f.assertInvisible(t, stB, "paused writer released on B")
	// Residue that did land (a restore on B) is hidden on A and B alike.
	plantResidue(t, connB, f, f.at.AddDate(-1, 0, 0))
	sync(connA)
	if n := f.rawRows(t, connA, "signals"); n != 1 {
		t.Fatalf("the residue replicates to A: %d", n)
	}
	f.assertInvisible(t, stA, "residue replicated to A")
	f.assertInvisible(t, stB, "residue on B")
	if r, err := stB.EnforceErasures(ctx, f.tenant); err != nil || !r.Complete() {
		t.Fatalf("enforce on B: %+v %v", r, err)
	}
	sync(connA)
	f.assertNoResidue(t, connA, "after enforcement on B")
	f.assertNoResidue(t, connB, "after enforcement on B")
}
