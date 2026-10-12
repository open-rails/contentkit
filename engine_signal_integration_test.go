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
	"github.com/open-rails/contentkit/contentref"
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
	deps := Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}, Resolver: galleries{}}
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
	routes, err := e.Routes(Routes{Prefix: "/ck", Groups: ContentRoutes})
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t, routes)
	u1 := access.Actor{ID: "u1", Kind: "user"}
	s.call(http.StatusOK, u1, "POST", "/ck/v1/gallery/"+cid(1)+"/like", nil, nil)
	s.call(http.StatusOK, u1, "POST", "/ck/v1/gallery/"+cid(1)+"/favorite", nil, nil)

	if _, err := e.own.Insert(ctx, preferenceSyncArgs{}, &river.InsertOpts{Queue: QueueContentKit}); err != nil {
		t.Fatal(err)
	}
	g1 := contentref.New(tenant, "gallery", cid(1))
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

	report, err := e.EraseSubjects(ctx, []signal.Subject{user})
	if err != nil || !report.Complete() {
		t.Fatalf("erasure %+v %v", report, err)
	}
	states, err := e.hub.States(ctx, user, []contentref.ContentRef{g1})
	if err != nil || states[g1.Key()].Feedback != 0 {
		t.Fatalf("erased subject still has state: %+v %v", states, err)
	}
}
