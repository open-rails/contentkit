package critical_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
)

func TestAccountNotificationsOptional(t *testing.T) {
	w := newWorld(t)
	base := w.apps[0]
	cfg, deps := base.cfg, base.deps
	cfg.Database.Schema = w.schema + "_off"
	cfg.Accounts = &ck.AccountsConfig{Schema: w.schema + "_off_accounts"}
	deps.Notifications = nil
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, schema := range []string{cfg.Database.Schema + "_media_worker", cfg.Database.Schema + "_river", cfg.Database.Schema, cfg.Accounts.Schema} {
			_, err := w.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			must(t, err)
		}
	})
	engine, err := ck.New(t.Context(), cfg, deps)
	must(t, err)
	a := &application{test: t, engine: engine, cfg: cfg, deps: deps, group: base.group, tenant: base.tenant}
	a.server = mountEngine(t, engine)
	t.Cleanup(func() {
		a.server.Close()
		must(t, engine.Close(context.Background()))
	})

	assertNoInbox := func(t *testing.T) {
		t.Helper()
		for _, schema := range []string{cfg.Database.Schema, cfg.Accounts.Schema} {
			var count int
			must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_tables WHERE schemaname=$1 AND tablename LIKE 'notify_%'`, schema).Scan(&count))
			if count != 0 {
				t.Fatalf("ContentKit created %d inbox tables with no publisher", count)
			}
		}
	}
	t.Run("CK-J25-S18_omitted_publisher_has_no_inbox_storage_or_routes", func(t *testing.T) {
		assertNoInbox(t)
		owner, token := w.person(t, 0)
		_, writerToken := w.person(t, 0)
		item := publishedArticle(t, a, "Content without an inbox")
		parent := postComment(t, a.client(token), item.ID, "A normal comment")
		_, err := a.client(writerToken).PostContentComments(t.Context(), item.ID, content.CommentInput{Body: "Reply without notification storage", ReplyToID: parent.ID}, ck.PostContentCommentsQuery{})
		must(t, err)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, a.server.URL+"/content/v1/notifications/inbox", nil)
		must(t, err)
		authtest.Authorize(t, w.tenant[0], req, token)
		response, err := a.server.Client().Do(req)
		must(t, err)
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("disabled inbox route returned HTTP%d", response.StatusCode)
		}
		must(t, engine.AnonymizeAccount(t.Context(), owner.ID))
		assertNoInbox(t)
	})
	t.Run("CK-J25-S19_partial_account_erasure_storage_fails_closed", func(t *testing.T) {
		must(t, engine.Close(t.Context()))
		// An incomplete restored account plane must not be mistaken for an
		// optional inbox that has never been enabled. This is corruption setup.
		_, err := w.pool.Exec(t.Context(), "CREATE TABLE "+pgx.Identifier{cfg.Accounts.Schema, "notify_recipients"}.Sanitize()+" (scope text)")
		must(t, err)
		candidate, err := ck.New(t.Context(), cfg, deps)
		if candidate != nil {
			must(t, candidate.Close(t.Context()))
		}
		if err == nil || !strings.Contains(err.Error(), "incomplete AuthKit account erasure storage") {
			t.Fatalf("partial erasure lineage was not refused: %v", err)
		}
		// A retained migration ledger also proves the optional inbox previously
		// existed; losing every data/fence table must not become a fresh opt-out.
		_, err = w.pool.Exec(t.Context(), "DROP TABLE "+pgx.Identifier{cfg.Accounts.Schema, "notify_recipients"}.Sanitize()+"; CREATE TABLE "+pgx.Identifier{cfg.Accounts.Schema, "notify_migrations"}.Sanitize()+" (version integer PRIMARY KEY)")
		must(t, err)
		candidate, err = ck.New(t.Context(), cfg, deps)
		if candidate != nil {
			must(t, candidate.Close(t.Context()))
		}
		if err == nil || !strings.Contains(err.Error(), "incomplete AuthKit account erasure storage") {
			t.Fatalf("retained ledger with lost erasure tables was not refused: %v", err)
		}
	})
}
