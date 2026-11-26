package critical_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/internal/jobhook"
	signalstore "github.com/open-rails/contentkit/internal/signalstore"
	"github.com/open-rails/contentkit/signal"
)

// preferenceSyncJob queues the engine's own preference job by its kind.
type preferenceSyncJob struct{}

func (preferenceSyncJob) Kind() string { return "contentkit_preference_sync" }

type preferenceKey struct{ axis, user, content string }

// TestPreferenceSync drives the real preference job on a River fleet whose
// jobs get one second each, over a legacy-sized backlog of imported
// reactions and favorites spread across a year. Merges of the owned signals
// table are stopped, so its row count is every row ever sent. Only time is
// controlled: samples of the revision sequence settle by the engine clock.
func TestPreferenceSync(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	configureDiscoverySignals(t, w, a)
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.cfg.Database.SkipMigrations = true
	var err error
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	var probe func(context.Context) error
	for _, p := range a.engine.Probes() {
		if p.Name == "contentkit_clickhouse" {
			probe = p.Check
		}
	}
	if probe == nil {
		t.Fatal("missing signal-plane probe")
	}
	waitForWithin(t, 30*time.Second, func() bool { return probe(t.Context()) == nil })

	ctx := t.Context()
	tenant := a.cfg.Scope.AppID.String()
	db := a.cfg.ClickHouse.Database
	admin := a.engine.Client()
	const works, people = 100, 100
	var records []importer.ContentRecord
	for i := range works {
		records = append(records, importer.ContentRecord{ExternalID: fmt.Sprintf("pref-%03d", i), Kind: "article", State: "draft",
			Access: &access.Rule{Level: access.Public}})
	}
	imported, err := admin.PostImport(ctx, importer.Batch{Source: "preference-backlog", Contents: records})
	must(t, err)
	if len(imported.Failed) != 0 || len(imported.IDs[importer.RecordContent]) != works {
		t.Fatalf("content import: %+v", imported.Failed)
	}
	ids := make([]string, works)
	for i := range works {
		ids[i] = imported.IDs[importer.RecordContent][fmt.Sprintf("pref-%03d", i)]
	}
	users := make([]string, people)
	for i := range users {
		users[i] = uuid.NewString()
	}
	want := map[preferenceKey]float64{}
	since := time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC)
	var reactions []importer.ReactionRecord
	var favorites []importer.FavoriteRecord
	for i, id := range ids {
		for u, user := range users {
			at := since.AddDate(0, (i*7+u)%12, 0)
			value := int16(1 - 2*((i+u)%2))
			target := importer.TargetRef{Content: &importer.Ref{ID: id}}
			reactions = append(reactions, importer.ReactionRecord{Target: target, UserID: user, Value: value, CreatedAt: &at, UpdatedAt: &at})
			favorites = append(favorites, importer.FavoriteRecord{Target: target, UserID: user, CreatedAt: &at})
			want[preferenceKey{"reaction", user, id}] = float64(value)
			want[preferenceKey{"favorite", user, id}] = 1
		}
	}
	importPreferences := func(t *testing.T, source string, reactions []importer.ReactionRecord, favorites []importer.FavoriteRecord) {
		t.Helper()
		res, err := admin.PostImport(ctx, importer.Batch{Source: source, Reactions: reactions, Favorites: favorites})
		must(t, err)
		if len(res.Failed) != 0 || res.Written[importer.SectionReactions] != len(reactions) || res.Written[importer.SectionFavorites] != len(favorites) {
			t.Fatalf("preference import %s: %+v", source, res)
		}
	}
	importPreferences(t, "preference-backlog", reactions, nil)
	importPreferences(t, "preference-backlog", nil, favorites)
	backlog := uint64(len(want))

	must(t, a.deps.ClickHouse.Exec(ctx, "SYSTEM STOP MERGES "+db+".signals"))
	scalar := func(t *testing.T, query string, args []any, dest ...any) {
		t.Helper()
		rows, err := a.deps.ClickHouse.Query(ctx, query, args...)
		must(t, err)
		defer rows.Close()
		if !rows.Next() {
			t.Fatalf("no row: %s", query)
		}
		must(t, rows.Scan(dest...))
		must(t, rows.Err())
	}
	sent := func(t *testing.T) (uint64, time.Time) {
		t.Helper()
		var n uint64
		var latest time.Time
		scalar(t, `SELECT count(), max(ingested_at) FROM `+db+`.signals WHERE tenant = ? AND event_id = 'current'`, []any{tenant}, &n, &latest)
		return n, latest
	}
	current := func(t *testing.T) map[preferenceKey]float64 {
		t.Helper()
		rows, err := a.deps.ClickHouse.Query(ctx, `SELECT signal_type, subject, content_id, value FROM `+db+`.signals FINAL
 WHERE tenant = ? AND event_id = 'current' AND subject_kind = 'user'`, tenant)
		must(t, err)
		defer rows.Close()
		got := map[preferenceKey]float64{}
		for rows.Next() {
			var k preferenceKey
			var v float64
			must(t, rows.Scan(&k.axis, &k.user, &k.content, &v))
			got[k] = v
		}
		must(t, rows.Err())
		return got
	}
	riverJobs := pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize()
	jobs := func(t *testing.T, states ...string) int {
		t.Helper()
		var n int
		must(t, w.pool.QueryRow(ctx, `SELECT count(*) FROM `+riverJobs+` WHERE kind = 'contentkit_preference_sync' AND state::text = ANY($1)`, states).Scan(&n))
		return n
	}
	failures := func(t *testing.T) []string {
		t.Helper()
		var errs []string
		must(t, w.pool.QueryRow(ctx, `SELECT coalesce(array_agg(e::text), '{}') FROM `+riverJobs+`, unnest(errors) e
 WHERE kind = 'contentkit_preference_sync'`).Scan(&errs))
		return errs
	}

	fleet, err := jobhook.Fleet(ctx, a.engine, &river.Config{Schema: w.schema + "_river", JobTimeout: time.Second,
		Queues: map[string]river.QueueConfig{"contentkit": {MaxWorkers: 1}}})
	must(t, err)
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		must(t, fleet.Stop(stop))
	})
	must(t, fleet.Start(ctx))
	run := func(t *testing.T) {
		t.Helper()
		res, err := fleet.Insert(ctx, preferenceSyncJob{}, &river.InsertOpts{Queue: "contentkit"})
		must(t, err)
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			job, err := fleet.JobGet(ctx, res.Job.ID)
			must(t, err)
			switch {
			case job.State == "completed":
				return
			case job.State == "discarded" || job.State == "cancelled" || time.Now().After(deadline):
				t.Fatalf("preference job %d is %s after %d attempts: %+v", job.ID, job.State, job.Attempt, job.Errors)
			}
		}
	}

	tick := func(d time.Duration) { w.clock.set(w.clock.now().Add(d)) }
	// A write settles once a sample taken after it is an overlap old.
	settle := func() { tick(content.DefaultPreferenceSyncOverlap + time.Minute) }
	// The start-up run samples the revision sequence after the backlog.
	waitForWithin(t, 30*time.Second, func() bool { return jobs(t, "completed", "retryable", "discarded") > 0 })

	t.Run("CK-J07-S34_backlog_completes_across_deadline_limited_runs_without_resending", func(t *testing.T) {
		settle()
		run(t)
		for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
			var n uint64
			scalar(t, `SELECT count() FROM `+db+`.signals FINAL WHERE tenant = ? AND event_id = 'current'`, []any{tenant}, &n)
			if n >= backlog {
				break
			}
			if time.Now().After(deadline) {
				sends, _ := sent(t)
				t.Fatalf("the backlog never completed: %d of %d rows after %d sends; job errors: %v", n, backlog, sends, failures(t))
			}
		}
		waitForWithin(t, 30*time.Second, func() bool { return jobs(t, "available", "running", "retryable", "scheduled", "pending") == 0 })
		// The start-up run, this one and at least one that resumed.
		if runs := jobs(t, "completed"); runs < 3 {
			t.Fatalf("backlog of %d rows finished in %d runs of a one-second job; it was not deadline-limited", backlog, runs)
		}
		got := current(t)
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("signal plane holds %v=%v; want %v", k, got[k], v)
			}
		}
		// A run its deadline cut mid-batch re-sends that batch only.
		cut := failures(t)
		if n, _ := sent(t); n < backlog || n > backlog+uint64(len(cut))*1000 {
			t.Fatalf("%d rows sent for a backlog of %d with %d cut runs: %v", n, backlog, len(cut), cut)
		}
	})

	t.Run("CK-J07-S34_a_run_with_nothing_new_sends_nothing", func(t *testing.T) {
		before, latest := sent(t)
		run(t)
		settle()
		run(t)
		if n, at := sent(t); n != before || !at.Equal(latest) {
			t.Fatalf("an idle run sent rows: %d -> %d, latest %s -> %s", before, n, latest, at)
		}
	})

	t.Run("CK-J07-S34_a_change_is_sent_once_after_it_settles", func(t *testing.T) {
		before, _ := sent(t)
		changed := importer.ReactionRecord{Target: importer.TargetRef{Content: &importer.Ref{ID: ids[0]}}, UserID: users[0], Value: 0}
		removed := importer.FavoriteRecord{Target: importer.TargetRef{Content: &importer.Ref{ID: ids[1]}}, UserID: users[1], Removed: true}
		importPreferences(t, "preference-backlog", []importer.ReactionRecord{changed}, []importer.FavoriteRecord{removed})
		want[preferenceKey{"reaction", users[0], ids[0]}] = 0
		want[preferenceKey{"favorite", users[1], ids[1]}] = 0
		tick(time.Minute)
		run(t) // samples the sequence past the change; nothing is settled yet
		if n, _ := sent(t); n != before {
			t.Fatalf("an unsettled change was sent: %d -> %d", before, n)
		}
		settle()
		run(t)
		if n, _ := sent(t); n != before+2 {
			t.Fatalf("two changes sent %d rows", n-before)
		}
		got := current(t)
		for _, k := range []preferenceKey{{"reaction", users[0], ids[0]}, {"favorite", users[1], ids[1]}} {
			if got[k] != want[k] {
				t.Fatalf("signal plane holds %v=%v; want %v", k, got[k], want[k])
			}
		}
	})

	t.Run("CK-J23-S09_a_preference_batch_racing_an_erasure_leaves_no_rows", func(t *testing.T) {
		must(t, a.deps.ClickHouse.Exec(ctx, "SYSTEM START MERGES "+db+".signals"))
		store, err := signalstore.NewStore(a.deps.ClickHouse, db)
		must(t, err)
		// The signal plane's fence lands after the batch read Postgres.
		erased := signal.Subject{UserID: users[2]}
		_, err = store.EraseSubjects(ctx, []string{tenant}, []signal.Subject{erased})
		must(t, err)
		importPreferences(t, "preference-backlog", []importer.ReactionRecord{
			{Target: importer.TargetRef{Content: &importer.Ref{ID: ids[2]}}, UserID: users[2], Value: 0},
			{Target: importer.TargetRef{Content: &importer.Ref{ID: ids[2]}}, UserID: users[3], Value: 0},
		}, nil)
		tick(time.Minute)
		run(t)
		settle()
		run(t)
		var left, kept uint64
		scalar(t, `SELECT countIf(subject = ?), countIf(subject = ? AND content_id = ? AND signal_type = 'reaction' AND value = 0)
 FROM `+db+`.signals WHERE tenant = ?`, []any{users[2], users[3], ids[2], tenant}, &left, &kept)
		if left != 0 || kept != 1 {
			t.Fatalf("erased account rows=%d; concurrent change rows=%d", left, kept)
		}
	})
}
