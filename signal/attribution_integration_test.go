package signal

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// attributionFixture is one tenant's renders and clicks with every shape the
// paged export must handle: zero-click and many-click renders, clicks that
// differ only by subject, superseded revisions, re-stamped retries, another
// surface, the window edge, served-only and never-recorded renders, an organic
// click, another tenant and an erased subject.
type attributionFixture struct {
	st     *Store
	conn   Conn
	tenant string
	window Window
	u1, u2 Subject
	u3     Subject // erased
	a1     Subject
	day1   time.Time
}

func loadAttributionFixture(t *testing.T) attributionFixture {
	t.Helper()
	st, conn := freshStore(t)
	ctx := context.Background()
	f := attributionFixture{st: st, conn: conn, tenant: "t",
		u1: Subject{UserID: "u1"}, u2: Subject{UserID: "u2"}, u3: Subject{UserID: "u3"}, a1: Subject{AnonKey: "a1"},
		day1: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)}
	f.window = Between(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC))
	day3 := f.day1.AddDate(0, 0, 2)
	g := func(tenant string, ids ...string) []Placement {
		out := make([]Placement, len(ids))
		for i, id := range ids {
			out[i] = Placement{ContentRef: gallery(tenant, lid(id)), Position: uint32(i + 1)}
		}
		return out
	}
	ex := func(id string, stage ExposureStage, rev uint64, sub Subject, surface string, at time.Time, shown []Placement) Exposure {
		return Exposure{RenderID: id, Stage: stage, Revision: rev, QueryID: "q-" + id, Surface: surface, Ranker: "base", Language: "en",
			Subject: sub, OccurredAt: at, Shown: shown}
	}
	exposures := []Exposure{
		ex("R01", StageRendered, 0, f.u1, SurfaceSearch, f.day1, g("t", "g1", "g2")),
		ex("R02", StageRendered, 0, f.u1, SurfaceSearch, f.day1, g("t", "g1", "g2", "g3", "g4", "g5")),
		ex("R03", StageRendered, 0, f.u2, SurfaceSearch, f.day1, g("t", "g6")),
		ex("R04", StageRendered, 0, f.a1, SurfaceSearch, f.day1, g("t", "g7", "g8")),
		ex("R05", StageRendered, 0, f.a1, SurfaceSearch, f.day1, g("t", "g7")),
		ex("R06", StageRendered, 0, f.u1, SurfaceSimilar, f.day1, g("t", "g10")),
		ex("R07", StageRendered, 2, f.u1, SurfaceSearch, f.day1, g("t", "g11", "g12")),
		ex("R07", StageRendered, 1, f.u1, SurfaceSearch, f.day1, g("t", "g11")),
		ex("R08", StageRendered, 0, f.u1, SurfaceSearch, f.day1, g("t", "g13")),
		ex("R09", StageRendered, 0, f.u3, SurfaceSearch, f.day1, g("t", "g14")),
		ex("R10", StageRendered, 0, f.u1, SurfaceSearch, f.day1, g("t", "g15")),
		ex("R11", StageRendered, 0, f.u1, SurfaceSearch, day3, g("t", "g16")),
		ex("S01", StageServed, 0, f.u1, SurfaceSearch, f.day1, g("t", "g17")),
	}
	if err := st.RecordExposures(ctx, f.tenant, exposures); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordExposures(ctx, "o", []Exposure{ex("R01", StageRendered, 0, f.u1, SurfaceSearch, f.day1, g("o", "g1"))}); err != nil {
		t.Fatal(err)
	}
	click := func(tenant string, sub Subject, render, id string, pos uint32, event string, at time.Time) Signal {
		return Signal{ContentRef: gallery(tenant, lid(id)), Subject: sub, Type: TypeClick, EventID: event, OccurredAt: at}.
			WithAttribution(Attribution{RenderID: render, Surface: SurfaceSearch, Position: pos})
	}
	s := func(n int) time.Time { return f.day1.Add(time.Duration(n) * time.Second) }
	clicks := []Signal{
		click("t", f.u1, "R02", "g1", 1, "c02-1", s(1)),
		click("t", f.u1, "R02", "g2", 2, "c02-2", s(2)),
		click("t", f.u1, "R02", "g3", 3, "c02-3", s(3)),
		click("t", f.u1, "R02", "g4", 4, "c02-4", s(4)),
		click("t", f.u1, "R02", "g5", 5, "c02-5", s(5)),
		click("t", f.u1, "R02", "g1", 1, "c02-6", s(6)),
		click("t", f.u1, "R02", "g9", 9, "c02-7", s(7)), // not shown
		click("t", f.u2, "R03", "g6", 1, "c03-1", s(10)),
		click("t", f.a1, "R05", "g7", 1, "same", s(40)), // identity differs from the next only by subject
		click("t", f.u1, "R05", "g7", 1, "same", s(40)),
		click("t", f.u1, "R06", "g10", 1, "c06-1", s(11)),
		click("t", f.u1, "R07", "g12", 2, "c07-1", s(12)), // exposed only by revision 2
		click("t", f.u1, "R08", "g13", 1, "c08-1", s(80)),
		click("t", f.u1, "R08", "g13", 1, "c08-1", s(81)), // retry, re-stamped
		click("t", f.u3, "R09", "g14", 1, "c09-1", s(13)),
		click("t", f.u1, "R10", "g15", 1, "c10-1", day3),  // outside the window
		click("t", f.u1, "R11", "g16", 1, "c11-1", s(14)), // render outside the window
		click("t", f.u1, "S01", "g17", 1, "cS1-1", s(15)), // served, never rendered
		click("t", f.u1, "N01", "g1", 1, "cN1-1", s(16)),  // render never recorded
		click("t", f.u2, "N01", "g2", 2, "cN1-2", s(17)),
		click("t", f.a1, "N02", "g3", 1, "cN2-1", s(18)),
		{ContentRef: gallery("t", lid("g1")), Subject: f.u1, Type: TypeClick, EventID: "organic", OccurredAt: s(19)},
	}
	// Deliver twice, the second time reversed.
	reversed := make([]Signal, len(clicks))
	for i := range clicks {
		reversed[len(clicks)-1-i] = clicks[i]
	}
	if err := st.RecordSignals(ctx, f.tenant, append(clicks, reversed...)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSignals(ctx, "o", []Signal{click("o", f.u1, "R01", "g1", 1, "o-1", s(1)), click("o", f.u1, "N01", "g1", 1, "o-2", s(2))}); err != nil {
		t.Fatal(err)
	}
	report, err := st.EraseSubjects(ctx, []string{f.tenant}, []Subject{f.u3})
	if err != nil || !report.Complete() {
		t.Fatalf("erase u3: %+v %v", report, err)
	}
	// Residue a writer that passed the fence check before the fence could leave:
	// the export must exclude it through the ledger, not only through deletion.
	if err := conn.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.signals (tenant, content_kind, content_id, subject_kind, subject, signal_type, event_id, occurred_at, payload)
VALUES (?, 'gallery', '%s', ?, ?, ?, 'c09-residue', ?, ?)`, testDB, lid("g14")), f.tenant, f.u3.Kind(), f.u3.Key(), TypeClick, s(20), `{"render_id":"R09","position":1}`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.exposures (tenant, render_id, stage, query_id, surface, subject_kind, subject, content_kinds, content_ids, positions, occurred_at)
VALUES (?, 'R09', ?, 'q-R09', ?, ?, ?, ['gallery'], ['%s'], [1], ?)`, testDB, lid("g14")), f.tenant, string(StageRendered), SurfaceSearch, f.u3.Kind(), f.u3.Key(), f.day1); err != nil {
		t.Fatal(err)
	}
	return f
}

