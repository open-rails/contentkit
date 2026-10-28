package content

import (
	"errors"
	"time"
)

const (
	// ScopeGlobal is the whole tenant, managed by holders of root:comments:ban.
	ScopeGlobal = "global"
)

// ErrCommentBanned refuses a comment write by a banned user. -> 403 comment_banned
var ErrCommentBanned = errors.New("content: banned from commenting")

// BanNotice is what a banned user is told: the scope, the reason and when the
// ban ends (nil: when lifted). It never names who banned them.
type BanNotice struct {
	Scope  string     `json:"scope"`
	Reason string     `json:"reason,omitempty"`
	Until  *time.Time `json:"until,omitempty"`
}

// BannedError is ErrCommentBanned with the ban that applies.
type BannedError struct{ BanNotice }

func (e *BannedError) Error() string {
	if e.Until != nil {
		return "you can't comment here until " + e.Until.UTC().Format(time.RFC3339)
	}
	return "you can't comment here"
}

func (e *BannedError) Is(target error) bool { return target == ErrCommentBanned }

// CommentBan is one user's ban in one scope. An expired ban stays, Expired,
// until it is lifted or replaced.
type CommentBan struct {
	UserID   string      `json:"user_id"`
	User     *PublicUser `json:"user,omitempty"`
	Scope    string      `json:"scope"`
	Reason   string      `json:"reason,omitempty"`
	Until    *time.Time  `json:"until,omitempty"`
	BannedBy string      `json:"banned_by"`
	BannedAt time.Time   `json:"banned_at"`
	Expired  bool        `json:"expired"`
}

// BanInput is the ban body; both fields are optional.
type BanInput struct {
	Reason string     `json:"reason,omitempty"`
	Until  *time.Time `json:"until"`
}

// BanScope names a ban route family a caller may use on a target's commenters.
type BanScope string

const (
	BanOwner BanScope = "owner" // the caller owns the target: /comment-bans
)

const (
	BanGlobal BanScope = "global" // root:comments:ban: /global-comment-bans
)

// CommentStanding is the caller's standing on a target: may they comment, if a
// ban stops them which, and what they may do to others' comments.
type CommentStanding struct {
	// CanComment: the caller may comment now. A signed-out caller may only
	// where Anonymous says so.
	CanComment bool `json:"can_comment"`
	// Comments is the target's comments setting: open, closed or hidden.
	Comments string `json:"comments"`
	// Count is the visible top-level thread size, including tombstones and
	// the caller's held/rejected roots, in the requested language. Zero is
	// present for an empty thread; hidden or locked comments withhold it.
	Count *int `json:"count,omitempty"`
	// Locked: the caller can't open the content (or the version) and its
	// kind's locked view hides comments, so they are withheld from them and
	// take none of their writes.
	Locked bool       `json:"locked"`
	Ban    *BanNotice `json:"ban,omitempty"`
	// Anonymous: signed-out visitors may comment here, under a name
	// (Options.Anonymous.Comments); otherwise they are asked to sign in.
	Anonymous bool `json:"anonymous"`
	// MaxLength is the longest comment, in characters (Options.CommentMaxLength).
	MaxLength int `json:"max_length"`
	// UserID is the caller's user id, absent when anonymous: the comments it
	// wrote are the ones it may edit and delete.
	UserID string `json:"user_id,omitempty"`
	// Moderate: the caller may edit, delete and restore anyone's comments
	// (root:comments:moderate).
	Moderate bool `json:"moderate"`
	// BanScopes are the scopes the caller may ban the target's commenters in.
	BanScopes []BanScope `json:"ban_scopes"`
}
