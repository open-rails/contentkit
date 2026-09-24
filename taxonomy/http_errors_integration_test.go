package taxonomy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A Postgres integrity violation answers a stable public code; its constraint
// name reaches the log and nothing else. An unmapped failure is a sanitized
// 500 whose cause is logged at ERROR.
func TestHandlerErrorContract(t *testing.T) {
	ctx := context.Background()
	pool, schema := testSchema(t, ctx)
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := newStore(t, pool, schema, tenant, func(o *Options) { o.Logger = log })
	srv := httptest.NewServer(Handler(s))
	t.Cleanup(srv.Close)

	post := func(path, body string) (int, string) {
		t.Helper()
		path, body = withIDs(path), withIDs(body)
		res, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, strings.TrimSpace(string(b))
	}

	if code, body := post("/nodes", `[{"taxonomy_id":"{{colored}}","kind":"tag","slug":"colored"}]`); code != http.StatusCreated {
		t.Fatalf("seed -> %d %s", code, body)
	}

	// 23505: the duplicate-slug unique index.
	logs.Reset()
	code, body := post("/nodes", `[{"kind":"tag","slug":"colored"}]`)
	if code != http.StatusConflict || body != `{"error":"taxonomy: conflict","code":"conflict"}` {
		t.Fatalf("duplicate slug -> %d %s", code, body)
	}
	assertNoSQLLeak(t, "duplicate slug", body)
	if !strings.Contains(logs.String(), "content_nodes_slug") || !strings.Contains(logs.String(), "sqlstate 23505") {
		t.Fatalf("the constraint name must reach the log, got: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "level=DEBUG") || strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("a 409 must log at DEBUG, got: %s", logs.String())
	}

	// 23503: the assignment/edge foreign key to an absent node.
	logs.Reset()
	code, body = post("/edges", `[{"from_taxonomy_id":"{{colored}}","relation":"synonym","to_taxonomy_id":"{{missing}}"}]`)
	if code != http.StatusNotFound || body != `{"error":"taxonomy: not found","code":"not_found"}` {
		t.Fatalf("missing edge target -> %d %s", code, body)
	}
	assertNoSQLLeak(t, "missing edge target", body)
	if !strings.Contains(logs.String(), "fkey") || !strings.Contains(logs.String(), "sqlstate 23503") {
		t.Fatalf("the constraint name must reach the log, got: %s", logs.String())
	}

	// An unmapped failure: the catalog tables are gone under the handler.
	logs.Reset()
	if _, err := pool.Exec(ctx, "DROP TABLE "+pgx.Identifier{schema, "content_nodes"}.Sanitize()+" CASCADE"); err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(srv.URL + "/nodes?kind=tag")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	body = strings.TrimSpace(string(b))
	if res.StatusCode != http.StatusInternalServerError || body != `{"error":"internal error","code":"internal_error"}` {
		t.Fatalf("internal failure -> %d %s", res.StatusCode, body)
	}
	assertNoSQLLeak(t, "internal failure", body)
	out := logs.String()
	if !strings.Contains(out, "level=ERROR") {
		t.Fatalf("a taxonomy 500 must log at ERROR, got: %s", out)
	}
	if !strings.Contains(out, "content_nodes") || !strings.Contains(out, "42P01") {
		t.Fatalf("a taxonomy 500 must log its cause, got: %s", out)
	}
}
