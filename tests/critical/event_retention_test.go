package critical_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	catalogstore "github.com/open-rails/contentkit/internal/catalogstore"
	"github.com/open-rails/helpers/eventlog"
)

// TestExpiredEventPositionResyncs reads the log as a host's reader does: it
// applies deletions to its own copy and, when its position has expired under
// the default retention, resyncs from current state and reads on.
func TestExpiredEventPositionResyncs(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	admin := a.engine.Client()
	base := w.clock.now()
	t.Run("CK-J26-S08_expired_position_resyncs_from_current_state", func(t *testing.T) {
		ctx := t.Context()
		kept, gone := publishedArticle(t, a, "kept through the gap"), publishedArticle(t, a, "deleted during the gap")
		followed := []string{kept.Versions[0].ID, gone.Versions[0].ID}
		types := []string{catalog.EventVersionDeleted}
		deleted, cursor := map[string]bool{}, ""
		resync := func() {
			t.Helper()
			statuses, err := admin.GetVersions(ctx, ck.GetVersionsQuery{Ids: followed})
			must(t, err)
			for _, s := range statuses {
				deleted[s.ContentID] = s.Deleted
			}
			cursor = ""
		}
		reader := eventlog.Consumer{Reader: admin.EventReader(), Limit: 100,
			Subscription: eventlog.Subscription{Source: "contentkit", Scope: a.cfg.Scope.AppID.String(), Name: "critical-resync", Types: types},
			Load:         func(context.Context, string) (string, error) { return cursor, nil },
			Commit: func(_ context.Context, _, previous string, page eventlog.Page) error {
				if previous != cursor {
					return errors.New("checkpoint moved underneath the reader")
				}
				for _, e := range page.Events {
					deleted[e.Subject.ID] = true
				}
				cursor = page.NextCursor
				return nil
			}}
		poll := func() error {
			for {
				more, err := reader.Poll(ctx)
				if err != nil || !more {
					return err
				}
			}
		}
		resync()
		must(t, poll())
		saved := cursor
		must(t, admin.DeleteContentVersion(ctx, gone.ID, gone.Versions[0].ID))

		w.clock.set(base.Add(13 * 24 * time.Hour))
		page, err := admin.ListEvents(ctx, catalog.EventQuery{After: saved, Types: types})
		must(t, err)
		if page.RetentionSeconds != int64(14*24*time.Hour/time.Second) || len(page.Events) != 1 || page.Events[0].ContentID != gone.ID {
			t.Fatalf("a 13-day-old position lost retained history: %+v", page)
		}

		w.clock.set(base.Add(15 * 24 * time.Hour))
		if err := poll(); !errors.Is(err, eventlog.ErrExpiredCursor) {
			t.Fatalf("a position past the default retention continued: %v", err)
		}
		runContentPurge(t, w, a)
		var retained int
		must(t, w.pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{w.schema, "eventlog_events"}.Sanitize()+`
 WHERE source='contentkit' AND scope=$1 AND recorded_at < $2`, a.cfg.Scope.AppID.String(), base.Add(24*time.Hour)).Scan(&retained))
		if retained != 0 {
			t.Fatalf("maintenance kept %d facts older than the default retention", retained)
		}

		// Pruned history stays expired at the reader's own time: it resyncs,
		// then reads on from the oldest retained fact.
		w.clock.set(base)
		if err := poll(); !errors.Is(err, eventlog.ErrExpiredCursor) {
			t.Fatalf("a pruned position continued: %v", err)
		}
		resync()
		must(t, poll())
		if !deleted[gone.ID] || deleted[kept.ID] {
			t.Fatalf("resync missed the deletion inside the gap: %v", deleted)
		}
		must(t, admin.DeleteContentVersion(ctx, kept.ID, kept.Versions[0].ID))
		must(t, poll())
		if !deleted[kept.ID] {
			t.Fatal("a fact after the resync did not reach the reader")
		}
	})
}

// runContentPurge runs one content purge, which also prunes the event log.
func runContentPurge(t *testing.T, w *world, a *application) {
	t.Helper()
	startPublicationRuntime(t, a)
	awaitPublicationJobsIdle(t, w, a)
	producer, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
	must(t, err)
	job, err := producer.Insert(t.Context(), scheduledContentJob{kind: "contentkit_content_purge_deleted"}, &river.InsertOpts{Queue: catalogstore.QueueFor(a.cfg.Scope.AppID.String()), MaxAttempts: 1})
	must(t, err)
	waitForWithin(t, 45*time.Second, func() bool {
		current, err := producer.JobGet(t.Context(), job.Job.ID)
		must(t, err)
		if current.State == rivertype.JobStateDiscarded {
			t.Fatalf("purge failed: %+v", current.Errors)
		}
		return current.State == rivertype.JobStateCompleted
	})
}
