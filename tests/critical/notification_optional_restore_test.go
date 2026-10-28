package critical_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
)

func TestOptionalInboxRestoreErasure(t *testing.T) {
	w, a := newOptionalInboxWorld(t)
	cfg, deps, engine := a.cfg, a.deps, a.engine
	user, token := w.person(t, 0)
	item := publishedArticle(t, a, "Optional inbox erasure restore")
	comment := postComment(t, a.client(token), item.ID, "Keep the discussion")
	_, err := a.client(token).PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
	must(t, err)
	sibling := newOptionalInboxApplication(t, w, w.apps[1])
	siblingItemID := publishedArticle(t, sibling, "Sibling keeps its activity").ID
	_, err = sibling.client(token).PostContentFavorite(t.Context(), siblingItemID, ck.PostContentFavoriteQuery{})
	must(t, err)
	qs := pgx.Identifier{cfg.Database.Schema}.Sanitize()
	tables := []string{"content_favorites", "content_interaction_counts", "content_erased_subjects", "content_account_lifecycle"}
	for _, table := range tables {
		_, err = w.pool.Exec(t.Context(), "CREATE TABLE "+qs+"."+pgx.Identifier{"backup_" + table}.Sanitize()+" AS TABLE "+qs+"."+pgx.Identifier{table}.Sanitize())
		must(t, err)
	}
	must(t, engine.AnonymizeAccount(t.Context(), user.ID))
	must(t, engine.AnonymizeAccount(t.Context(), user.ID))
	got := readComment(t, a, item.ID, comment.ID)
	if got.UserID != "" || got.AnonName != "<Deleted>" || got.Body != comment.Body {
		t.Fatalf("anonymization changed retained discussion: %+v", got)
	}
	state, err := a.client(token).GetContentFavorite(t.Context(), item.ID, ck.GetContentFavoriteQuery{})
	must(t, err)
	if state.Favorited || state.Count == nil || *state.Count != 1 {
		t.Fatal("account deletion kept favorite attribution")
	}
	a.server.Close()
	must(t, engine.Close(t.Context()))
	tx, err := w.pool.Begin(t.Context())
	must(t, err)
	defer tx.Rollback(context.Background())
	for _, table := range tables {
		tableName := qs + "." + pgx.Identifier{table}.Sanitize()
		_, err = tx.Exec(t.Context(), "DELETE FROM "+tableName+"; INSERT INTO "+tableName+" SELECT * FROM "+qs+"."+pgx.Identifier{"backup_" + table}.Sanitize())
		must(t, err)
	}
	must(t, tx.Commit(t.Context()))
	bad, err := ck.New(t.Context(), cfg, deps)
	if bad != nil {
		defer bad.Close(context.Background())
	}
	if bad != nil || !errors.Is(err, ck.ErrErasureReplayRequired) {
		t.Fatalf("restored attribution served after anonymization: engine=%v error=%v", bad != nil, err)
	}
	cfg.Database.SkipMigrations = true
	candidate, err := ck.New(t.Context(), cfg, deps)
	if candidate != nil {
		defer candidate.Close(context.Background())
	}
	if !errors.Is(err, ck.ErrErasureReplayRequired) || candidate != nil {
		var favorites int
		must(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+qs+".content_favorites WHERE user_id=$1", user.ID).Scan(&favorites))
		t.Fatalf("optional inbox allowed pre-erasure catalog restore: engine=%v error=%v restored_favorites=%d", candidate != nil, err, favorites)
	}
	sibling.server.Close()
	must(t, sibling.engine.Close(t.Context()))
	sibling.engine, err = ck.New(t.Context(), sibling.cfg, sibling.deps)
	must(t, err) // Another app's intent cannot block this app's startup.
	sibling.server = mountEngine(t, sibling.engine)
	recovery, err := ck.NewForErasureReplay(t.Context(), cfg, deps)
	must(t, err)
	defer recovery.Close(context.Background())
	report, err := recovery.ReplayErasureFences(t.Context())
	must(t, err)
	if !report.Complete() {
		t.Fatalf("restored replay incomplete: %+v", report)
	}
	a.engine, err = ck.New(t.Context(), cfg, deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	state, err = a.client(token).GetContentFavorite(t.Context(), item.ID, ck.GetContentFavoriteQuery{})
	must(t, err)
	if state.Favorited || state.Count == nil || *state.Count != 1 {
		t.Fatalf("replay kept restored attribution or lost the contribution: %+v", state)
	}
	_, err = a.client(token).PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
	requireError(t, err, 403, "forbidden")
	state, err = sibling.client(token).GetContentFavorite(t.Context(), siblingItemID, ck.GetContentFavoriteQuery{})
	must(t, err)
	if !state.Favorited || state.Count == nil || *state.Count != 1 {
		t.Fatalf("app intent erased sibling activity: %+v", state)
	}
}

