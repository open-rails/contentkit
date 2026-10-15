package openrails

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/helpers/notify"
)

func TestConsumeSkipsAbsentAndErasedAccountsButRetriesLookupOutage(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	failed := true
	outage := errors.New("account directory offline")
	store, err := notification.New(notification.Options{Pool: pool, Schema: schema, Tenant: "site", AppID: uuid.NewString(),
		AccountExists: func(_ context.Context, id string) (bool, error) {
			if id == "outage" && failed {
				return false, outage
			}
			return id != "absent", nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EraseRecipient(ctx, notify.Recipient{Audience: notify.AudienceUser, ID: "erased"}); err != nil {
		t.Fatal(err)
	}
	state := &eventServer{events: []Event{event("absent-event", "absent"), event("erased-event", "erased"), event("outage-event", "outage"), event("member-event", "member")}, ack: map[string]map[string]bool{}}
	server := httptest.NewServer(state)
	defer server.Close()
	source, err := New(Options{URL: server.URL, Name: "billing", MerchantID: "m", Consumer: "tenant-accounts"})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Register(ctx); err != nil {
		t.Fatal(err)
	}
	mapper := func(_ context.Context, e Event) (string, bool, error) { return e.CustomerID, true, nil }
	result, err := source.Consume(ctx, store, mapper, ConsumeOptions{})
	if !errors.Is(err, outage) || result != (ConsumeResult{Fetched: 4, Delivered: 1, Skipped: 2, Acknowledged: 3}) {
		t.Fatalf("first consume %+v %v", result, err)
	}
	if state.ack["tenant-accounts"]["outage-event"] {
		t.Fatal("transient lookup failure was acknowledged")
	}
	failed = false
	result, err = source.Consume(ctx, store, mapper, ConsumeOptions{})
	if err != nil || result != (ConsumeResult{Fetched: 1, Delivered: 1, Acknowledged: 1}) {
		t.Fatalf("retry %+v %v", result, err)
	}
	var absentRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+schema+`.notify_recipients WHERE recipient='absent'`).Scan(&absentRows); err != nil || absentRows != 0 {
		t.Fatalf("missing account created a receipt or tombstone: %d %v", absentRows, err)
	}
}
