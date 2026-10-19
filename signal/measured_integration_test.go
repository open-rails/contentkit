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

func TestMeasuredReaderProgressIsWholePagesRoundedDownButNeverZero(t *testing.T) {
	st, _ := freshStore(t)
	ctx := t.Context()
	tenant, work, version := "measured-pages", cid(420), cid(421)
	ref := contentref.New(tenant, "gallery", work)
	read := func(subject Subject, pages []PageEngagement) {
		t.Helper()
		var ms uint32
		for _, p := range pages {
			ms += p.ActiveMS
		}
		m, err := MeasurePages(pages, 8, ms)
		if err != nil {
			t.Fatal(err)
		}
		m.VersionID, m.VisitID, m.SegmentID = version, "visit-"+subject.UserID, "segment-"+subject.UserID
		observed := at(1, 10)
		if err := st.RecordSignals(ctx, tenant, []Signal{{ContentRef: ref.WithVersion(version), Subject: subject, Type: TypeEngagement,
			EventID: subject.UserID, Revision: 1, OccurredAt: observed, Resume: "page", Measurement: &m,
			Access: &AccessContext{ObservedAt: observed, ContentAccess: "public", ViewerAccess: "full", Audience: "public", EligibilityKnown: true}}}); err != nil {
			t.Fatal(err)
		}
	}
	full := CoverageRange{From: 0, To: 10_000}
	partial, glimpse := Subject{UserID: "partial"}, Subject{UserID: "glimpse"}
	read(partial, []PageEngagement{{Index: 0, Ranges: []CoverageRange{full}, ActiveMS: 2000}, {Index: 1, Ranges: []CoverageRange{full}, ActiveMS: 2000},
		{Index: 2, Ranges: []CoverageRange{{From: 0, To: 5000}}, ActiveMS: 2000}})
	read(glimpse, []PageEngagement{{Index: 0, Ranges: []CoverageRange{{From: 0, To: 1000}}, ActiveMS: 2000}})
	for subject, want := range map[Subject]uint32{partial: 2, glimpse: 1} {
		states, err := st.States(ctx, tenant, subject, []ContentRef{ref})
		if err != nil {
			t.Fatal(err)
		}
		if s := states[ref.Key()]; s.MaxProgress != want || s.ProgressMax != 8 || !s.Seen {
			t.Fatalf("%s: progress %d / %d seen=%v, want %d / 8 seen", subject.UserID, s.MaxProgress, s.ProgressMax, s.Seen, want)
		}
	}
}

func TestMeasuredPlaybackProgressStaysInSeconds(t *testing.T) {
	st, _ := freshStore(t)
	ctx := t.Context()
	tenant, work, version := "measured-seconds", cid(430), cid(431)
	ref := contentref.New(tenant, "video", work)
	subject := Subject{UserID: "viewer"}
	m, err := MeasurePlayback([]WatchedRange{{FromMS: 0, ToMS: 30_000}}, 60_000, 30_000)
	if err != nil {
		t.Fatal(err)
	}
	m.VersionID, m.VisitID, m.SegmentID = version, "visit", "segment"
	observed := at(1, 10)
	if err := st.RecordSignals(ctx, tenant, []Signal{{ContentRef: ref.WithVersion(version), Subject: subject, Type: TypeEngagement,
		EventID: "watch", Revision: 1, OccurredAt: observed, Resume: "30", Measurement: &m,
		Access: &AccessContext{ObservedAt: observed, ContentAccess: "public", ViewerAccess: "full", Audience: "public", EligibilityKnown: true}}}); err != nil {
		t.Fatal(err)
	}
	states, err := st.States(ctx, tenant, subject, []ContentRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	if s := states[ref.Key()]; s.MaxProgress != 30 || s.ProgressMax != 60 || !s.Seen {
		t.Fatalf("playback progress %d / %d seen=%v, want 30 / 60 seen", s.MaxProgress, s.ProgressMax, s.Seen)
	}
}
