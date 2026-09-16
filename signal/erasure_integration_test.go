package signal

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/open-rails/searchkit/internal/signaltest"
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
	// Start on the 0001 schema so legacy raw rows exist too.
	conn := env.Empty(t, testDB)
	env.ApplyRange(t, conn, 0, 1)
	if err := conn.Exec(ctx, `INSERT INTO signal_events (tenant, entity_type, entity_id, subject_kind, subject, signal_type, event_id, occurred_at, progress, progress_max) VALUES
('doujins', 'gallery', '1', 'user', 'gone', 'view', 'legacy-gone', '2026-04-01 10:00:00', 1, 2),
('doujins', 'gallery', '1', 'user', 'kept', 'view', 'legacy-kept', '2026-04-01 10:00:00', 1, 2)`); err != nil {
		t.Fatal(err)
	}
	env.ApplyRange(t, conn, 1, len(env.Migrations(t)))
	if err := CheckSchema(ctx, conn, testDB); err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(conn, testDB)
	if err != nil {
		t.Fatal(err)
	}
	gone, kept, anonGone := Subject{UserID: "gone"}, Subject{UserID: "kept"}, Subject{AnonKey: "gone"}
	at := time.Date(2026, 5, 2, 9, 0, 0, 0, time.UTC)
	view := func(sub Subject, id string, day int) Signal {
		return Signal{EntityRef: EntityRef{EntityType: "gallery", EntityID: id}, Subject: sub, Type: TypeView,
			EventID: sub.Kind() + sub.Key() + id, OccurredAt: at.AddDate(0, 0, day), Progress: 2, ProgressMax: 2, Score: 50, Completed: true}
	}
	for _, tenant := range []string{"doujins", "hentai0"} {
		batch := []Signal{view(gone, "1", 0), view(gone, "2", 1), view(kept, "1", 0), view(kept, "2", 0), view(anonGone, "2", 0)}
		like := view(gone, "1", 0)
		like.Type, like.EventID, like.Value = "reaction", "pref", 1
		batch = append(batch, like)
		if err := st.RecordSignals(ctx, tenant, batch); err != nil {
			t.Fatal(err)
		}
		shown := []Placement{{EntityRef: EntityRef{EntityType: "gallery", EntityID: "1"}, Position: 1}}
		if err := st.RecordExposures(ctx, tenant, []Exposure{
			{RenderID: tenant + "-gone", Stage: StageServed, Subject: gone, Shown: shown, OccurredAt: at},
			{RenderID: tenant + "-kept", Stage: StageServed, Subject: kept, Shown: shown, OccurredAt: at},
			{RenderID: tenant + "-anon", Stage: StageServed, Shown: shown, OccurredAt: at},
		}); err != nil {
			t.Fatal(err)
		}
		if err := conn.Exec(ctx, `INSERT INTO search_impressions (tenant, query_id, subject_kind, subject, shown_entity_types, shown_entity_ids, shown_positions, occurred_at)
VALUES (?, 'legacy-gone', 'user', 'gone', ['gallery'], ['1'], [1], '2026-05-02 09:00:00'), (?, 'legacy-kept', 'user', 'kept', ['gallery'], ['1'], [1], '2026-05-02 09:00:00')`, tenant, tenant); err != nil {
			t.Fatal(err)
		}
		if err := st.RefreshCoEngagement(ctx, tenant, RefreshCoEngagementOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.RepairProjections(ctx, "doujins", RepairOptions{Rebuild: true}); err != nil {
		t.Fatal(err)
	}
	before, err := st.Metrics(ctx, "doujins", "gallery", []string{"1", "2"}, AllTime())
	if err != nil {
		t.Fatal(err)
	}
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
		for _, table := range []string{"events", "subject_state", "subject_daily", "exposures", "search_impressions", "signal_events"} {
			if n := countWhere(t, conn, table, "tenant = ? AND subject_kind = 'user' AND subject = 'gone'", tenant); n != 0 {
				t.Fatalf("%s.%s still holds %d rows", tenant, table, n)
			}
		}
		if n := countWhere(t, conn, "events", "tenant = ? AND subject_kind = 'anon' AND subject = 'gone'", tenant); n != 1 {
			t.Fatalf("anonymous subject with the same key must survive: %d", n)
		}
		hist, err := st.History(ctx, tenant, gone, HistoryOptions{})
		if err != nil || len(hist) != 0 {
			t.Fatalf("history after erasure: %v %v", hist, err)
		}
		if n := countWhere(t, conn, "item_pairs", "tenant = ?", tenant); n != 0 {
			t.Fatalf("pairs involving erased contributions must be removed until refreshed: %d", n)
		}
	}
	after, err := st.Metrics(ctx, "doujins", "gallery", []string{"1", "2"}, AllTime())
	if err != nil {
		t.Fatal(err)
	}
	// kept has a canonical and a legacy-copied view of gallery 1.
	if after["1"].Viewers != 1 || after["2"].Viewers != 2 || after["1"].PositiveSubjects != 0 || after["1"].Views != 2 {
		t.Fatalf("aggregates must exclude exactly the erased subject: %+v", after)
	}
	if n := countWhere(t, conn, "exposures", "tenant = 'doujins'"); n != 2 {
		t.Fatalf("other and anonymous exposures must remain: %d", n)
	}
	if err := st.RefreshCoEngagement(ctx, "doujins", RefreshCoEngagementOptions{}); err != nil {
		t.Fatal(err)
	}
	co, err := st.CoEngaged(ctx, "doujins", EntityRef{EntityType: "gallery", EntityID: "1"}, CoEngagedOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(co) != 1 || co[0].EntityID != "2" || co[0].Strength != 1 {
		t.Fatalf("rebuilt pairs must count only kept: %+v", co)
	}

	// Ingestion fence: later writes and impressions for the subject vanish;
	// repair never resurrects.
	if err := st.RecordSignals(ctx, "doujins", []Signal{view(gone, "3", 2), view(kept, "3", 2)}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordExposures(ctx, "doujins", []Exposure{{RenderID: "late", Stage: StageServed, Subject: gone, Shown: []Placement{{EntityRef: EntityRef{EntityType: "gallery", EntityID: "3"}, Position: 1}}, OccurredAt: at}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RepairProjections(ctx, "doujins", RepairOptions{Rebuild: true}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"events", "subject_state", "subject_daily", "exposures"} {
		if n := countWhere(t, conn, table, "tenant = 'doujins' AND subject_kind = 'user' AND subject = 'gone'"); n != 0 {
			t.Fatalf("fence leaked into %s: %d", table, n)
		}
	}
	m3, err := st.Metrics(ctx, "doujins", "gallery", []string{"3"}, AllTime())
	if err != nil || m3["3"].Viewers != 1 {
		t.Fatalf("kept subject's write in the same batch must land: %+v %v", m3, err)
	}

	// Residue: a writer that passed the fence before it existed, or a restored
	// backup, re-introduces rows; enforcement removes them.
	if err := conn.Exec(ctx, `INSERT INTO events (tenant, entity_type, entity_id, subject_kind, subject, signal_type, event_id, occurred_at)
VALUES ('doujins', 'gallery', '9', 'user', 'gone', 'view', 'residue', '2026-05-09 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(ctx, `INSERT INTO exposures (tenant, render_id, stage, surface, subject_kind, subject, entity_types, entity_ids, positions, occurred_at)
VALUES ('doujins', 'residue', 'served', 'search', 'user', 'gone', ['gallery'], ['9'], [1], '2026-05-09 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RepairProjections(ctx, "doujins", RepairOptions{Rebuild: true}); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, conn, "subject_state", "tenant = 'doujins' AND subject = 'gone'"); n == 0 {
		t.Fatal("precondition: residue projected")
	}
	enforced, err := st.EnforceErasures(ctx, "doujins")
	if err != nil || !enforced.Complete() {
		t.Fatalf("enforce: %+v %v", enforced, err)
	}
	for _, table := range []string{"events", "subject_state", "subject_daily", "exposures"} {
		if n := countWhere(t, conn, table, "tenant = 'doujins' AND subject_kind = 'user' AND subject = 'gone'"); n != 0 {
			t.Fatalf("residue remains in %s: %d", table, n)
		}
	}
	if n := countWhere(t, conn, "events", "tenant = 'doujins' AND subject = 'kept'"); n != 4 {
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
