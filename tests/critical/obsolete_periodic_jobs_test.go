package critical_test

import (
	"strings"
	"testing"
	"time"

	ck "github.com/open-rails/contentkit"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

func TestObsoletePeriodicJobsDoNotBlockStartup(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	producer, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
	must(t, err)
	insert := func(kind, queue string) int64 {
		t.Helper()
		job, err := producer.Insert(t.Context(), scheduledContentJob{kind: kind}, &river.InsertOpts{Queue: queue, MaxAttempts: 3})
		must(t, err)
		return job.Job.ID
	}
	old := map[int64]string{}
	foreign := []int64{}
	terminal := []int64{}
	for _, kind := range []string{"contentkit_reclassify", "contentkit_projection_repair"} {
		old[insert(kind, "contentkit")] = kind
		foreign = append(foreign, insert(kind, "other_owner"))
		terminal = append(terminal, insert(kind, "contentkit"))
	}
	_, err = w.pool.Exec(t.Context(), `UPDATE `+w.schema+`_river.river_job SET state='completed',finalized_at=clock_timestamp() WHERE id=ANY($1::bigint[])`, terminal)
	must(t, err)
	control := insert("contentkit_search_sync", "contentkit")
	t.Run("CK-J30-S02_unknown_jobs_fail_finitely_while_supported_work_continues", func(t *testing.T) {
		must(t, ck.MigratePostgres(t.Context(), w.pool, a.cfg))
		a.server.Close()
		must(t, a.engine.Close(t.Context()))
		// Both read-only startup and the ordinary embedded migration path accept
		// obsolete rows. Neither runs a handler or rewrites their history.
		for _, skip := range []bool{true, false} {
			cfg := a.cfg
			cfg.Database.SkipMigrations = skip
			engine, err := ck.New(t.Context(), cfg, a.deps)
			must(t, err)
			_, err = engine.Client().GetKinds(t.Context())
			must(t, err)
			must(t, engine.Close(t.Context()))
		}
		a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
		must(t, err)
		a.server = mountEngine(t, a.engine)
		for id := range old {
			job, err := producer.JobGet(t.Context(), id)
			must(t, err)
			if job.State != rivertype.JobStateAvailable || job.Attempt != 0 {
				t.Fatalf("startup changed obsolete job %d: state=%s attempt=%d", id, job.State, job.Attempt)
			}
		}
		must(t, a.engine.StartJobs(t.Context()))
		waitForWithin(t, time.Minute, func() bool {
			for id := range old {
				job, err := producer.JobGet(t.Context(), id)
				must(t, err)
				if job.State != rivertype.JobStateDiscarded {
					return false
				}
			}
			job, err := producer.JobGet(t.Context(), control)
			must(t, err)
			return job.State == rivertype.JobStateCompleted
		})
		for id, kind := range old {
			job, err := producer.JobGet(t.Context(), id)
			must(t, err)
			if job.Attempt != 3 || len(job.Errors) != 3 {
				t.Fatalf("obsolete job did not exhaust its bounded attempts: %+v", job)
			}
			for _, attempt := range job.Errors {
				if !strings.Contains(attempt.Error, kind) {
					t.Fatalf("obsolete job failed for another cause: %+v", attempt)
				}
			}
		}
		for _, group := range []struct {
			ids   []int64
			state rivertype.JobState
		}{{foreign, rivertype.JobStateAvailable}, {terminal, rivertype.JobStateCompleted}} {
			for _, id := range group.ids {
				job, err := producer.JobGet(t.Context(), id)
				must(t, err)
				if job.State != group.state || job.Attempt != 0 || len(job.Errors) != 0 {
					t.Fatalf("unconsumed/terminal history changed: %+v", job)
				}
			}
		}
	})
}
