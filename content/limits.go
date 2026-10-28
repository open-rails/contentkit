package content

import (
	"errors"
	"fmt"
	"time"
)

// Action names one rate-limited interaction.
type Action string

const (
	ActionComment Action = "comment" // comment create or edit, top-level or reply
)

const (
	ActionCommentReaction Action = "comment_reaction" // like, dislike, neutral on a comment
)

const (
	ActionReaction Action = "reaction" // like, dislike, neutral on content
)

const (
	ActionFavorite Action = "favorite" // favorite and unfavorite
)

const (
	ActionPollVote Action = "poll_vote" // multiple-choice votes
)

// Rate allows at most Count interactions in any Per (a sliding window).
type Rate struct {
	Count int
	Per   time.Duration
}

// ErrRateLimited is a refusal a later attempt passes. -> 429 rate_limited
var ErrRateLimited = errors.New("content: rate limited")

// RateLimitError is ErrRateLimited for one action, with how long to wait.
type RateLimitError struct {
	Action     Action
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("content: %s rate limited; retry in %s", e.Action, e.RetryAfter.Round(time.Second))
}

func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }
