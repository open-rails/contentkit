package signal

import (
	"context"
	"errors"
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

func TestAttributionRoundTrip(t *testing.T) {
	base := Signal{
		EntityRef: EntityRef{EntityType: "gallery", EntityID: "1"},
		Subject:   Subject{UserID: "u1"},
		Type:      "click",
		Payload:   map[string]any{"existing": "keep"},
	}
	got := base.WithAttribution(Attribution{RenderID: "q1", Surface: SurfaceSearch, Position: 3})

	if _, ok := base.Payload[PayloadKeyRenderID]; ok {
		t.Fatal("WithAttribution must not mutate the original signal's payload")
	}
	if got.Payload["existing"] != "keep" {
		t.Fatal("existing payload entries must be preserved")
	}
	if a := got.Attribution(); a.RenderID != "q1" || a.Surface != SurfaceSearch || a.Position != 3 {
		t.Fatalf("attribution round-trip mismatch: %+v", a)
	}

	// Tolerant of the float64 a JSON round-trip produces.
	viaJSON := Signal{Payload: map[string]any{PayloadKeyRenderID: "q2", PayloadKeyPosition: float64(5)}}
	if a := viaJSON.Attribution(); a.Position != 5 || a.RenderID != "q2" {
		t.Fatalf("attribution must tolerate float64 position: %+v", a)
	}

	// Zero-valued attribution adds no keys.
	if empty := (Signal{}).WithAttribution(Attribution{}); len(empty.Payload) != 0 {
		t.Fatalf("zero attribution must add no keys, got %v", empty.Payload)
	}
}

func TestWindowLiteralDays(t *testing.T) {
	now := time.Date(2026, 9, 16, 23, 59, 59, 999, time.UTC)
	for n, from := range map[int]string{1: "2026-09-16", 7: "2026-09-10", 30: "2026-08-18", 90: "2026-06-19", 365: "2025-09-17"} {
		w := LastDays(n, now)
		if err := w.Validate(); err != nil {
			t.Fatal(err)
		}
		if got, want := w.String(), "["+from+",2026-09-17)"; got != want {
			t.Fatalf("LastDays(%d)=%s want %s", n, got, want)
		}
		if days := int(w.To.Sub(w.From).Hours() / 24); days != n {
			t.Fatalf("LastDays(%d) spans %d days", n, days)
		}
		// The key only moves at UTC midnight, in any input zone.
		early := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC).In(time.FixedZone("x", -7*3600))
		if LastDays(n, early) != w {
			t.Fatalf("LastDays(%d) changed within one UTC day", n)
		}
	}
	if AllTime().String() != "[*,*)" || AllTime().Validate() != nil {
		t.Fatal("all-time window")
	}
	midnight := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for _, bad := range []Window{
		LastDays(0, now), LastDays(-3, now),
		Between(midnight.Add(3*time.Hour), midnight.AddDate(0, 0, 1)),
		{To: midnight.Add(time.Nanosecond)},
		Between(midnight, midnight),
	} {
		if bad.Validate() == nil {
			t.Fatalf("window %s must be rejected", bad)
		}
	}
	pred, args := Between(midnight, midnight.AddDate(0, 0, 7)).predicate("occurred_at")
	if pred != " AND occurred_at >= ? AND occurred_at < ?" || len(args) != 2 {
		t.Fatalf("half-open predicate: %q %v", pred, args)
	}
}

