package openrails

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/helpers/notify"
)

func TestConsumeSkipsAbsentAndErasedAccountsButRollsBackLookupOutage(t *testing.T) {
	ctx := t.Context()
	pool, schema := testStorage(t)
	failed := true
	outage := errors.New("account directory offline")
	store, err := notification.New(notification.Options{Pool: pool, Schema: schema, Tenant: "site", AppID: uuid.NewString(),
		AccountExistsTx: func(_ context.Context, _ pgx.Tx, id string) (bool, error) {
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
	state := &eventServer{events: []Event{event("member-event", "member"), event("absent-event", "absent"), event("erased-event", "erased"), event("outage-event", "outage")}}
	source := sourceFor(t, state)
	mapper := func(_ context.Context, e Event) (string, bool, error) { return e.CustomerID, true, nil }
	result, err := source.Consume(ctx, store, mapper, ConsumeOptions{})
	if !errors.Is(err, outage) || result.Delivered != 0 {
		t.Fatalf("first consume %+v %v", result, err)
	}
	cursor, err := store.EventCursor(ctx, source.Origin())
	if err != nil || cursor != "" {
		t.Fatalf("failed transaction advanced %q %v", cursor, err)
	}
	page, err := store.List(ctx, notify.Recipient{Audience: notify.AudienceUser, ID: "member"}, notify.ListOptions{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("page not rolled back %+v %v", page, err)
	}
	failed = false
	result, err = source.Consume(ctx, store, mapper, ConsumeOptions{})
	if err != nil || result.Delivered != 2 || result.Skipped != 2 {
		t.Fatalf("retry %+v %v", result, err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+pgxIdentifier(schema)+".notify_recipients WHERE recipient='absent'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("absent account created receipt: %d %v", count, err)
	}
	// A stale replica cannot roll the destination cursor back.
	_, err = store.ImportEvents(ctx, source.Origin(), "", "old", nil)
	if !errors.Is(err, notification.ErrEventCursorChanged) {
		t.Fatalf("stale cursor accepted: %v", err)
	}
}
