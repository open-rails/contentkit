package publishing

import (
	"testing"
	"time"
)

func TestOperationMonitorWaitsOnlyForActiveWork(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	future := now.Add(72 * time.Hour)
	for _, tc := range []struct {
		name  string
		state State
		at    *time.Time
		err   *OperationError
		want  time.Duration
	}{
		{name: "processing", state: Processing, want: 5 * time.Second},
		{name: "exposing", state: Publishing, want: 5 * time.Second},
		{name: "future publication", state: Scheduled, at: &future, want: 72 * time.Hour},
		{name: "due publication", state: Scheduled, at: &now, want: 5 * time.Second},
		{name: "recoverable authority failure", state: Blocked, err: &OperationError{Retryable: true}, want: time.Minute},
		{name: "moderation review", state: ReviewRequired},
		{name: "published", state: Published},
		{name: "draft ready", state: Ready},
		{name: "failed", state: Failed},
		{name: "blocked", state: Blocked},
		{name: "upload not submitted", state: AwaitingUpload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := record{Operation: Operation{State: tc.state, Error: tc.err}, request: Request{PublishAt: tc.at}}
			if got := operationDelay(r, now); got != tc.want {
				t.Fatalf("delay=%s, want %s", got, tc.want)
			}
		})
	}
}
