package critical_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Only the fixture's River wakeup follows its controlled application clock.
// The actual publication worker still verifies the request's current revision,
// effective due time and completed storage work. Future requests stay scheduled.
func wakeDuePublicationJobs(t *testing.T, a *application) {
	t.Helper()
	at := time.Now()
	if a.deps.Clock != nil {
		at = a.deps.Clock()
	}
	tenant := a.cfg.Tenant
	if a.cfg.Scope != nil {
		tenant = a.cfg.LegacyCatalogKey
		if tenant == "" {
			tenant = a.cfg.Scope.AppID.String()
		}
	}
	schema := a.cfg.Database.RiverSchema
	if schema == "" {
		schema = a.cfg.Database.Schema + "_river"
	}
	requests := pgx.Identifier{a.cfg.Database.Schema, "content_publication_requests"}.Sanitize()
	versions := pgx.Identifier{a.cfg.Database.Schema, "content_versions"}.Sanitize()
	// River fetches against database now(), which the moderation fixture can
	// freeze independently of wall time. Wake to that same clock boundary.
	_, err := a.deps.Postgres.Exec(t.Context(), `UPDATE `+pgx.Identifier{schema, "river_job"}.Sanitize()+` job
 SET scheduled_at=now(),state='available'
 WHERE job.state IN ('scheduled','retryable') AND (job.args->>'due')::timestamptz<=$1
 AND ((job.kind='contentkit_version_release' AND EXISTS
  (SELECT 1 FROM `+requests+` request WHERE request.tenant_id=$2 AND request.id::text=job.args->>'id'))
 OR (job.kind='contentkit_content_publish_due' AND EXISTS
  (SELECT 1 FROM `+versions+` version WHERE version.tenant_id=$2 AND version.id=job.args->>'version'
   AND version.content_id=job.args->>'content')))`, at, tenant)
	must(t, err)
}
