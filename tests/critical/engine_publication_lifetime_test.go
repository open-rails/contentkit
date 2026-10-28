package critical_test

import (
	"context"
	"errors"
	"testing"
	"time"

	versionstore "github.com/open-rails/contentkit/internal/versionstore"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

func TestEnginePublicationLifetime(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.engine.StartJobs(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled startup accepted workers: %v", err)
	}
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelExpired()
	if err := a.engine.StartJobs(expired); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired startup lost its deadline error: %v", err)
	}
	startup, cancelStartup := context.WithCancel(t.Context())
	must(t, a.engine.StartJobs(startup))
	cancelStartup()
	// Start's caller can return or lose its request. Publication admitted
	// afterward must still be executed by the engine-owned background runtime.
	admin, reader := a.engine.Client(), a.client("")
	item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{
		"en": {Title: "Publication after startup caller returned", Body: "Engine.Close owns worker lifetime."},
	}}}})
	must(t, err)
	_, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	waitForWithin(t, 30*time.Second, func() bool {
		got, err := reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		return err == nil && got.Live && got.Body == "Engine.Close owns worker lifetime."
	})
	must(t, a.engine.Ready(t.Context()))
	closeCtx, closeCancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer closeCancel()
	must(t, a.engine.Close(closeCtx))
	if err := a.engine.Ready(t.Context()); err == nil {
		t.Fatal("closed engine still reports ready")
	}
	// Replaying this completed exact request probes the queue after Close.
	// If the old engine still fetched jobs, its idempotent replay would finish.
	request := currentPublicationJob(t, a, item.Versions[0].ID)
	queue, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
	must(t, err)
	job, err := queue.Insert(t.Context(), request, &river.InsertOpts{Queue: versionstore.QueueFor(publicationTenant(a))})
	must(t, err)
	deadline := time.NewTimer(500 * time.Millisecond)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	checking := true
	for checking {
		select {
		case <-tick.C:
			row, err := queue.JobGet(t.Context(), job.Job.ID)
			must(t, err)
			if row.State != rivertype.JobStateAvailable {
				t.Fatalf("Close left an owned worker consuming jobs: %s", row.State)
			}
		case <-deadline.C:
			checking = false
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
	// Reopening starts a new owner and drains the retained probe, proving the
	// queue was executable rather than misconfigured or unavailable.
	a.server.Close()
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	must(t, a.engine.StartJobs(t.Context()))
	waitForWithin(t, 30*time.Second, func() bool {
		row, err := queue.JobGet(t.Context(), job.Job.ID)
		must(t, err)
		if row.State == rivertype.JobStateDiscarded {
			t.Fatalf("reopened runtime could not execute retained publication work: %+v", row.Errors)
		}
		return row.State == rivertype.JobStateCompleted
	})
	must(t, w.pool.Ping(t.Context()))
}
