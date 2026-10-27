package critical_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
)

func TestRestoredAccountAnonymization(t *testing.T) {
	for _, accountReplay := range []bool{false, true} {
		t.Run(fmt.Sprintf("account_replay_%t", accountReplay), func(t *testing.T) {
			w := newWorld(t)
			a := w.apps[0]
			user, token, channel := ownedChannel(t, w, a, "Restored owner")
			authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(user.ID), w.contributor)
			startPublicationRuntime(t, a)
			client := a.client(token)
			credits := []catalog.Creator{{UserID: user.ID, Role: "author"}}
			item, err := client.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Creators: &credits,
				Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Restored attribution", Body: "Published contribution"}}}}})
			must(t, err)
			_, err = client.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
			must(t, err)
			awaitEditionPublication(t, a, client, item.ID, item.Versions[0].ID)
			comment := postComment(t, client, item.ID, "Keep this discussion")
			favorite, err := client.PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
			must(t, err)
			if favorite.Count == nil || *favorite.Count != 1 || readComment(t, a, item.ID, comment.ID).UserID != user.ID {
				t.Fatal("restore fixture lacks attributed contributions")
			}
			qs := pgx.Identifier{w.schema}.Sanitize()
			tables := []string{"content_comments", "content_favorites", "content_interaction_counts", "content_creators", "content_erased_subjects", "content_account_lifecycle"}
			for _, table := range tables {
				_, err := w.pool.Exec(t.Context(), "CREATE TABLE "+qs+"."+pgx.Identifier{"backup_" + table}.Sanitize()+" AS TABLE "+qs+"."+pgx.Identifier{table}.Sanitize())
				must(t, err)
			}
			// Restore only attribution columns on parents; replacing parent rows
			// would cascade-delete unrelated catalog and publication records.
			_, err = w.pool.Exec(t.Context(), "CREATE TABLE "+qs+".backup_owners AS SELECT i.id AS content_id,i.created_by,e.id AS entity_id,e.owner_user_id FROM "+qs+".content_items i CROSS JOIN "+qs+".content_entities e WHERE i.tenant_id=$1 AND i.id=$2 AND e.tenant_id=$1 AND e.id=$3", a.cfg.Scope.AppID.String(), item.ID, channel.ID)
			must(t, err)
			var createdBy, owner string
			must(t, w.pool.QueryRow(t.Context(), "SELECT created_by,owner_user_id FROM "+qs+".backup_owners").Scan(&createdBy, &owner))
			if createdBy != user.ID || owner != user.ID || len(item.Creators) != 1 || item.Creators[0].UserID != user.ID {
				t.Fatal("restore fixture lacks creator and ownership attribution")
			}
			eraseAuthKitAccount(t, w, a, user.ID)
			must(t, a.engine.AnonymizeAccount(t.Context(), user.ID))
			a.server.Close()
			must(t, a.engine.Close(t.Context()))
			tx, err := w.pool.Begin(t.Context())
			must(t, err)
			defer tx.Rollback(context.Background())
			for _, table := range tables {
				name := qs + "." + pgx.Identifier{table}.Sanitize()
				_, err := tx.Exec(t.Context(), "DELETE FROM "+name+"; INSERT INTO "+name+" SELECT * FROM "+qs+"."+pgx.Identifier{"backup_" + table}.Sanitize())
				must(t, err)
			}
			_, err = tx.Exec(t.Context(), "UPDATE "+qs+".content_items i SET created_by=b.created_by FROM "+qs+".backup_owners b WHERE i.tenant_id=$1 AND i.id=b.content_id", a.cfg.Scope.AppID.String())
			must(t, err)
			_, err = tx.Exec(t.Context(), "UPDATE "+qs+".content_entities e SET owner_user_id=b.owner_user_id FROM "+qs+".backup_owners b WHERE e.tenant_id=$1 AND e.id=b.entity_id", a.cfg.Scope.AppID.String())
			must(t, err)
			must(t, tx.Commit(t.Context()))
			a.cfg.Database.SkipMigrations = true
			bad, err := ck.New(t.Context(), a.cfg, a.deps)
			if bad != nil || !errors.Is(err, ck.ErrErasureReplayRequired) {
				t.Fatalf("restored attribution served before anonymization replay: %v", err)
			}
			recovery, err := ck.NewForErasureReplay(t.Context(), a.cfg, a.deps)
			must(t, err)
			defer func() { must(t, recovery.Close(context.Background())) }()
			replay := recovery.ReplayErasureFences
			if accountReplay {
				replay = recovery.ReplayAccountErasures
			}
			for range 2 {
				report, err := replay(t.Context())
				must(t, err)
				if !report.Complete() {
					t.Fatalf("anonymization replay incomplete: %+v", report)
				}
			}
			var state string
			var complete, receipt, unlinked bool
			must(t, w.pool.QueryRow(t.Context(), `SELECT a.state,a.anonymous_key IS NULL,f.engine_cleanup_complete,
EXISTS(SELECT 1 FROM contentkit_accounts.content_account_erasure_receipts r WHERE r.tenant_id=$3 AND r.app_id=$1::text::uuid AND r.actor_id=$2)
FROM `+qs+`.content_account_lifecycle a JOIN `+qs+`.content_erased_subjects f USING (tenant_id,actor_id)
WHERE a.tenant_id=$1 AND a.actor_id=$2`, a.cfg.Scope.AppID.String(), user.ID, a.cfg.Scope.TenantID.String()).Scan(&state, &unlinked, &complete, &receipt))
			if state != "purged" || !unlinked || !complete || !receipt {
				t.Fatalf("replay did not finish local and account cleanup: %s %v %v %v", state, unlinked, complete, receipt)
			}
			must(t, w.pool.QueryRow(t.Context(), "SELECT coalesce(i.created_by,''),coalesce(e.owner_user_id,'') FROM "+qs+".content_items i CROSS JOIN "+qs+".content_entities e WHERE i.tenant_id=$1 AND i.id=$2 AND e.tenant_id=$1 AND e.id=$3", a.cfg.Scope.AppID.String(), item.ID, channel.ID).Scan(&createdBy, &owner))
			var creators, attributedFavorites int
			must(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+qs+".content_creators WHERE tenant_id=$1 AND user_id=$2", a.cfg.Scope.AppID.String(), user.ID).Scan(&creators))
			must(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+qs+".content_favorites WHERE tenant_id=$1 AND (user_id=$2 OR anonymous_actor_id IS NULL)", a.cfg.Scope.AppID.String(), user.ID).Scan(&attributedFavorites))
			if createdBy != "" || owner != "" || creators != 0 || attributedFavorites != 0 {
				t.Fatal("replay retained creator, owner or favorite attribution")
			}
			a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
			must(t, err)
			a.server = mountEngine(t, a.engine)
			kept := readComment(t, a, item.ID, comment.ID)
			if kept.UserID != "" || kept.AnonName != "<Deleted>" || kept.Body != comment.Body || kept.Author != nil {
				t.Fatalf("replay resurrected comment attribution or lost discussion: %+v", kept)
			}
			favorite, err = a.client("").GetContentFavorite(t.Context(), item.ID, ck.GetContentFavoriteQuery{})
			must(t, err)
			if favorite.Count == nil || *favorite.Count != 1 {
				t.Fatalf("replayed favorite count: got %+v want 1", favorite)
			}
		})
	}
}

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
	tables := []string{"content_favorites", "content_interaction_counts", "content_erased_subjects", "content_account_lifecycle"}
	for _, table := range tables {
		_, err := w.pool.Exec(t.Context(), "CREATE TABLE "+qs+"."+pgx.Identifier{"backup_" + table}.Sanitize()+" AS TABLE "+qs+"."+pgx.Identifier{table}.Sanitize())
		must(t, err)
	}
	must(t, a.engine.AnonymizeAccount(t.Context(), user.ID))
	state, err := client.GetContentFavorite(t.Context(), item.ID, ck.GetContentFavoriteQuery{})
	must(t, err)
	if state.Favorited || state.Count == nil || *state.Count != 1 {
		t.Fatalf("deletion did not keep the favorite anonymously: %+v", state)
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
		if state.Favorited || state.Count == nil || *state.Count != 1 {
			t.Fatalf("replay did not end as live deletion (anonymous, counted once): %+v", state)
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
			erased <- a.engine.AnonymizeAccount(ctx, user.ID)
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
		if state.Favorited || state.Count == nil || *state.Count != 1 {
			t.Fatalf("pre-deletion write kept its attribution or was lost: %+v", state)
		}
		_, err = a.client(token).PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
		requireError(t, err, 403, "forbidden")
		_, sibling := w.person(t, 0)
		state, err = a.client(sibling).PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
		must(t, err)
		if !state.Favorited || state.Count == nil || *state.Count != 2 {
			t.Fatalf("fence blocked an unrelated reader: %+v", state)
		}
	})
}
