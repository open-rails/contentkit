package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/helpers/deps"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/internal/media/workqueue"
	"github.com/open-rails/contentkit/internal/pglock"
)

// completionHook ends each processing attempt: it snoozes through storage
// outages and retries transient failures. Progress, completion facts and
// release wakeups follow each manifest write (the item cache's hook).
type completionHook struct {
	river.HookDefaults
	manifests *mediastore.Manifests
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
	defer func() {
		result = mediastore.SnoozeUnavailable(ctx, h.manifests.Store(), job, result)
		if transientError(result) {
			result = retryTransientAttempt(ctx, job, result)
		}
	}()
	return workErr
}

func (c Config) checkAttemptBudget(ctx context.Context, job *rivertype.JobRow, fail func(context.Context, error) error) error {
	attempt := job.Attempt - transientAttempts(job)
	if attempt <= workqueue.MaxAttempts {
		return nil
	}
	return c.cancelFailedAttempt(ctx, job, fmt.Errorf("media/worker: %d failed attempts", attempt-1), fail)
}

func (c Config) finishAttempt(ctx context.Context, job *rivertype.JobRow, err error, fail func(context.Context, error) error) error {
	if errors.Is(ctx.Err(), context.Canceled) && !errors.Is(context.Cause(ctx), river.ErrJobCancelledRemotely) {
		return river.JobSnooze(0)
	}
	err = mediastore.SnoozeUnavailable(ctx, c.Store, job, err)
	if transientError(err) {
		return retryTransientAttempt(ctx, job, err)
	}
	var snooze *river.JobSnoozeError
	var cancel *river.JobCancelError
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	if err == nil || (ctx.Err() != nil && !timedOut) || job.Attempt-transientAttempts(job) < min(job.MaxAttempts, workqueue.MaxAttempts) || errors.As(err, &snooze) || errors.As(err, &cancel) {
		return err
	}
	if timedOut {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
	}
	return c.cancelFailedAttempt(ctx, job, err, fail)
}

func transientAttempts(job *rivertype.JobRow) int {
	var meta struct {
		Attempts int `json:"contentkit_transient_attempts"`
	}
	_ = json.Unmarshal(job.Metadata, &meta)
	return meta.Attempts
}

// Infrastructure failures leave sources pending even after the content retry budget.
func transientError(err error) bool {
	if errors.Is(err, pglock.ErrContended) || deps.PostgresUnavailable(err) {
		return true
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "53400", "40001", "40P01": // configuration limit, serialization failure, deadlock
			return true
		}
	}
	return false
}

func retryTransientAttempt(ctx context.Context, job *rivertype.JobRow, err error) error {
	// At River's limit, snooze with its backoff instead of discarding the work.
	if job.Attempt >= job.MaxAttempts {
		return river.JobSnooze(time.Until((&river.DefaultClientRetryPolicy{}).NextRetry(job)))
	}
	if setErr := river.MetadataSet(ctx, "contentkit_transient_attempts", transientAttempts(job)+1); setErr != nil {
		return errors.Join(err, setErr)
	}
	var cancel *river.JobCancelError
	if errors.As(err, &cancel) {
		return cancel.Unwrap()
	}
	return err
}

func (c Config) cancelFailedAttempt(ctx context.Context, job *rivertype.JobRow, err error, fail func(context.Context, error) error) error {
	if failure := fail(ctx, err); failure != nil {
		if transientError(failure) {
			return retryTransientAttempt(ctx, job, failure)
		}
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
				return river.JobSnooze(mediastore.UnavailableSnooze)
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
