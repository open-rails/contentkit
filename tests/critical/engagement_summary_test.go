package critical_test

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	signalstore "github.com/open-rails/contentkit/internal/signalstore"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
)

// summaryLog keeps actual concurrent slog writes race-safe. It replaces no
// storage operation: every failure below is raised by the real ClickHouse server.
type summaryLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *summaryLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *summaryLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func measuredRef(t *testing.T, a *application) contentref.ContentRef {
	t.Helper()
	item, err := uuid.NewV7()
	must(t, err)
	version, err := uuid.NewV7()
	must(t, err)
	return contentref.NewVersion(a.cfg.Scope.AppID.String(), "article", item.String(), version.String())
}

func measuredReceipt(t *testing.T, ref contentref.ContentRef, subject signal.Subject, at time.Time, page, active uint32) signal.Signal {
	t.Helper()
	m, err := signalstore.MeasurePages([]signal.PageEngagement{{Index: page, Ranges: []signal.CoverageRange{{From: 0, To: 10_000}}, ActiveMS: active}}, 2, active)
	must(t, err)
	m.VersionID, m.VisitID, m.SegmentID = ref.Version(), uuid.NewString(), uuid.NewString()
	return signal.Signal{ContentRef: ref, Subject: subject, Type: signal.TypeEngagement, EventID: uuid.NewString(), Revision: 1,
		OccurredAt: at, Measurement: &m, Resume: "page:" + strconv.Itoa(int(page)),
		Access: &signal.AccessContext{ObservedAt: at, ContentAccess: "public", ViewerAccess: "full", PolicyRevision: "critical-summary-v1", Audience: "public", EligibilityKnown: true}}
}

// rejectSignalInserts installs an actual INSERT constraint in this test's owned
// database. SELECT and other tables remain live, so preserved source and stale
// reads are real outcomes, not mocked acknowledgements.
func rejectSignalInserts(t *testing.T, a *application, table string) func() {
	t.Helper()
	name := a.cfg.ClickHouse.Database + "." + table
	must(t, a.deps.ClickHouse.Exec(t.Context(), "ALTER TABLE "+name+" ADD CONSTRAINT ck257_reject CHECK 0"))
	var once sync.Once
	restore := func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			must(t, a.deps.ClickHouse.Exec(ctx, "ALTER TABLE "+name+" DROP CONSTRAINT ck257_reject"))
		})
	}
	t.Cleanup(restore)
	return restore
}

func canonicalMeasured(t *testing.T, a *application, ref contentref.ContentRef) (uint64, uint64) {
	t.Helper()
	rows, err := a.deps.ClickHouse.Query(t.Context(), `SELECT count(),sum(active_ms) FROM `+a.cfg.ClickHouse.Database+`.engagement_segments FINAL
 WHERE tenant=? AND content_id=?`, ref.TenantID, ref.ContentID)
	must(t, err)
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("canonical measurement query returned no aggregate")
	}
	var count, active uint64
	must(t, rows.Scan(&count, &active))
	must(t, rows.Err())
	return count, active
}

