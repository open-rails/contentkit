package media

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// PendingOnce dedupes a job per args while one is waiting or running.
var PendingOnce = river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable,
	rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}

// RerunArgs are job args that can name the running job they follow.
type RerunArgs interface {
	river.JobArgs
	FollowUp(id int64) river.JobArgs
}

// InsertFunc inserts one job (a River client's Insert, or InsertTx bound to a transaction).
type InsertFunc func(ctx context.Context, args river.JobArgs, o *river.InsertOpts) (*rivertype.JobInsertResult, error)

// InsertOnce enqueues args after the caller's change to their inputs. An
// equal job still waiting to run absorbs it. River's uniqueness also covers
// running jobs, which may have read the inputs before the change, so one
// follow-up is queued behind a running equal job; a burst shares it. The
// follow-up's worker calls WaitFor first.
func InsertOnce(ctx context.Context, insert InsertFunc, args RerunArgs, o river.InsertOpts) error {
	o.UniqueOpts = PendingOnce
	var next river.JobArgs = args
	for {
		res, err := insert(ctx, next, &o)
		if err != nil || !res.UniqueSkippedAsDuplicate || res.Job.State != rivertype.JobStateRunning {
			return err
		}
		next = args.FollowUp(res.Job.ID)
	}
}

// WaitFor snoozes a follow-up (InsertOnce) while the job it follows still
// runs, so jobs for the same inputs do not overlap. The client is the one
// running the job (river.ClientFromContext).
func WaitFor(ctx context.Context, c *river.Client[pgx.Tx], id int64) error {
	if id == 0 {
		return nil
	}
	prev, err := c.JobGet(ctx, id)
	if errors.Is(err, river.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if prev.State == rivertype.JobStateRunning {
		return river.JobSnooze(time.Second)
	}
	return nil
}