func TestOptionalInboxFailedErasure(t *testing.T) {
	w, a := newOptionalInboxWorld(t)
	user, token := w.person(t, 0)
	item := publishedArticle(t, a, "Failed erasure retains intent")
	_, err := a.client(token).PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
	must(t, err)
	qs := pgx.Identifier{a.cfg.Database.Schema}.Sanitize()
	_, err = w.pool.Exec(t.Context(), `CREATE FUNCTION `+qs+`.fail_erasure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned cleanup failure'; END $$;
CREATE TRIGGER fail_erasure BEFORE UPDATE ON `+qs+`.content_favorites FOR EACH ROW EXECUTE FUNCTION `+qs+`.fail_erasure()`)
	must(t, err)
	if err := a.engine.AnonymizeAccount(t.Context(), user.ID); err == nil {
		t.Fatal("failed cleanup reported complete")
	}
	var intents, localFences int
	must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{a.cfg.Accounts.Schema, "content_account_erasure_intents"}.Sanitize()+` WHERE tenant_id=$1 AND app_id=$2 AND actor_id=$3`, a.cfg.Scope.TenantID.String(), a.cfg.Scope.AppID.String(), user.ID).Scan(&intents))
	must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+qs+`.content_erased_subjects WHERE actor_id=$1`, user.ID).Scan(&localFences))
	if intents != 1 || localFences != 0 {
		t.Fatalf("shared intent did not survive the rolled-back cleanup: intents=%d local_fences=%d", intents, localFences)
	}
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	_, err = w.pool.Exec(t.Context(), `DROP TRIGGER fail_erasure ON `+qs+`.content_favorites`)
	must(t, err)
	recovery, err := ck.NewForErasureReplay(t.Context(), a.cfg, a.deps)
	must(t, err)
	defer recovery.Close(context.Background())
	report, err := recovery.ReplayErasureFences(t.Context())
	must(t, err)
	if !report.Complete() {
		t.Fatalf("retry did not finish cleanup: %+v", report)
	}
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	_, err = a.client(token).PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
	requireError(t, err, 403, "forbidden")
	state, err := a.client("").GetContentFavorite(t.Context(), item.ID, ck.GetContentFavoriteQuery{})
	must(t, err)
	if state.Count == nil || *state.Count != 1 {
		t.Fatalf("retried deletion lost the anonymous contribution: %+v", state)
	}
}

func newOptionalInboxWorld(t *testing.T) (*world, *application) {
	t.Helper()
	w := newWorld(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, suffix := range []string{"_off_restore_media_worker", "_off_restore_river", "_off_restore", "_off_accounts"} {
			_, err := w.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{w.schema + suffix}.Sanitize()+" CASCADE")
			must(t, err)
		}
	})
	return w, newOptionalInboxApplication(t, w, w.apps[0])
}

func newOptionalInboxApplication(t *testing.T, w *world, base *application) *application {
	t.Helper()
	cfg, deps := base.cfg, base.deps
	cfg.Database.Schema = w.schema + "_off_restore"
	cfg.Accounts = &ck.AccountsConfig{Schema: w.schema + "_off_accounts"}
	deps.Notifications = nil
	engine, err := ck.New(t.Context(), cfg, deps)
	must(t, err)
	a := &application{test: t, engine: engine, cfg: cfg, deps: deps, group: base.group, tenant: base.tenant}
	a.server = mountEngine(t, engine)
	t.Cleanup(func() { a.server.Close(); must(t, a.engine.Close(context.Background())) })
	return a
}
