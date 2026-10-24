package critical_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
)

func TestHostLifecycle(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	appLifetime := t.Context()
	t.Run("CK-J01-S01_empty_migration_and_readiness", func(t *testing.T) {
		if err := a.engine.Ready(t.Context()); err == nil {
			t.Fatal("unstarted engine reported ready")
		}
		must(t, a.engine.Start(appLifetime))
		must(t, a.engine.Ready(t.Context()))
		if err := a.engine.Start(t.Context()); err == nil {
			t.Fatal("second Start created another worker owner")
		}
	})
	t.Run("CK-J01-S03_prepared_schema_can_start_read_only", func(t *testing.T) {
		cfg := a.cfg
		cfg.Database.SkipMigrations = true
		replica, err := ck.New(t.Context(), cfg, a.deps)
		must(t, err)
		defer func() { must(t, replica.Close(context.Background())) }()
		model, err := replica.Client().GetKinds(t.Context())
		must(t, err)
		if len(model.Kinds) != 2 {
			t.Fatalf("prepared schema changed the public model: %+v", model)
		}
		cfg.Database.Schema = "ckc_missing"
		unprepared, err := ck.New(t.Context(), cfg, a.deps)
		if err == nil || unprepared != nil {
			t.Fatal("read-only startup accepted an unprepared schema")
		}
	})
	t.Run("CK-J01-S04_invalid_scope_refuses_before_serving", func(t *testing.T) {
		cfg := a.cfg
		cfg.Scope = &ck.Scope{TenantID: cfg.Scope.TenantID}
		bad, err := ck.New(t.Context(), cfg, a.deps)
		if err == nil || bad != nil {
			t.Fatal("zero app scope was accepted")
		}
		cfg = a.cfg
		cfg.Tenant = "injected-legacy-selector"
		bad, err = ck.New(t.Context(), cfg, a.deps)
		if err == nil || bad != nil {
			t.Fatal("request-style tenant override was accepted beside canonical scope")
		}
	})
	t.Run("CK-J01-S05_route_selection_is_enforced", func(t *testing.T) {
		routes, err := a.engine.Routes(ck.Routes{Prefix: "/subset", Groups: ck.ContentRoutes})
		must(t, err)
		mux := http.NewServeMux()
		for _, route := range routes {
			mux.Handle(route.Path, route.Handler)
		}
		srv := httptest.NewServer(mux)
		defer srv.Close()
		for _, path := range []string{"/subset/v1/entities", "/subset/v1/media/presets", "/wrong/v1/kinds"} {
			res, err := http.Get(srv.URL + path)
			must(t, err)
			res.Body.Close()
			if res.StatusCode != 404 {
				t.Fatalf("omitted %s returned %d", path, res.StatusCode)
			}
		}
		if _, err := a.engine.Routes(ck.Routes{}); err == nil {
			t.Fatal("empty route selection accepted")
		}
		if _, err := a.engine.Routes(ck.Routes{Prefix: "relative", Groups: ck.ContentRoutes}); err == nil {
			t.Fatal("relative mount accepted")
		}
	})
	t.Run("CK-J01-S08_reopen_preserves_data_and_borrowed_pool", func(t *testing.T) {
		b := w.apps[1]
		created, err := b.engine.Client().PostContents(t.Context(), catalog.ContentInput{Kind: "article", Text: map[string]catalog.Text{"en": {Title: "Persistent draft"}}})
		must(t, err)
		must(t, b.engine.Close(t.Context()))
		must(t, b.engine.Close(t.Context()))
		if err := b.engine.Ready(t.Context()); err == nil {
			t.Fatal("closed engine remained ready")
		}
		if err := b.engine.Start(t.Context()); err == nil {
			t.Fatal("closed engine restarted")
		}
		must(t, w.pool.Ping(t.Context()))
		cfg := b.cfg
		cfg.Database.SkipMigrations = true
		reopened, err := ck.New(t.Context(), cfg, b.deps)
		must(t, err)
		b.engine = reopened
		got, err := reopened.Client().GetContent(t.Context(), created.ID, ck.GetContentQuery{})
		must(t, err)
		if got.ID != created.ID || got.Title != "Persistent draft" {
			t.Fatalf("restart changed committed public content: %+v", got)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		must(t, w.pool.Ping(ctx))
	})
}
