package critical_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

// The only fault is a severed connection to real ClickHouse. Content deletion,
// the durable obligation, the queue and the eventual mutation are all real.
func TestContentPurgeRetriesSignalCleanup(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	configureDiscoverySignals(t, w, a)
	direct := a.deps.ClickHouse
	wire := newTCPCut(t, a.cfg.ClickHouse.Addr)
	borrowed, err := clickhouse.Open(&clickhouse.Options{Addr: []string{wire.listener.Addr().String()}, Auth: clickhouse.Auth{Database: a.cfg.ClickHouse.Database, Username: a.cfg.ClickHouse.Username, Password: a.cfg.ClickHouse.Password}, DialTimeout: time.Second, ReadTimeout: 2 * time.Second})
	must(t, err)
	t.Cleanup(func() { must(t, borrowed.Close()) })
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.deps.ClickHouse = borrowed
	a.cfg.Database.SkipMigrations = true
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	// Compose the single-worker host queue before publication fixtures start
	// any runtime. It is stopped again before advancing the purge clock.
	queue := catalog.QueueFor(a.cfg.Scope.AppID.String())
	var fleet *river.Client[pgx.Tx]
	start := func() {
		t.Helper()
		if fleet == nil {
			fleet, err = riverhelpers.New(t.Context(), w.pool, &river.Config{Schema: w.schema + "_river", Queues: map[string]river.QueueConfig{queue: {MaxWorkers: 1}}}, a.engine.RiverJobs())
			must(t, err)
			must(t, a.engine.Start(t.Context(), ck.WithRiverClient(fleet)))
		}
		must(t, fleet.Start(t.Context()))
	}
	stop := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if fleet != nil {
			must(t, fleet.Stop(ctx))
		}
		must(t, a.engine.Close(ctx))
	}
	t.Cleanup(stop)
	start()
	user, _ := w.person(t, 0)
	deleted, untouched := publishedArticle(t, a, "Delete signal source"), publishedArticle(t, a, "Retain signal source")
	store, err := signal.NewStore(direct, a.cfg.ClickHouse.Database)
	must(t, err)
	for _, item := range []catalog.Content{deleted, untouched} {
		measurement, err := signal.MeasurePages([]signal.PageEngagement{{Index: 0, Ranges: []signal.CoverageRange{{From: 0, To: 10_000}}, ActiveMS: 1_000}}, 1, 1_000)
		must(t, err)
		measurement.VersionID = item.Versions[0].ID
		measurement.VisitID, measurement.SegmentID = uuid.NewString(), uuid.NewString()
		at := time.Now().UTC().Truncate(time.Microsecond)
		must(t, store.RecordSignals(t.Context(), a.cfg.Scope.AppID.String(), []signal.Signal{{
			ContentRef: contentref.NewVersion(a.cfg.Scope.AppID.String(), item.Kind, item.ID, item.Versions[0].ID),
			Subject:    signal.Subject{UserID: user.ID}, Type: signal.TypeEngagement, EventID: uuid.NewString(), Revision: 1,
			OccurredAt: at, Measurement: &measurement,
			Access: &signal.AccessContext{ObservedAt: at, ContentAccess: "public", ViewerAccess: "full", PolicyRevision: "critical-v1", Audience: "public", EligibilityKnown: true},
		}}))
	}
	assertState := func(item catalog.Content, want bool) {
		t.Helper()
		ref := contentref.New(a.cfg.Scope.AppID.String(), item.Kind, item.ID)
		states, err := store.States(t.Context(), a.cfg.Scope.AppID.String(), signal.Subject{UserID: user.ID}, []signal.ContentRef{ref})
		must(t, err)
		_, exists := states[ref.Key()]
		if exists != want {
			t.Fatalf("content %s signal presence=%v want%v", item.ID, exists, want)
		}
	}
	assertState(deleted, true)
	assertState(untouched, true)
	must(t, a.engine.Client().DeleteContentVersion(t.Context(), deleted.ID, deleted.Versions[0].ID))
	must(t, fleet.Stop(t.Context()))
	w.clock.set(time.Now().Add(31 * 24 * time.Hour))
	producer, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
	must(t, err)
	purge := func(want rivertype.JobState) {
		t.Helper()
		job, err := producer.Insert(t.Context(), scheduledContentJob{kind: "contentkit_content_purge_deleted"}, &river.InsertOpts{Queue: catalog.QueueFor(a.cfg.Scope.AppID.String()), MaxAttempts: 1})
		must(t, err)
		waitForWithin(t, 55*time.Second, func() bool {
			got, err := producer.JobGet(t.Context(), job.Job.ID)
			must(t, err)
			if got.State == rivertype.JobStateCompleted || got.State == rivertype.JobStateDiscarded {
				if got.State != want {
					t.Fatalf("purge=%s want%s: %+v", got.State, want, got.Errors)
				}
				return true
			}
			return false
		})
	}
	wire.cut()
	start()
	purge(rivertype.JobStateDiscarded)
	rows, err := a.engine.Client().GetManage(t.Context(), ck.GetManageQuery{State: "deleted", Ids: []string{deleted.ID}})
	must(t, err)
	if len(rows) != 0 {
		t.Fatal("ClickHouse outage prevented local content purge")
	}
	pending := pgx.Identifier{w.schema, "content_signal_purges"}.Sanitize()
	var count int
	must(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pending+" WHERE tenant_id=$1 AND content_id=$2", a.cfg.Scope.AppID.String(), deleted.ID).Scan(&count))
	if count == 0 {
		t.Fatal("local purge lost its durable ClickHouse cleanup obligation")
	}
	assertState(deleted, true)
	assertState(untouched, true)
	// Reopening loses no obligation, and only the ordinary purge worker exists.
	a.server.Close()
	stop()
	a.deps.ClickHouse = direct
	fleet = nil
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	start()
	purge(rivertype.JobStateCompleted)
	assertState(deleted, false)
	assertState(untouched, true)
	must(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pending+" WHERE tenant_id=$1", a.cfg.Scope.AppID.String()).Scan(&count))
	if count != 0 {
		t.Fatal("acknowledged cleanup remains pending")
	}
	must(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize()+" WHERE kind='contentkit_forget_contents'").Scan(&count))
	if count != 0 {
		t.Fatal("purge still creates separate forget jobs")
	}
	purge(rivertype.JobStateCompleted)
	assertState(untouched, true)
}

