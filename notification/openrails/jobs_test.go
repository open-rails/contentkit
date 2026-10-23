package openrails

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/helpers/notify"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
)

type jobBackend struct {
	events   []Event
	reads    int
	pageSize int
}

func (b *jobBackend) ListEvents(_ context.Context, after string, _ int) (EventPage, error) {
	b.reads++
	if b.pageSize > 0 {
		offset := 0
		if after != "" {
			var err error
			offset, err = strconv.Atoi(after)
			if err != nil {
				return EventPage{}, err
			}
		}
		end := min(offset+b.pageSize, len(b.events))
		return EventPage{Events: append([]Event{}, b.events[offset:end]...), NextCursor: strconv.Itoa(end), HasMore: end < len(b.events), RetentionSeconds: 90 * 86400}, nil
	}
	events := b.events
	if after == "done" {
		events = []Event{}
	}
	return EventPage{Events: events, NextCursor: "done", RetentionSeconds: 90 * 86400}, nil
}

type pageInbox struct {
	cursor string
	inputs []notify.Input
}

func (s *pageInbox) EventCursor(context.Context, notify.OriginScope) (string, error) {
	return s.cursor, nil
}
func (s *pageInbox) ImportEvents(_ context.Context, _ notify.OriginScope, after, next string, inputs []notify.Input) (notification.EventImportResult, error) {
	if after != s.cursor {
		return notification.EventImportResult{}, notification.ErrEventCursorChanged
	}
	s.inputs = append(s.inputs, inputs...)
	s.cursor = next
	return notification.EventImportResult{Published: len(inputs)}, nil
}

func TestJobDrainsBacklogWithBoundedWork(t *testing.T) {
	for _, count := range []int{7, 25} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			backend := &jobBackend{pageSize: 1}
			for i := range count {
				backend.events = append(backend.events, event(fmt.Sprint("notice-", i), "c"))
			}
			source, err := New(Options{Name: "billing", MerchantID: "m", Backend: backend})
			if err != nil {
				t.Fatal(err)
			}
			sink, jobs := &pageInbox{}, &Jobs{}
			if _, err := jobs.Add(source, sink, mapped, "accounts"); err != nil {
				t.Fatal(err)
			}
			worker := &billingWorker{jobs: jobs}
			for key := range jobs.sources {
				if err := worker.Work(t.Context(), &river.Job[billingArgs]{Args: billingArgs{Key: key}}); err != nil {
					t.Fatal(err)
				}
				if got, want := len(sink.inputs), min(count, 10); got != want {
					t.Fatalf("first run imported %d, want %d", got, want)
				}
				for len(sink.inputs) < count {
					if err := worker.Work(t.Context(), &river.Job[billingArgs]{Args: billingArgs{Key: key}}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if backend.reads != count || len(sink.inputs) != count || sink.cursor != strconv.Itoa(count) {
				t.Fatalf("backlog replay: reads=%d imported=%d cursor=%s", backend.reads, len(sink.inputs), sink.cursor)
			}
		})
	}
}

func TestJobsComposeMultipleSourcesAndDispatchByKey(t *testing.T) {
	ctx := t.Context()
	pool, schema := testStorage(t)
	destination := inbox(t, pool, schema, "accounts")
	jobs := &Jobs{}
	backends := []*jobBackend{{events: []Event{event("one", "c")}}, {events: []Event{event("two", "c")}}}
	contributions := []riverhelpers.Contribution{}
	for i, b := range backends {
		s, err := New(Options{Name: []string{"one", "two"}[i], MerchantID: "m", Backend: b})
		if err != nil {
			t.Fatal(err)
		}
		contribution, err := jobs.Add(s, destination, mapped, "accounts")
		if err != nil {
			t.Fatal(err)
		}
		contributions = append(contributions, contribution)
	}
	if _, err := riverhelpers.New(ctx, pool, &river.Config{}, contributions...); err != nil {
		t.Fatalf("sources cannot compose: %v", err)
	}
	for _, b := range backends {
		if b.reads != 0 {
			t.Fatal("construction read source")
		}
	}
	worker := &billingWorker{jobs: jobs}
	for key := range jobs.sources {
		if err := worker.Work(ctx, &river.Job[billingArgs]{Args: billingArgs{Key: key}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range backends {
		if b.reads != 1 {
			t.Fatalf("wrong source: %+v", b)
		}
	}
	if err := worker.Work(ctx, &river.Job[billingArgs]{Args: billingArgs{Key: "other"}}); err == nil {
		t.Fatal("unconfigured source accepted")
	}
}

func TestPollJobsCoalesceAcrossTicksAndRetryAfterFailure(t *testing.T) {
	ctx := t.Context()
	pool, schema := testStorage(t)
	if err := riverhelpers.ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	source, err := New(Options{Name: "billing", MerchantID: "m", Backend: &jobBackend{}})
	if err != nil {
		t.Fatal(err)
	}
	jobs := &Jobs{}
	contribution, err := jobs.Add(source, inbox(t, pool, schema, "accounts"), mapped, "accounts")
	if err != nil {
		t.Fatal(err)
	}
	client, err := riverhelpers.New(ctx, pool, &river.Config{Schema: schema}, contribution)
	if err != nil {
		t.Fatal(err)
	}
	for key := range jobs.sources {
		args := billingArgs{Key: key}
		first, err := client.Insert(ctx, args, &river.InsertOpts{ScheduledAt: time.Now().Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		second, err := client.Insert(ctx, args, &river.InsertOpts{ScheduledAt: time.Now().Add(2 * time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if first.Job.ID != second.Job.ID || !second.UniqueSkippedAsDuplicate || first.Job.MaxAttempts != 1 {
			t.Fatalf("periodic ticks accumulated retries: first=%+v second=%+v", first, second)
		}
		if _, err := pool.Exec(ctx, "UPDATE "+pgxIdentifier(schema)+".river_job SET state='discarded',finalized_at=clock_timestamp() WHERE id=$1", first.Job.ID); err != nil {
			t.Fatal(err)
		}
		next, err := client.Insert(ctx, args, nil)
		if err != nil {
			t.Fatal(err)
		}
		if next.Job.ID == first.Job.ID || next.UniqueSkippedAsDuplicate {
			t.Fatal("a failed poll prevented the next periodic retry")
		}
	}
}
