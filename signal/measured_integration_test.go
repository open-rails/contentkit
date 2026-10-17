package signal

import (
	"testing"

	"github.com/open-rails/contentkit/contentref"
)

func TestMeasuredSegmentsKeepCoverageAcrossVisitsWithoutCountingOpens(t *testing.T) {
	st, conn := freshStore(t)
	ctx := t.Context()
	tenant, work, version := "measured", cid(410), cid(411)
	subject := Subject{UserID: "reader"}
	makeSegment := func(receipt string, revision uint64, page, ms uint32) Signal {
		m, err := MeasurePages([]PageEngagement{{Index: page, Ranges: []CoverageRange{{From: 0, To: 10_000}}, ActiveMS: ms}}, 2, ms)
		if err != nil {
			t.Fatal(err)
		}
		m.VersionID, m.VisitID, m.SegmentID = version, "visit-"+receipt, "segment-"+receipt
		observed := at(1, 10)
		return Signal{ContentRef: contentref.New(tenant, "gallery", work).WithVersion(version), Subject: subject, Type: TypeEngagement,
			EventID: receipt, Revision: revision, OccurredAt: observed, Resume: "page", Measurement: &m,
			Access: &AccessContext{ObservedAt: observed, ContentAccess: "public", ViewerAccess: "full", Audience: "public", EligibilityKnown: true}}
	}
	first := makeSegment("receipt-a", 1, 0, 600)
	for range 3 {
		if err := st.RecordSignals(ctx, tenant, []Signal{first}); err != nil {
			t.Fatal(err)
		}
	}
	assert := func(active uint64, units uint64, qualified, completed bool) {
		t.Helper()
		rows, err := conn.Query(ctx, `SELECT active_ms,length(covered_units),qualified,completed FROM `+testDB+`.engagement_state FINAL WHERE tenant=? AND content_id=? AND content_version_id=''`, tenant, work)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatalf("missing measured work state: %v", rows.Err())
		}
		var gotActive, gotUnits uint64
		var gotQualified, gotCompleted bool
		if err := rows.Scan(&gotActive, &gotUnits, &gotQualified, &gotCompleted); err != nil {
			t.Fatal(err)
		}
		if gotActive != active || gotUnits != units || gotQualified != qualified || gotCompleted != completed {
			t.Fatalf("measured state active=%d units=%d qualified=%v completed=%v; want %d/%d/%v/%v", gotActive, gotUnits, gotQualified, gotCompleted, active, units, qualified, completed)
		}
	}
	assert(600, 100, false, false)
	for _, event := range []Signal{makeSegment("receipt-a", 2, 0, 1200), makeSegment("receipt-b", 1, 0, 8000)} {
		if err := st.RecordSignals(ctx, tenant, []Signal{event, event}); err != nil {
			t.Fatal(err)
		}
	}
	assert(9200, 100, true, false)
	if err := st.RecordSignals(ctx, tenant, []Signal{makeSegment("receipt-c", 1, 1, 8000)}); err != nil {
		t.Fatal(err)
	}
	assert(17_200, 200, true, true)
	if report, err := st.EraseSubjects(ctx, []string{tenant}, []Subject{subject}); err != nil || !report.Complete() {
		t.Fatalf("erase measured history: %+v %v", report, err)
	}
	for _, table := range []string{"engagement_segments", "engagement_state", "engagement_daily", "legacy_reading_state"} {
		if n := countWhere(t, conn, table, "tenant=? AND subject=?", tenant, subject.Key()); n != 0 {
			t.Fatalf("%s retained erased measurements: %d", table, n)
		}
	}
	if err := st.RecordSignals(ctx, tenant, []Signal{first}); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, conn, "engagement_segments", "tenant=?", tenant); n != 0 {
		t.Fatal("erased reader was resurrected by a replay")
	}
}
