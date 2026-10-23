package catalog

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/internal/pgtest"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
)

func readEventLog(t *testing.T, f *fixture) []EventRecord {
	t.Helper()
	var events []EventRecord
	query := EventQuery{Limit: 500}
	for {
		page, err := f.s.ListEvents(f.ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, page.Events...)
		if !page.HasMore {
			return events
		}
		query.After = page.NextCursor
	}
}

func TestCatalogEventLogRollsBackWithoutDeliveryJobs(t *testing.T) {
	f := newFixture(t, nil)
	id := f.create("staff", gallery("Durable fact", "en"))
	before := readEventLog(t, f)
	schema := pgtest.EmptySchema(t, f.ctx, f.pool)
	if err := riverhelpers.ApplyMigrations(f.ctx, f.pool, schema); err != nil {
		t.Fatal(err)
	}
	if _, err := riverhelpers.New(f.ctx, f.pool, &river.Config{Schema: schema}, f.s.RiverJobs()); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.emit(f.ctx, tx, Event{Type: EventContentUpdated, Kind: "gallery", ContentID: id}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := readEventLog(t, f); !reflect.DeepEqual(got, before) {
		t.Fatalf("rollback published a fact: %+v", got)
	}
	if err := f.s.Update(f.ctx, f.as("staff"), []Update{{ID: id, ContentPatch: ContentPatch{Comments: ptr("closed")}}}); err != nil {
		t.Fatal(err)
	}
	if got := readEventLog(t, f); len(got) != len(before)+1 || got[len(got)-1].Type != EventContentUpdated {
		t.Fatalf("committed mutation missing: %+v", got)
	}
	var jobs int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+pgx.Identifier{schema, "river_job"}.Sanitize()).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("event mutation enqueued %d jobs: %v", jobs, err)
	}
}

func TestCatalogEventLogPreservesCommitOrderWithoutLockingOtherApps(t *testing.T) {
	f := newFixture(t, nil)
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := f.s.emit(ctx, tx, Event{Type: EventContentCreated, Kind: "gallery", ContentID: "first"}); err != nil {
		t.Fatal(err)
	}
	entered, completed := make(chan struct{}), make(chan error, 1)
	go func() {
		later, err := f.pool.Begin(ctx)
		if err != nil {
			completed <- err
			return
		}
		defer later.Rollback(context.Background())
		close(entered)
		if err := f.s.emit(ctx, later, Event{Type: EventContentCreated, Kind: "gallery", ContentID: "second"}); err != nil {
			completed <- err
			return
		}
		completed <- later.Commit(ctx)
	}()
	<-entered
	select {
	case err := <-completed:
		t.Fatalf("later writer escaped the commit-order lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	page, err := f.s.ListEvents(ctx, EventQuery{})
	if err != nil || len(page.Events) != 0 || page.NextCursor == "" {
		t.Fatalf("uncommitted facts leaked: %+v %v", page, err)
	}
	other, err := New(ctx, Options{Pool: f.pool, Schema: f.schema, Tenant: "another-app", Kinds: config(), Auth: f.auth})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	independent, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.emit(ctx, independent, Event{Type: EventContentCreated, Kind: "gallery", ContentID: "independent"}); err != nil {
		independent.Rollback(ctx)
		t.Fatal(err)
	}
	if err := independent.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	page, err = f.s.ListEvents(ctx, EventQuery{After: page.NextCursor})
	if err != nil || len(page.Events) != 2 || page.Events[0].ContentID != "first" || page.Events[1].ContentID != "second" {
		t.Fatalf("committed order skipped a writer: %+v %v", page, err)
	}
}

func TestCatalogEventReadersAreIndependentAndReplayable(t *testing.T) {
	f := newFixture(t, nil)
	id := f.create("staff", gallery("Independent readers", "en"))
	f.publish("staff", id, nil)
	first, err := f.s.ListEvents(f.ctx, EventQuery{Limit: 1})
	if err != nil || len(first.Events) != 1 || !first.HasMore {
		t.Fatalf("first page: %+v %v", first, err)
	}
	for range 2 {
		replay, err := f.s.ListEvents(f.ctx, EventQuery{Limit: 1})
		if err != nil || !reflect.DeepEqual(replay, first) {
			t.Fatalf("one reader affected another or its replay: %+v %v", replay, err)
		}
	}
	next, err := f.s.ListEvents(f.ctx, EventQuery{After: first.NextCursor})
	if err != nil || len(next.Events) != 2 || next.Events[0].Type != EventVersionCreated || next.Events[1].Type != EventContentPublished {
		t.Fatalf("continuation: %+v %v", next, err)
	}
	end, err := f.s.ListEvents(f.ctx, EventQuery{After: next.NextCursor})
	if err != nil || len(end.Events) != 0 || end.HasMore || end.NextCursor == "" {
		t.Fatalf("empty continuation: %+v %v", end, err)
	}
	if err := f.s.ContentChanged(f.ctx, f.pool, []string{id}); err == nil {
		t.Fatal("accepted nontransactional group callback")
	}
	if _, err := f.s.ListEvents(f.ctx, EventQuery{Types: []string{"not.an.event"}}); !errors.Is(err, ErrEventCursorInvalid) {
		t.Fatalf("unknown type: %v", err)
	}
	if _, err := f.s.ListEvents(f.ctx, EventQuery{Limit: 501}); !errors.Is(err, ErrEventCursorInvalid) {
		t.Fatalf("unbounded limit: %v", err)
	}
}
