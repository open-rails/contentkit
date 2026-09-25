package media

import (
	"errors"
	"time"

	"github.com/riverqueue/river"
)

// UnavailableSnooze is how long a job waits after the store was unreachable.
var UnavailableSnooze = 30 * time.Second

// SnoozeUnavailable turns a store outage into a River snooze, which does not
// count as an attempt: an outage longer than the retry budget never discards
// a job. Other errors pass through.
func SnoozeUnavailable(err error) error {
	if errors.Is(err, ErrUnavailable) {
		return river.JobSnooze(UnavailableSnooze)
	}
	return err
}
