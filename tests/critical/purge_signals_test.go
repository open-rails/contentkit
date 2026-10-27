package critical_test

import (
	"context"
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
