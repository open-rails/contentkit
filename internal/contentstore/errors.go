package contentstore

import (
	"errors"
)

var (
	// ErrNotFound: the reference does not exist. -> 404
	ErrNotFound = errors.New("content: not found")
)

var (
	// ErrNotVisible: exists but unpublished or soft-deleted. -> 404 (hidden).
	ErrNotVisible = errors.New("content: not visible")
)

var (
	// ErrForbidden: visible but the actor may not consume it. -> 403
	ErrForbidden = errors.New("content: not accessible")
)

var (
	// ErrTenant: a reference of another tenant reached this runtime.
	ErrTenant = errors.New("content: reference belongs to another tenant")
)

var (
	// ErrCommentsClosed: the target's comments are closed or hidden. -> 403
	// comments_closed
	ErrCommentsClosed = errors.New("content: comments are closed")
)

var (
	// ErrCommentsLocked: the actor can't open the content the comments are
	// on, and its kind's locked view hides them. -> 403 comments_locked
	ErrCommentsLocked = errors.New("content: comments are locked")
)
