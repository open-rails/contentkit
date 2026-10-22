package catalog

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/internal/pgtest"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
)

func TestCatalogEventsQueueWithMutationAndKeepRunningFollowUp(t *testing.T) {
	f := newFixture(t, nil)
	id := f.create("staff", gallery("Queued events", "en"))
	schema := pgtest.EmptySchema(t, f.ctx, f.pool)
	if err := riverhelpers.ApplyMigrations(f.ctx, f.pool, schema); err != nil {
		t.Fatal(err)
	}
	_, err := riverhelpers.New(f.ctx, f.pool, &river.Config{Schema: schema}, f.s.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	jobs := pgx.Identifier{schema, "river_job"}.Sanitize()
	count := func(want int) {
		t.Helper()
		var got int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+jobs+` WHERE kind='contentkit_catalog_deliver_events'`).Scan(&got); err != nil || got != want {
			t.Fatalf("queued events=%d, want %d: %v", got, want, err)
		}
	}
	emit := func(commit bool) {
		t.Helper()
		tx, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(f.ctx)
		for range 2 {
			if err := f.s.emit(f.ctx, tx, Event{Type: EventContentUpdated, Kind: "gallery", ContentID: id}); err != nil {
				t.Fatal(err)
			}
		}
		if commit {
			if err := tx.Commit(f.ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	emit(false)
	count(0)
	emit(true)
	count(1) // A burst coalesces before the worker starts.
	var running int64
	if err := f.pool.QueryRow(f.ctx, `UPDATE `+jobs+` SET state='running',attempted_at=now() WHERE kind='contentkit_catalog_deliver_events' RETURNING id`).Scan(&running); err != nil {
		t.Fatal(err)
	}
	emit(true)
	emit(true)
	count(2) // A writer racing the last batch preserves exactly one follow-up.
	var after int64
	if err := f.pool.QueryRow(f.ctx, `SELECT (args->>'after')::bigint FROM `+jobs+` WHERE state='available'`).Scan(&after); err != nil || after != running {
		t.Fatalf("follow-up predecessor=%d, want %d: %v", after, running, err)
	}
}

func TestCatalogEventRecoveryFindsUnboundProducer(t *testing.T) {
	f := newFixture(t, nil)
	f.create("staff", gallery("Before the fleet starts", "en"))
	var delivered int
	f.s.Subscribe(func(_ context.Context, events []Event) error {
		delivered += len(events)
		return nil
	})
	if _, err := f.s.DeliverEvents(f.ctx); err != nil {
		t.Fatal(err)
	}
	if delivered == 0 {
		t.Fatal("recovery lost events written before the producer was bound")
	}
}

func TestCatalogEventsDeliverWithPeriodicJobsDisabled(t *testing.T) {
	f := newFixture(t, nil)
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	schema := pgtest.EmptySchema(t, f.ctx, f.pool)
	if err := riverhelpers.ApplyMigrations(f.ctx, f.pool, schema); err != nil {
		t.Fatal(err)
	}
	// The subscriber holds the first delivery after its batch was read. A
	// concurrent real mutation must create a runnable follow-up, not wait for
	// a periodic recovery that this fleet deliberately does not have.
	entered, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	delivered := make(chan Event, 16)
	f.s.Subscribe(func(ctx context.Context, events []Event) error {
		if held.CompareAndSwap(false, true) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		for _, event := range events {
			delivered <- event
		}
		return nil
	})
	client, err := riverhelpers.New(ctx, f.pool, &river.Config{Schema: schema, FetchCooldown: 50 * time.Millisecond, FetchPollInterval: 100 * time.Millisecond}, f.s.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	client.PeriodicJobs().Clear()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.StopAndCancel(context.Background()) })
	first := f.create("staff", gallery("First immediate delivery", "en"))
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("mutation did not wake the delivery worker:", ctx.Err())
	}
	second := f.create("staff", gallery("Follow-up immediate delivery", "en"))
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for {
		var waiting bool
		if err := f.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+pgx.Identifier{schema, "river_job"}.Sanitize()+` WHERE kind='contentkit_catalog_deliver_events' AND args ? 'after' AND coalesce((metadata->>'snoozes')::int,0)>0)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break // The real follow-up worker ran WaitFor and snoozed behind the held callback.
		}
		select {
		case <-poll.C:
		case <-ctx.Done():
			t.Fatal("follow-up worker did not wait for the running delivery:", ctx.Err())
		}
	}
	unblock()
	want := map[string]bool{first: true, second: true}
	for len(want) > 0 {
		select {
		case event := <-delivered:
			if event.Type == EventContentCreated {
				delete(want, event.ContentID)
			}
		case <-ctx.Done():
			t.Fatalf("events did not arrive with recovery disabled: %v (%v)", want, ctx.Err())
		}
	}
}
