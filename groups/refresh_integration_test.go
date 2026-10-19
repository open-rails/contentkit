package groups

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/internal/pgtest"
)

// Group facets grant access and must match the hierarchy when a write commits,
// even above the former asynchronous threshold and with River bound.
func TestLargeGroupRefreshCommitsWithTheWrite(t *testing.T) {
	f := newFixture(t)
	parent := f.create(GroupInput{Kind: "series", Slug: "parent"})[0]
	g := f.create(GroupInput{Kind: "series", Slug: "child", ParentID: parent.ID})[0]
	for n := 1; n <= 501; n++ {
		f.content("gallery", n, live("en"))
		f.add(Membership{GroupID: g.ID, ContentID: cid(n)})
	}
	check := func(want []string) {
		t.Helper()
		facets := f.facetGroups()
		if len(facets) != 501 {
			t.Fatalf("facets = %d, want 501", len(facets))
		}
		for version, got := range facets {
			if !slices.Equal(got, want) {
				t.Fatalf("version %s groups = %v, want %v", version, got, want)
			}
		}
	}
	check(ids(parent, g))

	schema := pgtest.EmptySchema(t, f.ctx, f.pool)
	if err := riverhelpers.ApplyMigrations(f.ctx, f.pool, schema); err != nil {
		t.Fatal(err)
	}
	_, err := riverhelpers.New(f.ctx, f.pool, &river.Config{Schema: schema}, f.s.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}

	// Leave River stopped. A failed refresh rolls back both hierarchy and facets.
	changed := f.s.changed
	failure := errors.New("refresh failed")
	f.s.changed = func(context.Context, Querier, []string) error { return failure }
	empty := GroupID("")
	_, err = f.s.UpdateGroups(f.ctx, []GroupUpdate{{ID: g.ID, ParentID: &empty}})
	f.s.changed = changed
	if !errors.Is(err, failure) {
		t.Fatalf("update with failed refresh = %v, want %v", err, failure)
	}
	check(ids(parent, g))
	var storedParent string
	if err := f.pool.QueryRow(f.ctx, `SELECT parent_id FROM `+f.s.table("content_groups")+` WHERE tenant_id=$1 AND id=$2`, tenant, string(g.ID)).Scan(&storedParent); err != nil {
		t.Fatal(err)
	}
	if storedParent != string(parent.ID) {
		t.Fatalf("failed update changed parent to %q", storedParent)
	}

	if _, err := f.s.UpdateGroups(f.ctx, []GroupUpdate{{ID: g.ID, ParentID: &empty}}); err != nil {
		t.Fatal(err)
	}
	check(ids(g))
	if _, err := f.s.DeleteGroups(f.ctx, []GroupID{g.ID}); err != nil {
		t.Fatal(err)
	}
	check(nil)
	var queued int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+pgx.Identifier{schema}.Sanitize()+`.river_job WHERE kind = 'contentkit_group_refresh'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("queued refresh jobs = %d, want 0", queued)
	}
}

// A refresh resumes after the content it stopped at and reports where.
func TestRefreshGroupMembersResumesInBatches(t *testing.T) {
	batch := refreshBatch
	refreshBatch = 2
	t.Cleanup(func() { refreshBatch = batch })
	f := newFixture(t)
	g := f.create(GroupInput{Kind: "tag", Slug: "big"})[0]
	for n := 1; n <= 5; n++ {
		f.content("gallery", n, live("en"))
		f.add(Membership{GroupID: g.ID, ContentID: cid(n)})
	}
	f.changed = nil
	next, err := f.s.RefreshGroupMembers(f.ctx, []string{string(g.ID)}, cid(2), 0)
	if err != nil || next != "" {
		t.Fatalf("refresh after %s = %q, %v; want done", cid(2), next, err)
	}
	want := [][]string{{cid(3), cid(4)}, {cid(5)}}
	if !reflect.DeepEqual(f.changed, want) {
		t.Fatalf("refreshed batches = %v, want %v", f.changed, want)
	}
}
