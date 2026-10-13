package content

import "errors"

// Sentinel errors a resolver may return to gate a target; mapped to HTTP
// status without leaking which one beyond the code.
var (
	// ErrNotFound: the reference does not exist. -> 404
	ErrNotFound = errors.New("content: not found")
	// ErrNotVisible: exists but unpublished or soft-deleted. -> 404 (hidden).
	ErrNotVisible = errors.New("content: not visible")
	// ErrForbidden: visible but the actor may not consume it. -> 403
	ErrForbidden = errors.New("content: not accessible")
	// ErrTenant: a reference of another tenant reached this runtime.
	ErrTenant = errors.New("content: reference belongs to another tenant")
	// ErrCommentsClosed: the target's comments are closed or hidden. -> 403
	// comments_closed
	ErrCommentsClosed = errors.New("content: comments are closed")
	// ErrCommentsLocked: the actor can't open the content the comments are
	// on. -> 403 comments_locked
	ErrCommentsLocked = errors.New("content: comments are locked")
	// ErrNoClassifier: a free-text poll needs an AnswerClassifier. -> 501
	ErrNoClassifier = errors.New("content: free-text polls need an AnswerClassifier")
)
