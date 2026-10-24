package critical_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/helpers/eventlog"
)

func TestRetainedLog(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	admin := a.engine.Client()
	baseTime := w.clock.now()
	t.Run("CK-J26-S01_committed_domain_effect_and_event_agree", func(t *testing.T) {
		before, err := admin.ListEvents(t.Context(), catalog.EventQuery{Types: []string{"content.created"}})
		must(t, err)
		item := publishedArticle(t, a, "committed event")
		page, err := admin.ListEvents(t.Context(), catalog.EventQuery{After: before.NextCursor, Types: []string{"content.created"}})
		must(t, err)
		if len(page.Events) != 1 || page.Events[0].ContentID != item.ID || page.Events[0].Scope != a.cfg.Scope.AppID.String() || page.Events[0].Type != "content.created" {
			t.Fatalf("committed public item has wrong source fact: %+v", page)
		}
	})
	t.Run("CK-J26-S03_filtered_pagination_has_no_gaps_or_duplicates", func(t *testing.T) {
		first, err := admin.ListEvents(t.Context(), catalog.EventQuery{Types: []string{"content.created"}})
		must(t, err)
		want := []string{}
		for _, title := range []string{"one", "two", "three"} {
			want = append(want, publishedArticle(t, a, title).ID)
		}
		cursor, got := first.NextCursor, []string{}
		for {
			page, err := admin.ListEvents(t.Context(), catalog.EventQuery{After: cursor, Limit: 1, Types: []string{"content.created"}})
			must(t, err)
			for _, event := range page.Events {
				if event.Type != "content.created" {
					t.Fatal("filter returned an unselected type")
				}
				got = append(got, event.ContentID)
			}
			if !page.HasMore {
				break
			}
			if page.NextCursor == cursor || len(got) > len(want) {
				t.Fatal("filtered cursor failed to make bounded progress")
			}
			cursor = page.NextCursor
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("filtered committed sequence = %v; want %v", got, want)
		}
	})
	t.Run("CK-J26-S04_new_reader_resumes_its_own_cursor", func(t *testing.T) {
		key, err := w.tenant[0].CreateAPIKey(t.Context(), iam.SystemIdentity(), a.group, iam.NewAPIKey{Name: "event reader", Role: w.viewer})
		must(t, err)
		old := a.client(key.Secret)
		checkpoint, err := old.ListEvents(t.Context(), catalog.EventQuery{})
		must(t, err)
		item := publishedArticle(t, a, "after reader restart")
		next, err := a.client(key.Secret).EventReader().ListEvents(t.Context(), eventlog.Query{After: checkpoint.NextCursor})
		must(t, err)
		found := false
		for _, event := range next.Events {
			if event.Subject.ID == item.ID {
				found = true
			}
		}
		if !found {
			t.Fatal("new remote client missed committed work after its saved cursor")
		}
	})
	t.Run("CK-J26-S05_reads_do_not_ack_or_delete_source_facts", func(t *testing.T) {
		query := catalog.EventQuery{Types: []string{"content.published"}}
		one, err := admin.ListEvents(t.Context(), query)
		must(t, err)
		two, err := admin.ListEvents(t.Context(), query)
		must(t, err)
		if len(one.Events) == 0 || !reflect.DeepEqual(one, two) {
			t.Fatal("reading changed the immutable source page")
		}
	})
	t.Run("CK-J26-S06_foreign_malformed_and_expired_cursors_are_explicit", func(t *testing.T) {
		w.clock.set(baseTime)
		page, err := admin.ListEvents(t.Context(), catalog.EventQuery{})
		must(t, err)
		_, err = w.apps[1].engine.Client().ListEvents(t.Context(), catalog.EventQuery{After: page.NextCursor})
		requireError(t, err, 400, "invalid_request")
		_, err = admin.ListEvents(t.Context(), catalog.EventQuery{After: "not-a-cursor"})
		requireError(t, err, 400, "invalid_request")
		publishedArticle(t, a, "retention boundary")
		w.clock.set(baseTime.Add(91 * 24 * time.Hour))
		_, err = admin.ListEvents(t.Context(), catalog.EventQuery{After: page.NextCursor})
		requireError(t, err, 410, "event_cursor_expired")
		w.clock.set(baseTime)
	})
	t.Run("CK-J26-S07_rollback_cannot_leave_an_event_without_content", func(t *testing.T) {
		before, err := admin.ListEvents(t.Context(), catalog.EventQuery{})
		must(t, err)
		qs := pgx.Identifier{w.schema}.Sanitize()
		// A real deferred database constraint forces commit failure. The
		// assertions remain public content/log reads, not SQL text matching.
		_, err = w.pool.Exec(t.Context(), `CREATE FUNCTION `+qs+`.critical_reject() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.original_title='rollback-fixture' THEN RAISE EXCEPTION 'critical injected commit failure'; END IF; RETURN NEW; END$$;
CREATE CONSTRAINT TRIGGER critical_reject AFTER INSERT ON `+qs+`.content_items DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION `+qs+`.critical_reject()`)
		must(t, err)
		defer func() {
			_, err := w.pool.Exec(context.Background(), `DROP TRIGGER critical_reject ON `+qs+`.content_items; DROP FUNCTION `+qs+`.critical_reject()`)
			must(t, err)
		}()
		_, err = admin.PostContents(t.Context(), catalog.ContentInput{Kind: "article", OriginalTitle: "rollback-fixture"})
		requireError(t, err, 500, "internal_error")
		items, err := admin.GetManage(t.Context(), ck.GetManageQuery{Q: "rollback-fixture"})
		must(t, err)
		if len(items) != 0 {
			t.Fatal("rolled-back content remained visible")
		}
		after, err := admin.ListEvents(t.Context(), catalog.EventQuery{After: before.NextCursor})
		must(t, err)
		if len(after.Events) != 0 {
			t.Fatalf("rollback emitted source facts: %+v", after.Events)
		}
	})
	t.Run("CK-J26-S02_delayed_commit_cannot_fall_behind_a_reader_cursor", func(t *testing.T) {
		one, two := publishedArticle(t, a, "delayed"), publishedArticle(t, a, "concurrent")
		checkpoint, err := admin.ListEvents(t.Context(), catalog.EventQuery{Types: []string{"content.updated"}})
		must(t, err)
		qs := pgx.Identifier{w.schema}.Sanitize()
		const barrier int64 = 612900126
		holder, err := w.pool.Acquire(t.Context())
		must(t, err)
		_, err = holder.Exec(t.Context(), "SELECT pg_advisory_lock($1)", barrier)
		must(t, err)
		defer func() {
			_, _ = holder.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", barrier)
			holder.Release()
		}()
		_, err = w.pool.Exec(t.Context(), fmt.Sprintf(`CREATE FUNCTION %s.critical_delay() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.subject_id=TG_ARGV[0] THEN PERFORM pg_advisory_xact_lock(%d); END IF; RETURN NEW; END$$;
CREATE CONSTRAINT TRIGGER zzz_critical_delay AFTER INSERT ON %s.eventlog_pending DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION %s.critical_delay('%s')`, qs, barrier, qs, qs, one.ID))
		must(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		first, second := make(chan error, 1), make(chan error, 1)
		update := func(id, title string, done chan<- error) {
			_, err := admin.PatchContent(ctx, id, catalog.ContentPatch{OriginalTitle: &title})
			done <- err
		}
		go update(one.ID, "first request waits at commit", first)
		waitFor(t, func() bool {
			var blocked int
			err := w.pool.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objid=$1 AND NOT granted", barrier).Scan(&blocked)
			must(t, err)
			return blocked > 0
		})
		go update(two.ID, "second request", second)
		secondFinished := false
		waitFor(t, func() bool {
			select {
			case err := <-second:
				must(t, err)
				secondFinished = true
				return true
			default:
			}
			var blocked int
			must(t, w.pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'").Scan(&blocked))
			return blocked >= 2
		})
		beforeRelease, err := admin.ListEvents(ctx, catalog.EventQuery{After: checkpoint.NextCursor, Types: []string{"content.updated"}})
		must(t, err)
		for _, event := range beforeRelease.Events {
			if event.ContentID == one.ID {
				t.Fatal("uncommitted first mutation was visible")
			}
		}
		_, err = holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", barrier)
		must(t, err)
		must(t, <-first)
		if !secondFinished {
			must(t, <-second)
		}
		afterRelease, err := admin.ListEvents(ctx, catalog.EventQuery{After: beforeRelease.NextCursor, Types: []string{"content.updated"}})
		must(t, err)
		seen := map[string]int{}
		for _, page := range []catalog.EventPage{beforeRelease, afterRelease} {
			for _, event := range page.Events {
				seen[event.ContentID]++
			}
		}
		if seen[one.ID] != 1 || seen[two.ID] != 1 || len(seen) != 2 {
			t.Fatalf("committed facts were skipped or duplicated around the held transaction: %v", seen)
		}
		_, err = w.pool.Exec(ctx, `DROP TRIGGER zzz_critical_delay ON `+qs+`.eventlog_pending; DROP FUNCTION `+qs+`.critical_delay()`)
		must(t, err)
	})
}

func waitFor(t *testing.T, ready func() bool) {
	t.Helper()
	waitForWithin(t, 10*time.Second, ready)
}

func waitForWithin(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()
	timer, tick := time.NewTimer(timeout), time.NewTicker(50*time.Millisecond)
	defer timer.Stop()
	defer tick.Stop()
	for {
		if ready() {
			return
		}
		select {
		case <-tick.C:
		case <-timer.C:
			t.Fatal("owned asynchronous condition did not complete")
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
}
