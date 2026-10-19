package groups

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/internal/pgtest"
)

// A write that reaches more than inlineRefresh contents commits without
// rewriting them, and a queued job refreshes them in batches.
func TestLargeGroupRefreshRunsAfterTheWriteInBatches(t *testing.T) {
	inline, batch := inlineRefresh, refreshBatch
	inlineRefresh, refreshBatch = 2, 2
	t.Cleanup(func() { inlineRefresh, refreshBatch = inline, batch })

	f := newFixture(t)
	g := f.create(GroupInput{Kind: "tag", Slug: "big"})[0]
	for n := 1; n <= 5; n++ {
		f.content("gallery", n, live("en"))
		f.add(Membership{GroupID: g.ID, ContentID: cid(n)})
	}
	named := func() int {
		n := 0
		for _, ids := range f.facetGroups() {
			if slices.Contains(ids, string(g.ID)) {
				n++
			}
		}
		return n
	}
	if got := named(); got != 5 {
		t.Fatalf("facets naming the tag before = %d, want 5", got)
	}

	schema := pgtest.EmptySchema(t, f.ctx, f.pool)
	if err := riverhelpers.ApplyMigrations(f.ctx, f.pool, schema); err != nil {
		t.Fatal(err)
	}
	client, err := riverhelpers.New(f.ctx, f.pool, &river.Config{Schema: schema, FetchPollInterval: 50 * time.Millisecond, FetchCooldown: 20 * time.Millisecond}, f.s.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}

	// Not started yet: the write queues the refresh and leaves the facets.
	if _, err := f.s.DeleteGroups(f.ctx, []GroupID{g.ID}); err != nil {
		t.Fatal(err)
	}
	if got := named(); got != 5 {
		t.Fatalf("facets naming the deleted tag right after the write = %d, want 5 (left to the job)", got)
	}
	var queued int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+pgx.Identifier{schema}.Sanitize()+`.river_job WHERE kind = 'contentkit_group_refresh'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("queued refresh jobs = %d, want 1", queued)
	}

	if err := client.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.StopAndCancel(ctx)
	})
	deadline := time.Now().Add(10 * time.Second)
	for named() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("facets still naming the deleted tag after the job: %d", named())
		}
		time.Sleep(50 * time.Millisecond)
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
