package contentkit

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/signal"
	"github.com/open-rails/helpers/notify"
)

func TestRuntimeNotificationsMountAndErasure(t *testing.T) {
	ctx := context.Background()
	pool := testPG(t)
	schema := pgtest.Schema(t, ctx, pool)
	rt, err := NewRuntime(ctx, RuntimeConfig{EmbeddedConfig: EmbeddedConfig{PG: pool, PGSchema: schema, Tenant: "site"}, Auth: testauth.Auth{Authz: testauth.All{}}, Content: content.Options{Resolver: itemResolver{}, ContentKinds: []string{"gallery"}}})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Notifications != rt.Content.Notifications {
		t.Fatal("Runtime and content do not share the inbox")
	}
	recipient := notify.Recipient{Audience: notify.AudienceUser, ID: "owner"}
	item, err := rt.Notifications.Publish(ctx, notify.Input{Recipient: recipient, Origin: notify.Origin{System: "app", Scope: "site", ID: "a"}, Kind: "app.future", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	response := do(t, rt.Handler(), access.Actor{ID: "owner"}, "GET", "/notifications/inbox", nil)
	var page notify.Page
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].ID != item.ID {
		t.Fatalf("inbox mount %d %s", response.Code, response.Body)
	}
	response = do(t, rt.Handler(), access.Actor{ID: "owner"}, "PUT", "/notifications/"+item.ID+"/read", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("read mount %d %s", response.Code, response.Body)
	}
	report, err := rt.EraseSubjects(ctx, []signal.Subject{{UserID: "owner"}})
	if err != nil || !report.Complete() {
		t.Fatalf("erasure %+v %v", report, err)
	}
	page, err = rt.Notifications.List(ctx, recipient, notify.ListOptions{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("erased inbox %+v %v", page, err)
	}
}
