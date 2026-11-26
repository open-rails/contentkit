package mediastore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/riverqueue/river"
)

// GCQueueName is the host River queue of items' garbage collection.
const GCQueueName = "contentkit_media_gc"

// GCArgs is one item's garbage collection: one job per item, woken by its
// manifest writes (PGCache) and snoozed until the item next needs it.
type GCArgs struct {
	Ref   contentref.ContentRef `json:"ref"`
	After int64                 `json:"after,omitempty"`
}

func (GCArgs) Kind() string { return "contentkit_media_gc" }

func (a GCArgs) FollowUp(id int64) river.JobArgs {
	a.After = id
	return a
}

// RiverGC schedules garbage collection on the host's River schema.
type RiverGC struct{ Client *river.Client[pgx.Tx] }

// ScheduleGC runs ref's collection no later than at.
func (g RiverGC) ScheduleGC(ctx context.Context, ref contentref.ContentRef, at time.Time) error {
	return Wake(ctx, g.Client, nil, GCArgs{Ref: ref}, river.InsertOpts{Queue: GCQueueName, MaxAttempts: 25}, at)
}

// GCWorker runs Jobs.Collect for one item.
type GCWorker struct {
	river.WorkerDefaults[GCArgs]
	Jobs *Jobs
}

func (w *GCWorker) Timeout(*river.Job[GCArgs]) time.Duration { return 10 * time.Minute }

func (w *GCWorker) Work(ctx context.Context, job *river.Job[GCArgs]) error {
	if err := WaitFor(ctx, river.ClientFromContext[pgx.Tx](ctx), job.Args.After); err != nil {
		return err
	}
	next, err := w.Jobs.Collect(ctx, job.Args.Ref)
	var settling *SettlingError
	switch {
	case errors.Is(err, ErrUnknownKind), errors.Is(err, ErrInvalidManifest):
		return river.JobCancel(err)
	case errors.As(err, &settling):
		return river.JobSnooze(time.Until(settling.Until))
	case err != nil:
		return err
	case next.IsZero():
		return nil
	}
	return river.JobSnooze(max(time.Second, time.Until(next)))
}
