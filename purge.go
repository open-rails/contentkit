package contentkit

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/signal"
)

// orphanGrace keeps any folder with an object newer than this out of the
// orphan sweep, so an item allocated meanwhile is never taken for an orphan.
const (
	orphanSweepEvery = 24 * time.Hour
	orphanGrace      = 7 * 24 * time.Hour
)

type forgetContentsArgs struct {
	Contents []signal.ForgottenContent `json:"contents"`
}

func (forgetContentsArgs) Kind() string { return "contentkit_forget_contents" }

// forgetContentsWorker erases purged content's signals; it retries while the
// signal plane is down and does nothing without one.
type forgetContentsWorker struct {
	river.WorkerDefaults[forgetContentsArgs]
	e *Engine
}

func (w *forgetContentsWorker) Timeout(*river.Job[forgetContentsArgs]) time.Duration {
	return 30 * time.Minute
}

func (w *forgetContentsWorker) Work(ctx context.Context, job *river.Job[forgetContentsArgs]) error {
	err := w.e.hub.ForgetContents(ctx, job.Args.Contents)
	if errors.Is(err, hub.ErrSignalPlaneDown) {
		return err // configured but unreachable: retry rather than drop the erasure
	}
	return ignoreDisabled(err)
}

// forgetPurged queues the signal erasure of purged items and versions from the
// catalog's outbox, so it survives a signal plane outage.
func (e *Engine) forgetPurged(ctx context.Context, events []catalog.Event) error {
	if !e.hub.SignalsConfigured() {
		return nil
	}
	var contents []signal.ForgottenContent
	for _, ev := range events {
		switch ev.Type {
		case catalog.EventContentPurged:
			contents = append(contents, signal.ForgottenContent{Kind: ev.Kind, ContentID: ev.ContentID})
		case catalog.EventVersionPurged:
			contents = append(contents, signal.ForgottenContent{Kind: ev.Kind, ContentID: ev.ContentID, VersionID: ev.VersionID})
		}
	}
	c := e.jobs.bound()
	if len(contents) == 0 || c == nil {
		return nil
	}
	_, err := c.Insert(ctx, forgetContentsArgs{Contents: contents}, &river.InsertOpts{Queue: QueueContentKit, MaxAttempts: 25})
	return err
}

type orphanSweepArgs struct{}

func (orphanSweepArgs) Kind() string { return "contentkit_media_orphan_sweep" }

// orphanSweepWorker deletes media folders of catalog kinds whose item and
// version no longer exist, e.g. after a failed folder deletion.
type orphanSweepWorker struct {
	river.WorkerDefaults[orphanSweepArgs]
	e *Engine
}

func (w *orphanSweepWorker) Timeout(*river.Job[orphanSweepArgs]) time.Duration { return 6 * time.Hour }

func (w *orphanSweepWorker) Work(ctx context.Context, _ *river.Job[orphanSweepArgs]) error {
	_, err := w.e.sweepOrphans(ctx)
	return err
}

// sweepOrphans runs the media orphan sweep over every catalog kind with media.
func (e *Engine) sweepOrphans(ctx context.Context) (int, error) {
	if e.media == nil {
		return 0, nil
	}
	deleted := 0
	for _, kind := range slices.Sorted(maps.Keys(e.catalog.Kinds().Kinds)) {
		if _, err := e.media.reg.Kind(kind); err != nil {
			continue
		}
		report, err := e.media.jobs.SweepOrphans(ctx, media.OrphanSweep{Kind: kind, Exists: e.catalog.Existing, Grace: orphanGrace, Delete: true})
		if err != nil {
			return deleted, err
		}
		for _, f := range report.Orphans {
			if f.Deleted {
				deleted++
			}
		}
		if len(report.Orphans) > 0 {
			e.log.InfoContext(ctx, "contentkit: media orphan sweep", "kind", kind, "folders", report.Folders, "orphans", len(report.Orphans))
		}
	}
	return deleted, nil
}
