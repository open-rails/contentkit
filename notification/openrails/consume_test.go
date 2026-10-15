package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/helpers/notify"
)

type eventServer struct {
	mu       sync.Mutex
	events   []Event
	ack      map[string]map[string]bool
	failAck  bool
	ackCheck func([]string)
}

func (e *eventServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r.Method != http.MethodGet && r.Header.Get("Idempotency-Key") == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	path := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/app/notification-consumers/"), "/")
	name := path[0]
	if len(path) == 1 && r.Method == http.MethodPost {
		if e.ack[name] == nil {
			e.ack[name] = map[string]bool{}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if e.ack[name] == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if len(path) == 1 && r.Method == http.MethodDelete {
		delete(e.ack, name)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(path) != 2 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if path[1] == "acknowledge" {
		var in struct {
			IDs []string `json:"notification_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			w.WriteHeader(400)
			return
		}
		if e.ackCheck != nil {
			e.ackCheck(in.IDs)
		}
		if e.failAck {
			e.failAck = false
			w.WriteHeader(503)
			return
		}
		for _, id := range in.IDs {
			e.ack[name][id] = true
		}
		w.WriteHeader(204)
		return
	}
	data := []Event{}
	for _, event := range e.events {
		if !e.ack[name][event.ID] {
			data = append(data, event)
		}
	}
	_ = json.NewEncoder(w).Encode(eventPage{Data: data})
}
func event(id, customer string) Event {
	return Event{ID: id, MerchantID: "m", CustomerID: customer, EventType: "future.kind", Data: json.RawMessage(`{"amount":"19.95"}`), CreatedAt: time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC), ActionRequested: true}
}
func mapped(_ context.Context, event Event) (string, bool, error) {
	return "local-" + event.CustomerID, true, nil
}

type interruptedPublisher struct {
	target notify.Publisher
	failID string
}

func (p *interruptedPublisher) Publish(ctx context.Context, in notify.Input) (notify.Item, error) {
	if p.failID == in.Origin.ID {
		return notify.Item{}, errors.New("transient destination failure")
	}
	return p.target.Publish(ctx, in)
}

func TestConsumeCommitsBeforeAckAndKeepsIndependentConsumers(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	store, err := notification.New(notification.Options{Pool: pool, Schema: schema, Tenant: "site"})
	if err != nil {
		t.Fatal(err)
	}
	optional := event("optional", "c")
	optional.Optional = true
	sourceState := &eventServer{events: []Event{event("first", "c"), event("fails", "c"), optional, event("last", "c")}, ack: map[string]map[string]bool{}}
	server := httptest.NewServer(sourceState)
	defer server.Close()
	a, _ := New(Options{URL: server.URL, Name: "openrails:prod", MerchantID: "m", Consumer: "a"})
	b, _ := New(Options{URL: server.URL, Name: "openrails:prod", MerchantID: "m", Consumer: "b"})
	if err := a.Register(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Register(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Register(ctx); err != nil {
		t.Fatal(err)
	}
	recipient := notify.Recipient{Audience: notify.AudienceUser, ID: "local-c"}
	sourceState.ackCheck = func(ids []string) {
		page, err := store.List(ctx, recipient, notify.ListOptions{})
		if err != nil {
			t.Error(err)
			return
		}
		seen := map[string]bool{}
		for _, n := range page.Items {
			seen[n.Origin.ID] = true
		}
		for _, id := range ids {
			if id != "optional" && !seen[id] {
				t.Errorf("ACK preceded destination commit: %s", id)
			}
		}
	}
	sink := &interruptedPublisher{target: store, failID: "fails"}
	result, err := a.Consume(ctx, sink, mapped, ConsumeOptions{})
	if err == nil || result.Delivered != 2 || result.Skipped != 1 || result.Acknowledged != 3 {
		t.Fatalf("partial %+v %v", result, err)
	}
	remaining, err := a.pending(ctx, 100)
	if err != nil || len(remaining) != 1 || remaining[0].ID != "fails" {
		t.Fatalf("pending %+v %v", remaining, err)
	}
	untouched, err := b.pending(ctx, 100)
	if err != nil || len(untouched) != 4 {
		t.Fatalf("consumer b lost events %+v %v", untouched, err)
	}
	page, err := store.List(ctx, recipient, notify.ListOptions{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("inbox %+v %v", page, err)
	}
	first := page.Items[0]
	if first.Status != notify.StatusNotice || !first.ActionRequested || first.Origin.Subject != "c" || first.Visibility != notify.VisibilityTenant || first.AppID != "" {
		t.Fatalf("historical event became workflow: %+v", first)
	}
	if _, err := store.SetRead(ctx, recipient, first.ID, true); err != nil {
		t.Fatal(err)
	}
	sink.failID = ""
	sourceState.failAck = true
	result, err = a.Consume(ctx, sink, mapped, ConsumeOptions{})
	if err == nil || result.Delivered != 1 || result.Acknowledged != 0 {
		t.Fatalf("ACK failure %+v %v", result, err)
	}
	result, err = a.Consume(ctx, sink, mapped, ConsumeOptions{})
	if err != nil || result.Delivered != 1 || result.Acknowledged != 1 {
		t.Fatalf("replay %+v %v", result, err)
	}
	page, err = store.List(ctx, recipient, notify.ListOptions{})
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("duplicate publication %+v %v", page, err)
	}
	for _, item := range page.Items {
		if item.ID == first.ID && item.ReadAt == nil {
			t.Fatal("replay reset read state")
		}
	}
	// A late event with an older timestamp is still pending: no time watermark.
	sourceState.mu.Lock()
	late := event("late", "c")
	late.CreatedAt = late.CreatedAt.Add(-time.Hour)
	sourceState.events = append(sourceState.events, late)
	sourceState.mu.Unlock()
	result, err = a.Consume(ctx, sink, mapped, ConsumeOptions{})
	if err != nil || result.Delivered != 1 {
		t.Fatalf("late event %+v %v", result, err)
	}
}

func TestConsumeOptionalMappingAndRecipientErasure(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	store, err := notification.New(notification.Options{Pool: pool, Schema: schema, Tenant: "site"})
	if err != nil {
		t.Fatal(err)
	}
	optional := event("optional", "active")
	optional.Optional = true
	state := &eventServer{events: []Event{optional, event("unlinked", "unlinked"), event("erased", "gone")}, ack: map[string]map[string]bool{}}
	server := httptest.NewServer(state)
	defer server.Close()
	source, _ := New(Options{URL: server.URL, MerchantID: "m", Consumer: "site"})
	if err := source.Register(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.EraseRecipient(ctx, notify.Recipient{Audience: notify.AudienceUser, ID: "local-gone"}); err != nil {
		t.Fatal(err)
	}
	mapper := func(ctx context.Context, e Event) (string, bool, error) {
		if e.CustomerID == "unlinked" {
			return "", false, nil
		}
		return mapped(ctx, e)
	}
	result, err := source.Consume(ctx, store, mapper, ConsumeOptions{IncludeOptional: true})
	if err != nil || result.Delivered != 1 || result.Skipped != 2 || result.Acknowledged != 3 {
		t.Fatalf("consume %+v %v", result, err)
	}
	if err := source.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Consume(ctx, store, mapper, ConsumeOptions{}); err == nil {
		t.Fatal("retired consumer still polled")
	}
}

func TestConsumeMappingFailuresDoNotAcknowledge(t *testing.T) {
	state := &eventServer{events: []Event{event("failed", "c")}, ack: map[string]map[string]bool{}}
	server := httptest.NewServer(state)
	defer server.Close()
	source, _ := New(Options{URL: server.URL, MerchantID: "m", Consumer: "site"})
	ctx := context.Background()
	if err := source.Register(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := source.Consume(ctx, &capturePublisher{}, func(context.Context, Event) (string, bool, error) {
		return "", false, fmt.Errorf("account lookup unavailable")
	}, ConsumeOptions{})
	if err == nil {
		t.Fatal("mapping failure swallowed")
	}
	pending, err := source.pending(ctx, 100)
	if err != nil || len(pending) != 1 {
		t.Fatalf("failed mapping ACKed %+v %v", pending, err)
	}
}

type capturePublisher struct{}

func (*capturePublisher) Publish(_ context.Context, in notify.Input) (notify.Item, error) {
	return notify.Item{Input: in}, nil
}
