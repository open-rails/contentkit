package signal

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSubject(t *testing.T) {
	u := Subject{UserID: "u1"}
	if u.Kind() != SubjectKindUser || u.Key() != "u1" {
		t.Fatalf("user subject: kind=%q key=%q", u.Kind(), u.Key())
	}
	a := Subject{AnonKey: "h1"}
	if a.Kind() != SubjectKindAnon || a.Key() != "h1" {
		t.Fatalf("anon subject: kind=%q key=%q", a.Kind(), a.Key())
	}
	if err := (Subject{}).Validate(); err == nil {
		t.Fatal("empty subject should be invalid")
	}
	if err := (Subject{UserID: "u", AnonKey: "a"}).Validate(); err == nil {
		t.Fatal("both-set subject should be invalid")
	}
	if err := u.Validate(); err != nil {
		t.Fatalf("user subject should be valid: %v", err)
	}
}

func TestEventIDDeterministic(t *testing.T) {
	at := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	s1 := Signal{
		EntityRef:  EntityRef{EntityType: "blog_post", EntityID: "42"},
		Subject:    Subject{UserID: "u1"},
		Type:       "view",
		OccurredAt: at,
	}
	s2 := s1
	if s1.eventID() != s2.eventID() {
		t.Fatal("same signal must produce the same event id")
	}
	s2.OccurredAt = at.Add(time.Second)
	if s1.eventID() == s2.eventID() {
		t.Fatal("different occurred_at must produce different event ids")
	}
	s3 := s1
	s3.EventID = "explicit"
	if s3.eventID() != "explicit" {
		t.Fatal("explicit EventID must win")
	}
}

// TestEventIDDistinguishesSubSecond guards against the same-second collapse:
// two distinct interactions sharing (entity, subject, type) and the same
// wall-clock second must not hash to the same default event id.
func TestEventIDDistinguishesSubSecond(t *testing.T) {
	base := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	s1 := Signal{
		EntityRef:  EntityRef{EntityType: "blog_post", EntityID: "42"},
		Subject:    Subject{UserID: "u1"},
		Type:       "view",
		OccurredAt: base,
	}
	s2 := s1
	s2.OccurredAt = base.Add(5 * time.Millisecond)

	if s1.OccurredAt.Unix() != s2.OccurredAt.Unix() {
		t.Fatal("test setup: both timestamps must share the same wall-clock second")
	}
	if s1.eventID() == s2.eventID() {
		t.Fatal("distinct sub-second interactions must produce different event ids")
	}
}

func TestAttributionRoundTrip(t *testing.T) {
	base := Signal{
		EntityRef: EntityRef{EntityType: "gallery", EntityID: "1"},
		Subject:   Subject{UserID: "u1"},
		Type:      "click",
		Payload:   map[string]any{"existing": "keep"},
	}
	got := base.WithAttribution(Attribution{QueryID: "q1", Surface: SurfaceSearch, Position: 3})

	if _, ok := base.Payload[PayloadKeyQueryID]; ok {
		t.Fatal("WithAttribution must not mutate the original signal's payload")
	}
	if got.Payload["existing"] != "keep" {
		t.Fatal("existing payload entries must be preserved")
	}
	if a := got.Attribution(); a.QueryID != "q1" || a.Surface != SurfaceSearch || a.Position != 3 {
		t.Fatalf("attribution round-trip mismatch: %+v", a)
	}

	// Tolerant of the float64 a JSON round-trip produces.
	viaJSON := Signal{Payload: map[string]any{PayloadKeyQueryID: "q2", PayloadKeyPosition: float64(5)}}
	if a := viaJSON.Attribution(); a.Position != 5 || a.QueryID != "q2" {
		t.Fatalf("attribution must tolerate float64 position: %+v", a)
	}

	// Zero-valued attribution adds no keys.
	if empty := (Signal{}).WithAttribution(Attribution{}); len(empty.Payload) != 0 {
		t.Fatalf("zero attribution must add no keys, got %v", empty.Payload)
	}
}

func TestImpressionValidate(t *testing.T) {
	ref := EntityRef{EntityType: "gallery", EntityID: "1"}
	if err := (Impression{QueryID: "q", Shown: []EntityRef{ref}}).validate(); err != nil {
		t.Fatalf("valid impression rejected: %v", err)
	}
	if err := (Impression{Shown: []EntityRef{ref}}).validate(); err == nil {
		t.Fatal("missing QueryID must error")
	}
	if err := (Impression{QueryID: "q"}).validate(); err == nil {
		t.Fatal("empty Shown must error")
	}
	bad := EntityRef{EntityType: "gallery"}
	if err := (Impression{QueryID: "q", Shown: []EntityRef{bad}}).validate(); err == nil {
		t.Fatal("shown entity missing entity id must error")
	}
}

