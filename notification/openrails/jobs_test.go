package openrails

import (
	"context"
	"testing"

	"github.com/open-rails/contentkit/internal/pgtest"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
)

type jobBackend struct {
	events            []Event
	registered, acked int
}

func (b *jobBackend) Register(context.Context) error                { b.registered++; return nil }
func (b *jobBackend) Retire(context.Context) error                  { return nil }
func (b *jobBackend) Pending(context.Context, int) ([]Event, error) { return b.events, nil }
func (b *jobBackend) Acknowledge(_ context.Context, ids []string) error {
	b.acked += len(ids)
	return nil
}

func TestJobsComposeMultipleSourcesAndDispatchByKey(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	jobs := &Jobs{}
	backends := []*jobBackend{{events: []Event{event("one", "c")}}, {events: []Event{event("two", "c")}}}
	contributions := []riverhelpers.Contribution{}
	for i, b := range backends {
		b.events[0].EventType = "payment_method_failed"
		s, err := New(Options{Name: []string{"one", "two"}[i], MerchantID: "m", Consumer: "accounts", Backend: b})
		if err != nil {
			t.Fatal(err)
		}
		contribution, err := jobs.Add(s, &capturePublisher{}, mapped, "accounts")
		if err != nil {
			t.Fatal(err)
		}
		contributions = append(contributions, contribution)
	}
	if _, err := riverhelpers.New(ctx, pool, &river.Config{}, contributions...); err != nil {
		t.Fatalf("multiple sources could not compose: %v", err)
	}
	for _, b := range backends {
		if b.registered != 1 {
			t.Fatalf("registration count %d", b.registered)
		}
	}
	worker := &billingWorker{jobs: jobs}
	for key := range jobs.sources {
		if err := worker.Work(ctx, &river.Job[billingArgs]{Args: billingArgs{Key: key}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range backends {
		if b.acked != 1 {
			t.Fatalf("wrong source dispatched: %+v", b)
		}
	}
	if err := worker.Work(ctx, &river.Job[billingArgs]{Args: billingArgs{Key: "other"}}); err == nil {
		t.Fatal("unconfigured source accepted")
	}
}
