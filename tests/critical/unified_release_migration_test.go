package critical_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

func TestUnifiedReleaseMigration(t *testing.T) {
	w := newWorld(t)
	a, b := w.apps[0], w.apps[1]
	tenant := publicationTenant(a)
	items := pgx.Identifier{w.schema, "content_items"}.Sanitize()
	editions := pgx.Identifier{w.schema, "content_versions"}.Sanitize()
	requests := pgx.Identifier{w.schema, "content_publication_requests"}.Sanitize()
	jobs := pgx.Identifier{publicationRiverSchema(a), "river_job"}.Sanitize()
	create := func(app *application, name string) catalog.Content {
		t.Helper()
		item, err := app.engine.Client().PostContents(t.Context(), catalog.ContentInput{Kind: "article", Text: map[string]catalog.Text{"en": {Title: name, Body: name}}})
		must(t, err)
		return item
	}
	baseline, deleted := create(a, "Existing publication"), create(a, "Retained publication")
	future, immediate := create(a, "Accepted future"), create(a, "Accepted without sweep")
	draft, foreign := create(a, "Unaccepted draft"), create(b, "Foreign accepted publication")
	frozen := create(a, "Frozen request")
	past := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	due := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Microsecond)
	policy := access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}, Releases: []access.Release{{At: due, Level: access.Public}}}
	released := access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}}
	for _, item := range []catalog.Content{baseline, deleted} {
		state := "published"
		if item.ID == deleted.ID {
			state = "deleted"
		}
		_, err := w.pool.Exec(t.Context(), `UPDATE `+items+` SET state=$2,published_at=$3 WHERE id=$1`, item.ID, state, past)
		must(t, err)
		_, err = w.pool.Exec(t.Context(), `UPDATE `+editions+` SET state=$2,published_at=$3,released=true,access=$4,released_access=$5 WHERE id=$1`, item.Versions[0].ID, state, past, policy, released)
		must(t, err)
	}
	for _, item := range []catalog.Content{future, immediate, foreign, frozen} {
		_, err := w.pool.Exec(t.Context(), `UPDATE `+items+` SET state='published' WHERE id=$1`, item.ID)
		must(t, err)
		var at *time.Time
		if item.ID == future.ID {
			at = &due
		}
		_, err = w.pool.Exec(t.Context(), `UPDATE `+editions+` SET state='published',publish_at=$2 WHERE id=$1`, item.Versions[0].ID, at)
		must(t, err)
	}
	frozenID, retiringID := uuid.NewString(), uuid.NewString()
	frozenPlan := json.RawMessage(`{"fingerprint":"accepted-frozen-input","view":null,"copies":[]}`)
	_, err := w.pool.Exec(t.Context(), `INSERT INTO `+requests+` (tenant_id,id,content_id,version_id,scheduled_at,state,plan,current)
 VALUES($1,$2,$3,$4,$5,'failed',$6,true),($1,$7,$3,$4,$5,'retiring',$6,false)`, tenant, frozenID, frozen.ID, frozen.Versions[0].ID, due, frozenPlan, retiringID)
	must(t, err)
	producer, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: publicationRiverSchema(a)})
	must(t, err)
	insert := func(kind, queue string, args any, at time.Time) int64 {
		t.Helper()
		raw, err := json.Marshal(args)
		must(t, err)
		job, err := producer.Insert(t.Context(), publicationMigrationJob{kind: kind, args: raw}, &river.InsertOpts{Queue: queue, ScheduledAt: at, MaxAttempts: 8})
		must(t, err)
		return job.Job.ID
	}
	oldArgs := map[string]any{"content": future.ID, "version": future.Versions[0].ID, "due": due}
	targeted := insert("contentkit_catalog_publish_due", catalog.QueueFor(tenant), oldArgs, due)
	// A stale later wakeup must not delay a currently due request after polling ends.
	late := insert("contentkit_content_publish_due", catalog.QueueFor(tenant), map[string]any{"content": immediate.ID, "version": immediate.Versions[0].ID, "due": due}, due)
	foreignJob := insert("contentkit_content_publish_due", "another_application", oldArgs, due)
	terminal := insert("contentkit_content_publish_due", catalog.QueueFor(tenant), oldArgs, due)
	_, err = w.pool.Exec(t.Context(), `UPDATE `+jobs+` SET state='completed',finalized_at=now() WHERE id=$1`, terminal)
	must(t, err)
	_, err = w.pool.Exec(t.Context(), `UPDATE `+jobs+` SET state='running',attempt=2,attempted_at=now() WHERE id=$1`, targeted)
	must(t, err)
	before, err := a.engine.Client().ListEvents(t.Context(), catalog.EventQuery{})
	must(t, err)
	if err := ck.MigratePostgres(t.Context(), w.pool, a.cfg); err == nil || !strings.Contains(err.Error(), "stop legacy publication workers") {
		t.Fatalf("running legacy worker did not refuse handoff: %v", err)
	}
	_, err = w.pool.Exec(t.Context(), `UPDATE `+jobs+` SET state='scheduled' WHERE id=$1`, targeted)
	must(t, err)
	badArgs, err := json.Marshal(map[string]any{"content": foreign.ID, "version": foreign.Versions[0].ID, "due": due})
	must(t, err)
	_, err = w.pool.Exec(t.Context(), `UPDATE `+jobs+` SET args=$2 WHERE id=$1`, targeted, badArgs)
	must(t, err)
	if err := ck.MigratePostgres(t.Context(), w.pool, a.cfg); err == nil || !strings.Contains(err.Error(), "ambiguous catalog ownership") {
		t.Fatalf("cross-catalog target was silently converted: %v", err)
	}
	var untouched bool
	must(t, w.pool.QueryRow(t.Context(), `SELECT release_id IS NULL FROM `+editions+` WHERE id=$1`, baseline.Versions[0].ID).Scan(&untouched))
	if !untouched {
		t.Fatal("failed handoff partially adopted reader state")
	}
	originalArgs, err := json.Marshal(oldArgs)
	must(t, err)
	_, err = w.pool.Exec(t.Context(), `UPDATE `+jobs+` SET args=$2 WHERE id=$1`, targeted, originalArgs)
	must(t, err)
	readonly := a.cfg
	readonly.Database.SkipMigrations = true
	unprepared, err := ck.New(t.Context(), readonly, a.deps)
	if unprepared != nil {
		must(t, unprepared.Close(t.Context()))
	}
	if !errors.Is(err, ck.ErrMigrationsRequired) {
		t.Fatalf("startup accepted old publication obligations: %v", err)
	}
	must(t, ck.MigratePostgres(t.Context(), w.pool, a.cfg))
	check := func() {
		t.Helper()
		for _, item := range []catalog.Content{baseline, deleted} {
			var id, state string
			var current bool
			var at time.Time
			var got access.Rule
			var raw []byte
			must(t, w.pool.QueryRow(t.Context(), `SELECT v.release_id::text,r.state,r.current,r.completed_at,v.released_access,r.result FROM `+editions+` v
 JOIN `+requests+` r ON r.tenant_id=v.tenant_id AND r.id=v.release_id WHERE v.tenant_id=$1 AND v.id=$2`, tenant, item.Versions[0].ID).Scan(&id, &state, &current, &at, &got, &raw))
			if state != "completed" || !at.Equal(past) || got.Level != released.Level || len(got.Releases) != 0 || strings.Contains(string(raw), "\"view\":{") {
				t.Fatalf("migration changed completed access/time or fabricated a manifest: state=%s at=%s access=%+v plan=%s", state, at, got, raw)
			}
			if current != (item.ID == deleted.ID) {
				t.Fatalf("wrong baseline/current relationship for %s", item.ID)
			}
		}
		var count int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+requests+` WHERE version_id=ANY($1)`, []string{draft.Versions[0].ID, foreign.Versions[0].ID}).Scan(&count))
		if count != 0 {
			t.Fatalf("migration admitted draft or foreign publication: %d", count)
		}
		var requestID string
		var scheduled time.Time
		must(t, w.pool.QueryRow(t.Context(), `SELECT id::text,scheduled_at FROM `+requests+` WHERE tenant_id=$1 AND version_id=$2 AND current`, tenant, future.Versions[0].ID).Scan(&requestID, &scheduled))
		if !scheduled.Equal(due) {
			t.Fatalf("accepted future deadline changed: %s", scheduled)
		}
		job, err := producer.JobGet(t.Context(), targeted)
		must(t, err)
		var payload map[string]any
		must(t, json.Unmarshal(job.EncodedArgs, &payload))
		if job.Kind != "contentkit_version_release" || job.Queue != versions.QueueFor(tenant) || job.Attempt != 2 || !job.ScheduledAt.Equal(due) || payload["id"] != requestID || payload["content"] != nil || payload["version"] != nil {
			t.Fatalf("targeted handoff lost history or retained the alternate payload: %+v %v", job, payload)
		}
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+jobs+` j JOIN `+requests+` r ON r.id::text=j.args->>'id'
 WHERE r.tenant_id=$1 AND r.version_id=$2 AND j.kind='contentkit_version_release' AND j.scheduled_at<=now()
 AND j.state IN ('available','pending','scheduled','retryable')`, tenant, immediate.Versions[0].ID).Scan(&count))
		if count == 0 {
			t.Fatalf("stale future job %d suppressed an immediately due release", late)
		}
		for _, id := range []string{frozenID, retiringID} {
			var plan []byte
			must(t, w.pool.QueryRow(t.Context(), `SELECT plan FROM `+requests+` WHERE tenant_id=$1 AND id=$2`, tenant, id).Scan(&plan))
			var equal bool
			must(t, w.pool.QueryRow(t.Context(), `SELECT $1::jsonb=$2::jsonb`, frozenPlan, plan).Scan(&equal))
			if !equal {
				t.Fatalf("migration changed frozen request %s", id)
			}
			must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+jobs+` WHERE kind='contentkit_version_release' AND queue=$1 AND args->>'id'=$2`, versions.QueueFor(tenant), id).Scan(&count))
			if count != 1 {
				t.Fatalf("request %s has %d wakeups", id, count)
			}
		}
		for id, state := range map[int64]rivertype.JobState{foreignJob: rivertype.JobStateScheduled, terminal: rivertype.JobStateCompleted} {
			job, err := producer.JobGet(t.Context(), id)
			must(t, err)
			if job.Kind != "contentkit_content_publish_due" || job.State != state {
				t.Fatalf("foreign/terminal history changed: %+v", job)
			}
		}
		after, err := a.engine.Client().ListEvents(t.Context(), catalog.EventQuery{After: before.NextCursor})
		must(t, err)
		if len(after.Events) != 0 {
			t.Fatalf("migration replayed publication events: %+v", after.Events)
		}
	}
	check()
	var counts [2]int
	for n := range counts {
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+requests+` WHERE tenant_id=$1`, tenant).Scan(&counts[n]))
		if n == 0 {
			must(t, ck.MigratePostgres(t.Context(), w.pool, a.cfg))
		}
	}
	if counts[0] != counts[1] {
		t.Fatalf("repeated migration created successors: %v", counts)
	}
	check()
	// Current request-ID workers are compatible with ordinary replica startup.
	_, err = w.pool.Exec(t.Context(), `UPDATE `+jobs+` SET state='running',attempted_at=now() WHERE id=$1`, targeted)
	must(t, err)
	ready, err := ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	must(t, ready.Close(t.Context()))
}

func TestUnifiedReleaseMigrationKeepsCompletedRequest(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	fleet := publicationOnlyFleet(t, a)
	item, err := a.engine.Client().PostContents(t.Context(), catalog.ContentInput{Kind: "article", Text: map[string]catalog.Text{"en": {Title: "Already unified", Body: "A completed empty-file plan."}}})
	must(t, err)
	_, err = a.engine.Client().PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	awaitDuePublications(t, a)
	must(t, fleet.Stop(t.Context()))
	initial := currentReleaseJob(t, a, item.Versions[0].ID)
	before, err := a.engine.Client().ListEvents(t.Context(), catalog.EventQuery{})
	must(t, err)
	for range 2 {
		must(t, ck.MigratePostgres(t.Context(), w.pool, a.cfg))
		if next := currentReleaseJob(t, a, item.Versions[0].ID); next != initial {
			t.Fatalf("migration created a successor to unchanged completion: before=%+v after=%+v", initial, next)
		}
	}
	after, err := a.engine.Client().ListEvents(t.Context(), catalog.EventQuery{After: before.NextCursor})
	must(t, err)
	if len(after.Events) != 0 {
		t.Fatalf("migration replayed completed publication events: %+v", after.Events)
	}
}
