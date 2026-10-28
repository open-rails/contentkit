package media

import (
	"net/http"
	"time"
)

const (
	CodeInvalid = "invalid_request" // 400
)

const (
	CodeForbidden = "forbidden" // 403
)

const (
	CodeNotFound = "not_found" // 404: unknown kind, file or upload
)

const (
	CodeConflict = "conflict" // 409: a path taken, or a new item's folder not empty
)

const (
	CodeIncomplete = "incomplete" // 409: multipart parts missing
)

const (
	CodeNotUploaded = "not_uploaded" // 409: commit before the object landed
)

const (
	CodeTooManyFiles = "too_many_files" // 409: the commit would exceed an Upload's Max
)

const (
	CodeTooLarge = "too_large" // 413: over the Upload's MaxBytes
)

const (
	CodeQuota = "quota_exceeded" // 413: owner quota
)

const (
	CodeType = "type_not_allowed" // 415
)

const (
	CodeChecksum = "checksum_mismatch" // 422: stored bytes differ from the declared hash
)

const (
	CodeRate = "rate_limited" // 429
)

const (
	CodeUnavailable = "unavailable" // 503: the bucket cannot be reached; retry
)

const (
	CodeUpgrade = "upgrade_required" // 503: the item's publication is being restored
)

const (
	// Image refusals (ImageError): the rules refuse the image; never a server fault.
	CodeImageTooSmall = "image_too_small" // 422: edited narrower than the public preset's MinWidth
)

const (
	CodeImageTooLarge = "image_too_large" // 413: more pixels than the processor decodes
)

const (
	CodeImageUnreadable = "image_unreadable" // 422: not a decodable image of its declared type
)

const (
	CodeAnimationNotAllowed = "animation_not_allowed" // 422: an animated image where the policy is AnimationReject
)

const (
	CodeAnimationTooLong = "animation_too_long" // 422: more frames or seconds than the processor allows
)

const (
	CodeAnimationUnsupported = "animation_unsupported" // 415: an AVIF/HEIF image sequence (decoded as one frame)
)

const (
	// Video refusals (ImageError, recorded as the upload's Failure): its
	// Upload.Video limits refuse what the real stream would cost.
	CodeVideoTooLong = "video_too_long" // 422: runs longer than MaxSeconds (audio too)
)

const (
	CodeVideoTooLarge = "video_too_large" // 422: frames larger than MaxPixels
)

const (
	CodeVideoOverBudget = "video_over_budget" // 422: the planned encode is over MaxWork, or the source averages under a frame a second
)

const (
	// CodeVideoAspectUnsupported: the display aspect is outside its HLS
	// presets' MinAspect and MaxAspect.
	CodeVideoAspectUnsupported = "video_aspect_unsupported" // 422
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
	CodeUnavailable:  http.StatusServiceUnavailable,
	CodeUpgrade:      http.StatusServiceUnavailable,

	CodeImageTooSmall:   http.StatusUnprocessableEntity,
	CodeImageTooLarge:   http.StatusRequestEntityTooLarge,
	CodeImageUnreadable: http.StatusUnprocessableEntity,

	CodeAnimationNotAllowed:  http.StatusUnprocessableEntity,
	CodeAnimationTooLong:     http.StatusUnprocessableEntity,
	CodeAnimationUnsupported: http.StatusUnsupportedMediaType,

	CodeVideoTooLong:    http.StatusUnprocessableEntity,
	CodeVideoTooLarge:   http.StatusUnprocessableEntity,
	CodeVideoOverBudget: http.StatusUnprocessableEntity,

	CodeVideoAspectUnsupported: http.StatusUnprocessableEntity,
}

// ErrorDetails qualifies an image refusal so clients can state the rule.
type ErrorDetails struct {
	Width      int      `json:"width,omitempty"`       // image_too_small: the edited width; image_too_large, video_too_large, video_aspect_unsupported: the source's
	Height     int      `json:"height,omitempty"`      // image_too_large, video_too_large, video_aspect_unsupported: the source's
	MinWidth   int      `json:"min_width,omitempty"`   // image_too_small
	MaxPixels  int      `json:"max_pixels,omitempty"`  // image_too_large, video_too_large
	Type       string   `json:"type,omitempty"`        // the declared type (image_unreadable, type_not_allowed, too_large)
	Allowed    []string `json:"allowed,omitempty"`     // type_not_allowed: the kind's types
	Size       int64    `json:"size,omitempty"`        // too_large: bytes
	MaxBytes   int64    `json:"max_bytes,omitempty"`   // too_large
	Frames     int      `json:"frames,omitempty"`      // animation_too_long, image_too_large: the animation's
	MaxFrames  int      `json:"max_frames,omitempty"`  // animation_too_long
	Seconds    float64  `json:"seconds,omitempty"`     // animation_too_long, video_too_long: running time
	MaxSeconds float64  `json:"max_seconds,omitempty"` // animation_too_long, video_too_long
	MinAspect  float64  `json:"min_aspect,omitempty"`  // video_aspect_unsupported: the narrowest width/height accepted
	MaxAspect  float64  `json:"max_aspect,omitempty"`  // video_aspect_unsupported: the widest width/height accepted
	Max        int      `json:"max,omitempty"`         // too_many_files: the files allowed; comment_too_long: the characters allowed
}

// ImageError is an image the rules refuse, synchronously (an edit checked
// against known dims) or in the image job (recorded as the upload's Failure).
type ImageError struct {
	Code    string
	Message string
	Details ErrorDetails
}

func (e *ImageError) Error() string { return e.Message }

// UploadError is a refused upload request. UploadLimiter implementations
// refuse with CodeRate or CodeQuota.
type UploadError struct {
	Code       string
	Message    string
	RetryAfter time.Duration // CodeRate: when the window frees
	Blobs      []string      // CodeNotUploaded at commit: the blobs to upload again
	Details    *ErrorDetails // image refusals
}

func (e *UploadError) Error() string { return "media: " + e.Message }

// Status is the HTTP status for Code.
func (e *UploadError) Status() int {
	if s, ok := codeStatus[e.Code]; ok {
		return s
	}
	return http.StatusInternalServerError
}
