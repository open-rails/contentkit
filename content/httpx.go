package content

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
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
	CodeUnprocessable      = "unprocessable"
	// CodeNotConfigured: the capability exists but the host never wired its
	// port (Media, AnswerClassifier). Retrying does not help. -> 501
	CodeNotConfigured = "not_configured"
	// CodeTenantMismatch: a host port answered with another tenant's data.
	// A configuration fault, not a client fault; the cause stays in the log.
	CodeTenantMismatch = "tenant_mismatch"
	CodeInternal       = "internal_error"
)

// errorBody is ContentKit's flat error shape.
type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
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
	writeJSON(w, status, errorBody{Error: msg, Code: code})
}

// classifyErr maps an error to status, public code and safe message. Resolver
// sentinels hide existence (not-visible -> 404); authorization and identity
// failures are fail-closed; 5xx messages carry nothing internal.
func classifyErr(err error) (status int, code, msg string) {
	switch {
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
	case http.StatusUnprocessableEntity:
		return CodeUnprocessable
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
		return badRequest("invalid JSON body: %v", err)
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
