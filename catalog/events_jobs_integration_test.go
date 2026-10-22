package catalog

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

func TestCatalogEventEnqueueDoesNotDeadlockWithCompletion(t *testing.T) {
	f := newFixture(t, nil)
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	schema := pgtest.EmptySchema(t, ctx, f.pool)
	if err := riverhelpers.ApplyMigrations(ctx, f.pool, schema); err != nil {
		t.Fatal(err)
	}
	c, err := riverhelpers.New(ctx, f.pool, &river.Config{Schema: schema}, f.s.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	opts := &river.InsertOpts{Queue: Queue, UniqueOpts: media.PendingOnce}
	first, err := c.Insert(ctx, refreshCountsArgs{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := c.Insert(ctx, deliverEventsArgs{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	jobs := pgx.Identifier{schema, "river_job"}.Sanitize()
	if _, err := f.pool.Exec(ctx, `UPDATE `+jobs+` SET state='running',attempted_at=now()`); err != nil {
		t.Fatal(err)
	}
	writer, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(f.ctx)
	// A mutation can already hold another job's lock (the engine's search
	// sync) before queuing events. A unique insert takes that lock even when
	// it only returns an existing job.
	if _, err := c.InsertTx(ctx, writer, refreshCountsArgs{}, opts); err != nil {
		t.Fatal(err)
	}
	completer, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer completer.Rollback(f.ctx)
	// Force the completer's first row to be the delivery. Its UPDATE FROM
	// has no guaranteed lock order; the final ORDER BY only sorts results.
	if _, err := completer.Exec(ctx, `SELECT id FROM `+jobs+` WHERE id=$1 FOR NO KEY UPDATE`, delivery.Job.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	writerPID, completerPID := writer.Conn().PgConn().PID(), completer.Conn().PgConn().PID()
	go func() {
		err := f.s.enqueueEvents(ctx, writer)
		if err == nil {
			err = writer.Commit(ctx)
		} else {
			_ = writer.Rollback(ctx)
		}
		done <- err
	}()
	// Continue once the writer either commits or is demonstrably waiting
	// for the completer, so the old code always forms the same cycle.
	var writerErr error
	finished := false
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
wait:
	for {
		select {
		case writerErr = <-done:
			finished = true
			break wait
		case <-tick.C:
			var blocked bool
			if err := f.pool.QueryRow(ctx, `SELECT $1::int = ANY(pg_blocking_pids($2::int))`,
				completerPID, writerPID).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				break wait
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	now := time.Now()
	_, completeErr := riverpgxv5.New(f.pool).UnwrapExecutor(completer).JobSetStateIfRunningMany(ctx, &riverdriver.JobSetStateIfRunningManyParams{
		Schema: schema, ID: []int64{first.Job.ID, delivery.Job.ID},
		Attempt: []*int{nil, nil}, ErrData: [][]byte{nil, nil}, FinalizedAt: []*time.Time{&now, &now},
		MetadataDoMerge: []bool{false, false}, ScheduledAt: []*time.Time{nil, nil},
		State: []rivertype.JobState{rivertype.JobStateCompleted, rivertype.JobStateCompleted},
	})
	if completeErr == nil {
		completeErr = completer.Commit(ctx)
	} else {
		_ = completer.Rollback(ctx)
	}
	if !finished {
		writerErr = <-done
	}
	for _, err := range []error{writerErr, completeErr} {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
			t.Fatalf("event enqueue and River completion deadlocked: %v", err)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestCatalogEventsQueueAWakeUpWithEachMutation(t *testing.T) {
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
	count(2)
	if _, err := f.pool.Exec(f.ctx, `UPDATE `+jobs+` SET state='running',attempted_at=now() WHERE kind='contentkit_catalog_deliver_events'`); err != nil {
		t.Fatal(err)
	}
	emit(true)
	emit(true)
	count(6) // Writes racing a running delivery retain their own wake-ups.
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
	var old int64
	if err := f.pool.QueryRow(f.ctx, `UPDATE `+f.s.t.events+` SET delivered_at=$1 WHERE tenant_id=$2 RETURNING id`, f.s.now().Add(-deliveredRetention-time.Hour), tenant).Scan(&old); err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err := tx.Exec(f.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "contentkit:catalog:events:"+f.schema+":"+tenant); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.DeliverEvents(f.ctx); n != 0 || !errors.Is(err, ErrEventDeliveryBusy) {
		t.Fatalf("busy delivery = %d, %v; want 0, ErrEventDeliveryBusy", n, err)
	}
	var exists bool
	if err := f.pool.QueryRow(f.ctx, `SELECT EXISTS (SELECT 1 FROM `+f.s.t.events+` WHERE tenant_id=$1 AND id=$2)`, tenant, old).Scan(&exists); err != nil || !exists {
		t.Fatalf("busy delivery retained old event = %v: %v", exists, err)
	}
	if err := tx.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.DeliverEvents(f.ctx); n != 0 || err != nil {
		t.Fatalf("empty delivery = %d, %v; want 0, nil", n, err)
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT EXISTS (SELECT 1 FROM `+f.s.t.events+` WHERE tenant_id=$1 AND id=$2)`, tenant, old).Scan(&exists); err != nil || exists {
		t.Fatalf("empty delivery retained old event = %v: %v", exists, err)
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
	jobs := pgx.Identifier{schema, "river_job"}.Sanitize()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		var waiting bool
		if err := f.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+jobs+` WHERE kind='contentkit_catalog_deliver_events'
 AND state IN ('available','scheduled') AND attempt=0 AND coalesce((metadata->>'snoozes')::int,0)>0)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case event := <-delivered:
			t.Fatalf("delivery overtook the held subscriber: %+v", event)
		case <-poll.C:
		case <-ctx.Done():
			t.Fatal("second delivery did not snooze without consuming an attempt:", ctx.Err())
		}
	}
	if n, err := f.s.DeliverEvents(ctx); n != 0 || !errors.Is(err, ErrEventDeliveryBusy) {
		t.Fatalf("concurrent direct delivery = %d, %v; want 0, ErrEventDeliveryBusy", n, err)
	}
	// The snoozed wake-up must leave the other catalog worker slot free.
	other, err := client.Insert(ctx, refreshCountsArgs{}, &river.InsertOpts{Queue: Queue})
	if err != nil {
		t.Fatal(err)
	}
	for {
		var completed bool
		if err := f.pool.QueryRow(ctx, `SELECT state='completed' FROM `+jobs+` WHERE id=$1`, other.Job.ID).Scan(&completed); err != nil {
			t.Fatal(err)
		}
		if completed {
			break
		}
		select {
		case event := <-delivered:
			t.Fatalf("delivery overtook the held subscriber: %+v", event)
		case <-poll.C:
		case <-ctx.Done():
			t.Fatal("snoozed delivery did not free its worker slot:", ctx.Err())
		}
	}
	select {
	case event := <-delivered:
		t.Fatalf("delivery overtook the held subscriber: %+v", event)
	default:
	}
	unblock()
	want := map[string]bool{first: true, second: true}
	for len(want) > 0 {
		select {
		case event := <-delivered:
			if event.Type == EventContentCreated {
				if event.ContentID == second && want[first] {
					t.Fatal("second event arrived before the first")
				}
				delete(want, event.ContentID)
			}
		case <-ctx.Done():
			t.Fatalf("events did not arrive with recovery disabled: %v (%v)", want, ctx.Err())
		}
	}
}
