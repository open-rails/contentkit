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
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/internal/engagementtest"
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
		var liked, favorited uint64
		if err := admin.QueryRow(ctx, "SELECT countIf(signal_type=? AND value=1),countIf(signal_type=? AND value=1) FROM "+chDB+".signals FINAL WHERE tenant=? AND subject_kind='user' AND subject=? AND content_id=?", content.PreferenceAxisReaction, content.PreferenceAxisFavorite, tenant, user.Key(), c.ID).Scan(&liked, &favorited); err != nil {
			t.Fatal(err)
		}
		if liked == 1 && favorited == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("preferences never reached the signal plane: likes=%d favorites=%d", liked, favorited)
		}
		time.Sleep(250 * time.Millisecond)
	}

	// Content detail carries the reader's standing to resume from: the item's
	// and each version's.
	vid := c.Versions[0].ID
	if err := e.hub.RecordSignals(ctx, engagementtest.Native([]engagementtest.Observation{{ContentRef: g1.WithVersion(vid), Subject: user,
		Type: engagementtest.TypeReading, EventID: "reading-1", OccurredAt: time.Now().UTC().Add(-time.Minute), Progress: 7, ProgressMax: 10, Resume: "page:7", DurationS: 60}})); err != nil {
		t.Fatal(err)
	}
	var detail catalog.Content
	s.call(http.StatusOK, u1, "GET", "/ck/v1/contents/"+c.ID, nil, &detail)
	if v := detail.Viewer; v == nil || !v.Favorited || !v.Seen || v.ProgressMax == 0 || uint64(v.Progress)*10 != uint64(v.ProgressMax)*7 || v.Resume != "page:7" || v.LastReadAt == nil {
		t.Fatalf("detail standing %+v", v)
	}
	if v := detail.Versions[0].Viewer; v == nil || v.Favorited || v.ProgressMax == 0 || uint64(v.Progress)*10 != uint64(v.ProgressMax)*7 || v.Resume != "page:7" {
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
	if err != nil || len(states) != 0 {
		t.Fatalf("erased subject still has state: %+v %v", states, err)
	}
	var feedback uint64
	if err := admin.QueryRow(ctx, "SELECT count() FROM "+chDB+".signals FINAL WHERE tenant=? AND subject_kind='user' AND subject=?", tenant, user.Key()).Scan(&feedback); err != nil || feedback != 0 {
		t.Fatalf("erasure left preference signals: %d %v", feedback, err)
	}
}

// A ClickHouse that is down at boot, comes up, drops and comes back: signals
// follow it without a restart, and the content model and the probe tell the
// truth throughout.
func TestEngineSignalPlaneOutageIntegration(t *testing.T) {
	env := signaltest.FromEnv(t)
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := engineSchema(t, pool)
	chDB := "ck_outage_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	env.Empty(t, chDB)
	admin := env.Open(t, "")
	t.Cleanup(func() {
		env.Drop(t, admin, chDB)
		_, _ = pool.Exec(context.Background(), "DELETE FROM public.migrations WHERE app = 'contentkit_signal' AND database LIKE '%' || $1", chDB)
	})
	ch := newCHProxy(t, env.Addr)
	cfg := siteConfig(schema)
	cfg.ClickHouse = ClickHouseConfig{Database: chDB, Addr: ch.addr(), Username: env.User, Password: env.Password, Cluster: env.Cluster}
	e, err := New(ctx, cfg, Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	routes, err := e.Routes(Routes{Prefix: "/ck", Groups: ContentRoutes | DiscoveryRoutes})
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t, routes)
	staff, u1 := access.Actor{ID: "staff", Kind: "user"}, access.Actor{ID: "u1", Kind: "user"}
	var c catalog.Content
	s.call(http.StatusCreated, staff, "POST", "/ck/v1/contents", catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: "Tide"}},
		Versions: []catalog.VersionInput{{Languages: []string{"en"}}}}, &c)
	s.call(http.StatusOK, staff, "POST", "/ck/v1/contents/"+c.ID+"/publish", catalog.PublishInput{}, nil)
	probe := e.Probes()[0]
	signals := func(status int) {
		t.Helper()
		s.call(status, u1, "POST", "/ck/v1/signals", discovery.Signals{Signals: []discovery.SignalInput{{ContentID: c.ID, Type: discovery.TypeClick,
			EventID: uuid.NewString(), OccurredAt: time.Now().UTC()}}}, nil)
	}
	await := func(on bool) {
		t.Helper()
		for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(250 * time.Millisecond) {
			var model kindsroute.ContentModel
			s.call(http.StatusOK, access.Actor{Anonymous: true}, "GET", "/ck/v1/kinds", nil, &model)
			if model.Signals == on {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the content model never said signals=%v", on)
			}
		}
		if err := probe.Check(ctx); (err == nil) != on {
			t.Fatalf("signals=%v but the probe says %v", on, err)
		}
	}

	await(false)
	signals(http.StatusServiceUnavailable)
	ch.set(true)
	await(true)
	signals(http.StatusNoContent)
	ch.set(false)
	await(false)
	signals(http.StatusServiceUnavailable)
	ch.set(true)
	await(true)
	signals(http.StatusNoContent)
}
