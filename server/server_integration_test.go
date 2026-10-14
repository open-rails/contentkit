package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The binary's path: a config file, AuthKit as the resource server, the
// engine and its jobs on one fleet, ContentKit's routes and health, and a
// drain when the context ends.
func TestServerServesAndDrains(t *testing.T) {
	dsn := os.Getenv("CONTENTKIT_TEST_URL")
	if dsn == "" {
		t.Skip("CONTENTKIT_TEST_URL not set")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `SELECT pg_advisory_lock(hashtext('contentkit:pgtest:extensions'));
CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA public; CREATE EXTENSION IF NOT EXISTS pgroonga SCHEMA public;
SELECT pg_advisory_unlock(hashtext('contentkit:pgtest:extensions'))`); err != nil {
		t.Fatal(err)
	}
	schema := "ck_srv_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, s := range []string{schema, schema + "_river", schema + "_auth"} {
			_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{s}.Sanitize()+" CASCADE")
		}
		_, _ = pool.Exec(ctx, "DELETE FROM public.migrations WHERE schema = ANY($1)", []string{schema, schema + "_auth"})
	})
	cfg, err := Load(fmt.Appendf(nil, `
engine:
  tenant: site
  database: {schema: %[1]s}
  content:
    languages: [en]
    kinds:
      gallery: {route: g, presentation: reader, versions: {languages: one}}
auth:
  database: {schema: %[1]s_auth}
  token: {issuer: https://contentkit.test, issued_audiences: [contentkit]}
  resource: {id: https://contentkit.test}
prefix: /api/contentkit
drain_delay: 200ms
`, schema), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(t.Context(), cfg, Deps{Postgres: pool})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	get := func(path string, bearer string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		res.Body.Close()
		return res.StatusCode
	}
	deadline := time.Now().Add(30 * time.Second)
	for get("/readyz", "") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("never ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for path, want := range map[string]int{
		"/livez": 200, "/statusz": 200, "/api/contentkit/v1/config": 200, "/api/contentkit/v1/contents": 200,
		"/api/contentkit/v1/comments/admin": 401, "/api/contentkit/v1/notifications/inbox": 401, "/api/contentkit/v1/codes/ZZZZZZZZ9": 404,
	} {
		if got := get(path, ""); got != want {
			t.Errorf("GET %s: %d, want %d", path, got, want)
		}
	}
	if got := get("/api/contentkit/v1/contents", "not-a-token"); got != http.StatusUnauthorized {
		t.Errorf("a bad token: %d", got)
	}

	stop()
	time.Sleep(50 * time.Millisecond)
	if got := get("/readyz", ""); got != http.StatusServiceUnavailable {
		t.Errorf("draining /readyz: %d", got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Serve did not return")
	}
}
