package video

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

func TestVideoFailureBudget(t *testing.T) {
	previous := make([]rivertype.AttemptError, 4)
	for i := range previous {
		previous[i].Error = "encode failed"
	}
	row := &rivertype.JobRow{Attempt: 5, Errors: previous}
	err := (WorkerConfig{}).runVideoJob(context.Background(), row, func() error { return errors.New("encode failed") })
	var cancel *river.JobCancelError
	if !errors.As(err, &cancel) {
		t.Fatalf("fifth worker failure returned %v, want cancellation", err)
	}
	row.Attempt = 6
	row.Errors = append(row.Errors, rivertype.AttemptError{Error: "encode failed"})
	called := false
	err = (WorkerConfig{}).runVideoJob(context.Background(), row, func() error { called = true; return nil })
	if !errors.As(err, &cancel) || called {
		t.Fatalf("exhausted job ran %v, error %v", called, err)
	}
}

func TestSnoozeOnlyOnShutdown(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	stop()
	var snooze *river.JobSnoozeError
	if err := snoozeOnShutdown(ctx, context.Canceled); !errors.As(err, &snooze) {
		t.Fatalf("shutdown returned %v, want snooze", err)
	}
	deadline, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := snoozeOnShutdown(deadline, context.DeadlineExceeded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline returned %v, want failure", err)
	}
	remote, stopRemote := context.WithCancelCause(context.Background())
	stopRemote(river.ErrJobCancelledRemotely)
	if err := snoozeOnShutdown(remote, context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("remote cancellation returned %v", err)
	}
}
