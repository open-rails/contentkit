package notification_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/helpers/notify"
)

func TestAppVisibilityThroughInboxAndReceiptRoutes(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	shared := &notification.Config{Schema: schema, Namespace: "accounts"}
	makeApp := func(key string) *notification.Store {
		t.Helper()
		s, err := notification.New(notification.Options{Pool: pool, Schema: schema, Tenant: key, AppID: uuid.NewString(), Shared: shared})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := makeApp("a"), makeApp("b")
	user := notify.Recipient{Audience: notify.AudienceUser, ID: "alice"}
	publish := func(s *notification.Store, id string, visibility notify.Visibility) notify.Item {
		t.Helper()
		n, err := s.Publish(ctx, notify.Input{Recipient: user, Origin: notify.Origin{System: "any-producer", Scope: "shared-provenance", ID: id}, Kind: "test", CreatedAt: time.Now(), Visibility: visibility})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	own := publish(a, "app-a", "")
	other := publish(b, "app-b", "")
	common := publish(a, "tenant", notify.VisibilityTenant)
	if own.Visibility != notify.VisibilityApp || own.AppID != a.AppID() {
		t.Fatalf("native publication not stamped: %+v", own)
	}
	handler, err := a.Handler(testauth.Auth{Authz: testauth.All{}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var payload bytes.Buffer
		if body != nil {
			if err := json.NewEncoder(&payload).Encode(body); err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest(method, path, &payload)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	rec := call("GET", "/inbox?app_id="+b.AppID(), "user:alice", nil)
	var page notify.Page
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || rec.Code != 200 || len(page.Items) != 2 {
		t.Fatalf("list %d %s %v", rec.Code, rec.Body, err)
	}
	for _, item := range page.Items {
		if item.ID != own.ID && item.ID != common.ID {
			t.Fatalf("foreign app visible: %+v", item)
		}
	}
	for _, path := range []string{"/" + other.ID + "/read", "/" + other.ID + "/dismiss"} {
		if rec := call("PUT", path, "user:alice", nil); rec.Code != 404 {
			t.Fatalf("foreign mutation %s: %d %s", path, rec.Code, rec.Body)
		}
	}
	if rec := call("PUT", "/"+common.ID+"/read", "user:alice", nil); rec.Code != 200 {
		t.Fatalf("shared read %d %s", rec.Code, rec.Body)
	}
	if got, err := b.Get(ctx, user, common.ID); err != nil || got.ReadAt == nil {
		t.Fatalf("shared receipt %+v %v", got, err)
	}
	for _, in := range []notify.Input{
		{Recipient: user, Origin: notify.Origin{System: "host", Scope: "app", ID: "wide"}, Kind: "test", CreatedAt: time.Now(), Visibility: notify.VisibilityTenant},
		{Recipient: user, Origin: notify.Origin{System: "host", Scope: "app", ID: "other"}, Kind: "test", CreatedAt: time.Now(), Visibility: notify.VisibilityApp, AppID: b.AppID()},
	} {
		if rec := call("POST", "/publish", "app:host", notification.Publication{Notifications: []notify.Input{in}}); rec.Code != http.StatusBadRequest {
			t.Fatalf("app widened publication: %d %s", rec.Code, rec.Body)
		}
	}
}
