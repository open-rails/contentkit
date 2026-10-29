package content

import (
	"time"

	"github.com/open-rails/contentkit/contentref"
)

// ImportComment is a comment as the bulk import writes it: approved, its id,
// thread and times kept. ReplyToID names a comment on the same reference; a
// reply to a reply joins its thread.
type ImportComment struct {
	ID        string
	Ref       contentref.ContentRef
	Language  string
	ReplyToID string
	UserID    string
	AnonName  string
	Body      string
	DeletedAt *time.Time
	CreatedAt *time.Time
	UpdatedAt *time.Time
}

// ImportReaction is a person's (or an IP's) reaction: -1, 0 or 1.
type ImportReaction struct {
	Ref       contentref.ContentRef
	UserID    string
	IP        string
	Value     int16
	CreatedAt *time.Time
	UpdatedAt *time.Time
}

// ImportFavorite is a person's bookmark.
type ImportFavorite struct {
	Ref       contentref.ContentRef
	UserID    string
	CreatedAt *time.Time
}

// ImportPoll is a poll with its options. Options replace the stored choices
// before voting starts; once votes exist, their IDs, labels and positions must match.
type ImportPoll struct {
	ID        string
	Kind      string
	Question  string
	Language  string
	Active    bool
	LiveAt    *time.Time
	ClosesAt  *time.Time
	DeletedAt *time.Time
	CreatedAt *time.Time
	UpdatedAt *time.Time
	Options   []ImportPollOption
}

// ImportPollOption is one option of an ImportPoll.
type ImportPollOption struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Position int    `json:"position"`
}

// ImportVote is one person's (or IP's) choice in a multiple-choice poll.
type ImportVote struct {
	PollID    string
	OptionID  string
	UserID    string
	IP        string
	CreatedAt *time.Time
}
