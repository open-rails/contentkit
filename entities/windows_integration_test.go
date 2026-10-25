package entities

import (
	"slices"
	"testing"
	"time"
)

// Facets were written before a window opened and before it closed, as after a
// multi-day worker outage; one windows run catches up both boundaries, and the
// recount then follows the refreshed facets.
func TestWindowsAndRecountAfterOutage(t *testing.T) {
	f := newFixture(t)
	rail := f.s.cfg.EntityKinds["rail"]
	rail.Parent, rail.VersionLevel = "rail", true
	f.s.cfg.EntityKinds["rail"] = rail
	f.content("gallery", 1, live("en"), live("es"))
	parent := f.create(EntityInput{Kind: "rail", Slug: "parent"})[0]
	child := f.create(EntityInput{Kind: "rail", Slug: "child", ParentID: parent.ID})[0]
	manual := f.create(EntityInput{Kind: "rail", Slug: "manual", Cover: &Cover{ContentID: cid(1)}})[0]
	future := time.Now().Add(24 * time.Hour)
	f.add(Membership{EntityID: child.ID, ContentID: cid(1), VersionID: vid(1, 1), ActiveFrom: &future})
	f.add(Membership{EntityID: manual.ID, ContentID: cid(1)})
	refresh := func() {
		t.Helper()
		if err := f.s.RefreshWindows(f.ctx); err != nil {
			t.Fatal(err)
		}
		if err := f.s.Recount(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	refresh()
	if got := f.facetEntities()[vid(1, 1)]; !slices.Equal(got, ids(manual)) {
		t.Fatalf("future membership applied: %v", got)
	}
	f.exec(`UPDATE {s}.content_memberships SET active_from=now()-interval '2 days' WHERE entity_id=$1`, string(child.ID))
	f.exec(`UPDATE {s}.content_facets SET updated_at=now()-interval '3 days'`)
	refresh()
	if got := f.facetEntities(); !slices.Equal(got[vid(1, 1)], ids(parent, child, manual)) || !slices.Equal(got[vid(1, 0)], ids(manual)) {
		t.Fatalf("opened window: %v", got)
	}
	detail, err := f.s.Detail(f.ctx, parent.ID, "es")
	if err != nil || len(detail.Counts) != 2 || detail.Cover == nil || !detail.Cover.Auto || detail.Cover.ContentID != cid(1) {
		t.Fatalf("parent after open: %+v %v", detail, err)
	}
	f.exec(`UPDATE {s}.content_memberships SET active_until=now()-interval '1 day' WHERE entity_id=$1`, string(child.ID))
	f.exec(`UPDATE {s}.content_facets SET updated_at=now()-interval '36 hours'`)
	refresh()
	if got := f.facetEntities()[vid(1, 1)]; !slices.Equal(got, ids(manual)) {
		t.Fatalf("closed window: %v", got)
	}
	detail, err = f.s.Detail(f.ctx, parent.ID, "es")
	if err != nil || len(detail.Counts) != 0 || detail.Cover != nil {
		t.Fatalf("parent after close: %+v %v", detail, err)
	}
	got, err := f.s.Entities(f.ctx, []EntityID{manual.ID})
	if err != nil || len(got) != 1 || got[0].Cover == nil || got[0].Cover.Auto || got[0].Cover.ContentID != cid(1) {
		t.Fatalf("manual cover changed: %+v %v", got, err)
	}
}
