package critical_test

import (
	"errors"
	"testing"
	"time"

	versionstore "github.com/open-rails/contentkit/internal/versionstore"

	"github.com/jackc/pgx/v5"
)

// publicationJob is a version's release job (contentkit_publication): one
// per version, woken by catalog changes and manifest writes and snoozed
// until the version's next deadline.
type publicationJob struct {
	ID    int64
	Due   time.Time // when River next runs it, in database time
	State string
}

// releaseArgs are a release job's arguments, for replaying one.
type releaseArgs struct {
	Content string `json:"content"`
	Version string `json:"version"`
}

func (releaseArgs) Kind() string { return "contentkit_publication" }

// currentPublicationJob is version's unfinished release job; zero when it
// has none (the last run found nothing further to wait for).
func currentPublicationJob(t *testing.T, a *application, version string) publicationJob {
	t.Helper()
	var job publicationJob
	err := a.deps.Postgres.QueryRow(t.Context(), `SELECT id,scheduled_at,state FROM `+
		pgx.Identifier{publicationRiverSchema(a), "river_job"}.Sanitize()+`
 WHERE kind='contentkit_publication' AND queue=$1 AND args->>'version'=$2
 AND state NOT IN ('completed','cancelled','discarded') ORDER BY id DESC LIMIT 1`,
		versionstore.QueueFor(publicationTenant(a)), version).Scan(&job.ID, &job.Due, &job.State)
	if !errors.Is(err, pgx.ErrNoRows) {
		must(t, err)
	}
	return job
}

// publicationJobs fingerprints version's release jobs: comparing it shows
// whether anything woke or added one.
func publicationJobs(t *testing.T, a *application, version string) string {
	t.Helper()
	var out string
	must(t, a.deps.Postgres.QueryRow(t.Context(), `SELECT coalesce(string_agg(id::text||'@'||scheduled_at::text||'@'||state,',' ORDER BY id),'') FROM `+
		pgx.Identifier{publicationRiverSchema(a), "river_job"}.Sanitize()+` WHERE kind='contentkit_publication' AND args->>'version'=$1`, version).Scan(&out))
	return out
}

func publicationTenant(a *application) string {
	return a.cfg.Scope.AppID.String()
}

func publicationRiverSchema(a *application) string {
	if a.cfg.Database.RiverSchema != "" {
		return a.cfg.Database.RiverSchema
	}
	return a.cfg.Database.Schema + "_river"
}

// awaitDuePublications runs every waiting release once and waits until none
// is running or ready to run. A release plans from the application's clock
// and snoozes until its next deadline, so waking one early only re-plans.
func awaitDuePublications(t *testing.T, a *application) {
	t.Helper()
	wakeDuePublicationJobs(t, a)
	waitForChapterPublication(t, func() bool {
		var discarded, pending int
		must(t, a.deps.Postgres.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE state='discarded'),
 count(*) FILTER (WHERE state IN ('available','pending','running','retryable')) FROM `+
			pgx.Identifier{publicationRiverSchema(a), "river_job"}.Sanitize()+`
 WHERE kind='contentkit_publication' AND queue=$1`,
			versionstore.QueueFor(publicationTenant(a))).Scan(&discarded, &pending))
		if discarded != 0 {
			t.Fatalf("%d publication jobs were discarded", discarded)
		}
		return pending == 0
	})
}

// wakeDuePublicationJobs runs the application's waiting releases now. River
// fetches against database now(), which the moderation fixture can freeze
// independently of wall time, and releases snooze by the application's
// clock: each re-plans and snoozes again until what is really due.
func wakeDuePublicationJobs(t *testing.T, a *application) {
	t.Helper()
	_, err := a.deps.Postgres.Exec(t.Context(), `UPDATE `+pgx.Identifier{publicationRiverSchema(a), "river_job"}.Sanitize()+`
 SET scheduled_at=now(),state='available'
 WHERE state IN ('scheduled','retryable') AND kind='contentkit_publication' AND queue=$1`,
		versionstore.QueueFor(publicationTenant(a)))
	must(t, err)
}
