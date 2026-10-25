package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// This adapter controls only the optional external policy boundary. Auth,
// source answers, worker scheduling, erasure, CAS and projections remain real.
// The local HTTP provider retains no personal data beyond its active request.
type classifierWire struct{ address string }

func (classifierWire) StatelessPolicy() {}
func (c classifierWire) Classify(ctx context.Context, answer content.Answer) (content.GroupAssignment, error) {
	data, err := json.Marshal(answer)
	if err != nil {
		return content.GroupAssignment{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.address, bytes.NewReader(data))
	if err != nil {
		return content.GroupAssignment{}, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return content.GroupAssignment{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return content.GroupAssignment{}, fmt.Errorf("classifier HTTP %d", response.StatusCode)
	}
	var group content.GroupAssignment
	err = json.NewDecoder(response.Body).Decode(&group)
	return group, err
}

func TestErasedProjectionAndReclassification(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	configureDiscoverySignals(t, w, a)
	target, targetCredential := w.person(t, 0)
	live, liveCredential := w.person(t, 0)
	entered, release := make(chan struct{}), make(chan struct{})
	var signalOnce, releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		var answer content.Answer
		if err := json.NewDecoder(r.Body).Decode(&answer); err != nil {
			http.Error(out, "invalid fixture request", 400)
			return
		}
		group := content.GroupAssignment{GroupID: "retained-control", Label: "Retained control"}
		if answer.SubjectID == target.ID {
			if calls.Add(1) == 1 {
				http.Error(out, "classifier temporarily unavailable", 503)
				return
			}
			signalOnce.Do(func() { close(entered) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			group = content.GroupAssignment{GroupID: "erased-answer", Label: "Must never return"}
		}
		out.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(out).Encode(group)
	}))
	defer provider.Close()
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.cfg.Database.SkipMigrations = true
	a.deps.Classifier = classifierWire{provider.URL}
	var err error
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	poll, err := a.engine.Client().PostPolls(t.Context(), content.PollInput{Kind: content.PollFreeText, Question: "Which answer survives?", Language: "en"})
	must(t, err)
	pending, err := a.client(targetCredential).PostPollAnswer(t.Context(), poll.ID, content.PollAnswerInput{Text: "Erase this private answer"})
	must(t, err)
	if pending.MyAnswer == nil || pending.MyAnswer.Classified {
		t.Fatal("failed classifier did not leave a real pending source answer")
	}
	control, err := a.client(liveCredential).PostPollAnswer(t.Context(), poll.ID, content.PollAnswerInput{Text: "Keep this answer"})
	must(t, err)
	if control.MyAnswer == nil || !control.MyAnswer.Classified || len(control.Groups) != 1 || control.Groups[0].Count != 1 {
		t.Fatalf("live classifier positive control absent: %+v", control)
	}
	// Compose the host fleet before the published fixture starts any owned
	// workers. This same fleet performs publication and the later real replay.
	fleet, err := riverhelpers.New(t.Context(), w.pool, &river.Config{Schema: w.schema + "_river", Queues: map[string]river.QueueConfig{}}, a.engine.RiverJobs())
	must(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		must(t, fleet.Stop(ctx))
	})
	must(t, a.engine.Start(t.Context(), ck.WithRiverClient(fleet)))
	must(t, fleet.Start(t.Context()))
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
	assertState := func(user string, present bool) {
		state, err := store.States(t.Context(), a.cfg.Scope.AppID.String(), signal.Subject{UserID: user}, []signal.ContentRef{ref})
		must(t, err)
		value, exists := state[ref.Key()]
		if exists != present || present && value.ActiveMS != 1_000 {
			t.Fatalf("measured projection presence=%v active=%d for%s, expected presence=%v", exists, value.ActiveMS, user, present)
		}
	}
	assertState(target.ID, true)
	assertState(live.ID, true)
	job, err := fleet.Insert(t.Context(), scheduledContentJob{kind: "contentkit_reclassify"}, &river.InsertOpts{Queue: ck.QueueContentKit, MaxAttempts: 1})
	must(t, err)
	t.Run("CK-J23-S07_delayed_classifier_and_projection_rebuild_cannot_resurrect_erased_subject", func(t *testing.T) {
		select {
		case <-entered:
		case <-time.After(15 * time.Second):
			t.Fatal("real reclassification worker never reached external provider")
		}
		report, err := a.engine.EraseAppSubjects(t.Context(), []signal.Subject{{UserID: target.ID}})
		must(t, err)
		if !report.Complete() {
			t.Fatalf("erasure incomplete: %+v", report)
		}
		releaseOnce.Do(func() { close(release) })
		waitForWithin(t, 30*time.Second, func() bool {
			row, err := fleet.JobGet(t.Context(), job.Job.ID)
			must(t, err)
			if row.State == rivertype.JobStateDiscarded {
				t.Fatalf("reclassifier failed: %+v", row.Errors)
			}
			return row.State == rivertype.JobStateCompleted
		})
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		must(t, fleet.Stop(ctx))
		got, err := a.client(targetCredential).GetPoll(t.Context(), poll.ID)
		must(t, err)
		if got.MyAnswer != nil || got.AnswerCount != 1 || len(got.Groups) != 1 || got.Groups[0].ID != "retained-control" || got.Groups[0].Count != 1 {
			t.Fatalf("delayed assignment restored erased answer: %+v", got)
		}
		if calls.Load() < 2 {
			t.Fatal("test did not execute an actual failed and delayed classifier call")
		}
		// A delayed trusted ingestion bypasses the HTTP writer gate, exercising
		// ClickHouse's own permanent fence and a non-vacuous projection rebuild.
		event.Revision = 2
		must(t, store.RecordSignals(t.Context(), a.cfg.Scope.AppID.String(), []signal.Signal{event}))
		repaired, err := store.RepairProjections(t.Context(), a.cfg.Scope.AppID.String(), signal.RepairOptions{Rebuild: true})
		must(t, err)
		if repaired.Examined == 0 || repaired.Repaired == 0 {
			t.Fatal("repair did not rebuild the live positive-control projection")
		}
		assertState(target.ID, false)
		assertState(live.ID, true)
		_, err = a.client(targetCredential).PostPollAnswer(t.Context(), poll.ID, content.PollAnswerInput{Text: "Late answer must remain absent"})
		requireError(t, err, 403, "forbidden")
	})
}
