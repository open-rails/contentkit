package notification_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/helpers/notify"
)

func TestScopedNotificationsRequireTenantRecipient(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	options := notification.Options{Pool: pool, Schema: schema, Tenant: "catalog", AppID: uuid.NewString()}
	input := func(id, event string) notify.Input {
		return notify.Input{Recipient: notify.Recipient{Audience: notify.AudienceUser, ID: id}, Origin: notify.Origin{System: "host", Scope: "catalog", ID: event}, Kind: "test", CreatedAt: time.Now()}
	}
	store, err := notification.New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(ctx, input("member", "missing-resolver")); !errors.Is(err, auth.ErrUnavailable) {
		t.Fatalf("missing scoped lookup: %v", err)
	}
	outage := errors.New("directory unavailable")
	options.AccountExists = func(_ context.Context, id string) (bool, error) {
		if id == "outage" {
			return false, outage
		}
		return id == "member", nil
	}
	withoutTx, err := notification.New(options)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withoutTx.PublishTx(ctx, tx, input("member", "missing-tx-resolver")); !errors.Is(err, auth.ErrUnavailable) {
		t.Fatalf("held transaction fell back to a pool lookup: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	options.AccountExistsTx = func(ctx context.Context, _ pgx.Tx, id string) (bool, error) { return options.AccountExists(ctx, id) }
	store, err = notification.New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(ctx, input("member", "valid")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"foreign", "unknown"} {
		if _, err := store.Publish(ctx, input(id, "rejected")); !errors.Is(err, notify.ErrInvalid) {
			t.Fatalf("recipient %s: %v", id, err)
		}
	}
	if _, err := store.Publish(ctx, input("outage", "failed")); !errors.Is(err, outage) || !errors.Is(err, auth.ErrUnavailable) {
		t.Fatalf("lookup outage lost its cause: %v", err)
	}

	// Rejection precedes writes even if the native caller commits its transaction.
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishTx(ctx, tx, input("foreign", "transaction")); !errors.Is(err, notify.ErrInvalid) {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishBatch(ctx, []notify.Input{input("member", "batch-valid"), input("foreign", "batch-invalid")}); !errors.Is(err, notify.ErrInvalid) {
		t.Fatalf("batch did not fail: %v", err)
	}
	page, err := store.List(ctx, input("member", "").Recipient, notify.ListOptions{Limit: 100})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("failed batch partially committed: %+v %v", page, err)
	}

	handler, err := store.Handler(testauth.Auth{Authz: testauth.All{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		recipient string
		want      int
	}{{"member", 200}, {"foreign", 400}, {"unknown", 400}, {"outage", 503}} {
		body, err := json.Marshal(notification.Publication{Notifications: []notify.Input{input(tc.recipient, "http-"+tc.recipient)}})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/publish", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer app:publisher")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("HTTP recipient %s: %d %s", tc.recipient, rec.Code, rec.Body)
		}
	}
	var invalidRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+schema+`.notify_items WHERE recipient <> 'member'`).Scan(&invalidRows); err != nil || invalidRows != 0 {
		t.Fatalf("nonmember receipt persisted: %d %v", invalidRows, err)
	}
	if err := store.EraseRecipient(ctx, input("member", "").Recipient); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(ctx, input("member", "after-erasure")); !errors.Is(err, notify.ErrRecipientErased) {
		t.Fatalf("membership check replaced the durable erasure fence: %v", err)
	}
}

type accountAliasAuth struct{ testauth.Auth }

func (accountAliasAuth) ResolveActor(context.Context, auth.Verified) (string, error) {
	return "source", nil
}
func (accountAliasAuth) Authorize(context.Context, auth.Verified, string, string) (bool, error) {
	return true, nil
}
func (accountAliasAuth) ResolveTenantAccount(context.Context, auth.Verified, string) (string, bool, error) {
	return "member", true, nil
}

func TestNotificationSelfUsesResolvedAccountID(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	store, err := notification.New(notification.Options{Pool: pool, Schema: schema, Tenant: "catalog", AppID: uuid.NewString(), AccountExists: func(_ context.Context, id string) (bool, error) { return id == "member", nil }})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Publish(ctx, notify.Input{Recipient: notify.Recipient{Audience: notify.AudienceUser, ID: "member"}, Origin: notify.Origin{System: "host", Scope: "catalog", ID: "one"}, Kind: "test", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := store.Handler(accountAliasAuth{})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []struct{ method, path string }{{"GET", "/inbox"}, {"PUT", "/" + item.ID + "/read"}, {"PUT", "/" + item.ID + "/dismiss"}} {
		req := httptest.NewRequest(action.method, action.path, nil)
		req.Header.Set("Authorization", "Bearer user:source")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("resolved account %s: %d %s", action.path, rec.Code, rec.Body)
		}
		if action.method == "GET" {
			var page notify.Page
			if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Items) != 1 || page.Items[0].ID != item.ID {
				t.Fatalf("source identity used as recipient: %s %v", rec.Body, err)
			}
		}
	}
}
