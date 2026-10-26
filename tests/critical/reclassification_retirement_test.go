package critical_test

import (
	"strings"
	"testing"

	ck "github.com/open-rails/contentkit"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

type retiredPollJob struct {
	Tenant string `json:"tenant,omitempty"`
}

func (retiredPollJob) Kind() string { return "contentkit_reclassify" }

func TestClassificationJobRetirement(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	producer, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
	must(t, err)
	insert := func(queue, tenant string) int64 {
		t.Helper()
		job, err := producer.Insert(t.Context(), retiredPollJob{Tenant: tenant}, &river.InsertOpts{Queue: queue})
		must(t, err)
		return job.Job.ID
	}
	waiting := insert(ck.QueueContentKit, "")
	foreign := insert("other_poll_owner", "")
	terminal := insert(ck.QueueContentKit, "")
	_, err = w.pool.Exec(t.Context(), `UPDATE `+w.schema+`_river.river_job SET state='completed',finalized_at=clock_timestamp() WHERE id=$1`, terminal)
	must(t, err)
	assertState := func(id int64, want rivertype.JobState) {
		t.Helper()
		job, err := producer.JobGet(t.Context(), id)
		must(t, err)
		if job.State != want {
			t.Fatalf("job%d state=%s want%s", id, job.State, want)
		}
	}
	t.Run("CK-J30-S02_retirement_requires_exact_reviewed_stopped_work", func(t *testing.T) {
		cfg := a.cfg
		cfg.Database.SkipMigrations = true
		engine, err := ck.New(t.Context(), cfg, a.deps)
		if engine != nil {
			_ = engine.Close(t.Context())
			t.Fatal("runtime silently adopted ambiguous work")
		}
		if err == nil || !strings.Contains(err.Error(), "RetireReclassificationJobs") {
			t.Fatalf("missing actionable startup refusal: %v", err)
		}
		err = ck.MigratePostgres(t.Context(), w.pool, a.cfg)
		if err == nil || !strings.Contains(err.Error(), "no app identity") {
			t.Fatalf("migration guessed job ownership: %v", err)
		}
		assertState(waiting, rivertype.JobStateAvailable)
		err = ck.MigratePostgres(t.Context(), w.pool, a.cfg, ck.MigrationOptions{RetireReclassificationJobs: []int64{waiting, foreign}})
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("foreign queue accepted: %v", err)
		}
		assertState(waiting, rivertype.JobStateAvailable)
		_, err = w.pool.Exec(t.Context(), `UPDATE `+w.schema+`_river.river_job SET state='running',attempted_at=clock_timestamp() WHERE id=$1`, waiting)
		must(t, err)
		err = ck.MigratePostgres(t.Context(), w.pool, a.cfg, ck.MigrationOptions{RetireReclassificationJobs: []int64{waiting}})
		if err == nil || !strings.Contains(err.Error(), "running") {
			t.Fatalf("running classifier accepted: %v", err)
		}
		assertState(waiting, rivertype.JobStateRunning)
		_, err = w.pool.Exec(t.Context(), `UPDATE `+w.schema+`_river.river_job SET state='available' WHERE id=$1`, waiting)
		must(t, err)
		for range 2 {
			must(t, ck.MigratePostgres(t.Context(), w.pool, a.cfg, ck.MigrationOptions{RetireReclassificationJobs: []int64{waiting, terminal}}))
		}
		assertState(waiting, rivertype.JobStateCancelled)
		assertState(terminal, rivertype.JobStateCompleted)
		assertState(foreign, rivertype.JobStateAvailable)
		engine, err = ck.New(t.Context(), cfg, a.deps)
		must(t, err)
		must(t, engine.Close(t.Context()))
	})
}
