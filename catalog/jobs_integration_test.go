package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/pgtest"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
)

// queuedContentArgs represents the empty arguments persisted under old kinds.
type queuedContentArgs struct{ kind string }

func (a queuedContentArgs) Kind() string { return a.kind }

func TestContentJobsDispatchLegacyKinds(t *testing.T) {
	f := newFixture(t, nil)
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	schema := pgtest.EmptySchema(t, ctx, f.pool)
	if err := riverhelpers.ApplyMigrations(ctx, f.pool, schema); err != nil {
		t.Fatal(err)
	}
	client, err := riverhelpers.New(ctx, f.pool, &river.Config{Schema: schema}, f.s.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	client.PeriodicJobs().Clear()
	events, unsubscribe := client.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	defer unsubscribe()
	want := make(map[int64]string)
	for _, name := range []string{"publish_due", "purge_deleted"} {
		for _, prefix := range []string{"contentkit_catalog_", "contentkit_content_"} {
			kind := prefix + name
			job, err := client.Insert(ctx, queuedContentArgs{kind: kind}, &river.InsertOpts{Queue: "contentkit_catalog", MaxAttempts: 1})
			if err != nil {
				t.Fatalf("insert %s: %v", kind, err)
			}
			want[job.Job.ID] = kind
		}
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.StopAndCancel(context.Background()) }()
	for len(want) > 0 {
		select {
		case event := <-events:
			if event.Kind != river.EventKindJobCompleted {
				t.Fatalf("%s did not complete: %+v", event.Job.Kind, event.Job.Errors)
			}
			delete(want, event.Job.ID)
		case <-ctx.Done():
			t.Fatalf("jobs did not dispatch: %v (%v)", want, ctx.Err())
		}
	}
}
