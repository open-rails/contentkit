package critical_test

import (
	"testing"
	"time"

	versionstore "github.com/open-rails/contentkit/internal/versionstore"

	"github.com/jackc/pgx/v5"
)

// releaseJob replays the existing request-ID contract; it never manufactures a
// publication intent or turns a broad scan into executable work.
type releaseJob struct {
	ID    string    `json:"id"`
	Due   time.Time `json:"due"`
	After int64     `json:"after,omitempty"`
}

func (releaseJob) Kind() string { return "contentkit_version_release" }

func currentReleaseJob(t *testing.T, a *application, version string) releaseJob {
	t.Helper()
	var args releaseJob
	must(t, a.deps.Postgres.QueryRow(t.Context(), `SELECT id::text,scheduled_at FROM `+
		pgx.Identifier{a.cfg.Database.Schema, "content_publication_requests"}.Sanitize()+`
 WHERE tenant_id=$1 AND version_id=$2 AND current`, publicationTenant(a), version).Scan(&args.ID, &args.Due))
	return args
}

func publicationTenant(a *application) string {
	if a.cfg.Scope == nil {
		return a.cfg.Tenant
	}
	if a.cfg.LegacyCatalogKey != "" {
		return a.cfg.LegacyCatalogKey
	}
	return a.cfg.Scope.AppID.String()
}

func publicationNow(a *application) time.Time {
	if a.deps.Clock != nil {
		return a.deps.Clock()
	}
	return time.Now()
}

func publicationRiverSchema(a *application) string {
	if a.cfg.Database.RiverSchema != "" {
		return a.cfg.Database.RiverSchema
	}
	return a.cfg.Database.Schema + "_river"
}

// awaitDuePublications follows only already admitted exact requests. A missing
// obligation cannot be repaired by this fixture, unlike the removed sweep.
func awaitDuePublications(t *testing.T, a *application) {
	t.Helper()
	waitForChapterPublication(t, func() bool {
		wakeDuePublicationJobs(t, a)
		var discarded, pending int
		must(t, a.deps.Postgres.QueryRow(t.Context(), `SELECT count(*) FROM `+
			pgx.Identifier{publicationRiverSchema(a), "river_job"}.Sanitize()+`
 WHERE kind='contentkit_version_release' AND queue=$1 AND state='discarded'`,
			versionstore.QueueFor(publicationTenant(a))).Scan(&discarded))
		if discarded != 0 {
			t.Fatalf("%d exact publication jobs were discarded", discarded)
		}
		must(t, a.deps.Postgres.QueryRow(t.Context(), `SELECT count(*) FROM `+
			pgx.Identifier{a.cfg.Database.Schema, "content_publication_requests"}.Sanitize()+`
 WHERE tenant_id=$1 AND current AND scheduled_at<=$2 AND state NOT IN ('completed','cancelled')`,
			publicationTenant(a), publicationNow(a)).Scan(&pending))
		return pending == 0
	})
}

// Only the fixture's River wakeup follows its controlled application clock.
// The actual publication worker still verifies the request's current revision,
// effective due time and completed storage work. Future requests stay scheduled.
func wakeDuePublicationJobs(t *testing.T, a *application) {
	t.Helper()
	requests := pgx.Identifier{a.cfg.Database.Schema, "content_publication_requests"}.Sanitize()
	// River fetches against database now(), which the moderation fixture can
	// freeze independently of wall time. Wake to that same clock boundary.
	_, err := a.deps.Postgres.Exec(t.Context(), `UPDATE `+pgx.Identifier{publicationRiverSchema(a), "river_job"}.Sanitize()+` job
 SET scheduled_at=now(),state='available'
 WHERE job.state IN ('scheduled','retryable') AND (job.args->>'due')::timestamptz<=$1
 AND job.kind='contentkit_version_release' AND EXISTS
  (SELECT 1 FROM `+requests+` request WHERE request.tenant_id=$2 AND request.id::text=job.args->>'id')`, publicationNow(a), publicationTenant(a))
	must(t, err)
}
