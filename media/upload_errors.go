package media

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Stable upload error codes: clients (the browser SDK) branch on Code.
const (
	CodeInvalid      = "invalid_request"   // 400
	CodeForbidden    = "forbidden"         // 403
	CodeNotFound     = "not_found"         // 404: unknown kind, file or upload
	CodeConflict     = "conflict"          // 409: file name taken
	CodeIncomplete   = "incomplete"        // 409: multipart parts missing
	CodeNotUploaded  = "not_uploaded"      // 409: commit before the object landed
	CodeTooManyFiles = "too_many_files"    // 409: the commit would exceed the kind's file caps
	CodeTooLarge     = "too_large"         // 413: over the kind's cap
	CodeQuota        = "quota_exceeded"    // 413: owner quota
	CodeType         = "type_not_allowed"  // 415
	CodeChecksum     = "checksum_mismatch" // 422: stored bytes differ from the declared hash
	CodeRate         = "rate_limited"      // 429
)

var codeStatus = map[string]int{
	CodeInvalid:      http.StatusBadRequest,
	CodeForbidden:    http.StatusForbidden,
	CodeNotFound:     http.StatusNotFound,
	CodeConflict:     http.StatusConflict,
	CodeIncomplete:   http.StatusConflict,
	CodeNotUploaded:  http.StatusConflict,
	CodeTooManyFiles: http.StatusConflict,
	CodeTooLarge:     http.StatusRequestEntityTooLarge,
	CodeQuota:        http.StatusRequestEntityTooLarge,
	CodeType:         http.StatusUnsupportedMediaType,
	CodeChecksum:     http.StatusUnprocessableEntity,
	CodeRate:         http.StatusTooManyRequests,
}

// UploadError is a refused upload request. UploadLimiter implementations
// refuse with CodeRate or CodeQuota.
type UploadError struct {
	Code       string
	Message    string
	RetryAfter time.Duration // CodeRate: when the window frees
	Originals  []string      // CodeNotUploaded at commit: the originals to upload again
}

func (e *UploadError) Error() string { return "media: " + e.Message }

// Status is the HTTP status for Code.
func (e *UploadError) Status() int {
	if s, ok := codeStatus[e.Code]; ok {
		return s
	}
	return http.StatusInternalServerError
}

func uploadErr(code, format string, a ...any) *UploadError {
	return &UploadError{Code: code, Message: fmt.Sprintf(format, a...)}
}

// AsUploadError classifies err: an *UploadError, or the kind and registry
// sentinels. ok is false for internal failures.
func AsUploadError(err error) (*UploadError, bool) {
	var ue *UploadError
	switch {
	case errors.As(err, &ue):
		return ue, true
	case errors.Is(err, ErrType):
		return &UploadError{Code: CodeType, Message: err.Error()}, true
	case errors.Is(err, ErrTooLarge):
		return &UploadError{Code: CodeTooLarge, Message: err.Error()}, true
	case errors.Is(err, ErrUnknownKind):
		return &UploadError{Code: CodeNotFound, Message: err.Error()}, true
	}
	return nil, false
}
