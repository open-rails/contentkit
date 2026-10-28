package critical_test

import (
	"context"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/server"
)

func TestStandaloneParity(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	item := publishedArticle(t, a, "Drain keeps accepted work")
	var authConfig authkit.Config
	// Capture the public test-support replica configuration, including its
	// isolated AuthKit schema. No private engine/store fields are inspected.
	authtest.Replica(t, w.auth, authtest.WithConfig(func(cfg *authkit.Config) { authConfig = *cfg }))
	cfg := server.Config{Engine: a.cfg, Auth: authConfig, Authorization: &server.AuthorizationConfig{Issuer: authtest.Issuer, AppGroupID: a.cfg.Scope.AppID.String()}, Prefix: "/content", DrainDelay: 2 * time.Second, ShutdownTimeout: 10 * time.Second}
	cfg.Engine.Database.SkipMigrations = true
	standalone, err := server.New(t.Context(), cfg, server.Deps{Postgres: w.pool})
	must(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	base := "http://" + listener.Addr().String()
	client := ck.NewClient(base+"/content", nil)
	run, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	serverFinished := false
	go func() { done <- standalone.Serve(run, listener) }()
	t.Cleanup(func() {
		cancel()
		if !serverFinished {
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("standalone shutdown: %v", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("owned standalone did not stop")
				return
			}
		}
		must(t, standalone.Close(context.Background()))
	})
	waitFor(t, func() bool {
		select {
		case err := <-done:
			serverFinished = true
			t.Fatalf("standalone stopped before readiness: %v", err)
		default:
		}
		res, err := http.Get(base + "/readyz")
		if err != nil {
			return false
		}
		defer res.Body.Close()
		return res.StatusCode == http.StatusNoContent || res.StatusCode == http.StatusOK
	})
	t.Run("CK-J01-S06_standalone_and_embedded_public_surfaces_agree", func(t *testing.T) {
		left, err := a.client("").GetKinds(t.Context())
		must(t, err)
		right, err := client.GetKinds(t.Context())
		must(t, err)
		if !reflect.DeepEqual(left, right) {
			t.Fatalf("standalone changed the content model: %+v / %+v", left, right)
		}
		leftConfig, err := a.client("").GetConfig(t.Context())
		must(t, err)
		rightConfig, err := client.GetConfig(t.Context())
		must(t, err)
		if !reflect.DeepEqual(leftConfig, rightConfig) {
			t.Fatal("standalone changed interaction configuration")
		}
		if leftConfig.CommentMaxLength != 2200 || leftConfig.Anonymous.Comments || leftConfig.Anonymous.Reactions || leftConfig.Anonymous.Votes {
			t.Fatalf("default interaction configuration is wrong: %+v", leftConfig)
		}
		leftContent, err := a.client("").GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		rightContent, err := client.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if !reflect.DeepEqual(leftContent, rightContent) {
			t.Fatalf("equivalent anonymous reads differ: %+v / %+v", leftContent, rightContent)
		}
		for _, prefix := range []string{a.server.URL + "/content", base + "/content"} {
			bad := ck.NewClient(prefix, &http.Client{Transport: bearerTransport{"invalid", http.DefaultTransport}})
			_, err := bad.GetContents(t.Context(), ck.GetContentsQuery{})
			requireError(t, err, 401, "unauthorized")
			_, err = ck.NewClient(prefix, nil).GetMediaPresets(t.Context())
			requireError(t, err, 404, "not_found")
		}
	})
	t.Run("CK-J01-S07_drain_fails_readiness_and_finishes_accepted_HTTP_read", func(t *testing.T) {
		tx, err := w.pool.Begin(t.Context())
		must(t, err)
		defer tx.Rollback(context.Background())
		_, err = tx.Exec(t.Context(), "LOCK TABLE "+pgx.Identifier{w.schema, "content_items"}.Sanitize()+" IN ACCESS EXCLUSIVE MODE")
		must(t, err)
		type result struct {
			item catalog.Content
			err  error
		}
		finished := make(chan result, 1)
		go func() {
			got, err := client.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
			finished <- result{got, err}
		}()
		waitFor(t, func() bool {
			var n int
			must(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_locks WHERE relation=$1::regclass AND NOT granted AND mode='AccessShareLock'", w.schema+".content_items").Scan(&n))
			return n > 0
		})
		cancel()
		waitFor(t, func() bool {
			res, err := http.Get(base + "/readyz")
			if err != nil {
				return false
			}
			defer res.Body.Close()
			return res.StatusCode == http.StatusServiceUnavailable
		})
		select {
		case got := <-finished:
			t.Fatalf("in-flight read completed before the real database lock was released: %+v", got)
		default:
		}
		must(t, tx.Commit(t.Context()))
		select {
		case got := <-finished:
			must(t, got.err)
			if got.item.ID != item.ID || got.item.Body != "Published body: Drain keeps accepted work" {
				t.Fatalf("drain lost the accepted response: %+v", got.item)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("accepted read did not finish during drain")
		}
		select {
		case err := <-done:
			serverFinished = true
			must(t, err)
		case <-time.After(15 * time.Second):
			t.Fatal("standalone did not finish graceful shutdown")
		}
		must(t, w.pool.Ping(t.Context()))
	})
}
