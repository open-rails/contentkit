package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/helpers/notify"
)

type eventServer struct {
	mu     sync.Mutex
	events []Event
	calls  int
}

func (e *eventServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if r.Method != http.MethodGet || r.URL.Path != "/v1/app/events" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	offset := 0
	if after := r.URL.Query().Get("after"); after != "" {
		var err error
		offset, err = strconv.Atoi(strings.TrimPrefix(after, "c"))
		if err != nil || offset < 0 || offset > len(e.events) {
			w.WriteHeader(410)
			return
		}
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	end := min(offset+limit, len(e.events))
	data := append([]Event{}, e.events[offset:end]...)
	_ = json.NewEncoder(w).Encode(EventPage{Events: data, NextCursor: fmt.Sprint("c", end), HasMore: end < len(e.events), RetentionSeconds: 90 * 86400})
}
func event(id, customer string) Event {
	data, _ := json.Marshal(map[string]any{"amount": "19.95", "account_id": customer, "customer_id": customer, "account_issuer": ""})
	return Event{ID: id, MerchantID: "m", Source: "notification", Version: 1, Subject: EventSubject{Type: "customer", ID: customer}, EventType: "payment_method_failed", Data: data, CreatedAt: time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)}
}
func mapped(_ context.Context, e Event) (string, bool, error) {
	return "local-" + e.CustomerID, true, nil
}

