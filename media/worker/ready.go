package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

// completionHook is part of the processing attempt. It records observable
// progress/completion and the pending release wakeup before River completes the job.
// A failed transaction retries this same attempt; there is no delivery job.
type completionHook struct {
	river.HookDefaults
	manifests *media.Manifests
	publish   func(context.Context, pgx.Tx, contentref.ContentRef) error
}

func (h *completionHook) WorkEnd(ctx context.Context, job *rivertype.JobRow, err error) (result error) {
	workErr := err
	var snooze *river.JobSnoozeError
	var cancel *rivertype.JobCancelError
	checkpoint := errors.As(err, &snooze) && snooze.Duration == 0 && ctx.Err() == nil
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	settled := errors.As(err, &cancel) && (ctx.Err() == nil || timedOut)
	if err != nil && !checkpoint && !settled || job.Kind != (workqueue.IngestArgs{}).Kind() && job.Kind != (workqueue.DerivativeArgs{}).Kind() {
		return err
	}
	if settled && timedOut {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
	}
	defer func() { result = media.SnoozeUnavailable(ctx, h.manifests.Store(), job, result) }()
	var args struct {
		Ref contentref.ContentRef `json:"ref"`
	}
	if err := json.Unmarshal(job.EncodedArgs, &args); err != nil {
		return workErr
	}
	hooks := h.manifests.Registry().Hooks()
	err = media.CompleteProcessing(ctx, h.manifests, args.Ref, func(ctx context.Context, tx pgx.Tx, r media.Readiness) error {
		if err := h.publish(ctx, tx, args.Ref); err != nil {
			return err
		}
		// Existing custom embedded integrations keep their transaction contract.
		// Stock workers wake publication directly without a preparation job.
		if r.State == media.StateProcessing {
			if hooks.ItemProgressed != nil {
				return hooks.ItemProgressed(ctx, tx, args.Ref)
			}
		} else if hooks.ItemReady != nil {
			return hooks.ItemReady(ctx, tx, args.Ref, r)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return workErr
}

func (c Config) checkAttemptBudget(ctx context.Context, job *rivertype.JobRow, fail func(context.Context, error) error) error {
	if job.Attempt <= workqueue.MaxAttempts {
		return nil
	}
	return c.cancelFailedAttempt(ctx, job, fmt.Errorf("media/worker: %d failed attempts", job.Attempt-1), fail)
}

func (c Config) finishAttempt(ctx context.Context, job *rivertype.JobRow, err error, fail func(context.Context, error) error) error {
	if errors.Is(ctx.Err(), context.Canceled) && !errors.Is(context.Cause(ctx), river.ErrJobCancelledRemotely) {
		return river.JobSnooze(0)
	}
	err = media.SnoozeUnavailable(ctx, c.Store, job, err)
	var snooze *river.JobSnoozeError
	var cancel *river.JobCancelError
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	if err == nil || (ctx.Err() != nil && !timedOut) || job.Attempt < min(job.MaxAttempts, workqueue.MaxAttempts) || errors.As(err, &snooze) || errors.As(err, &cancel) {
		return err
	}
	if timedOut {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
	}
	return c.cancelFailedAttempt(ctx, job, err, fail)
}

func (c Config) cancelFailedAttempt(ctx context.Context, job *rivertype.JobRow, err error, fail func(context.Context, error) error) error {
	if failure := fail(ctx, err); failure != nil {
		var cancel *river.JobCancelError
		if errors.As(failure, &cancel) {
			return failure
		}
		// Snoozes do not spend attempts, so bound failure-record retries separately.
		var meta struct {
			Snoozes int `json:"contentkit_failure_record_snoozes"`
		}
		_ = json.Unmarshal(job.Metadata, &meta)
		if meta.Snoozes < 10 {
			if setErr := river.MetadataSet(ctx, "contentkit_failure_record_snoozes", meta.Snoozes+1); setErr == nil {
				return river.JobSnooze(media.UnavailableSnooze)
			} else {
				failure = errors.Join(failure, setErr)
			}
		}
		c.Logger.Error("media-worker: could not record final failure", "job_id", job.ID, "error", failure.Error())
		return river.JobCancel(errors.Join(err, fmt.Errorf("record final failure: %w", failure)))
	}
	return river.JobCancel(err)
}

// Rescued hard kills did not return a failure and do not spend the retry budget.
func (c Config) restoreRescuedAttempt(ctx context.Context, job *rivertype.JobRow) error {
	failures := 0
	for _, attempt := range job.Errors {
		if attempt.Error != "Stuck job rescued by JobRescuer" {
			failures++
		}
	}
	want := failures + 1
	if job.Attempt <= want {
		return nil
	}
	result, err := c.Pool.Exec(ctx, `UPDATE `+pgx.Identifier{c.Schema, "river_job"}.Sanitize()+`
SET attempt = $2 WHERE id = $1 AND state = 'running' AND attempt = $3`, job.ID, want, job.Attempt)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("media/worker: job %d changed while restoring rescued attempt", job.ID)
	}
	job.Attempt = want
	return nil
}
