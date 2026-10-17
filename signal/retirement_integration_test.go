package signal

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/signaltest"
)

func TestRetirementPreservesResumeAndMeasuredDataWithoutLegacyCounters(t *testing.T) {
	st, conn := freshStore(t)
	ctx := t.Context()
	env := signaltest.FromEnv(t)
	tenant := "retirement"
	subject := Subject{UserID: "reader"}
	old, new := gallery(tenant, cid(510)), gallery(tenant, cid(511))
	bookmark, finished, opened := gallery(tenant, cid(512)), gallery(tenant, cid(513)), gallery(tenant, cid(514))
	if err := conn.Exec(ctx, `INSERT INTO `+testDB+`.subject_content_state
 (tenant,subject_kind,subject,content_kind,content_id,first_seen_at,last_view_at,max_progress,progress_max,completed,resume,version)
 VALUES (?,'user',?,'gallery',?,toDateTime('2026-05-01'),toDateTime('2026-05-02'),9,10,true,'009',1)`, tenant, subject.Key(), old.ContentID); err != nil {
		t.Fatal(err)
	}
	// Legacy players persisted an exact resume selector or a completion without
	// numeric progress; both are useful history. A bare open is not.
	for _, row := range []struct {
		ref       ContentRef
		completed bool
		resume    string
	}{{bookmark, false, "17.5"}, {finished, true, ""}, {opened, false, ""}} {
		if err := conn.Exec(ctx, `INSERT INTO `+testDB+`.subject_content_state
 (tenant,subject_kind,subject,content_kind,content_id,first_seen_at,last_view_at,views,completed,resume,version)
 VALUES (?,'user',?,'gallery',?,toDateTime('2026-05-01'),toDateTime('2026-05-02'),3,?,?,1)`, tenant, subject.Key(), row.ref.ContentID, row.completed, row.resume); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Exec(ctx, `INSERT INTO `+testDB+`.signals
 (tenant,subject_kind,subject,content_kind,content_id,signal_type,event_id,occurred_at)
 VALUES (?,'user',?,'gallery',?,'view','legacy-open',toDateTime('2026-05-02'))`, tenant, subject.Key(), old.ContentID); err != nil {
		t.Fatal(err)
	}
	event := readingFixture{ContentRef: new, Subject: subject, Type: TypeView, EventID: "actual-page", Revision: 1, OccurredAt: at(3, 10), DurationS: 2, Progress: 1, ProgressMax: 1}.native()
	if err := st.RecordSignals(ctx, tenant, []Signal{event}); err != nil {
		t.Fatal(err)
	}
	if err := st.RetireOpenCounters(ctx, RetirementOptions{}); err == nil {
		t.Fatal("retirement must require a stopped-writer confirmation")
	}
	if err := st.RetireOpenCounters(ctx, RetirementOptions{LegacyWritersStopped: true, Cluster: env.Cluster}); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchema(ctx, conn, testDB); err != nil {
		t.Fatalf("post-retirement runtime schema: %v", err)
	}
	refs := []ContentRef{old, new, bookmark, finished, opened}
	states, err := st.States(ctx, tenant, subject, refs)
	if err != nil {
		t.Fatal(err)
	}
	if value := states[old.Key()]; value.Resume != "009" || !value.Completed || value.Quality != 0 || value.ActiveMS != 0 {
		t.Fatalf("legacy history was lost or promoted into quality: %+v", value)
	}
	if value := states[bookmark.Key()]; value.Resume != "17.5" || value.MaxProgress != 0 || value.Quality != 0 || value.ActiveMS != 0 {
		t.Fatalf("resume-only bookmark was lost or promoted into measured evidence: %+v", value)
	}
	if value := states[finished.Key()]; !value.Completed || value.MaxProgress != 0 || value.Quality != 0 || value.ActiveMS != 0 {
		t.Fatalf("complete-only bookmark was lost or promoted into measured evidence: %+v", value)
	}
	if value, ok := states[opened.Key()]; ok {
		t.Fatalf("a bare legacy open became reading history: %+v", value)
	}
	history, err := st.History(ctx, tenant, subject, HistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, row := range history {
		listed[row.ContentID] = true
	}
	if !listed[old.ContentID] || !listed[new.ContentID] || !listed[bookmark.ContentID] || !listed[finished.ContentID] || listed[opened.ContentID] {
		t.Fatalf("history after retirement: %+v", history)
	}
	if value := states[new.Key()]; !value.Completed || value.ActiveMS != 2000 || value.Quality <= 0 {
		t.Fatalf("measured history changed: %+v", value)
	}
	metrics, err := st.Metrics(ctx, tenant, refs, AllTime())
	if err != nil {
		t.Fatal(err)
	}
	if metrics[old.Key()].Readers != 0 || metrics[new.Key()].Readers != 1 {
		t.Fatalf("legacy opens leaked into measured readership: %+v", metrics)
	}
	completed, err := st.CompletedCount(ctx, tenant, subject, "gallery")
	if err != nil || completed != 1 {
		t.Fatalf("legacy completion became measured account evidence: %d %v", completed, err)
	}
	click := Signal{ContentRef: new, Subject: subject, Type: "click", EventID: "after-retirement", OccurredAt: at(4, 10)}
	if err := st.RecordSignals(ctx, tenant, []Signal{click}); err != nil {
		t.Fatalf("new generic writer still depended on retired source columns: %v", err)
	}
	if err := st.RetireOpenCounters(ctx, RetirementOptions{LegacyWritersStopped: true, Cluster: env.Cluster}); err != nil {
		t.Fatalf("idempotent retirement: %v", err)
	}
	report, err := st.EraseSubjects(ctx, []string{tenant}, []Subject{subject})
	if err != nil || !report.Complete() {
		t.Fatalf("new erasure failed after optional old tables were removed: %+v %v", report, err)
	}
	states, err = st.States(ctx, tenant, subject, refs)
	if err != nil || len(states) != 0 {
		t.Fatalf("retired legacy/measured history survived erasure: %+v %v", states, err)
	}
}

// Migration 0003 copies useful legacy reading (progress, resume or completion)
// out of the old projection; bare opens and erased subjects never cross over.
func TestIntegrationMeasuredMigrationCopiesUsefulLegacyReading(t *testing.T) {
	e := signaltest.FromEnv(t)
	ctx := t.Context()
	db := "ck_legacy_copy_" + strings.ReplaceAll(contentref.NewID(), "-", "")
	conn := e.Empty(t, db)
	t.Cleanup(func() { e.Drop(t, conn, db) })
	const measured = "0003_"
	e.ApplyRange(t, conn, "", measured)
	tenant := "legacy"
	for _, row := range []struct {
		subject, id   string
		progress, max uint32
		completed     bool
		resume        string
	}{
		{"reader", cid(1), 4, 10, false, "p:4"},
		{"reader", cid(2), 0, 0, false, "17.5"},
		{"reader", cid(3), 0, 0, true, ""},
		{"reader", cid(4), 0, 0, false, ""},
		{"erased", cid(5), 9, 10, true, "p:9"},
	} {
		if err := conn.Exec(ctx, `INSERT INTO subject_content_state
 (tenant,subject_kind,subject,content_kind,content_id,first_seen_at,last_view_at,views,max_progress,progress_max,completed,resume,version)
 VALUES (?,'user',?,'gallery',?,toDateTime('2026-05-01'),toDateTime('2026-05-02'),3,?,?,?,?,1)`,
			tenant, row.subject, row.id, row.progress, row.max, row.completed, row.resume); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Exec(ctx, `INSERT INTO erasures (tenant,subject_hash,erased_at) SELECT ?,sipHash128(concat('user',':','erased')),now64(6)`, tenant); err != nil {
		t.Fatal(err)
	}
	e.ApplyRange(t, conn, measured, "")
	rows, err := conn.Query(ctx, `SELECT content_id,progress,completed,resume FROM legacy_reading_state FINAL WHERE tenant=? ORDER BY content_id`, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var (
			id, resume string
			progress   uint32
			completed  bool
		)
		if err := rows.Scan(&id, &progress, &completed, &resume); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s:%d:%t:%s", id, progress, completed, resume))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{cid(1) + ":4:false:p:4", cid(2) + ":0:false:17.5", cid(3) + ":0:true:"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy reading copy: got %v want %v", got, want)
	}
}