func TestEngagementSummaryFailuresAreBestEffort(t *testing.T) {
	for _, failedTable := range []string{"engagement_state", "engagement_daily"} {
		t.Run(failedTable, func(t *testing.T) {
			w := newWorld(t)
			a := w.apps[0]
			configureDiscoverySignals(t, w, a)
			store, err := signalstore.NewStore(a.deps.ClickHouse, a.cfg.ClickHouse.Database)
			must(t, err)
			ref := measuredRef(t, a)
			subject := signal.Subject{UserID: uuid.NewString()}
			at := time.Now().UTC().Truncate(time.Second)
			first := measuredReceipt(t, ref, subject, at, 0, 1000)
			must(t, store.RecordSignals(t.Context(), ref.TenantID, []signal.Signal{first}))
			state := func() signal.State {
				t.Helper()
				states, err := store.States(t.Context(), ref.TenantID, subject, []signal.ContentRef{ref})
				must(t, err)
				got, ok := states[ref.Key()]
				if !ok {
					t.Fatal("successful positive-control measurement has no readable state")
				}
				return got
			}
			activity := func() uint64 {
				t.Helper()
				days, err := store.Activity(t.Context(), ref.TenantID, ref.ContentKind, signalstore.LastDays(1, at))
				must(t, err)
				if len(days) != 1 || days[0].Readers != 1 {
					t.Fatalf("activity positive control changed: %+v", days)
				}
				return days[0].ActiveMS
			}
			if state().ActiveMS != 1000 || activity() != 1000 {
				t.Fatal("healthy summaries do not reflect canonical measurement")
			}
			restore := rejectSignalInserts(t, a, failedTable)
			logs := &summaryLog{}
			priorLog := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
			defer slog.SetDefault(priorLog)
			second := measuredReceipt(t, ref, subject, at.Add(time.Second), 1, 2000)
			if err := store.RecordSignals(t.Context(), ref.TenantID, []signal.Signal{second}); err != nil {
				count, active := canonicalMeasured(t, a, ref)
				t.Fatalf("derived %s error rejected accepted canonical engagement (receipts=%d active=%d): %v", failedTable, count, active, err)
			}
			// Retry uses the same receipt, and a higher cumulative revision replaces
			// that receipt rather than becoming another consumption event.
			must(t, store.RecordSignals(t.Context(), ref.TenantID, []signal.Signal{second}))
			revised := measuredReceipt(t, ref, subject, second.OccurredAt, 1, 3000)
			revised.EventID, revised.Revision = second.EventID, 2
			revised.Measurement.VisitID, revised.Measurement.SegmentID = second.Measurement.VisitID, second.Measurement.SegmentID
			must(t, store.RecordSignals(t.Context(), ref.TenantID, []signal.Signal{revised}))
			if count, active := canonicalMeasured(t, a, ref); count != 2 || active != 4000 {
				t.Fatalf("canonical replay/revision changed identity: receipts=%d active=%d", count, active)
			}
			wantState := uint64(1000)
			if failedTable == "engagement_daily" {
				wantState = 4000
			}
			if state().ActiveMS != wantState || activity() != 1000 {
				t.Fatalf("expected stale %s only after canonical acceptance: state=%+v daily=%d", failedTable, state(), activity())
			}
			if log := logs.String(); strings.Count(log, "engagement summaries are stale") != 3 || !strings.Contains(log, "ck257_reject") || !strings.Contains(log, ref.TenantID) {
				t.Fatalf("accepted derived failures were not logged with the actual server error: %s", log)
			}
			if _, err := store.RepairProjections(t.Context(), ref.TenantID, signal.RepairOptions{Rebuild: true}); err == nil || !strings.Contains(err.Error(), "ck257_reject") {
				t.Fatalf("explicit repair swallowed real derived error: %v", err)
			}
			restore()
			// Merely recovering the table starts no repair. A fresh interaction is
			// what recomputes all source receipts for the touched user/item.
			if state().ActiveMS != wantState || activity() != 1000 {
				t.Fatal("table recovery unexpectedly rewrote summaries")
			}
			third := measuredReceipt(t, ref, subject, at.Add(2*time.Second), 0, 1000)
			must(t, store.RecordSignals(t.Context(), ref.TenantID, []signal.Signal{third}))
			must(t, store.RecordSignals(t.Context(), ref.TenantID, []signal.Signal{third}))
			if count, active := canonicalMeasured(t, a, ref); count != 3 || active != 5000 {
				t.Fatalf("new interaction/replay changed source totals: receipts=%d active=%d", count, active)
			}
			got := state()
			if got.ActiveMS != 5000 || got.MaxProgress != 2 || got.ProgressMax != 2 || !got.Completed || got.Resume != third.Resume || activity() != 5000 {
				t.Fatalf("next interaction did not recompute saved history: state=%+v daily=%d", got, activity())
			}
			count, err := store.CompletedCount(t.Context(), ref.TenantID, subject, ref.ContentKind)
			must(t, err)
			if count != 1 {
				t.Fatalf("completion counted receipts instead of one item: %d", count)
			}
			repaired, err := store.RepairProjections(t.Context(), ref.TenantID, signal.RepairOptions{Rebuild: true})
			must(t, err)
			if repaired.Examined == 0 || repaired.Repaired == 0 {
				t.Fatalf("manual rebuild was vacuous: %+v", repaired)
			}
			if state().ActiveMS != 5000 || activity() != 5000 {
				t.Fatal("explicit rebuild duplicated accepted engagement")
			}
		})
	}
}

