package critical_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
)

func TestErasedProjectionAndHistoricalAnswers(t *testing.T) {
	w := newWorld(t)
	a, sibling := w.apps[0], w.apps[1]
	configureDiscoverySignals(t, w, a)
	// The fixture helper provisions ClickHouse and updates dependencies; reopen
	// the engine so erasure and the direct projection reader share that plane.
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.cfg.Database.SkipMigrations = true
	var err error
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	target, _ := w.person(t, 0)
	live, _ := w.person(t, 0)
	// Preserve real pre-retirement rows without reviving their removed writer.
	poll, siblingPoll := uuid.NewString(), uuid.NewString()
	_, err = w.pool.Exec(t.Context(), `INSERT INTO `+w.schema+`.content_poll_questions (id,tenant_id,kind,question,language)
 VALUES($1,$2,'free_text','Historical question','en'),($3,$4,'free_text','Sibling history','en')`, poll, a.cfg.Scope.AppID.String(), siblingPoll, sibling.cfg.Scope.AppID.String())
	must(t, err)
	_, err = w.pool.Exec(t.Context(), `INSERT INTO `+w.schema+`.content_poll_answers (tenant_id,question_id,actor_id,text)
 VALUES($1,$2,$3,'Erase this historical answer'),($1,$2,$4,'Keep this historical answer'),($5,$6,$3,'Keep sibling history')`, a.cfg.Scope.AppID.String(), poll, target.ID, live.ID, sibling.cfg.Scope.AppID.String(), siblingPoll)
	must(t, err)
	item := publishedArticle(t, a, "Projection source")
	ref := contentref.NewVersion(a.cfg.Scope.AppID.String(), "article", item.ID, item.Versions[0].ID)
	store, err := signal.NewStore(a.deps.ClickHouse, a.cfg.ClickHouse.Database)
	must(t, err)
	measurement, err := signal.MeasurePages([]signal.PageEngagement{{Index: 0, Ranges: []signal.CoverageRange{{From: 0, To: 10_000}}, ActiveMS: 1_000}}, 1, 1_000)
	must(t, err)
	measurement.VersionID = item.Versions[0].ID
	measurement.VisitID, measurement.SegmentID = uuid.NewString(), uuid.NewString()
	at := time.Now().UTC().Truncate(time.Microsecond)
	event := signal.Signal{ContentRef: ref, Subject: signal.Subject{UserID: target.ID}, Type: signal.TypeEngagement, EventID: uuid.NewString(), Revision: 1, OccurredAt: at, Measurement: &measurement, Access: &signal.AccessContext{ObservedAt: at, ContentAccess: "public", ViewerAccess: "full", PolicyRevision: "critical-v1", Audience: "public", EligibilityKnown: true}}
	other := event
	other.Subject = signal.Subject{UserID: live.ID}
	other.EventID = uuid.NewString()
	must(t, store.RecordSignals(t.Context(), a.cfg.Scope.AppID.String(), []signal.Signal{event, other}))
	assertState := func(t *testing.T, user string, present bool) {
		t.Helper()
		state, err := store.States(t.Context(), a.cfg.Scope.AppID.String(), signal.Subject{UserID: user}, []signal.ContentRef{ref})
		must(t, err)
		value, exists := state[ref.Key()]
		if exists != present || present && value.ActiveMS != 1_000 {
			t.Fatalf("measured projection presence=%v active=%d for%s, expected presence=%v", exists, value.ActiveMS, user, present)
		}
	}
	assertState(t, target.ID, true)
	assertState(t, live.ID, true)
	t.Run("CK-J23-S07_historical_answers_and_projection_rebuild_remain_erased", func(t *testing.T) {
		for range 2 {
			must(t, a.engine.AnonymizeAccount(t.Context(), target.ID))
		}
		var removed, retained, otherApp int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE tenant_id=$1 AND actor_id=$2), count(*) FILTER (WHERE tenant_id=$1 AND actor_id=$3),count(*) FILTER (WHERE tenant_id=$4 AND actor_id=$2) FROM `+w.schema+`.content_poll_answers`, a.cfg.Scope.AppID.String(), target.ID, live.ID, sibling.cfg.Scope.AppID.String()).Scan(&removed, &retained, &otherApp))
		if removed != 0 || retained != 1 || otherApp != 1 {
			t.Fatalf("historical answer erasure crossed its scope: removed=%d retained=%d sibling=%d", removed, retained, otherApp)
		}
		// A delayed trusted ingestion still exercises ClickHouse's permanent fence.
		event.Revision = 2
		must(t, store.RecordSignals(t.Context(), a.cfg.Scope.AppID.String(), []signal.Signal{event}))
		repaired, err := store.RepairProjections(t.Context(), a.cfg.Scope.AppID.String(), signal.RepairOptions{Rebuild: true})
		must(t, err)
		if repaired.Examined == 0 || repaired.Repaired == 0 {
			t.Fatal("repair did not rebuild the live positive-control projection")
		}
		assertState(t, target.ID, false)
		assertState(t, live.ID, true)
	})
}
