package media

import (
	"errors"
	"fmt"
	"time"
)

// ErrRateLimited is a refusal that a later attempt will pass.
var ErrRateLimited = errors.New("media: rate limited")

// LimitError is ErrRateLimited with how long to wait.
type LimitError struct{ RetryAfter time.Duration }

func (e *LimitError) Error() string {
	return fmt.Sprintf("media: rate limited; retry in %s", e.RetryAfter.Round(time.Second))
}

func (e *LimitError) Is(target error) bool { return target == ErrRateLimited }
