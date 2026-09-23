package signal

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/signaltest"
)

func countWhere(t *testing.T, conn Conn, table, where string, args ...any) uint64 {
	t.Helper()
	rows, err := conn.Query(context.Background(), fmt.Sprintf("SELECT count() FROM %s.%s WHERE %s", testDB, table, where), args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n uint64
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// #877: complete subject erasure across tenants and every table, aggregate
// and pair replacement, ingestion fence, rebuild barrier, residue enforcement
// and restore behavior.
func TestIntegrationEraseSubjectsCompletely(t *testing.T) {
	env := signaltest.FromEnv(t)
	ctx := context.Background()
	// Include an earlier distinct session alongside the current sessions.
	conn := env.Fresh(t, testDB)
	if err := conn.Exec(ctx, `INSERT INTO signals (tenant, content_kind, content_id, subject_kind, subject, signal_type, event_id, occurred_at, progress, progress_max) VALUES
('doujins', 'gallery', '1', 'user', 'gone', 'view', 'legacy-gone', '2026-04-01 10:00:00', 1, 2),
('doujins', 'gallery', '1', 'user', 'kept', 'view', 'legacy-kept', '2026-04-01 10:00:00', 1, 2)`); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchema(ctx, conn, testDB); err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(conn, testDB)
	if err != nil {
		t.Fatal(err)
	}
	gone, kept, anonGone := Subject{UserID: "gone"}, Subject{UserID: "kept"}, Subject{AnonKey: "gone"}
	at := time.Date(2026, 5, 2, 9, 0, 0, 0, time.UTC)
	view := func(tenant string, sub Subject, id string, day int) Signal {
		return Signal{ContentRef: gallery(tenant, id), Subject: sub, Type: TypeView,
			EventID: sub.Kind() + sub.Key() + id, OccurredAt: at.AddDate(0, 0, day), Progress: 2, ProgressMax: 2, Score: 50, Completed: true}
	}
	for _, tenant := range []string{"doujins", "hentai0"} {
		batch := []Signal{view(tenant, gone, "1", 0), view(tenant, gone, "2", 1), view(tenant, kept, "1", 0), view(tenant, kept, "2", 0), view(tenant, anonGone, "2", 0)}
		like := view(tenant, gone, "1", 0)
		like.Type, like.EventID, like.Value = "reaction", "pref", 1
		batch = append(batch, like)
		if err := st.RecordSignals(ctx, tenant, batch); err != nil {
			t.Fatal(err)
		}
		shown := []Placement{{ContentRef: gallery(tenant, "1"), Position: 1}}
		if err := st.RecordExposures(ctx, tenant, []Exposure{
			{RenderID: tenant + "-gone", Stage: StageServed, Subject: gone, Shown: shown, OccurredAt: at},
			{RenderID: tenant + "-kept", Stage: StageServed, Subject: kept, Shown: shown, OccurredAt: at},
			{RenderID: tenant + "-anon", Stage: StageServed, Shown: shown, OccurredAt: at},
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.RefreshCoEngagement(ctx, tenant, RefreshCoEngagementOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.RepairProjections(ctx, "doujins", RepairOptions{Rebuild: true}); err != nil {
		t.Fatal(err)
	}
	before := metricsByID(t, st, "doujins", []string{"1", "2"}, AllTime())
	if before["1"].Viewers != 2 || before["2"].Viewers != 3 || before["1"].PositiveSubjects != 1 {
		t.Fatalf("precondition metrics: %+v", before)
	}

	// Validation.
	if _, err := st.EraseSubjects(ctx, nil, []Subject{gone}); err == nil {
		t.Fatal("tenants are required")
	}
	if _, err := st.EraseSubjects(ctx, []string{"doujins"}, []Subject{{}}); err == nil {
		t.Fatal("invalid subject must error")
	}
	var limit *LimitError
	if _, err := st.EraseSubjects(ctx, []string{"doujins"}, make([]Subject, MaxErasureSubjects+1)); !errors.As(err, &limit) {
		t.Fatalf("limit: %v", err)
	}

	// Erase the user in both tenants; the anonymous key spelled the same stays.
	report, err := st.EraseSubjects(ctx, []string{"doujins", "hentai0"}, []Subject{gone, gone})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete() || report.PairsRemoved == 0 {
		t.Fatalf("report: %+v", report)
	}
	for _, tenant := range []string{"doujins", "hentai0"} {
		for _, table := range []string{"signals", "subject_content_state", "subject_content_daily", "exposures"} {
			if n := countWhere(t, conn, table, "tenant = ? AND subject_kind = 'user' AND subject = 'gone'", tenant); n != 0 {
				t.Fatalf("%s.%s still holds %d rows", tenant, table, n)
			}
		}
		if n := countWhere(t, conn, "signals", "tenant = ? AND subject_kind = 'anon' AND subject = 'gone'", tenant); n != 1 {
			t.Fatalf("anonymous subject with the same key must survive: %d", n)
		}
		hist, err := st.History(ctx, tenant, gone, HistoryOptions{})
		if err != nil || len(hist) != 0 {
			t.Fatalf("history after erasure: %v %v", hist, err)
		}
		if n := countWhere(t, conn, "content_pairs", "tenant = ?", tenant); n != 0 {
			t.Fatalf("pairs involving erased contributions must be removed until refreshed: %d", n)
		}
	}
	after := metricsByID(t, st, "doujins", []string{"1", "2"}, AllTime())
	// kept retains both distinct sessions on gallery 1.
	if after["1"].Viewers != 1 || after["2"].Viewers != 2 || after["1"].PositiveSubjects != 0 || after["1"].Views != 2 {
		t.Fatalf("aggregates must exclude exactly the erased subject: %+v", after)
	}
	if n := countWhere(t, conn, "exposures", "tenant = 'doujins'"); n != 2 {
		t.Fatalf("other and anonymous exposures must remain: %d", n)
	}
	if err := st.RefreshCoEngagement(ctx, "doujins", RefreshCoEngagementOptions{}); err != nil {
		t.Fatal(err)
	}
	co, err := st.CoEngaged(ctx, "doujins", gallery("doujins", "1"), CoEngagedOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(co) != 1 || co[0].ContentID != "2" || co[0].Strength != 1 {
		t.Fatalf("rebuilt pairs must count only kept: %+v", co)
	}

	// Ingestion fence: later writes and impressions for the subject vanish;
	// repair never resurrects.
	if err := st.RecordSignals(ctx, "doujins", []Signal{view("doujins", gone, "3", 2), view("doujins", kept, "3", 2)}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordExposures(ctx, "doujins", []Exposure{{RenderID: "late", Stage: StageServed, Subject: gone, Shown: []Placement{{ContentRef: gallery("doujins", "3"), Position: 1}}, OccurredAt: at}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RepairProjections(ctx, "doujins", RepairOptions{Rebuild: true}); err != nil {
		t.Fatal(err)
	}
	for _, table := range subjectTables {
		if n := countWhere(t, conn, table, "tenant = 'doujins' AND subject_kind = 'user' AND subject = 'gone'"); n != 0 {
			t.Fatalf("fence leaked into %s: %d", table, n)
		}
	}
	if m3 := metricsByID(t, st, "doujins", []string{"3"}, AllTime()); m3["3"].Viewers != 1 {
		t.Fatalf("kept subject's write in the same batch must land: %+v", m3)
	}

	// Residue: a writer that passed the fence before it existed, or a restored
	// backup, re-introduces rows; enforcement removes them.
	if err := conn.Exec(ctx, `INSERT INTO signals (tenant, content_kind, content_id, subject_kind, subject, signal_type, event_id, occurred_at)
VALUES ('doujins', 'gallery', '9', 'user', 'gone', 'view', 'residue', '2026-05-09 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(ctx, `INSERT INTO exposures (tenant, render_id, stage, surface, subject_kind, subject, content_kinds, content_ids, positions, occurred_at)
VALUES ('doujins', 'residue', 'served', 'search', 'user', 'gone', ['gallery'], ['9'], [1], '2026-05-09 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RepairProjections(ctx, "doujins", RepairOptions{Rebuild: true}); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, conn, "subject_content_state", "tenant = 'doujins' AND subject = 'gone'"); n == 0 {
		t.Fatal("precondition: residue projected")
	}
	enforced, err := st.EnforceErasures(ctx, "doujins")
	if err != nil || !enforced.Complete() {
		t.Fatalf("enforce: %+v %v", enforced, err)
	}
	for _, table := range subjectTables {
		if n := countWhere(t, conn, table, "tenant = 'doujins' AND subject_kind = 'user' AND subject = 'gone'"); n != 0 {
			t.Fatalf("residue remains in %s: %d", table, n)
		}
	}
	if n := countWhere(t, conn, "signals", "tenant = 'doujins' AND subject = 'kept'"); n != 4 {
		t.Fatalf("kept subject damaged by enforcement: %d", n)
	}
	// Idempotent.
	if report, err := st.EraseSubjects(ctx, []string{"doujins"}, []Subject{gone}); err != nil || !report.Complete() {
		t.Fatalf("second erasure: %+v %v", report, err)
	}
	if n := countWhere(t, conn, "erasures", "tenant = 'doujins'"); n == 0 {
		t.Fatal("fence must be recorded")
	}
}

func TestIntegrationRekeySubjectPreservesHistoryAndErasure(t *testing.T) {
	st, conn := freshStore(t)
	ctx := context.Background()
	tenant := "doujins"
	old := Subject{AnonKey: "anon_old"}
	next := Subject{AnonKey: "anon_v1_hashed"}
	at := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	view := func(subject Subject, id string) Signal {
		return Signal{ContentRef: gallery(tenant, id), Subject: subject, Type: TypeView,
			EventID: "view-" + id, OccurredAt: at, Progress: 1, ProgressMax: 2}
	}
	if err := st.RecordSignals(ctx, tenant, []Signal{view(old, "1"), view(next, "2")}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordExposures(ctx, tenant, []Exposure{{RenderID: "old-render", Stage: StageRendered,
		Subject: old, Surface: SurfaceSearch, Shown: []Placement{{ContentRef: gallery(tenant, "1"), Position: 1}}, OccurredAt: at}}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := st.RekeyAnonymousSubject(ctx, tenant, old, next); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range subjectTables {
		if n := countWhere(t, conn, table, "tenant = ? AND subject_kind = 'anon' AND subject = ?", tenant, old.Key()); n != 0 {
			t.Fatalf("%s retained %d rows under the cookie", table, n)
		}
	}
	history, err := st.History(ctx, tenant, next, HistoryOptions{ContentKind: "gallery", Status: HistorySeen})
	if err != nil || len(history) != 2 {
		t.Fatalf("rekeyed history: %+v %v", history, err)
	}
	if n := countWhere(t, conn, "exposures", "tenant = ? AND subject_kind = 'anon' AND subject = ?", tenant, next.Key()); n != 1 {
		t.Fatalf("rekeyed exposures: %d", n)
	}
	if err := st.RecordSignals(ctx, tenant, []Signal{view(old, "3")}); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, conn, "signals", "tenant = ? AND subject_kind = 'anon' AND subject = ?", tenant, old.Key()); n != 0 {
		t.Fatalf("old subject accepted a late write: %d", n)
	}
	if _, err := st.EraseSubjects(ctx, []string{tenant}, []Subject{next}); err != nil {
		t.Fatal(err)
	}
	for _, table := range subjectTables {
		if n := countWhere(t, conn, table, "tenant = ? AND subject_kind = 'anon' AND subject = ?", tenant, next.Key()); n != 0 {
			t.Fatalf("%s retained %d rows after erasure", table, n)
		}
	}
}
