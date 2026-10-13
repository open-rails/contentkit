package contentkit

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/internal/kindsroute"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/signal"
)

// With ClickHouse configured, New applies the signal plane's migrations, the
// preference job carries reactions and favorites into it, and erasure covers
// both planes.
func TestEngineSignalPlaneIntegration(t *testing.T) {
	env := signaltest.FromEnv(t)
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := engineSchema(t, pool)
	chDB := "ck_engine_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	env.Empty(t, chDB)
	admin := env.Open(t, "")
	t.Cleanup(func() {
		env.Drop(t, admin, chDB)
		_, _ = pool.Exec(context.Background(), "DELETE FROM public.migrations WHERE app = 'contentkit_signal' AND database LIKE '%' || $1", chDB)
	})
	cfg := siteConfig(schema)
	cfg.ClickHouse = ClickHouseConfig{Database: chDB, Addr: env.Addr, Username: env.User, Password: env.Password, Cluster: env.Cluster}
	deps := Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}}
	for range 2 { // idempotent
		e, err := New(ctx, cfg, deps)
		if err != nil {
			t.Fatal(err)
		}
		_ = e.Close(ctx)
	}
	e, err := New(ctx, cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	if p := e.Probes(); len(p) != 1 || p[0].Name != "contentkit_clickhouse" || p[0].Check(ctx) != nil {
		t.Fatalf("probes %+v", p)
	}
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	routes, err := e.Routes(Routes{Prefix: "/ck", Groups: ContentRoutes | DiscoveryRoutes})
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t, routes)
	u1, staff := access.Actor{ID: "u1", Kind: "user"}, access.Actor{ID: "staff", Kind: "user"}
	var model kindsroute.ContentModel
	if s.call(http.StatusOK, access.Actor{Anonymous: true}, "GET", "/ck/v1/kinds", nil, &model); !model.Signals {
		t.Fatal("the content model says signals are off")
	}
	var c catalog.Content
	s.call(http.StatusCreated, staff, "POST", "/ck/v1/contents", catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: "Harbor"}},
		Versions: []catalog.VersionInput{{Languages: []string{"en"}}}}, &c)
	s.call(http.StatusOK, staff, "POST", "/ck/v1/contents/"+c.ID+"/publish", catalog.PublishInput{}, nil)
	s.call(http.StatusOK, u1, "POST", "/ck/v1/contents/"+c.ID+"/like", nil, nil)
	s.call(http.StatusOK, u1, "POST", "/ck/v1/contents/"+c.ID+"/favorite", nil, nil)

	if _, err := e.own.Insert(ctx, preferenceSyncArgs{}, &river.InsertOpts{Queue: QueueContentKit}); err != nil {
		t.Fatal(err)
	}
	g1 := contentref.New(tenant, "gallery", c.ID)
	user := signal.Subject{UserID: "u1"}
	deadline := time.Now().Add(60 * time.Second)
	for {
		states, err := e.hub.States(ctx, user, []contentref.ContentRef{g1})
		if err != nil {
			t.Fatal(err)
		}
		if st := states[g1.Key()]; st.Feedback == 2 && st.NetValue == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("preferences never reached the signal plane: %+v", states)
		}
		time.Sleep(250 * time.Millisecond)
	}

	// Content detail carries the reader's standing to resume from: the item's
	// and each version's.
	vid := c.Versions[0].ID
	s.call(http.StatusNoContent, u1, "POST", "/ck/v1/signals", discovery.Signals{Signals: []discovery.SignalInput{{ContentID: c.ID, VersionID: vid,
		Type: discovery.TypeView, EventID: "resume-1", OccurredAt: time.Now().UTC().Add(-time.Minute), Progress: 7, Resume: "page:7", DurationS: 60}}}, nil)
	var detail catalog.Content
	s.call(http.StatusOK, u1, "GET", "/ck/v1/contents/"+c.ID, nil, &detail)
	if v := detail.Viewer; v == nil || !v.Favorited || !v.Seen || v.Progress != 7 || v.Resume != "page:7" || v.LastViewAt == nil {
		t.Fatalf("detail standing %+v", v)
	}
	if v := detail.Versions[0].Viewer; v == nil || v.Favorited || v.Progress != 7 || v.Resume != "page:7" {
		t.Fatalf("version standing %+v", v)
	}
	var version catalog.Version
	s.call(http.StatusOK, u1, "GET", "/ck/v1/contents/"+c.ID+"/versions/"+vid, nil, &version)
	if v := version.Viewer; v == nil || v.Resume != "page:7" {
		t.Fatalf("version route standing %+v", v)
	}
	var other catalog.Content
	s.call(http.StatusOK, staff, "GET", "/ck/v1/contents/"+c.ID, nil, &other)
	if v := other.Viewer; v == nil || v.Seen || v.Favorited {
		t.Fatalf("another reader's standing %+v", v)
	}

	report, err := e.EraseSubjects(ctx, []signal.Subject{user})
	if err != nil || !report.Complete() {
		t.Fatalf("erasure %+v %v", report, err)
	}
	states, err := e.hub.States(ctx, user, []contentref.ContentRef{g1})
	if err != nil || states[g1.Key()].Feedback != 0 {
		t.Fatalf("erased subject still has state: %+v %v", states, err)
	}
}
