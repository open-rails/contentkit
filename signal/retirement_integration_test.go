package signal

import (
	"testing"

	"github.com/open-rails/contentkit/internal/signaltest"
)

func TestRetirementPreservesResumeAndMeasuredDataWithoutLegacyCounters(t *testing.T) {
	st, conn := freshStore(t)
	ctx := t.Context()
	env := signaltest.FromEnv(t)
	tenant := "retirement"
	subject := Subject{UserID: "reader"}
	old, new := gallery(tenant, cid(510)), gallery(tenant, cid(511))
	if err := conn.Exec(ctx, `INSERT INTO `+testDB+`.subject_content_state
 (tenant,subject_kind,subject,content_kind,content_id,first_seen_at,last_view_at,max_progress,progress_max,completed,resume,version)
 VALUES (?,'user',?,'gallery',?,toDateTime('2026-05-01'),toDateTime('2026-05-02'),9,10,true,'009',1)`, tenant, subject.Key(), old.ContentID); err != nil {
		t.Fatal(err)
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
	states, err := st.States(ctx, tenant, subject, []ContentRef{old, new})
	if err != nil {
		t.Fatal(err)
	}
	if value := states[old.Key()]; value.Resume != "009" || !value.Completed || value.Quality != 0 || value.ActiveMS != 0 {
		t.Fatalf("legacy history was lost or promoted into quality: %+v", value)
	}
	if value := states[new.Key()]; !value.Completed || value.ActiveMS != 2000 || value.Quality <= 0 {
		t.Fatalf("measured history changed: %+v", value)
	}
	metrics, err := st.Metrics(ctx, tenant, []ContentRef{old, new}, AllTime())
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
	states, err = st.States(ctx, tenant, subject, []ContentRef{old, new})
	if err != nil || len(states) != 0 {
		t.Fatalf("retired legacy/measured history survived erasure: %+v %v", states, err)
	}
}
