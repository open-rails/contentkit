package content

import (
	"time"

	"github.com/open-rails/contentkit/contentref"
)

// CommentInput is the body of a new comment.
type CommentInput struct {
	Body      string `json:"body"`
	ReplyToID string `json:"reply_to_id,omitempty"`
	AnonName  string `json:"anon_name,omitempty"`
	// Language is the thread the comment joins: one of the site's languages,
	// or none. A reply joins its comment's.
	Language string `json:"language,omitempty"`
}

// CommentEdit is the body of a comment edit.
type CommentEdit struct {
	Body string `json:"body"`
}

// Comment is the API view of a content_comments row.
type Comment struct {
	ID         string      `json:"id"`
	ReplyToID  string      `json:"reply_to_id,omitempty"`
	UserID     string      `json:"user_id,omitempty"`
	AnonName   string      `json:"anon_name,omitempty"`
	Language   string      `json:"language,omitempty"`
	Body       string      `json:"body"`
	Deleted    bool        `json:"deleted"`
	Likes      int         `json:"likes"`
	Dislikes   int         `json:"dislikes"`
	Mine       int16       `json:"mine"` // caller's own reaction: -1/0/1
	ReplyCount int         `json:"reply_count"`
	Author     *PublicUser `json:"author,omitempty"`
	// Moderation is "held" or "rejected" on the author's own unpublished
	// comments (with the reason); absent on published ones.
	Moderation       string    `json:"moderation,omitempty"`
	ModerationReason string    `json:"moderation_reason,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// FeedItem is a Comment plus its canonical content reference, for the
// cross-content latest feed (hosts hydrate titles/covers from the reference).
type FeedItem struct {
	Comment
	contentref.ContentRef
}

// AdminComment is the moderation view: raw body (no tombstoning), deletion
// and moderation state and the content reference.
type AdminComment struct {
	Comment
	contentref.ContentRef
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
	AuthorHidden bool       `json:"author_hidden"`
}

// Restored confirms a comment's restoration.
type Restored struct {
	Restored bool `json:"restored"`
}
