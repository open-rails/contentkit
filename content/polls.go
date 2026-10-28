package content

import (
	"time"
)

// PollOption is one choice with its denormalized vote_count.
type PollOption struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	ImageURL  string `json:"image_url,omitempty"`
	Position  int    `json:"position"`
	VoteCount int    `json:"vote_count"`
}

// PollMultipleChoice is the supported poll kind.
const PollMultipleChoice = "multiple_choice"

// Poll is a question, its predefined options and the caller's own vote.
type Poll struct {
	ID         string       `json:"id"`
	Kind       string       `json:"kind"`
	Question   string       `json:"question"`
	Language   string       `json:"language"`
	IsActive   bool         `json:"is_active"`
	ImageURL   string       `json:"image_url,omitempty"`
	LiveAt     time.Time    `json:"live_at"`
	ClosesAt   *time.Time   `json:"closes_at,omitempty"`
	Closed     bool         `json:"closed"` // inactive or past closes_at: no more votes
	TotalVotes int          `json:"total_votes"`
	Options    []PollOption `json:"options"`
	Voted      bool         `json:"voted"`
	MyOption   string       `json:"my_option,omitempty"`
}

// PollInput is a new poll.
type PollInput struct {
	Kind     string            `json:"kind,omitempty"` // default multiple_choice
	Question string            `json:"question"`
	Language string            `json:"language"`
	LiveAt   *time.Time        `json:"live_at,omitempty"`   // nil = live now
	ClosesAt *time.Time        `json:"closes_at,omitempty"` // nil = open until deactivated
	Options  []PollOptionInput `json:"options,omitempty"`
}

// PollOptionInput is one option of a new poll. Images are set once the poll
// exists (PUT /polls/{id}/options/{oid}/image), since they live in its folder.
type PollOptionInput struct {
	Label    string `json:"label"`
	Position int    `json:"position"`
}

// PollUpdate leaves absent fields untouched; closes_at null clears it (open
// until deactivated).
type PollUpdate struct {
	Question *string             `json:"question"`
	IsActive *bool               `json:"is_active"`
	LiveAt   *time.Time          `json:"live_at"`
	ClosesAt Nullable[time.Time] `json:"closes_at,omitzero"`
}

// PollVote is a vote for one option.
type PollVote struct {
	OptionID string `json:"option_id"`
}

// PollImage is the image's public URL after an image change; null when
// cleared or while it has no public file.
type PollImage struct {
	ImageURL *string `json:"image_url"`
}

// PollOptionPatch is the option add and update body; pointers so an absent field is untouched.
type PollOptionPatch struct {
	Label    *string `json:"label"`
	Position *int    `json:"position"`
}
