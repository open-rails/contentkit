package content

import (
	"fmt"
)

// AuthError is a permission check the host's auth could not answer: 401
// unauthorized when the credential ended since the request was verified
// (auth.ErrExpired, auth.ErrRevoked), else 503 unavailable. Never a grant.
type AuthError struct{ Err error }

func (e *AuthError) Error() string { return "content: permission check failed: " + e.Err.Error() }

func (e *AuthError) Unwrap() error { return e.Err }

// TooLongError refuses a comment longer than Max characters. -> 422 comment_too_long
type TooLongError struct{ Max int }

func (e *TooLongError) Error() string {
	return fmt.Sprintf("a comment is at most %d characters", e.Max)
}

// RejectedError is a policy rejection of a text write, answered as 422 with
// its reason: a ContentModerator's reject verdict.
type RejectedError struct{ Reason string }

func (e RejectedError) Error() string { return e.Reason }

const (
	CodeInvalidRequest = "invalid_request"
)

const (
	CodeUnauthorized = "unauthorized"
)

const (
	CodeForbidden = "forbidden"
)

const (
	CodeNotFound = "not_found"
)

const (
	CodeConflict = "conflict"
)

const (
	CodeModerationRejected = "moderation_rejected"
)

const (
	CodeDuplicateComment = "duplicate_comment"
)

const (
	// CodeRateLimited: too many of one interaction; the body carries action
	// and retry_after (seconds, as the Retry-After header). -> 429
	CodeRateLimited = "rate_limited"
)

const (
	// CodeCommentBanned: the caller is banned from commenting on this target;
	// the body's ban says the scope, reason and until. -> 403
	CodeCommentBanned = "comment_banned"
)

const (
	// CodeCommentsClosed: the target's comments are closed or hidden. -> 403
	CodeCommentsClosed = "comments_closed"
)

const (
	// CodeCommentsLocked: the caller can't open the content and its kind's
	// locked view hides comments, so they take no writes from them. -> 403
	CodeCommentsLocked = "comments_locked"
)

const (
	// CodeCommentModerated: the comment is held or rejected, so its author
	// can't edit it; a reviewer's approval makes it editable again. -> 403
	CodeCommentModerated = "comment_moderated"
)

const (
	// CodeCommentTooLong: the comment is longer than Options.CommentMaxLength;
	// details.max is the limit in characters. -> 422
	CodeCommentTooLong = "comment_too_long"
)

const (
	// CodeNotConfigured: the capability exists but the host never wired its
	// media port. Retrying does not help. -> 501
	CodeNotConfigured = "not_configured"
)

const (
	// CodeTenantMismatch: a host port answered with another tenant's data.
	// A configuration fault, not a client fault; the cause stays in the log.
	CodeTenantMismatch = "tenant_mismatch"
)

const (
	// CodeUnavailable: the host's auth could not answer a permission check;
	// retry. -> 503
	CodeUnavailable = "unavailable"
)

const (
	CodeInternal = "internal_error"
)