func inbox(t *testing.T, pool *pgxpool.Pool, schema, namespace string) *notification.Store {
	t.Helper()
	s, err := notification.New(notification.Options{Pool: pool, Schema: schema, Tenant: namespace})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func testStorage(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool := pgtest.Pool(t, func(c *pgxpool.Config) { c.MaxConns = 1 })
	schema := pgtest.EmptySchema(t, t.Context(), pool)
	if err := notification.Migrate(t.Context(), pool, schema); err != nil {
		t.Fatal(err)
	}
	return pool, schema
}
func sourceFor(t *testing.T, state *eventServer) *Source {
	t.Helper()
	server := httptest.NewServer(state)
	t.Cleanup(server.Close)
	source, err := New(Options{URL: server.URL, Name: "billing", MerchantID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestConsumeAtomicPageIndependentInboxesAndReplay(t *testing.T) {
	ctx := t.Context()
	pool, schema := testStorage(t)
	a, b := inbox(t, pool, schema, "one"), inbox(t, pool, schema, "two")
	state := &eventServer{events: []Event{event("first", "c"), event("second", "c")}}
	source := sourceFor(t, state)
	badMapper := func(ctx context.Context, e Event) (string, bool, error) {
		if e.ID == "second" {
			return "", false, errors.New("directory unavailable")
		}
		return mapped(ctx, e)
	}
	result, err := source.Consume(ctx, a, badMapper, ConsumeOptions{})
	if err == nil || result.Delivered != 0 {
		t.Fatalf("partial page committed: %+v %v", result, err)
	}
	cursor, err := a.EventCursor(ctx, source.Origin())
	if err != nil || cursor != "" {
		t.Fatalf("failure advanced cursor: %q %v", cursor, err)
	}
	recipient := notify.Recipient{Audience: notify.AudienceUser, ID: "local-c"}
	page, err := a.List(ctx, recipient, notify.ListOptions{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("failure left notices: %+v %v", page, err)
	}
	result, err = source.Consume(ctx, a, mapped, ConsumeOptions{})
	if err != nil || result.Delivered != 2 {
		t.Fatalf("retry: %+v %v", result, err)
	}
	page, err = a.List(ctx, recipient, notify.ListOptions{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("inbox: %+v %v", page, err)
	}
	first := page.Items[0]
	if !first.ActionRequested || first.Origin.Subject != "c" || first.Visibility != notify.VisibilityTenant {
		t.Fatalf("lost fact: %+v", first)
	}
	if _, err = a.SetRead(ctx, recipient, first.ID, true); err != nil {
		t.Fatal(err)
	}
	result, err = source.Consume(ctx, b, mapped, ConsumeOptions{})
	if err != nil || result.Delivered != 2 {
		t.Fatalf("independent reader: %+v %v", result, err)
	}
	result, err = source.Consume(ctx, a, mapped, ConsumeOptions{})
	if err != nil || result.Fetched != 0 {
		t.Fatalf("shared replica reread: %+v %v", result, err)
	}
	// Simulate restoring an earlier reader cursor: receipts remain idempotent.
	_, err = pool.Exec(ctx, "UPDATE "+pgxIdentifier(schema)+".notification_event_cursors SET cursor='' WHERE namespace='one'")
	if err != nil {
		t.Fatal(err)
	}
	result, err = source.Consume(ctx, a, mapped, ConsumeOptions{})
	if err != nil || result.Delivered != 2 {
		t.Fatalf("replay: %+v %v", result, err)
	}
	page, err = a.List(ctx, recipient, notify.ListOptions{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("replay duplicated notices: %+v %v", page, err)
	}
	for _, item := range page.Items {
		if item.ID == first.ID && item.ReadAt == nil {
			t.Fatal("replay reset read state")
		}
	}
	// Event time never determines progress: a late fact can have an old time.
	state.mu.Lock()
	late := event("late", "c")
	late.CreatedAt = late.CreatedAt.Add(-time.Hour)
	state.events = append(state.events, late)
	state.mu.Unlock()
	result, err = source.Consume(ctx, a, mapped, ConsumeOptions{})
	if err != nil || result.Delivered != 1 {
		t.Fatalf("late fact skipped: %+v %v", result, err)
	}
}

func TestConsumeSkipsUnrelatedFactsAndKeepsExpiredCursor(t *testing.T) {
	ctx := t.Context()
	pool, schema := testStorage(t)
	store := inbox(t, pool, schema, "one")
	host := event("host", "c")
	host.Source = "host"
	optional := event("optional", "c")
	optional.EventType = "premium_renewed"
	future := event("future", "c")
	future.EventType = "future_kind"
	future.Data = json.RawMessage(`{"customer_id":"c","account_id":"c","account_issuer":"","downgrade_applied":"future-kind-value"}`)
	state := &eventServer{events: []Event{host, optional, future, event("wanted", "c")}}
	source := sourceFor(t, state)
	result, err := source.Consume(ctx, store, mapped, ConsumeOptions{Filter: BillingLifecycle})
	if err != nil || result.Fetched != 4 || result.Skipped != 3 || result.Delivered != 1 {
		t.Fatalf("skip: %+v %v", result, err)
	}
	if cursor, err := store.EventCursor(ctx, source.Origin()); err != nil || cursor != "c4" {
		t.Fatalf("unrelated future kind blocked progress: %q %v", cursor, err)
	}
	_, err = pool.Exec(ctx, "UPDATE "+pgxIdentifier(schema)+".notification_event_cursors SET cursor='c999'")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Consume(ctx, store, mapped, ConsumeOptions{}); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("expiry hidden: %v", err)
	}
	cursor, err := store.EventCursor(ctx, source.Origin())
	if err != nil || cursor != "c999" {
		t.Fatalf("expiry reset progress: %q %v", cursor, err)
	}
}

func pgxIdentifier(value string) string { return pgx.Identifier{value}.Sanitize() }

func TestCursorCommitSerializesIndependentReplicas(t *testing.T) {
	ctx := t.Context()
	pool, schema := testStorage(t)
	other, err := pgxpool.NewWithConfig(ctx, pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	stores := []*notification.Store{inbox(t, pool, schema, "shared"), inbox(t, other, schema, "shared")}
	origin := notify.OriginScope{System: "billing", Scope: "m"}
	start, results := make(chan struct{}), make(chan error, 2)
	for i, store := range stores {
		go func() {
			<-start
			input := notify.Input{Recipient: notify.Recipient{Audience: notify.AudienceUser, ID: "viewer"},
				Origin: notify.Origin{System: origin.System, Scope: origin.Scope, ID: fmt.Sprint(i)},
				Kind:   "payment_method_failed", Data: json.RawMessage(`{}`), CreatedAt: time.Now(), Visibility: notify.VisibilityTenant}
			_, err := store.ImportEvents(ctx, origin, "", "next", []notify.Input{input})
			results <- err
		}()
	}
	close(start)
	won, stale := 0, 0
	for range stores {
		err := <-results
		switch {
		case err == nil:
			won++
		case errors.Is(err, notification.ErrEventCursorChanged):
			stale++
		default:
			t.Fatal(err)
		}
	}
	if won != 1 || stale != 1 {
		t.Fatalf("committed=%d stale=%d", won, stale)
	}
	page, err := stores[0].List(ctx, notify.Recipient{Audience: notify.AudienceUser, ID: "viewer"}, notify.ListOptions{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("stale replica wrote notice: %+v %v", page, err)
	}
	_, err = stores[0].ImportEvents(ctx, origin, "next", "later", []notify.Input{{Origin: notify.Origin{System: "foreign", Scope: "m"}}})
	if err == nil {
		t.Fatal("foreign source accepted")
	}
}
