package signal

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// #881: served, rendered and visible lists are separate observations; clicks
// join their render; hidden, prefetched, cancelled, reordered and paginated
// lists, repeated clicks and retries have the expected attribution; a click
// without exposure at the stage is reported, never turned into a label.
func TestIntegrationExposureAttribution(t *testing.T) {
	st, conn := freshStore(t)
	ctx := context.Background()
	tenant := "t"
	user := Subject{UserID: "u1"}
	at := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	g := func(id string, pos uint32) Placement {
		return Placement{EntityRef: EntityRef{EntityType: "gallery", EntityID: id}, Position: pos}
	}
	click := func(renderID, id string, pos uint32, eventID string, when time.Time) Signal {
		return Signal{EntityRef: EntityRef{EntityType: "gallery", EntityID: id}, Subject: user, Type: "click",
			EventID: eventID, OccurredAt: when}.WithAttribution(Attribution{RenderID: renderID, Surface: SurfaceSearch, Position: pos})
	}
	// Keep superseded rows physically present.
	for _, table := range []string{"exposures", "events"} {
		if err := conn.Exec(ctx, "SYSTEM STOP MERGES "+testDB+"."+table); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, table := range []string{"exposures", "events"} {
			_ = conn.Exec(context.Background(), "SYSTEM START MERGES "+testDB+"."+table)
		}
	})

	exposures := []Exposure{
		// r1: served a,b,c,d; client rendered them reordered; only a,c became visible (growing list).
		{RenderID: "r1", Stage: StageServed, QueryID: "q1", Surface: SurfaceSearch, Ranker: "baseline", Language: "en", Subject: user, OccurredAt: at,
			Shown: []Placement{g("a", 1), g("b", 2), g("c", 3), g("d", 4)}},
		{RenderID: "r1", Stage: StageRendered, QueryID: "q1", Surface: SurfaceSearch, Ranker: "baseline", Language: "en", Subject: user, OccurredAt: at,
			Shown: []Placement{g("b", 1), g("a", 2), g("c", 3), g("d", 4)}},
		{RenderID: "r1", Stage: StageVisible, Revision: 1, QueryID: "q1", Surface: SurfaceSearch, Ranker: "baseline", Language: "en", Subject: user, OccurredAt: at,
			Shown: []Placement{g("b", 1)}},
		{RenderID: "r1", Stage: StageVisible, Revision: 2, QueryID: "q1", Surface: SurfaceSearch, Ranker: "baseline", Language: "en", Subject: user, OccurredAt: at,
			Shown: []Placement{g("b", 1), g("a", 2)}},
		// r2: page 2 of the same query, absolute positions.
		{RenderID: "r2", Stage: StageServed, QueryID: "q1", Surface: SurfaceSearch, Ranker: "baseline", Language: "en", Subject: user, OccurredAt: at.Add(time.Minute),
			Shown: []Placement{g("e", 11), g("f", 12)}},
		{RenderID: "r2", Stage: StageRendered, QueryID: "q1", Surface: SurfaceSearch, Ranker: "baseline", Language: "en", Subject: user, OccurredAt: at.Add(time.Minute),
			Shown: []Placement{g("e", 11), g("f", 12)}},
		// r3: prefetched/cancelled: served, never rendered.
		{RenderID: "r3", Stage: StageServed, QueryID: "q2", Surface: SurfaceSearch, Subject: user, OccurredAt: at.Add(2 * time.Minute),
			Shown: []Placement{g("x", 1)}},
		// r4: a shelf on another surface, anonymous render.
		{RenderID: "r4", Stage: StageRendered, Surface: SurfaceSimilar, OccurredAt: at.Add(3 * time.Minute),
			Shown: []Placement{g("s1", 1), g("s2", 2)}},
	}
	// Deliver twice, second time reversed.
	if err := st.RecordExposures(ctx, tenant, exposures); err != nil {
		t.Fatal(err)
	}
	reversed := make([]Exposure, len(exposures))
	for i := range exposures {
		reversed[len(exposures)-1-i] = exposures[i]
	}
	if err := st.RecordExposures(ctx, tenant, reversed); err != nil {
		t.Fatal(err)
	}
	clicks := []Signal{
		click("r1", "a", 2, "c1", at.Add(10*time.Second)),
		click("r1", "a", 2, "c1", at.Add(15*time.Second)), // retry, re-stamped
		click("r1", "c", 3, "c2", at.Add(20*time.Second)), // rendered but not visible
		click("r2", "f", 12, "c3", at.Add(70*time.Second)),
		click("r3", "x", 1, "c4", at.Add(130*time.Second)),    // never rendered
		click("r9", "z", 1, "c5", at.Add(140*time.Second)),    // render never recorded
		click("r4", "s2", 2, "c6", at.Add(190*time.Second)),   // other surface
		click("r1", "a", 2, "c1-dup", at.Add(30*time.Second)), // repeated distinct click
	}
	if err := st.RecordSignals(ctx, tenant, append(append([]Signal{}, clicks...), clicks...)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSignals(ctx, "other", []Signal{click("r1", "a", 2, "c1", at)}); err != nil {
		t.Fatal(err)
	}

	if _, err := st.Attribution(ctx, tenant, AttributionOptions{Stage: "shown"}); err == nil {
		t.Fatal("invalid stage must be rejected")
	}
	window := Between(time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC))
	ids := func(cs []AttributedClick) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.EntityID+":"+c.EventID)
		}
		return out
	}

	visible, err := st.Attribution(ctx, tenant, AttributionOptions{Stage: StageVisible, Window: window, Surface: SurfaceSearch})
	if err != nil {
		t.Fatal(err)
	}
	if len(visible.Renders) != 1 || visible.Renders[0].RenderID != "r1" {
		t.Fatalf("only r1 reached the visible stage: %+v", visible.Renders)
	}
	r1 := visible.Renders[0]
	if !reflect.DeepEqual(r1.Shown, []Placement{g("b", 1), g("a", 2)}) || r1.Ranker != "baseline" || r1.QueryID != "q1" || r1.Subject != user {
		t.Fatalf("visible list must be the highest revision: %+v", r1)
	}
	// Chronological; the retry collapsed, the distinct repeated click kept.
	if got := ids(r1.Clicks); !reflect.DeepEqual(got, []string{"a:c1", "c:c2", "a:c1-dup"}) {
		t.Fatalf("r1 clicks (dedup retry, keep distinct repeat): %v", got)
	}
	if !r1.Clicks[0].Exposed || r1.Clicks[1].Exposed || !r1.Clicks[2].Exposed {
		t.Fatalf("c was rendered but not visible: %+v", r1.Clicks)
	}
	if !r1.Clicks[0].OccurredAt.Equal(at.Add(15*time.Second)) || r1.Clicks[0].Position != 2 {
		t.Fatalf("retry resolves to one canonical click: %+v", r1.Clicks[0])
	}
	if got := ids(visible.Unattributed); !reflect.DeepEqual(got, []string{"f:c3", "x:c4", "s2:c6", "z:c5"}) {
		t.Fatalf("clicks without a visible exposure are reported, not labelled: %v", got)
	}

	rendered, err := st.Attribution(ctx, tenant, AttributionOptions{Stage: StageRendered, Window: window})
	if err != nil {
		t.Fatal(err)
	}
	var renderIDs []string
	for _, r := range rendered.Renders {
		renderIDs = append(renderIDs, r.RenderID)
	}
	if !reflect.DeepEqual(renderIDs, []string{"r1", "r2", "r4"}) {
		t.Fatalf("rendered stage: %v", renderIDs)
	}
	if got := ids(rendered.Renders[0].Clicks); !reflect.DeepEqual(got, []string{"a:c1", "c:c2", "a:c1-dup"}) || !rendered.Renders[0].Clicks[1].Exposed {
		t.Fatalf("rendered r1 clicks: %v %+v", got, rendered.Renders[0].Clicks)
	}
	if r2 := rendered.Renders[1]; !reflect.DeepEqual(r2.Shown, []Placement{g("e", 11), g("f", 12)}) || ids(r2.Clicks)[0] != "f:c3" || !r2.Clicks[0].Exposed || r2.Clicks[0].Position != 12 {
		t.Fatalf("paginated render keeps absolute positions: %+v", r2)
	}
	if r4 := rendered.Renders[2]; r4.Subject != (Subject{}) || r4.Surface != SurfaceSimilar || !r4.Clicks[0].Exposed {
		t.Fatalf("anonymous shelf render: %+v", r4)
	}
	if got := ids(rendered.Unattributed); !reflect.DeepEqual(got, []string{"x:c4", "z:c5"}) {
		t.Fatalf("prefetched/cancelled and unknown renders: %v", got)
	}

	// Cursor paging.
	first, err := st.Attribution(ctx, tenant, AttributionOptions{Stage: StageServed, Window: window, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Renders) != 2 || first.Next != "r2" {
		t.Fatalf("page 1: %+v", first)
	}
	second, err := st.Attribution(ctx, tenant, AttributionOptions{Stage: StageServed, Window: window, Limit: 2, After: first.Next})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Renders) != 1 || second.Renders[0].RenderID != "r3" || second.Next != "" || ids(second.Renders[0].Clicks)[0] != "x:c4" {
		t.Fatalf("page 2: %+v", second)
	}

	// Outside the window nothing is exported; another tenant sees nothing.
	empty, err := st.Attribution(ctx, tenant, AttributionOptions{Stage: StageServed, Window: LastDays(1, at.AddDate(0, 0, 5))})
	if err != nil || len(empty.Renders) != 0 || len(empty.Unattributed) != 0 {
		t.Fatalf("window: %+v %v", empty, err)
	}
	foreign, err := st.Attribution(ctx, "other", AttributionOptions{Stage: StageVisible})
	if err != nil || len(foreign.Renders) != 0 || len(foreign.Unattributed) != 1 {
		t.Fatalf("tenant isolation: %+v %v", foreign, err)
	}

	// Merges change nothing.
	for _, table := range []string{"exposures", "events"} {
		if err := conn.Exec(ctx, "SYSTEM START MERGES "+testDB+"."+table); err != nil {
			t.Fatal(err)
		}
		if err := conn.Exec(ctx, "OPTIMIZE TABLE "+testDB+"."+table+" FINAL"); err != nil {
			t.Fatal(err)
		}
	}
	again, err := st.Attribution(ctx, tenant, AttributionOptions{Stage: StageVisible, Window: window, Surface: SurfaceSearch})
	if err != nil || !reflect.DeepEqual(again, visible) {
		t.Fatalf("merges changed attribution: %v", err)
	}

	// Search-history clearing removes only this subject's exposures.
	if err := st.ForgetExposures(ctx, tenant, user); err != nil {
		t.Fatal(err)
	}
	served, err := st.Attribution(ctx, tenant, AttributionOptions{Stage: StageRendered, Window: window})
	if err != nil || len(served.Renders) != 1 || served.Renders[0].RenderID != "r4" {
		t.Fatalf("after forget only the anonymous render remains: %+v %v", served.Renders, err)
	}
}
