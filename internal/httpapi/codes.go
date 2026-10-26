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
	// Message is what people are told when the handler states no rule of
	// its own; never an error chain.
	Message string
}

// The codes more than one module answers.
const (
	CodeInvalidRequest     = "invalid_request"
	CodeUnauthorized       = "unauthorized"
	CodeForbidden          = "forbidden"
	CodeNotFound           = "not_found"
	CodeConflict           = "conflict"
	CodeRateLimited        = "rate_limited"
	CodeUnavailable        = "unavailable"
	CodeUpgrade            = "upgrade_required"
	CodeInternal           = "internal_error"
	CodeTenantMismatch     = "tenant_mismatch"
	CodeLimitReached       = "limit_reached"
	CodeEventCursorExpired = "event_cursor_expired"
)

var codes = []ErrorCode{
	{CodeEventCursorExpired, http.StatusGone, "The event cursor predates retained history or names a reset stream; resync current state before continuing.", "Event history expired; resync current state."},
	{CodeInvalidRequest, http.StatusBadRequest, "The request is malformed: its body, a query parameter or a path value.", "The request is invalid."},
	{CodeUnauthorized, http.StatusUnauthorized, "The request needs a valid credential: it has none where the route needs one, or one that is expired, revoked or invalid. WWW-Authenticate says which.", "Sign in to do this."},
	{CodeForbidden, http.StatusForbidden, "The actor may not do this.", "You may not do this."},
	{"download_unavailable", http.StatusForbidden, "The host's download policy refuses this download now: `reason` is its code, `available_at` when it opens if a matter of time. Nothing was charged.", "This isn't available to download right now."},
	{"comment_banned", http.StatusForbidden, "The actor is banned from commenting on this target; `ban` says the scope, the reason and when it ends.", "You are banned from commenting here."},
	{"comments_closed", http.StatusForbidden, "The target's comments are closed or hidden: existing ones may show, new ones and edits are refused.", "Comments are closed here."},
	{"comments_locked", http.StatusForbidden, "The actor can't open the content (or the comment's version), so its comments take no writes from them; reads withhold them.", "Open the content to see and write its comments."},
	{"comment_moderated", http.StatusForbidden, "The comment is held for review or was rejected, so its author can't edit it; a reviewer's approval makes a held one editable again.", "A held or rejected comment can't be edited."},
	{"comment_too_long", http.StatusUnprocessableEntity, "The comment is longer than the site allows; `details.max` is the limit in characters.", "The comment is too long."},
	{CodeNotFound, http.StatusNotFound, "Nothing by that name, or nothing the actor may see.", "Not found."},
	{CodeConflict, http.StatusConflict, "A concurrent change, a path or slug already taken, or an operation id already used for another commit.", "This changed meanwhile or is already taken; reload and try again."},
	{"not_ready", http.StatusConflict, "A version that would go live still has files processing or a publication not yet current; `error` says what. Retry once processing finishes.", "Files are still processing; publish when they're done."},
	{"incomplete", http.StatusConflict, "A multipart upload is missing parts.", "The upload is missing parts."},
	{"not_joinable", http.StatusConflict, "The group has no free offer to join: it sells its membership, or nothing.", "This group isn't open to join right now."},
	{CodeLimitReached, http.StatusConflict, "The owner already has as many groups of the kind as the site allows (max_per_owner).", "You already have as many of these as the site allows."},
	{"no_download", http.StatusConflict, "The version has no file its kind's download serves: nothing uploaded, or its upload failed or was dropped.", "This version has nothing to download."},
	{"not_uploaded", http.StatusConflict, "A commit names an upload that has not landed; `blobs` lists the ones to upload again.", "A file did not finish uploading; upload it again."},
	{"too_many_files", http.StatusConflict, "The commit would put more files in an upload path, or the item, than allowed; `details.max` is the limit.", "Too many files."},
	{"gone", http.StatusGone, "The content was removed.", "This content was removed."},
	{"too_large", http.StatusRequestEntityTooLarge, "The file is larger than its upload path allows; `details` has `size` and `max_bytes`.", "The file is too large."},
	{"quota_exceeded", http.StatusRequestEntityTooLarge, "The uploader's storage quota is used up.", "Your storage is full."},
	{"image_too_large", http.StatusRequestEntityTooLarge, "The image has more pixels or frames than the processor decodes.", "The image is too large to process."},
	{"type_not_allowed", http.StatusUnsupportedMediaType, "The file type is not allowed in the upload path; `details.allowed` lists the types.", "This file type is not allowed here."},
	{"animation_unsupported", http.StatusUnsupportedMediaType, "An AVIF or HEIF image sequence, which is decoded as one frame.", "Animated AVIF and HEIF images are not supported."},
	{"moderation_rejected", http.StatusUnprocessableEntity, "The moderator refused the text; `error` says why.", "The text was rejected."},
	{"checksum_mismatch", http.StatusUnprocessableEntity, "The stored bytes differ from the declared SHA-256.", "The file changed on the way; upload it again."},
	{"image_too_small", http.StatusUnprocessableEntity, "The edited image is narrower than its public preset's minimum width.", "The image is too small."},
	{"image_unreadable", http.StatusUnprocessableEntity, "The file is not a decodable image of its declared type.", "The file is not a readable image."},
	{"animation_not_allowed", http.StatusUnprocessableEntity, "An animated image where the upload path refuses animation.", "Animated images are not allowed here."},
	{"animation_too_long", http.StatusUnprocessableEntity, "The animation has more frames or seconds than the processor allows.", "The animation is too long."},
	{"video_too_long", http.StatusUnprocessableEntity, "The video or audio runs longer than its upload path allows.", "The video is too long."},
	{"video_too_large", http.StatusUnprocessableEntity, "The video's frames are larger than its upload path allows.", "The video's frames are too large."},
	{"video_over_budget", http.StatusUnprocessableEntity, "The planned encode costs more than its upload path allows, or the source averages under a frame a second.", "The video would cost too much to encode."},
	{"video_aspect_unsupported", http.StatusUnprocessableEntity, "The video's display aspect is outside what its upload path accepts; `details` has `min_aspect` and `max_aspect` (width/height).", "The video's shape is not supported here."},
	{CodeRateLimited, http.StatusTooManyRequests, "Too many requests; retry after `retry_after` seconds (also the Retry-After header).", "Too many requests; try again later."},
	{CodeInternal, http.StatusInternalServerError, "A server fault; the cause is in the server log.", "Internal error."},
	{CodeTenantMismatch, http.StatusInternalServerError, "A host port answered with another tenant's data: a configuration fault.", "Internal error."},
	{"not_configured", http.StatusNotImplemented, "The capability needs a port the host did not configure; retrying does not help.", "This site does not offer this."},
	{CodeUnavailable, http.StatusServiceUnavailable, "The host's auth, media storage or signal plane cannot be reached, or a commit's recovery is pending; retry.", "Temporarily unavailable; try again."},
	{CodeUpgrade, http.StatusServiceUnavailable, "The media item was stored before v0.68 and must be migrated using ContentKit v0.119.1 before this release can read it.", "This media requires an administrator migration."},
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

// Message is code's message for people (Internal error's for an
// unregistered code).
func Message(code string) string {
	if c, ok := byCode[code]; ok {
		return c.Message
	}
	return byCode[CodeInternal].Message
}

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
