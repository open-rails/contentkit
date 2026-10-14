package content

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/media"
)

// httpError carries an explicit status for handler-level failures (validation,
// auth). Sentinel resolver errors are mapped separately in writeErr.
type httpError struct {
	status int
	msg    string
}

func (e httpError) Error() string { return e.msg }

func badRequest(format string, a ...any) httpError {
	return httpError{status: http.StatusBadRequest, msg: fmt.Sprintf(format, a...)}
}

var (
	errContentChanged = httpError{status: http.StatusConflict, msg: "content changed while being screened; retry the edit"}
	errUnauthorized   = httpError{status: http.StatusUnauthorized, msg: "authentication required"}
	errForbidden      = httpError{status: http.StatusForbidden, msg: "forbidden"}
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

// Stable public error codes: the machine-readable half of ContentKit's error
// body. Clients branch on Code; Error is a human message and may change.
const (
	CodeInvalidRequest     = "invalid_request"
	CodeUnauthorized       = "unauthorized"
	CodeForbidden          = "forbidden"
	CodeNotFound           = "not_found"
	CodeConflict           = "conflict"
	CodeModerationRejected = "moderation_rejected"
	// CodeRateLimited: too many of one interaction; the body carries action
	// and retry_after (seconds, as the Retry-After header). -> 429
	CodeRateLimited = "rate_limited"
	// CodeCommentBanned: the caller is banned from commenting on this target;
	// the body's ban says the scope, reason and until. -> 403
	CodeCommentBanned = "comment_banned"
	// CodeCommentsClosed: the target's comments are closed or hidden. -> 403
	CodeCommentsClosed = "comments_closed"
	// CodeCommentsLocked: the caller can't open the content and its kind's
	// locked view hides comments, so they take no writes from them. -> 403
	CodeCommentsLocked = "comments_locked"
	// CodeCommentTooLong: the comment is longer than Options.CommentMaxLength;
	// details.max is the limit in characters. -> 422
	CodeCommentTooLong = "comment_too_long"
	// CodeNotConfigured: the capability exists but the host never wired its
	// port (Media, AnswerClassifier). Retrying does not help. -> 501
	CodeNotConfigured = "not_configured"
	// CodeTenantMismatch: a host port answered with another tenant's data.
	// A configuration fault, not a client fault; the cause stays in the log.
	CodeTenantMismatch = "tenant_mismatch"
	// CodeUnavailable: the host's auth could not answer a permission check;
	// retry. -> 503
	CodeUnavailable = "unavailable"
	CodeInternal    = "internal_error"
)

// errorBody is ContentKit's flat error shape.
type errorBody struct {
	Error      string     `json:"error"`
	Code       string     `json:"code"`
	Action     Action     `json:"action,omitempty"`      // rate_limited
	RetryAfter int        `json:"retry_after,omitempty"` // rate_limited: seconds
	Ban        *BanNotice `json:"ban,omitempty"`         // comment_banned
	// Details.Max is the limit: comment_too_long.
	Details *media.ErrorDetails `json:"details,omitempty"`
}

// statusWriter records the response status and the cause of a 5xx (set by
// writeErr) for Runtime.accessLog. Status defaults to 200.
type statusWriter struct {
	http.ResponseWriter
	status      int
	internalErr error
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// writeErr answers with the mapped status, a public code and a safe message.
// Every 5xx hands its cause to accessLog through statusWriter and never puts
// it on the wire.
func writeErr(w http.ResponseWriter, err error) {
	status, code, msg := classifyErr(err)
	if status >= http.StatusInternalServerError {
		if sw, ok := w.(*statusWriter); ok {
			sw.internalErr = err
		}
	}
	body := errorBody{Error: msg, Code: code}
	var limited *RateLimitError
	if errors.As(err, &limited) {
		body.Action, body.RetryAfter = limited.Action, max(1, int(math.Ceil(limited.RetryAfter.Seconds())))
		w.Header().Set("Retry-After", strconv.Itoa(body.RetryAfter))
	}
	var banned *BannedError
	if errors.As(err, &banned) {
		body.Ban = &banned.BanNotice
	}
	var long *TooLongError
	if errors.As(err, &long) {
		body.Details = &media.ErrorDetails{Max: long.Max}
	}
	writeJSON(w, status, body)
}

// classifyErr maps an error to status, public code and safe message. Resolver
// sentinels hide existence (not-visible -> 404); authorization and identity
// failures are fail-closed; 5xx messages carry nothing internal.
func classifyErr(err error) (status int, code, msg string) {
	var banned *BannedError
	var long *TooLongError
	var authErr *AuthError
	switch {
	case errors.As(err, &authErr):
		if errors.Is(err, auth.ErrExpired) || errors.Is(err, auth.ErrRevoked) {
			return http.StatusUnauthorized, CodeUnauthorized, "authentication required"
		}
		return http.StatusServiceUnavailable, CodeUnavailable, "authentication is unavailable; retry"
	case errors.As(err, &long):
		return http.StatusUnprocessableEntity, CodeCommentTooLong, long.Error()
	case errors.Is(err, ErrRateLimited):
		return http.StatusTooManyRequests, CodeRateLimited, "too many requests; try again later"
	case errors.As(err, &banned):
		return http.StatusForbidden, CodeCommentBanned, banned.Error()
	case errors.Is(err, ErrCommentsClosed):
		return http.StatusForbidden, CodeCommentsClosed, "comments are closed here"
	case errors.Is(err, ErrCommentsLocked):
		return http.StatusForbidden, CodeCommentsLocked, "open the content to see and write its comments"
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNotVisible):
		return http.StatusNotFound, CodeNotFound, "not found"
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrSubjectErased):
		return http.StatusForbidden, CodeForbidden, "forbidden"
	case errors.Is(err, ErrNoClassifier):
		return http.StatusNotImplemented, CodeNotConfigured, "free-text polls need an AnswerClassifier"
	case errors.Is(err, errMediaNotConfigured):
		return http.StatusNotImplemented, CodeNotConfigured, "images need ContentKit media"
	case errors.Is(err, ErrTenant):
		return http.StatusInternalServerError, CodeTenantMismatch, "internal error"
	}
	var he httpError
	if errors.As(err, &he) {
		return he.status, codeForStatus(he.status), he.msg
	}
	var rej RejectedError
	if errors.As(err, &rej) {
		return http.StatusUnprocessableEntity, CodeModerationRejected, rej.Reason
	}
	return http.StatusInternalServerError, CodeInternal, "internal error"
}

// codeForStatus gives handler-level httpErrors a code without touching their
// construction sites.
func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return CodeInvalidRequest
	case http.StatusUnauthorized:
		return CodeUnauthorized
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusConflict:
		return CodeConflict
	case http.StatusNotImplemented:
		return CodeNotConfigured
	}
	return CodeInternal
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// orEmpty makes a list endpoint answer "no items" with [] rather than null.
func orEmpty[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

// decodeJSON reads a JSON body with a sane size cap.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return badRequest("%s", httpapi.BodyError(err))
	}
	return nil
}

// parsePage reads limit/offset with a default limit of 20 and a hard cap of 100.
func parsePage(req *http.Request) (limit, offset int) {
	limit, offset = 20, 0
	if v, err := strconv.Atoi(req.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > 100 {
		limit = 100
	}
	if v, err := strconv.Atoi(req.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = v
	}
	return limit, offset
}