func clickIDs(cs []AttributedClick) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, lname(c.ContentID)+":"+c.EventID+"@"+c.Subject.Key())
	}
	return out
}

func renderIDs(rs []AttributedRender) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, fmt.Sprintf("%s(%d)", r.RenderID, len(r.Clicks)))
	}
	return out
}

func pageClicks(p AttributionPage) int {
	n := len(p.Unattributed)
	for _, r := range p.Renders {
		n += len(r.Clicks)
	}
	return n
}

// walkAttribution pages through an export, checking every page's bounds and
// continuity, and returns the concatenation (continued renders merged).
func walkAttribution(t *testing.T, st *Store, tenant string, opts AttributionOptions) (AttributionPage, int) {
	t.Helper()
	ctx := context.Background()
	limit, clickLimit := opts.Limit, opts.ClickLimit
	if limit <= 0 {
		limit = DefaultAttributionRenders
	}
	if clickLimit <= 0 {
		clickLimit = DefaultAttributionClicks
	}
	var merged AttributionPage
	pages := 0
	for {
		page, err := st.Attribution(ctx, tenant, opts)
		if err != nil {
			t.Fatalf("page %d: %v", pages+1, err)
		}
		pages++
		if len(page.Renders) > limit || pageClicks(page) > clickLimit {
			t.Fatalf("page %d exceeds bounds (%d renders, %d clicks): %v", pages, len(page.Renders), pageClicks(page), renderIDs(page.Renders))
		}
		if pages > 1 && len(page.Renders) == 0 && len(page.Unattributed) == 0 {
			t.Fatalf("page %d is empty although page %d advertised more", pages, pages-1)
		}
		if len(merged.Unattributed) > 0 && len(page.Renders) > 0 {
			t.Fatalf("page %d emits renders after the unattributed stream began", pages)
		}
		for i, r := range page.Renders {
			if r.Continued {
				if i != 0 || len(merged.Renders) == 0 {
					t.Fatalf("page %d: continued render %s not at the page start", pages, r.RenderID)
				}
				prev := &merged.Renders[len(merged.Renders)-1]
				if prev.RenderID != r.RenderID {
					t.Fatalf("page %d: continues %s but previous page ended with %s", pages, r.RenderID, prev.RenderID)
				}
				head, prevHead := r, *prev
				head.Clicks, head.Continued, prevHead.Clicks = nil, false, nil
				if !reflect.DeepEqual(head, prevHead) {
					t.Fatalf("page %d: continued render header changed: %+v vs %+v", pages, head, prevHead)
				}
				prev.Clicks = append(prev.Clicks, r.Clicks...)
				continue
			}
			for _, m := range merged.Renders {
				if m.RenderID == r.RenderID {
					t.Fatalf("page %d: render %s emitted twice without Continued", pages, r.RenderID)
				}
			}
			merged.Renders = append(merged.Renders, r)
		}
		merged.Unattributed = append(merged.Unattributed, page.Unattributed...)
		if page.Next == "" {
			return merged, pages
		}
		opts.After = page.Next
	}
}