func TestEngagementCanonicalFailuresRemainErrors(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	configureDiscoverySignals(t, w, a)
	store, err := signalstore.NewStore(a.deps.ClickHouse, a.cfg.ClickHouse.Database)
	must(t, err)
	ref := measuredRef(t, a)
	subject := signal.Subject{UserID: uuid.NewString()}
	at := time.Now().UTC().Truncate(time.Second)
	valid := measuredReceipt(t, ref, subject, at, 0, 1000)
	invalid := measuredReceipt(t, ref, subject, at, 1, 1000)
	invalid.Measurement.Units = nil // retained source geometry no longer matches its normalization
	if err := store.RecordSignals(t.Context(), ref.TenantID, []signal.Signal{invalid}); err == nil {
		t.Fatal("invalid canonical measurement was accepted")
	}
	if count, _ := canonicalMeasured(t, a, ref); count != 0 {
		t.Fatal("invalid canonical measurement reached storage")
	}
	restore := rejectSignalInserts(t, a, "engagement_segments")
	if err := store.RecordSignals(t.Context(), ref.TenantID, []signal.Signal{valid}); err == nil || !strings.Contains(err.Error(), "ck257_reject") {
		t.Fatalf("canonical server error was swallowed: %v", err)
	}
	if count, _ := canonicalMeasured(t, a, ref); count != 0 {
		t.Fatal("failed canonical insert left a receipt")
	}
	restore()
	// The existing bounded partial-write contract is retained: a later invalid
	// row reports its error without pretending an earlier valid insert rolled back.
	if err := store.RecordSignals(t.Context(), ref.TenantID, []signal.Signal{valid, invalid}); err == nil {
		t.Fatal("later invalid canonical row was silently accepted")
	}
	if count, active := canonicalMeasured(t, a, ref); count != 1 || active != 1000 {
		t.Fatalf("partial canonical failure lost accepted row: count=%d active=%d", count, active)
	}
	stateRestore := rejectSignalInserts(t, a, "engagement_state")
	genericRestore := rejectSignalInserts(t, a, "signals")
	generic := signal.Signal{ContentRef: ref, Subject: subject, Type: "click", EventID: uuid.NewString(), Revision: 1, OccurredAt: at}
	if err := store.RecordSignals(t.Context(), ref.TenantID, []signal.Signal{valid, generic}); err == nil || !strings.Contains(err.Error(), "insert signals") {
		t.Fatalf("derived best effort masked a later canonical generic failure: %v", err)
	}
	if count, active := canonicalMeasured(t, a, ref); count != 1 || active != 1000 {
		t.Fatalf("mixed canonical replay doubled saved measurement: count=%d active=%d", count, active)
	}
	genericRestore()
	stateRestore()
	must(t, store.RecordSignals(t.Context(), ref.TenantID, []signal.Signal{valid, generic}))
	states, err := store.States(t.Context(), ref.TenantID, subject, []signal.ContentRef{ref})
	must(t, err)
	if states[ref.Key()].ActiveMS != 1000 {
		t.Fatalf("successful canonical retry did not restore summary: %+v", states)
	}
}