func TestWindowDayAligned(t *testing.T) {
	if !(AllTime()).dayAligned() {
		t.Fatal("all-time window is day aligned")
	}
	midnight := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if !Between(midnight, midnight.AddDate(0, 0, 7)).dayAligned() {
		t.Fatal("midnight-to-midnight window is day aligned")
	}
	if Between(midnight.Add(3*time.Hour), midnight.AddDate(0, 0, 1)).dayAligned() {
		t.Fatal("sub-day window must not be day aligned")
	}
	if !LastDays(30).dayAligned() {
		t.Fatal("LastDays must be day aligned so Popular stays on the rollup")
	}
}

func TestReplaceWord(t *testing.T) {
	got := replaceWord("subjects + scored_signals + signals", "signals", "X")
	want := "subjects + scored_signals + X"
	if got != want {
		t.Fatalf("replaceWord: got %q want %q", got, want)
	}
	got = replaceWord("log10(1 + toFloat64(subjects))", "subjects", "uniqExactMerge(subjects)")
	want = "log10(1 + toFloat64(uniqExactMerge(subjects)))"
	if got != want {
		t.Fatalf("replaceWord: got %q want %q", got, want)
	}
}

func TestRecordSignalBuildsEventAndState(t *testing.T) {
	fc := &fakeConn{}
	st, err := NewStore(fc, "hub")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	err = st.RecordSignal(context.Background(), "doujins", Signal{
		EntityRef:   EntityRef{EntityType: "blog_post", EntityID: "42"},
		Subject:     Subject{UserID: "u1"},
		Type:        "view",
		OccurredAt:  at,
		Progress:    80,
		ProgressMax: 100,
		Score:       65,
		Completed:   false,
		Resume:      "scroll:80",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.execs) != 2 {
		t.Fatalf("expected 2 execs (event insert + state reproject), got %d", len(fc.execs))
	}
	ins := fc.execs[0]
	if !strings.Contains(ins.query, "INSERT INTO hub.signal_events") {
		t.Fatalf("first exec must insert the event, got: %s", ins.query)
	}
	// tenant, type, id, kind, subject, signal_type, event_id, occurred_at, ...
	if ins.args[0] != "doujins" || ins.args[3] != "user" || ins.args[4] != "u1" {
		t.Fatalf("unexpected insert args: %v", ins.args)
	}
	if w := ins.args[13]; w != float64(1) {
		t.Fatalf("weight must default to 1, got %v", w)
	}
	proj := fc.execs[1]
	if !strings.Contains(proj.query, "INSERT INTO hub.signal_state") ||
		!strings.Contains(proj.query, "uniqExact(event_id)") {
		t.Fatalf("second exec must reproject state, got: %s", proj.query)
	}
}

func TestRecordSignalValidation(t *testing.T) {
	st, _ := NewStore(&fakeConn{}, "hub")
	ctx := context.Background()
	if err := st.RecordSignal(ctx, "", Signal{}); err == nil {
		t.Fatal("missing tenant must error")
	}
	if err := st.RecordSignal(ctx, "t", Signal{}); err == nil {
		t.Fatal("missing entity must error")
	}
	if err := st.RecordSignal(ctx, "t", Signal{
		EntityRef: EntityRef{EntityType: "a", EntityID: "1"},
		Subject:   Subject{UserID: "u"},
	}); err == nil {
		t.Fatal("missing type must error")
	}
}

func TestForgetImpressionsValidation(t *testing.T) {
	fc := &fakeConn{}
	st, _ := NewStore(fc, "hub")
	for _, tc := range []struct {
		tenant  string
		subject Subject
	}{
		{"", Subject{UserID: "u1"}}, {"doujins", Subject{}},
	} {
		if err := st.ForgetImpressions(context.Background(), tc.tenant, tc.subject); err == nil {
			t.Fatal("expected validation error")
		}
	}
	if len(fc.execs) != 0 {
		t.Fatal("invalid request executed mutation")
	}
}

func TestHistoryStatusFilters(t *testing.T) {
	fc := &fakeConn{}
	st, _ := NewStore(fc, "hub")
	ctx := context.Background()
	sub := Subject{UserID: "u1"}

	for _, tc := range []struct {
		status HistoryStatus
		expect string
	}{
		{HistorySeen, "max_progress > 0"},
		{HistoryInProgress, "max_progress > 0 AND NOT completed"},
		{HistoryCompleted, "AND completed"},
	} {
		fc.queries = nil
		if _, err := st.History(ctx, "t", sub, HistoryOptions{Status: tc.status}); err != nil {
			t.Fatal(err)
		}
		if q := fc.queries[0].query; !strings.Contains(q, tc.expect) {
			t.Fatalf("status %q: query missing %q:\n%s", tc.status, tc.expect, q)
		}
	}
	if _, err := st.History(ctx, "t", sub, HistoryOptions{Status: "bogus"}); err == nil {
		t.Fatal("invalid status must error")
	}
}

func TestStatesGroupsRefsByType(t *testing.T) {
	now := time.Now().UTC()
	fc := &fakeConn{rowsFor: map[string][][]any{
		"signal_state": {
			{"gallery", "7", now, now, uint32(2), uint32(5), uint32(20), false, "p:5", true, int16(40), float64(0)},
		},
	}}
	st, _ := NewStore(fc, "hub")
	got, err := st.States(context.Background(), "t", Subject{UserID: "u1"}, []EntityRef{
		{EntityType: "gallery", EntityID: "7"},
		{EntityType: "gallery", EntityID: "8"},
		{EntityType: "blog_post", EntityID: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	q := fc.queries[0].query
	if c := strings.Count(q, "entity_type = ? AND entity_id IN ?"); c != 2 {
		t.Fatalf("expected 2 per-type clauses, got %d:\n%s", c, q)
	}
	s, ok := got[EntityRef{EntityType: "gallery", EntityID: "7"}]
	if !ok {
		t.Fatalf("missing state for gallery/7: %v", got)
	}
	if !s.Seen || s.MaxProgress != 5 || s.ProgressMax != 20 || !s.HasInteracted || s.LastScore != 40 {
		t.Fatalf("unexpected state: %+v", s)
	}
	if _, ok := got[EntityRef{EntityType: "gallery", EntityID: "8"}]; ok {
		t.Fatal("no state row for gallery/8 should mean absent from map")
	}
}

func TestStatesEmptyRefs(t *testing.T) {
	st, _ := NewStore(&fakeConn{}, "hub")
	got, err := st.States(context.Background(), "t", Subject{UserID: "u"}, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty refs: got %v err %v", got, err)
	}
}

func TestPopularPathSelection(t *testing.T) {
	ctx := context.Background()
	midnight := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// Day-aligned popularity uses the exact event path until the rollup stores view-only subjects.
	fc := &fakeConn{}
	st, _ := NewStore(fc, "hub")
	if _, err := st.Popular(ctx, "t", "gallery", PopularOptions{
		Window: Between(midnight, midnight.AddDate(0, 0, 30)),
	}); err != nil {
		t.Fatal(err)
	}
	if q := fc.queries[0].query; !strings.Contains(q, "signal_events") || !strings.Contains(q, "signal_type = 'view'") {
		t.Fatalf("day-aligned window must use view events:\n%s", q)
	}

	// Sub-day -> events.
	fc.queries = nil
	if _, err := st.Popular(ctx, "t", "gallery", PopularOptions{
		Window: Between(midnight.Add(time.Hour), midnight.Add(26*time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}
	if q := fc.queries[0].query; !strings.Contains(q, "signal_events") {
		t.Fatalf("sub-day window must scan events:\n%s", q)
	}

	// All-time -> exact events, no day predicates.
	fc.queries = nil
	if _, err := st.Popular(ctx, "t", "gallery", PopularOptions{}); err != nil {
		t.Fatal(err)
	}
	if q := fc.queries[0].query; !strings.Contains(q, "signal_events") || strings.Contains(q, "occurred_at >=") {
		t.Fatalf("all-time must use events with no time bounds:\n%s", q)
	}

	// Host RankExpr replaces the default ranking and is alias-rewritten.
	fc.queries = nil
	if _, err := st.Popular(ctx, "t", "gallery", PopularOptions{
		RankExpr: "toFloat64(subjects) + toFloat64(completions)",
	}); err != nil {
		t.Fatal(err)
	}
	if q := fc.queries[0].query; !strings.Contains(q, "toFloat64(uniqExact(tuple(subject_kind, subject))) + toFloat64(toUInt64(countIf(completed)))") {
		t.Fatalf("rank aliases must be rewritten to aggregates:\n%s", q)
	}
}

func TestCoEngagedExcludesAnchor(t *testing.T) {
	fc := &fakeConn{}
	st, _ := NewStore(fc, "hub")
	if _, err := st.CoEngaged(context.Background(), "t", EntityRef{EntityType: "gallery", EntityID: "9"}, CoEngagedOptions{}); err != nil {
		t.Fatal(err)
	}
	// Empty rollup -> falls back to the event scan (second query).
	if len(fc.queries) != 2 || !strings.Contains(fc.queries[0].query, "item_pairs") {
		t.Fatalf("co-engaged must try the rollup first, got %d queries", len(fc.queries))
	}
	q := fc.queries[1].query
	if !strings.Contains(q, "NOT (entity_type = ? AND entity_id = ?)") {
		t.Fatalf("co-engaged must exclude the anchor:\n%s", q)
	}
	if !strings.Contains(q, "value >= 0") || !strings.Contains(q, "HAVING strength > 0") {
		t.Fatalf("co-engaged strength must be negative-aware:\n%s", q)
	}
}

func TestRecordSignalsBatch(t *testing.T) {
	fc := &fakeConn{}
	st, _ := NewStore(fc, "hub")
	at := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	mk := func(entityType, id string) Signal {
		return Signal{
			EntityRef:  EntityRef{EntityType: entityType, EntityID: id},
			Subject:    Subject{UserID: "u1"},
			Type:       "view",
			OccurredAt: at,
		}
	}
	err := st.RecordSignals(context.Background(), "t", []Signal{
		mk("gallery", "g1"), mk("artist", "a1"), mk("tag", "t1"), mk("tag", "t2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.execs) != 2 {
		t.Fatalf("batch must be 2 statements (insert + grouped reprojection), got %d", len(fc.execs))
	}
	if c := strings.Count(fc.execs[0].query, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"); c != 4 {
		t.Fatalf("expected 4 value rows, got %d:\n%s", c, fc.execs[0].query)
	}
	proj := fc.execs[1]
	if !strings.Contains(proj.query, "(entity_type, entity_id, subject_kind, subject) IN ((?, ?, ?, ?), (?, ?, ?, ?), (?, ?, ?, ?), (?, ?, ?, ?))") {
		t.Fatalf("grouped reprojection must cover all 4 keys:\n%s", proj.query)
	}

	// Empty batch is a no-op; single falls through to RecordSignal.
	fc.execs = nil
	if err := st.RecordSignals(context.Background(), "t", nil); err != nil || len(fc.execs) != 0 {
		t.Fatalf("empty batch: %v %d", err, len(fc.execs))
	}
	if err := st.RecordSignals(context.Background(), "t", []Signal{mk("gallery", "g9")}); err != nil {
		t.Fatal(err)
	}
	if len(fc.execs) != 2 || !strings.Contains(fc.execs[1].query, "entity_id = ?") {
		t.Fatalf("single-signal batch should use the point reprojection path")
	}
}

func TestSchemaHelpersValidateIdentifiers(t *testing.T) {
	ctx := context.Background()
	if err := CreateDatabase(ctx, &fakeConn{}, "bad-name", ""); err == nil {
		t.Fatal("invalid database name must error")
	}
	if err := CreateDatabase(ctx, &fakeConn{}, "hub", "bad cluster"); err == nil {
		t.Fatal("invalid cluster name must error")
	}
	if err := CheckSchema(ctx, &fakeConn{}, "hub;drop"); err == nil {
		t.Fatal("invalid database name must error")
	}
	for full, want := range map[string]string{
		"ReplicatedReplacingMergeTree('/clickhouse/tables/hub/t', '{replica}', recorded_at) ORDER BY (a, b)": "recorded_at",
		"ReplacingMergeTree(version) PARTITION BY toYYYYMM(occurred_at) ORDER BY a":                          "version",
		"ReplicatedAggregatingMergeTree('/clickhouse/tables/hub/t', '{replica}') ORDER BY a":                 "",
	} {
		if got := engineVersionColumn(full); got != want {
			t.Fatalf("engineVersionColumn(%q)=%q want %q", full, got, want)
		}
	}
}