func TestIntegrationAttributionPagedExport(t *testing.T) {
	f := loadAttributionFixture(t)
	ctx := context.Background()
	st := f.st
	// Keep superseded rows physically present until the merge check below.
	for _, table := range []string{"exposures", "signals"} {
		if err := f.conn.Exec(ctx, "SYSTEM STOP MERGES "+testDB+"."+table); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, table := range []string{"exposures", "signals"} {
			_ = f.conn.Exec(context.Background(), "SYSTEM START MERGES "+testDB+"."+table)
		}
	})
	base := AttributionOptions{Stage: StageRendered, Window: f.window}
	whole := AttributionOptions{Stage: StageRendered, Window: f.window, Limit: 1000, ClickLimit: 100000}

	ref, err := st.Attribution(ctx, f.tenant, whole)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Next != "" {
		t.Fatalf("whole export must fit one page: %+v", ref)
	}
	// Erased subject, out-of-window render and other tenant are absent; the
	// rest is in render id order with click counts.
	if got := renderIDs(ref.Renders); !reflect.DeepEqual(got, []string{"R01(0)", "R02(7)", "R03(1)", "R04(0)", "R05(2)", "R06(1)", "R07(1)", "R08(1)", "R10(0)"}) {
		t.Fatalf("renders: %v", got)
	}
	r02 := ref.Renders[1]
	if got := clickIDs(r02.Clicks); !reflect.DeepEqual(got, []string{"g1:c02-1@u1", "g2:c02-2@u1", "g3:c02-3@u1", "g4:c02-4@u1", "g5:c02-5@u1", "g1:c02-6@u1", "g9:c02-7@u1"}) {
		t.Fatalf("R02 clicks chronological: %v", got)
	}
	if !r02.Clicks[5].Exposed || r02.Clicks[6].Exposed {
		t.Fatalf("exposed flags: %+v", r02.Clicks)
	}
	if got := clickIDs(ref.Renders[4].Clicks); !reflect.DeepEqual(got, []string{"g7:same@a1", "g7:same@u1"}) {
		t.Fatalf("clicks differing only by subject are both canonical: %v", got)
	}
	if r07 := ref.Renders[6]; len(r07.Shown) != 2 || !r07.Clicks[0].Exposed {
		t.Fatalf("highest revision wins: %+v", r07)
	}
	if r08 := ref.Renders[7]; len(r08.Clicks) != 1 || r08.Clicks[0].EventID != "c08-1" {
		t.Fatalf("re-stamped retry collapses to one canonical click: %+v", r08.Clicks)
	}
	if got := clickIDs(ref.Unattributed); !reflect.DeepEqual(got, []string{"g1:cN1-1@u1", "g2:cN1-2@u2", "g3:cN2-1@a1", "g16:c11-1@u1", "g17:cS1-1@u1"}) {
		t.Fatalf("unattributed (never recorded, render outside window, served only; no organic, erased or foreign): %v", got)
	}

	// Every page size yields the same rows once, in the same order.
	for _, size := range [][2]int{{1, 1}, {1, 2}, {1, 3}, {2, 1}, {2, 3}, {3, 4}, {4, 7}, {5, 5}, {9, 3}, {9, 100}, {0, 0}, {100, 100}} {
		opts := base
		opts.Limit, opts.ClickLimit = size[0], size[1]
		merged, pages := walkAttribution(t, st, f.tenant, opts)
		if !reflect.DeepEqual(merged.Renders, ref.Renders) || !reflect.DeepEqual(merged.Unattributed, ref.Unattributed) {
			t.Fatalf("limit=%d clicks=%d (%d pages): concatenation differs\n got renders %v unattributed %v\nwant renders %v unattributed %v",
				size[0], size[1], pages, renderIDs(merged.Renders), clickIDs(merged.Unattributed), renderIDs(ref.Renders), clickIDs(ref.Unattributed))
		}
		if size[0] >= len(ref.Renders) && size[1] >= pageClicks(ref) && pages != 1 {
			t.Fatalf("limit=%d clicks=%d: dataset fits one page, got %d", size[0], size[1], pages)
		}
	}

	// A render with more clicks than the click bound spans pages.
	opts := base
	opts.Limit, opts.ClickLimit = 10, 3
	p1, err := st.Attribution(ctx, f.tenant, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := renderIDs(p1.Renders); !reflect.DeepEqual(got, []string{"R01(0)", "R02(3)"}) || p1.Next == "" || p1.Renders[1].Continued || len(p1.Unattributed) != 0 {
		t.Fatalf("page 1 stops inside R02: %v %+v", got, p1)
	}
	opts.After = p1.Next
	p2, err := st.Attribution(ctx, f.tenant, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := renderIDs(p2.Renders); !reflect.DeepEqual(got, []string{"R02(3)"}) || !p2.Renders[0].Continued || p2.Next == "" {
		t.Fatalf("page 2 continues R02: %v %+v", got, p2)
	}
	if got := clickIDs(p2.Renders[0].Clicks); !reflect.DeepEqual(got, []string{"g4:c02-4@u1", "g5:c02-5@u1", "g1:c02-6@u1"}) {
		t.Fatalf("page 2 resumes after the third click: %v", got)
	}
	opts.After = p2.Next
	p3, err := st.Attribution(ctx, f.tenant, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := renderIDs(p3.Renders); !reflect.DeepEqual(got, []string{"R02(1)", "R03(1)", "R04(0)", "R05(1)"}) || !p3.Renders[0].Continued || p3.Renders[3].Continued {
		t.Fatalf("page 3 finishes R02, carries the zero-click R04 and stops inside R05: %v", got)
	}

	// Full page of renders, then the unattributed stream on the final page.
	opts = base
	opts.Limit = len(ref.Renders)
	full, err := st.Attribution(ctx, f.tenant, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Renders) != len(ref.Renders) || full.Next != "" || !reflect.DeepEqual(full.Unattributed, ref.Unattributed) {
		t.Fatalf("exactly-full page is final when nothing follows: %+v", full)
	}
	opts.Limit = 3
	first, err := st.Attribution(ctx, f.tenant, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := renderIDs(first.Renders); !reflect.DeepEqual(got, []string{"R01(0)", "R02(7)", "R03(1)"}) || first.Next == "" || len(first.Unattributed) != 0 {
		t.Fatalf("full page: %v %+v", got, first)
	}

	// The unattributed stream alone is paged when the click budget is exhausted.
	opts = base
	opts.Limit, opts.ClickLimit = 100, 13 // 13 attributed clicks: renders fit, no room for unattributed
	edge, err := st.Attribution(ctx, f.tenant, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(edge.Renders) != len(ref.Renders) || len(edge.Unattributed) != 0 || edge.Next == "" {
		t.Fatalf("budget exhausted exactly at the last attributed click: %+v", edge)
	}
	opts.After = edge.Next
	rest, err := st.Attribution(ctx, f.tenant, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Renders) != 0 || !reflect.DeepEqual(rest.Unattributed, ref.Unattributed) || rest.Next != "" {
		t.Fatalf("remaining page carries only unattributed clicks: %+v", rest)
	}

	// Surface filter: another surface's render leaves the renders and its click
	// joins the unattributed stream; paging still concatenates exactly.
	search := AttributionOptions{Stage: StageRendered, Window: f.window, Surface: SurfaceSearch}
	sref, err := st.Attribution(ctx, f.tenant, search)
	if err != nil {
		t.Fatal(err)
	}
	if got := renderIDs(sref.Renders); !reflect.DeepEqual(got, []string{"R01(0)", "R02(7)", "R03(1)", "R04(0)", "R05(2)", "R07(1)", "R08(1)", "R10(0)"}) {
		t.Fatalf("search renders: %v", got)
	}
	if got := clickIDs(sref.Unattributed); !reflect.DeepEqual(got, []string{"g1:cN1-1@u1", "g2:cN1-2@u2", "g3:cN2-1@a1", "g10:c06-1@u1", "g16:c11-1@u1", "g17:cS1-1@u1"}) {
		t.Fatalf("search unattributed: %v", got)
	}
	for _, size := range [][2]int{{1, 1}, {2, 2}, {3, 5}} {
		opts := search
		opts.Limit, opts.ClickLimit = size[0], size[1]
		merged, _ := walkAttribution(t, st, f.tenant, opts)
		if !reflect.DeepEqual(merged.Renders, sref.Renders) || !reflect.DeepEqual(merged.Unattributed, sref.Unattributed) {
			t.Fatalf("surface paging %v: %v %v", size, renderIDs(merged.Renders), clickIDs(merged.Unattributed))
		}
	}

	// Served stage: only S01 rendered nothing else; every rendered click is unattributed there.
	served, err := st.Attribution(ctx, f.tenant, AttributionOptions{Stage: StageServed, Window: f.window})
	if err != nil {
		t.Fatal(err)
	}
	if got := renderIDs(served.Renders); !reflect.DeepEqual(got, []string{"S01(1)"}) || len(served.Unattributed) != 17 {
		t.Fatalf("served stage: %v %d", got, len(served.Unattributed))
	}

	// Tenant isolation and the empty export.
	foreign, err := st.Attribution(ctx, "o", base)
	if err != nil || !reflect.DeepEqual(renderIDs(foreign.Renders), []string{"R01(1)"}) || !reflect.DeepEqual(clickIDs(foreign.Unattributed), []string{"g1:o-2@u1"}) || foreign.Next != "" {
		t.Fatalf("tenant isolation: %+v %v", foreign, err)
	}
	empty, err := st.Attribution(ctx, "nobody", AttributionOptions{Stage: StageRendered, Limit: 1, ClickLimit: 1})
	if err != nil || len(empty.Renders) != 0 || len(empty.Unattributed) != 0 || empty.Next != "" {
		t.Fatalf("empty export: %+v %v", empty, err)
	}

	// Cursors are scoped to their export and validated.
	if _, err := st.Attribution(ctx, f.tenant, AttributionOptions{Stage: StageServed, Window: f.window, After: first.Next}); err == nil || !strings.Contains(err.Error(), "another export") {
		t.Fatalf("cursor from another stage must be rejected: %v", err)
	}
	if _, err := st.Attribution(ctx, f.tenant, AttributionOptions{Stage: StageRendered, Window: f.window, After: "R02"}); err == nil || !strings.Contains(err.Error(), "invalid attribution cursor") {
		t.Fatalf("a bare render id is not a cursor: %v", err)
	}

	// Merges change nothing.
	for _, table := range []string{"exposures", "signals"} {
		if err := f.conn.Exec(ctx, "SYSTEM START MERGES "+testDB+"."+table); err != nil {
			t.Fatal(err)
		}
		if err := f.conn.Exec(ctx, "OPTIMIZE TABLE "+testDB+"."+table+" FINAL"); err != nil {
			t.Fatal(err)
		}
	}
	again, err := st.Attribution(ctx, f.tenant, whole)
	if err != nil || !reflect.DeepEqual(again, ref) {
		t.Fatalf("merges changed the export: %v", err)
	}
	merged, _ := walkAttribution(t, st, f.tenant, AttributionOptions{Stage: StageRendered, Window: f.window, Limit: 2, ClickLimit: 2})
	if !reflect.DeepEqual(merged.Renders, ref.Renders) || !reflect.DeepEqual(merged.Unattributed, ref.Unattributed) {
		t.Fatalf("merged paging differs: %v %v", renderIDs(merged.Renders), clickIDs(merged.Unattributed))
	}
}