type priorForgetJob struct {
	Tenant   string                    `json:"tenant,omitempty"`
	Contents []signal.ForgottenContent `json:"contents"`
}

func (priorForgetJob) Kind() string { return "contentkit_forget_contents" }

func TestContentSignalPurgeJobMigration(t *testing.T) {
	w := newWorld(t)
	a, foreign := w.apps[0], w.apps[1]
	owned, other := publishedArticle(t, a, "Owned old cleanup"), publishedArticle(t, foreign, "Foreign old cleanup")
	producer, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
	must(t, err)
	// Produce an actual retained purge fact, with no surviving content row,
	// so legacy ownership inference is tested at the real historical boundary.
	startPublicationRuntime(t, a)
	must(t, a.engine.Client().DeleteContentVersion(t.Context(), owned.ID, owned.Versions[0].ID))
	w.clock.set(time.Now().Add(31 * 24 * time.Hour))
	purge, err := producer.Insert(t.Context(), scheduledContentJob{kind: "contentkit_content_purge_deleted"}, &river.InsertOpts{Queue: catalog.QueueFor(a.cfg.Scope.AppID.String()), MaxAttempts: 1})
	must(t, err)
	waitForWithin(t, 30*time.Second, func() bool {
		got, err := producer.JobGet(t.Context(), purge.Job.ID)
		must(t, err)
		if got.State == rivertype.JobStateDiscarded {
			t.Fatalf("purge fixture failed: %+v", got.Errors)
		}
		return got.State == rivertype.JobStateCompleted
	})
	must(t, a.engine.Close(t.Context()))
	var retained int
	must(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, "content_items"}.Sanitize()+" WHERE tenant_id=$1 AND id=$2", a.cfg.Scope.AppID.String(), owned.ID).Scan(&retained))
	if retained != 0 {
		t.Fatal("migration fixture did not hard-purge its content")
	}
	job := func(item catalog.Content, tenant, queue string) *rivertype.JobRow {
		t.Helper()
		j, err := producer.Insert(t.Context(), priorForgetJob{Tenant: tenant, Contents: []signal.ForgottenContent{{Kind: item.Kind, ContentID: item.ID}}}, &river.InsertOpts{Queue: queue, ScheduledAt: time.Now().Add(time.Hour)})
		must(t, err)
		return j.Job
	}
	ownedJob := job(owned, "", ck.QueueContentKit)
	foreignJob := job(other, "", ck.QueueContentKit)
	explicitForeign := job(owned, foreign.cfg.Scope.AppID.String(), ck.QueueContentKit)
	foreignQueue := job(owned, "", "another_application")
	terminal := job(owned, "", ck.QueueContentKit)
	discarded := job(catalog.Content{Kind: "article", ID: uuid.NewString()}, a.cfg.Scope.AppID.String(), ck.QueueContentKit)
	foreignDiscarded := job(other, foreign.cfg.Scope.AppID.String(), ck.QueueContentKit)
	table := pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize()
	_, err = w.pool.Exec(t.Context(), "UPDATE "+table+" SET state='completed',finalized_at=now() WHERE id=$1", terminal.ID)
	must(t, err)
	_, err = w.pool.Exec(t.Context(), "UPDATE "+table+" SET state='discarded',finalized_at=now() WHERE id=ANY($1)", []int64{discarded.ID, foreignDiscarded.ID})
	must(t, err)
	snapshot := func(id int64) string {
		t.Helper()
		var value string
		must(t, w.pool.QueryRow(t.Context(), "SELECT row_to_json(j)::text FROM "+table+" j WHERE id=$1", id).Scan(&value))
		return value
	}
	preserved := map[int64]string{}
	for _, j := range []*rivertype.JobRow{foreignJob, explicitForeign, foreignQueue, terminal, discarded, foreignDiscarded} {
		preserved[j.ID] = snapshot(j.ID)
	}
	_, err = w.pool.Exec(t.Context(), "UPDATE "+table+" SET state='running',attempted_at=now(),attempt=1,attempted_by=ARRAY['owned-fixture'] WHERE id=$1", ownedJob.ID)
	must(t, err)
	before := snapshot(ownedJob.ID)
	if err := ck.MigratePostgres(t.Context(), w.pool, a.cfg); err == nil {
		t.Fatal("migration accepted running old signal erasure")
	}
	if snapshot(ownedJob.ID) != before {
		t.Fatal("refused migration changed running old job")
	}
	_, err = w.pool.Exec(t.Context(), "UPDATE "+table+" SET state='retryable' WHERE id=$1", ownedJob.ID)
	must(t, err)
	unknown := job(catalog.Content{Kind: "article", ID: uuid.NewString()}, "", ck.QueueContentKit)
	before = snapshot(ownedJob.ID)
	if err := ck.MigratePostgres(t.Context(), w.pool, a.cfg); err == nil {
		t.Fatal("migration guessed an app for an unattributable old obligation")
	}
	if snapshot(ownedJob.ID) != before {
		t.Fatal("failed migration partially cancelled earlier jobs")
	}
	_, err = producer.JobCancel(t.Context(), unknown.ID)
	must(t, err)
	must(t, ck.MigratePostgres(t.Context(), w.pool, a.cfg))
	got, err := producer.JobGet(t.Context(), ownedJob.ID)
	must(t, err)
	if got.State != rivertype.JobStateCancelled {
		t.Fatalf("old job remains%s after handoff", got.State)
	}
	for id, want := range preserved {
		if snapshot(id) != want {
			t.Fatalf("migration altered foreign/terminal job%d", id)
		}
	}
	var count int
	must(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, "content_signal_purges"}.Sanitize()+" WHERE tenant_id=$1 AND content_kind=$2 AND content_id=$3", a.cfg.Scope.AppID.String(), owned.Kind, owned.ID).Scan(&count))
	if count != 1 {
		t.Fatalf("want1 exact cleanup obligation, got%d", count)
	}
	var failed priorForgetJob
	must(t, json.Unmarshal(discarded.EncodedArgs, &failed))
	must(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, "content_signal_purges"}.Sanitize()+" WHERE tenant_id=$1 AND content_id=$2", a.cfg.Scope.AppID.String(), failed.Contents[0].ContentID).Scan(&count))
	if count != 1 {
		t.Fatal("discarded old job lost its outstanding cleanup")
	}
	must(t, ck.MigratePostgres(t.Context(), w.pool, a.cfg))
	cfg := a.cfg
	cfg.Database.SkipMigrations = true
	reopened, err := ck.New(t.Context(), cfg, a.deps)
	must(t, err)
	must(t, reopened.Close(context.Background()))
	must(t, ck.MigratePostgres(t.Context(), w.pool, foreign.cfg))
	got, err = producer.JobGet(t.Context(), foreignJob.ID)
	must(t, err)
	if got.State != rivertype.JobStateCancelled {
		t.Fatalf("foreign engine did not adopt own job: %s", got.State)
	}
}
