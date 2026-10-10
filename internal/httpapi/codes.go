package httpapi

import (
	"net/http"
	"sort"
)

// ErrorCode is one stable error code: every module answers the flat body
// {"error", "code", …}, where code is one of these with its status. Clients
// branch on the code; the message is for people and may change.
type ErrorCode struct {
	Code    string
	Status  int
	Meaning string
}

// The codes more than one module answers.
const (
	CodeInvalidRequest = "invalid_request"
	CodeUnauthorized   = "unauthorized"
	CodeForbidden      = "forbidden"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodeRateLimited    = "rate_limited"
	CodeUnavailable    = "unavailable"
	CodeInternal       = "internal_error"
	CodeTenantMismatch = "tenant_mismatch"
)

var codes = []ErrorCode{
	{CodeInvalidRequest, http.StatusBadRequest, "The request is malformed: its body, a query parameter or a path value."},
	{CodeUnauthorized, http.StatusUnauthorized, "The route needs a signed-in actor."},
	{CodeForbidden, http.StatusForbidden, "The actor may not do this."},
	{"comment_banned", http.StatusForbidden, "The actor is banned from commenting on this target; `ban` says the scope, the reason and when it ends."},
	{CodeNotFound, http.StatusNotFound, "Nothing by that name, or nothing the actor may see."},
	{CodeConflict, http.StatusConflict, "A concurrent change, a path or slug already taken, or an operation id already used for another commit."},
	{"incomplete", http.StatusConflict, "A multipart upload is missing parts."},
	{"not_uploaded", http.StatusConflict, "A commit names an upload that has not landed; `blobs` lists the ones to upload again."},
	{"too_many_files", http.StatusConflict, "The commit would put more files in an upload path than its rules allow."},
	{"gone", http.StatusGone, "The content was removed."},
	{"too_large", http.StatusRequestEntityTooLarge, "The file is larger than its upload path allows; `details` has `size` and `max_bytes`."},
	{"quota_exceeded", http.StatusRequestEntityTooLarge, "The uploader's storage quota is used up."},
	{"image_too_large", http.StatusRequestEntityTooLarge, "The image has more pixels or frames than the processor decodes."},
	{"type_not_allowed", http.StatusUnsupportedMediaType, "The file type is not allowed in the upload path; `details.allowed` lists the types."},
	{"animation_unsupported", http.StatusUnsupportedMediaType, "An AVIF or HEIF image sequence, which is decoded as one frame."},
	{"moderation_rejected", http.StatusUnprocessableEntity, "The moderator refused the text; `error` says why."},
	{"checksum_mismatch", http.StatusUnprocessableEntity, "The stored bytes differ from the declared SHA-256."},
	{"image_too_small", http.StatusUnprocessableEntity, "The edited image is narrower than its public preset's minimum width."},
	{"image_unreadable", http.StatusUnprocessableEntity, "The file is not a decodable image of its declared type."},
	{"animation_not_allowed", http.StatusUnprocessableEntity, "An animated image where the upload path refuses animation."},
	{"animation_too_long", http.StatusUnprocessableEntity, "The animation has more frames or seconds than the processor allows."},
	{"video_too_long", http.StatusUnprocessableEntity, "The video or audio runs longer than its upload path allows."},
	{"video_too_large", http.StatusUnprocessableEntity, "The video's frames are larger than its upload path allows."},
	{"video_over_budget", http.StatusUnprocessableEntity, "The planned encode costs more than its upload path allows, or the source averages under a frame a second."},
	{CodeRateLimited, http.StatusTooManyRequests, "Too many requests; retry after `retry_after` seconds (also the Retry-After header)."},
	{CodeInternal, http.StatusInternalServerError, "A server fault; the cause is in the server log."},
	{CodeTenantMismatch, http.StatusInternalServerError, "A host port answered with another tenant's data: a configuration fault."},
	{"not_configured", http.StatusNotImplemented, "The capability needs a port the host did not configure; retrying does not help."},
	{CodeUnavailable, http.StatusServiceUnavailable, "Media storage cannot be reached, or a commit's recovery is pending; retry."},
}

var byCode = func() map[string]ErrorCode {
	m := make(map[string]ErrorCode, len(codes))
	for _, c := range codes {
		if _, dup := m[c.Code]; dup {
			panic("httpapi: error code registered twice: " + c.Code)
		}
		m[c.Code] = c
	}
	return m
}()

// LookupErrorCode returns a registered code.
func LookupErrorCode(code string) (ErrorCode, bool) {
	c, ok := byCode[code]
	return c, ok
}

// ErrorCodes is every registered code, sorted.
func ErrorCodes() []ErrorCode {
	out := append([]ErrorCode(nil), codes...)
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}
