package content

import "errors"

// Sentinel errors a ContentResolver returns to gate a target; mapped to HTTP
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
)

// errUnsupportedMedia is the default MediaStore's response (no store wired).
var errUnsupportedMedia = errors.New("content: no MediaStore configured")
