package notification_test

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/helpers/notify"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSharedInboxKeepsSiteVisibilityAndSharedReceipts(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	shared := &notification.Config{Schema: schema, Namespace: "accounts"}
	makeSite := func(tenant string) *notification.Store {
		t.Helper()
		s, err := notification.New(notification.Options{Pool: pool, Schema: schema, Tenant: tenant, Shared: shared})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := makeSite("a"), makeSite("b")
	billing := notify.OriginScope{System: "openrails", Scope: "merchant"}
	if err := a.AllowOrigin(billing); err != nil {
		t.Fatal(err)
	}
	if err := b.AllowOrigin(billing); err != nil {
		t.Fatal(err)
	}
	user := notify.Recipient{Audience: notify.AudienceUser, ID: "alice"}
	publish := func(s *notification.Store, origin notify.Origin) notify.Item {
		t.Helper()
		n, err := s.Publish(ctx, notify.Input{Recipient: user, Origin: origin, Kind: "test", CreatedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	an := publish(a, notify.Origin{System: "contentkit", Scope: "a", ID: "comment"})
	bn := publish(b, notify.Origin{System: "contentkit", Scope: "b", ID: "comment"})
	common := publish(a, notify.Origin{System: "openrails", Scope: "merchant", ID: "billing"})
	for _, site := range []struct {
		s          *notification.Store
		own, other notify.Item
	}{{a, an, bn}, {b, bn, an}} {
		page, err := site.s.List(ctx, user, notify.ListOptions{Limit: 1})
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != common.ID || page.NextCursor == "" {
			t.Fatalf("first page %+v %v", page, err)
		}
		next, err := site.s.List(ctx, user, notify.ListOptions{Limit: 1, Cursor: page.NextCursor})
		if err != nil || len(next.Items) != 1 || next.Items[0].ID != site.own.ID || next.NextCursor != "" {
			t.Fatalf("next page %+v %v", next, err)
		}
		if _, err := site.s.SetRead(ctx, user, site.other.ID, true); !errors.Is(err, notify.ErrNotFound) {
			t.Fatalf("other site writable %v", err)
		}
		h, err := site.s.Handler(testauth.Auth{})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPut, "/"+site.other.ID+"/dismiss", nil)
		req.Header.Set("Authorization", "Bearer user:alice")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Fatalf("other site HTTP writable %d", rec.Code)
		}
	}
	if _, err := a.SetRead(ctx, user, common.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SetDismissed(ctx, user, common.ID, true); err != nil {
		t.Fatal(err)
	}
	copied, err := b.Get(ctx, user, common.ID)
	if err != nil || copied.ReadAt == nil || copied.DismissedAt == nil {
		t.Fatalf("receipt state not shared %+v %v", copied, err)
	}
	page, err := a.List(ctx, user, notify.ListOptions{UnreadOnly: true})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != an.ID {
		t.Fatalf("unread leaks %+v %v", page, err)
	}
	if err := a.EraseRecipient(ctx, user); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish(ctx, bn.Input); !errors.Is(err, notify.ErrRecipientErased) {
		t.Fatalf("erasure not shared %v", err)
	}
}

func TestSharedSchemaConcurrentStartup(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	schema := "notify_start_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- notification.Migrate(t.Context(), pool, schema) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	s, err := notification.New(notification.Options{Pool: pool, Schema: schema, Tenant: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(t.Context(), notify.Input{Recipient: notify.Recipient{Audience: notify.AudienceUser, ID: "alice"}, Origin: notify.Origin{System: "test", Scope: "test", ID: "one"}, Kind: "test", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}
