package groups

import (
	"slices"
	"testing"
	"time"
)

// No River client is bound. Stored facets deliberately retain their old window
// state, as after a multi-day outage; reads must use the current memberships.
func TestRuntimeWindowsAcrossOutage(t *testing.T) {
	f := newFixture(t)
	rail := f.s.cfg.GroupKinds["rail"]
	rail.Parent, rail.VersionLevel = "rail", true
	f.s.cfg.GroupKinds["rail"] = rail
	f.content("gallery", 1, live("en"), live("es"))
	parent := f.create(GroupInput{Kind: "rail", Slug: "parent"})[0]
	child := f.create(GroupInput{Kind: "rail", Slug: "child", ParentID: parent.ID})[0]
	manual := f.create(GroupInput{Kind: "rail", Slug: "manual", Cover: &Cover{ContentID: cid(1)}})[0]
	future := time.Now().Add(24 * time.Hour)
	f.add(Membership{GroupID: child.ID, ContentID: cid(1), VersionID: vid(1, 1), ActiveFrom: &future})
	read := func(version string) []string {
		t.Helper()
		var ids []string
		if err := f.pool.QueryRow(f.ctx, `SELECT group_ids FROM `+f.s.table("content_current_facets")+` WHERE tenant_id=$1 AND version_id=$2`, tenant, version).Scan(&ids); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	if got := read(vid(1, 1)); len(got) != 0 {
		t.Fatalf("future membership visible: %v", got)
	}
	f.exec(`UPDATE {s}.content_memberships SET active_from=now()-interval '3 days' WHERE group_id=$1`, string(child.ID))
	if got := read(vid(1, 1)); !slices.Equal(got, ids(parent, child)) {
		t.Fatalf("outage lost inherited membership: %v", got)
	}
	if got := read(vid(1, 0)); len(got) != 0 {
		t.Fatalf("version association leaked: %v", got)
	}
	if len(f.facetGroups()[vid(1, 1)]) != 0 {
		t.Fatal("test did not leave the materialized facet stale")
	}
	detail, err := f.s.Detail(f.ctx, parent.ID, "es")
	if err != nil || len(detail.Counts) != 2 || detail.Cover == nil || detail.Cover.ContentID != cid(1) {
		t.Fatalf("runtime parent: %+v %v", detail, err)
	}
	// Exercise inclusive start and exclusive end at one exact database clock.
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err = tx.Exec(f.ctx, `UPDATE `+f.s.table("content_memberships")+` SET active_from=now(),active_until=NULL WHERE group_id=$1`, string(child.ID)); err != nil {
		t.Fatal(err)
	}
	var boundary []string
	currentQuery := `SELECT group_ids FROM ` + f.s.table("content_current_facets") + ` WHERE tenant_id=$1 AND version_id=$2`
	if err = tx.QueryRow(f.ctx, currentQuery, tenant, vid(1, 1)).Scan(&boundary); err != nil || !slices.Contains(boundary, string(child.ID)) {
		t.Fatalf("inclusive start: %v %v", boundary, err)
	}
	if _, err = tx.Exec(f.ctx, `UPDATE `+f.s.table("content_memberships")+` SET active_from=now()-interval '3 days',active_until=now() WHERE group_id=$1`, string(child.ID)); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(f.ctx, currentQuery, tenant, vid(1, 1)).Scan(&boundary); err != nil || len(boundary) != 0 {
		t.Fatalf("exclusive end: %v %v", boundary, err)
	}
	if err = tx.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	// The end is exclusive, including the exact current transaction boundary.
	f.exec(`UPDATE {s}.content_memberships SET active_until=now()-interval '2 days' WHERE group_id=$1`, string(child.ID))
	if got := read(vid(1, 1)); len(got) != 0 {
		t.Fatalf("expired membership survives outage: %v", got)
	}
	detail, err = f.s.Detail(f.ctx, parent.ID, "es")
	if err != nil || len(detail.Counts) != 0 || detail.Cover != nil {
		t.Fatalf("expired count/cover: %+v %v", detail, err)
	}
	got, err := f.s.Groups(f.ctx, []GroupID{manual.ID})
	if err != nil || len(got) != 1 || got[0].Cover == nil || got[0].Cover.Auto || got[0].Cover.ContentID != cid(1) {
		t.Fatalf("manual cover changed: %+v %v", got, err)
	}
	f.exec(`UPDATE {s}.content_memberships SET active_until=NULL WHERE group_id=$1`, string(child.ID))
	if _, err := f.s.DeleteGroups(f.ctx, []GroupID{child.ID}); err != nil {
		t.Fatal(err)
	}
	if got := read(vid(1, 1)); len(got) != 0 {
		t.Fatalf("deleted entity still grants membership: %v", got)
	}
	// Each query is tenant-bound, including recursive ancestors.
	var count int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.s.table("content_current_facets")+` WHERE tenant_id='other'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("tenant isolation: %d %v", count, err)
	}
}
