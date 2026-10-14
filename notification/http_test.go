package notification_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/helpers/notify"
)

func TestHandlerUsesAuthenticatedUserAndTenant(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	s, err := notification.New(notification.Options{Pool: pool, Schema: schema, Tenant: "a"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := notification.New(notification.Options{Pool: pool, Schema: schema, Tenant: "b"})
	if err != nil {
		t.Fatal(err)
	}
	publish := func(s *notification.Store, recipient notify.Recipient, origin string) notify.Item {
		t.Helper()
		n, err := s.Publish(ctx, notify.Input{Recipient: recipient, Origin: notify.Origin{System: "host", Scope: "site", ID: origin}, Kind: "future.kind", CreatedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	own := publish(s, notify.Recipient{Audience: notify.AudienceUser, ID: "alice"}, "own")
	foreign := publish(s, notify.Recipient{Audience: notify.AudienceUser, ID: "bob"}, "foreign")
	publish(s, notify.Recipient{Audience: notify.AudienceOperator, ID: "alice"}, "operator")
	publish(other, notify.Recipient{Audience: notify.AudienceUser, ID: "alice"}, "other-tenant")
	h, err := s.Handler(testauth.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		token string
		want  int
	}{{"", 401}, {"bad", 401}, {"app:alice", 403}, {"user:alice", 200}} {
		if w := request("GET", "/inbox", tc.token); w.Code != tc.want {
			t.Fatalf("token %q status %d: %s", tc.token, w.Code, w.Body)
		}
	}
	w := request("GET", "/inbox?recipient=bob&tenant=b&audience=operator", "user:alice")
	var p notify.Page
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 1 || p.Items[0].ID != own.ID {
		t.Fatalf("cross-scope notice exposed: %+v", p)
	}
	if w := request("PUT", "/"+foreign.ID+"/read", "user:alice"); w.Code != http.StatusNotFound {
		t.Fatalf("foreign read = %d", w.Code)
	}
	if w := request("PUT", "/"+own.ID+"/read", "user:alice"); w.Code != http.StatusOK {
		t.Fatalf("own read = %d %s", w.Code, w.Body)
	}
	w = request("GET", "/inbox?unread=true", "user:alice")
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || len(p.Items) != 0 {
		t.Fatalf("unread filter %+v %v", p, err)
	}
	if w := request("DELETE", "/"+own.ID+"/read", "user:alice"); w.Code != http.StatusOK {
		t.Fatalf("unread = %d", w.Code)
	}
	if w := request("POST", "/inbox", "user:alice"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("user ingest = %d", w.Code)
	}
}

func TestHandlerArchivesReceiptWithoutResolvingWork(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	store, err := notification.New(notification.Options{Pool: pool, Schema: schema, Tenant: "site"})
	if err != nil {
		t.Fatal(err)
	}
	target := notify.Recipient{Audience: notify.AudienceUser, ID: "alice"}
	task, err := store.Publish(ctx, notify.Input{Recipient: target, Origin: notify.Origin{System: "app", Scope: "site", ID: "review"}, Kind: "app.review", Status: notify.StatusOpen, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := store.Handler(testauth.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, user string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer user:"+user)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request("PUT", "/"+task.ID+"/dismiss", "bob"); w.Code != http.StatusNotFound {
		t.Fatalf("foreign archive = %d", w.Code)
	}
	var archived notify.Item
	w := request("PUT", "/"+task.ID+"/dismiss", "alice")
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &archived) != nil || archived.DismissedAt == nil || archived.ReadAt != nil || archived.Status != notify.StatusOpen || archived.Revision != task.Revision+1 {
		t.Fatalf("archive = %d %s", w.Code, w.Body)
	}
	var page notify.Page
	w = request("GET", "/inbox", "alice")
	if json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 0 {
		t.Fatalf("active inbox %s", w.Body)
	}
	w = request("GET", "/inbox?dismissed=true", "alice")
	if json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].ID != task.ID {
		t.Fatalf("archived inbox %s", w.Body)
	}
	var restored notify.Item
	w = request("DELETE", "/"+task.ID+"/dismiss", "alice")
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &restored) != nil || restored.DismissedAt != nil || restored.Status != notify.StatusOpen {
		t.Fatalf("restore = %d %s", w.Code, w.Body)
	}
	var read notify.Item
	w = request("PUT", "/"+task.ID+"/read", "alice")
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &read) != nil || read.ReadAt == nil || read.Revision != restored.Revision+1 || read.Status != notify.StatusOpen {
		t.Fatalf("read = %d %s", w.Code, w.Body)
	}
	var again notify.Item
	w = request("PUT", "/"+task.ID+"/read", "alice")
	if json.Unmarshal(w.Body.Bytes(), &again) != nil || again.Revision != read.Revision || !again.ReadAt.Equal(*read.ReadAt) {
		t.Fatalf("read retry changed receipt %s", w.Body)
	}
}
