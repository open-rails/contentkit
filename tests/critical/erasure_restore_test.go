package critical_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/signal"
)

func TestRestoredErasureFence(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	user, token := w.person(t, 0)
	item := publishedArticle(t, a, "Erased app backup")
	client := a.client(token)
	_, err := client.PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
	must(t, err)
	qs := pgx.Identifier{w.schema}.Sanitize()
	// These are actual pre-erasure rows copied inside this owned database,
	// equivalent to restoring the app's interaction tables. Shared account
	// tombstones stay current, just as required by the recovery contract.
	tables := []string{"content_favorites", "content_interaction_counts", "content_erased_subjects"}
	for _, table := range tables {
		_, err := w.pool.Exec(t.Context(), "CREATE TABLE "+qs+"."+pgx.Identifier{"backup_" + table}.Sanitize()+" AS TABLE "+qs+"."+pgx.Identifier{table}.Sanitize())
		must(t, err)
	}
	report, err := a.engine.EraseAppSubjects(t.Context(), []signal.Subject{{UserID: user.ID}})
	must(t, err)
	if !report.Complete() {
		t.Fatalf("initial erasure incomplete: %+v", report)
	}
	state, err := client.GetContentFavorite(t.Context(), item.ID, ck.GetContentFavoriteQuery{})
	must(t, err)
	if state.Favorited || state.Count == nil || *state.Count != 0 {
		t.Fatalf("initial erasure did not clear real favorite: %+v", state)
	}
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	t.Run("CK-J23-S04_restored_app_refuses_serving_until_shared_fence_replay", func(t *testing.T) {
		tx, err := w.pool.Begin(t.Context())
		must(t, err)
		defer tx.Rollback(context.Background())
		for _, table := range tables {
			name := qs + "." + pgx.Identifier{table}.Sanitize()
			_, err := tx.Exec(t.Context(), "DELETE FROM "+name+"; INSERT INTO "+name+" SELECT * FROM "+qs+"."+pgx.Identifier{"backup_" + table}.Sanitize())
			must(t, err)
		}
		must(t, tx.Commit(t.Context()))
		a.cfg.Database.SkipMigrations = true
		bad, err := ck.New(t.Context(), a.cfg, a.deps)
		if !errors.Is(err, ck.ErrErasureReplayRequired) || bad != nil {
			t.Fatalf("restored app served before replay: engine=%v err=%v", bad, err)
		}
		recovery, err := ck.NewForErasureReplay(t.Context(), a.cfg, a.deps)
		must(t, err)
		defer func() { must(t, recovery.Close(context.Background())) }()
		report, err := recovery.ReplayErasureFences(t.Context())
		must(t, err)
		if !report.Complete() {
			t.Fatalf("restored replay incomplete: %+v", report)
		}
		a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
		must(t, err)
		a.server = mountEngine(t, a.engine)
		state, err := a.client(token).GetContentFavorite(t.Context(), item.ID, ck.GetContentFavoriteQuery{})
		must(t, err)
		if state.Favorited || state.Count == nil || *state.Count != 0 {
			t.Fatalf("replay resurrected old attribution: %+v", state)
		}
		_, err = a.client(token).PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
		requireError(t, err, 403, "forbidden")
		_, err = a.client("").GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
	})
}

func TestPausedInteractionWriterErasure(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	user, token := w.person(t, 0)
	item := publishedArticle(t, a, "Paused write")
	qs := pgx.Identifier{w.schema}.Sanitize()
	const barrier int64 = 612900232
	holder, err := w.pool.Acquire(t.Context())
	must(t, err)
	_, err = holder.Exec(t.Context(), "SELECT pg_advisory_lock($1)", barrier)
	must(t, err)
	defer func() {
		_, _ = holder.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", barrier)
		holder.Release()
	}()
	_, err = w.pool.Exec(t.Context(), fmt.Sprintf(`CREATE FUNCTION %s.critical_pause_favorite() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.user_id=TG_ARGV[0] THEN PERFORM pg_advisory_xact_lock(%d); END IF; RETURN NEW; END$$;
CREATE TRIGGER critical_pause_favorite BEFORE INSERT ON %s.content_favorites FOR EACH ROW EXECUTE FUNCTION %s.critical_pause_favorite('%s')`, qs, barrier, qs, qs, user.ID))
	must(t, err)
	t.Run("CK-J23-S02_paused_pre_erasure_writer_cannot_resurrect_activity", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		written, erased := make(chan error, 1), make(chan error, 1)
		go func() {
			_, err := a.client(token).PostContentFavorite(ctx, item.ID, ck.PostContentFavoriteQuery{})
			written <- err
		}()
		waitFor(t, func() bool {
			var n int
			must(t, w.pool.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objid=$1 AND NOT granted", barrier).Scan(&n))
			return n == 1
		})
		go func() {
			report, err := a.engine.EraseAppSubjects(ctx, []signal.Subject{{UserID: user.ID}})
			if err == nil && !report.Complete() {
				err = fmt.Errorf("erasure incomplete: %+v", report)
			}
			erased <- err
		}()
		waitFor(t, func() bool {
			var n int
			must(t, w.pool.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted AND pid IN (SELECT pid FROM pg_stat_activity WHERE datname=current_database())").Scan(&n))
			return n >= 2
		})
		_, err := holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", barrier)
		must(t, err)
		must(t, <-written)
		must(t, <-erased)
		state, err := a.client(token).GetContentFavorite(t.Context(), item.ID, ck.GetContentFavoriteQuery{})
		must(t, err)
		if state.Favorited || state.Count == nil || *state.Count != 0 {
			t.Fatalf("pre-erasure writer survived the completed fence: %+v", state)
		}
		_, err = a.client(token).PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
		requireError(t, err, 403, "forbidden")
		_, sibling := w.person(t, 0)
		state, err = a.client(sibling).PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
		must(t, err)
		if !state.Favorited || state.Count == nil || *state.Count != 1 {
			t.Fatalf("fence blocked an unrelated reader: %+v", state)
		}
	})
}
