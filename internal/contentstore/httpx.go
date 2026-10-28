package contentstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/open-rails/authkit/auth"
	content "github.com/open-rails/contentkit/content"
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
	errDuplicateComment = errors.New("content: duplicate comment")
)

var (
	errContentChanged = httpError{status: http.StatusConflict, msg: "content changed while being screened; retry the edit"}
)

var (
	errUnauthorized = httpError{status: http.StatusUnauthorized, msg: "authentication required"}
)

var (
	errForbidden = httpError{status: http.StatusForbidden, msg: "forbidden"}
)

var (
	// errCommentModerated refuses an author's edit of their held or rejected
	// comment. -> 403 comment_moderated
	errCommentModerated = errors.New("content: comment is held or rejected")
)

// errorBody is ContentKit's flat error shape.
type errorBody struct {
	Error      string             `json:"error"`
	Code       string             `json:"code"`
	Action     content.Action     `json:"action,omitempty"`      // rate_limited
	RetryAfter int                `json:"retry_after,omitempty"` // rate_limited: seconds
	Ban        *content.BanNotice `json:"ban,omitempty"`         // comment_banned
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
	var limited *content.RateLimitError
	if errors.As(err, &limited) {
		body.Action, body.RetryAfter = limited.Action, max(1, int(math.Ceil(limited.RetryAfter.Seconds())))
		w.Header().Set("Retry-After", strconv.Itoa(body.RetryAfter))
	}
	var banned *content.BannedError
	if errors.As(err, &banned) {
		body.Ban = &banned.BanNotice
	}
	var long *content.TooLongError
	if errors.As(err, &long) {
		body.Details = &media.ErrorDetails{Max: long.Max}
	}
	writeJSON(w, status, body)
}

// classifyErr maps an error to status, public code and safe message. Resolver
// sentinels hide existence (not-visible -> 404); authorization and identity
// failures are fail-closed; 5xx messages carry nothing internal.
func classifyErr(err error) (status int, code, msg string) {
	var banned *content.BannedError
	var long *content.TooLongError
	var authErr *content.AuthError
	switch {
	case errors.As(err, &authErr):
		if errors.Is(err, auth.ErrExpired) || errors.Is(err, auth.ErrRevoked) {
			return http.StatusUnauthorized, content.CodeUnauthorized, "authentication required"
		}
		return http.StatusServiceUnavailable, content.CodeUnavailable, "authentication is unavailable; retry"
	case errors.Is(err, errDuplicateComment):
		return http.StatusUnprocessableEntity, content.CodeDuplicateComment, "duplicate comment"
	case errors.As(err, &long):
		return http.StatusUnprocessableEntity, content.CodeCommentTooLong, long.Error()
	case errors.Is(err, content.ErrRateLimited):
		return http.StatusTooManyRequests, content.CodeRateLimited, "too many requests; try again later"
	case errors.As(err, &banned):
		return http.StatusForbidden, content.CodeCommentBanned, banned.Error()
	case errors.Is(err, ErrCommentsClosed):
		return http.StatusForbidden, content.CodeCommentsClosed, "comments are closed here"
	case errors.Is(err, ErrCommentsLocked):
		return http.StatusForbidden, content.CodeCommentsLocked, "open the content to see and write its comments"
	case errors.Is(err, errCommentModerated):
		return http.StatusForbidden, content.CodeCommentModerated, "a held or rejected comment can't be edited"
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNotVisible):
		return http.StatusNotFound, content.CodeNotFound, "not found"
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrSubjectErased):
		return http.StatusForbidden, content.CodeForbidden, "forbidden"
	case errors.Is(err, errMediaNotConfigured):
		return http.StatusNotImplemented, content.CodeNotConfigured, "images need ContentKit media"
	case errors.Is(err, ErrTenant):
		return http.StatusInternalServerError, content.CodeTenantMismatch, "internal error"
	}
	var he httpError
	if errors.As(err, &he) {
		return he.status, codeForStatus(he.status), he.msg
	}
	var rej content.RejectedError
	if errors.As(err, &rej) {
		return http.StatusUnprocessableEntity, content.CodeModerationRejected, rej.Reason
	}
	return http.StatusInternalServerError, content.CodeInternal, "internal error"
}

// codeForStatus gives handler-level httpErrors a code without touching their
// construction sites.
func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return content.CodeInvalidRequest
	case http.StatusUnauthorized:
		return content.CodeUnauthorized
	case http.StatusForbidden:
		return content.CodeForbidden
	case http.StatusNotFound:
		return content.CodeNotFound
	case http.StatusConflict:
		return content.CodeConflict
	case http.StatusNotImplemented:
		return content.CodeNotConfigured
	}
	return content.CodeInternal
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