func TestRecordSignalsValidation(t *testing.T) {
	fc := &fakeConn{}
	st, _ := NewStore(fc, "hub")
	ctx := context.Background()
	valid := Signal{
		EntityRef: EntityRef{EntityType: "a", EntityID: "1"}, Subject: Subject{UserID: "u"},
		Type: TypeView, EventID: "e1", OccurredAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := st.RecordSignals(ctx, "", []Signal{valid}); err == nil {
		t.Fatal("missing tenant must error")
	}
	for name, mutate := range map[string]func(*Signal){
		"entity":      func(s *Signal) { s.EntityID = "" },
		"subject":     func(s *Signal) { s.Subject = Subject{} },
		"type":        func(s *Signal) { s.Type = "" },
		"event id":    func(s *Signal) { s.EventID = " " },
		"occurred at": func(s *Signal) { s.OccurredAt = time.Time{} },
	} {
		bad := valid
		mutate(&bad)
		if err := st.RecordSignals(ctx, "t", []Signal{valid, bad}); err == nil {
			t.Fatalf("missing %s must error", name)
		}
	}
	if len(fc.execs) != 0 {
		t.Fatal("an invalid batch must write nothing")
	}
}

func TestStatesEmptyRefs(t *testing.T) {
	st, _ := NewStore(&fakeConn{}, "hub")
	got, err := st.States(context.Background(), "t", Subject{UserID: "u"}, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty refs: got %v err %v", got, err)
	}
}

func TestExposureValidate(t *testing.T) {
	at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	item := func(id string, pos uint32) Placement {
		return Placement{EntityRef: EntityRef{EntityType: "gallery", EntityID: id}, Position: pos}
	}
	valid := Exposure{RenderID: "r1", Stage: StageVisible, OccurredAt: at, Shown: []Placement{item("a", 1), item("b", 3)}}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid exposure rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Exposure){
		"render id":          func(e *Exposure) { e.RenderID = " " },
		"stage":              func(e *Exposure) { e.Stage = "shown" },
		"time":               func(e *Exposure) { e.OccurredAt = time.Time{} },
		"empty":              func(e *Exposure) { e.Shown = nil },
		"zero position":      func(e *Exposure) { e.Shown = []Placement{item("a", 0)} },
		"duplicate position": func(e *Exposure) { e.Shown = []Placement{item("a", 2), item("b", 2)} },
		"entity":             func(e *Exposure) { e.Shown = []Placement{{Position: 1}} },
		"subject":            func(e *Exposure) { e.Subject = Subject{UserID: "u", AnonKey: "a"} },
	} {
		bad := valid
		mutate(&bad)
		if err := bad.validate(); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
	s := Signal{}.WithAttribution(Attribution{RenderID: "r1", Surface: SurfaceSearch, Position: 3})
	if a := s.Attribution(); a.RenderID != "r1" || a.Position != 3 || a.Surface != SurfaceSearch {
		t.Fatalf("attribution round trip: %+v", a)
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

func TestIngestionLimitsRejectOversizeInput(t *testing.T) {
	fc := &fakeConn{}
	st, _ := NewStore(fc, "hub")
	ctx := context.Background()
	at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	valid := Signal{EntityRef: EntityRef{EntityType: "a", EntityID: "1"}, Subject: Subject{UserID: "u"}, Type: TypeView, EventID: "e", OccurredAt: at}
	var limit *LimitError
	long := strings.Repeat("x", MaxIdentifierBytes+1)
	for name, mutate := range map[string]func(*Signal){
		"entity id": func(s *Signal) { s.EntityID = long },
		"event id":  func(s *Signal) { s.EventID = long },
		"subject":   func(s *Signal) { s.Subject = Subject{AnonKey: long} },
		"resume":    func(s *Signal) { s.Resume = strings.Repeat("r", MaxResumeBytes+1) },
		"payload":   func(s *Signal) { s.Payload = map[string]any{"k": strings.Repeat("p", MaxPayloadBytes)} },
	} {
		bad := valid
		mutate(&bad)
		if err := st.RecordSignals(ctx, "t", []Signal{bad}); !errors.As(err, &limit) {
			t.Fatalf("%s: want LimitError, got %v", name, err)
		}
	}
	if err := st.RecordSignals(ctx, "t", make([]Signal, MaxSignalsPerBatch+1)); !errors.As(err, &limit) || limit.Got != MaxSignalsPerBatch+1 {
		t.Fatalf("batch: %v", err)
	}
	shown := make([]Placement, MaxShownPerExposure+1)
	for i := range shown {
		shown[i] = Placement{EntityRef: EntityRef{EntityType: "a", EntityID: "1"}, Position: uint32(i + 1)}
	}
	exposure := Exposure{RenderID: "r", Stage: StageServed, OccurredAt: at, Shown: shown}
	if err := st.RecordExposures(ctx, "t", []Exposure{exposure}); !errors.As(err, &limit) {
		t.Fatalf("shown: %v", err)
	}
	if err := st.RecordExposures(ctx, "t", make([]Exposure, MaxExposuresPerBatch+1)); !errors.As(err, &limit) {
		t.Fatalf("exposures batch: %v", err)
	}
	if len(fc.execs) != 0 {
		t.Fatal("rejected input reached ClickHouse")
	}
	if err := st.PurgeEntityTypes(ctx, "t", nil); err == nil {
		t.Fatal("purge without entity types must error")
	}
}
