package openrails

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/helpers/notify"
)

type interruptedPublisher struct {
	target notify.Publisher
	fail   bool
}

func (p *interruptedPublisher) Publish(ctx context.Context, in notify.Input) (notify.Item, error) {
	if p.fail && in.Origin.ID == "n2" {
		return notify.Item{}, errors.New("transient sink failure")
	}
	return p.target.Publish(ctx, in)
}

func TestSyncReplaysInterruptedPagesPreservingLocalState(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	store, err := notification.New(notification.Options{Pool: pool, Schema: schema, Tenant: "site"})
	if err != nil {
		t.Fatal(err)
	}
	sourceCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceCalls++
		if r.Method != http.MethodGet {
			t.Errorf("source mutation %s", r.Method)
		}
		fmt.Fprint(w, `{"data":[{"id":"n1","merchant_id":"m","customer_id":"customer","event_type":"payment.failed","action_requested":true,"data":{"amount":"19.95"},"created_at":"2026-10-11T00:00:00Z","seen":true},{"id":"n2","merchant_id":"m","customer_id":"customer","event_type":"future.kind","data":{"new":true},"created_at":"2026-10-11T01:00:00Z"}],"next_cursor":null}`)
	}))
	defer server.Close()
	source, _ := New(Options{URL: server.URL, Name: "openrails:billing-prod", MerchantID: "m"})
	binding := Binding{CustomerID: "customer", RecipientID: "user"}
	sink := &interruptedPublisher{target: store, fail: true}
	result, err := source.Sync(ctx, sink, binding, ScanOptions{})
	if err == nil || result.Complete || result.NextCursor != "" || result.Processed != 1 {
		t.Fatalf("interrupted scan %+v %v", result, err)
	}
	recipient := notify.Recipient{Audience: notify.AudienceUser, ID: "user"}
	page, err := store.List(ctx, recipient, notify.ListOptions{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("partial page %+v %v", page, err)
	}
	first := page.Items[0]
	if first.Status != notify.StatusNotice || !first.ActionRequested {
		t.Fatalf("historical action request became workflow or lost hint: %+v", first)
	}
	if first.ReadAt != nil {
		t.Fatal("source seen state copied")
	}
	if err := store.SetRead(ctx, recipient, first.ID, true); err != nil {
		t.Fatal(err)
	}
	sink.fail = false
	for range 2 {
		result, err = source.Sync(ctx, sink, binding, ScanOptions{})
		if err != nil || !result.Complete || result.Processed != 2 {
			t.Fatalf("rescan %+v %v", result, err)
		}
	}
	page, err = store.List(ctx, recipient, notify.ListOptions{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("dedup %+v %v", page, err)
	}
	for _, item := range page.Items {
		if item.Origin.System != "openrails:billing-prod" || item.Origin.Scope != "m" {
			t.Fatalf("provenance %+v", item)
		}
		if item.ID == first.ID && (item.Status != notify.StatusNotice || !item.ActionRequested) {
			t.Fatal("reading changed historical action request")
		}
		if item.ID == first.ID && item.ReadAt == nil {
			t.Fatal("rescan reset read state")
		}
	}
	if sourceCalls != 3 {
		t.Fatalf("calls %d", sourceCalls)
	}
}

func TestSyncBoundsAndResumesPages(t *testing.T) {
	ctx := context.Background()
	sink := &capturePublisher{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "next" {
			fmt.Fprint(w, `{"data":[],"next_cursor":null}`)
		} else {
			fmt.Fprint(w, `{"data":[],"next_cursor":"next"}`)
		}
	}))
	defer server.Close()
	source, _ := New(Options{URL: server.URL, MerchantID: "m"})
	binding := Binding{CustomerID: "c", RecipientID: "u"}
	first, err := source.Sync(ctx, sink, binding, ScanOptions{MaxPages: 1})
	if err != nil || first.Complete || first.NextCursor != "next" {
		t.Fatalf("bounded %+v %v", first, err)
	}
	last, err := source.Sync(ctx, sink, binding, ScanOptions{Cursor: first.NextCursor, MaxPages: 1})
	if err != nil || !last.Complete || last.NextCursor != "" {
		t.Fatalf("resume %+v %v", last, err)
	}
}

type capturePublisher struct{}

func (*capturePublisher) Publish(_ context.Context, in notify.Input) (notify.Item, error) {
	return notify.Item{Input: in}, nil
}
